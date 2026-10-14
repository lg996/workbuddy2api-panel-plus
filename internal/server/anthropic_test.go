package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// anthropicSSEText 是「一段正文 + 末帧 usage」的 OpenAI 流，用于 Anthropic
// 端点的非流式聚合与流式翻译两条路径。
const anthropicSSEText = `data: {"id":"chatcmpl-anthropic","object":"chat.completion.chunk","created":1753600000,"model":"glm-5.2","choices":[{"index":0,"delta":{"role":"assistant","content":"你好"}}]}

data: {"id":"chatcmpl-anthropic","object":"chat.completion.chunk","created":1753600000,"model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}

data: [DONE]

`

// anthropicSSETool 是「首片声明 tool_call + 后续片拼 arguments + finish_reason
// tool_calls」的 OpenAI 流，覆盖 tool_use 方向的翻译。
const anthropicSSETool = `data: {"id":"chatcmpl-tool","object":"chat.completion.chunk","created":1753600000,"model":"glm-5.2","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}

data: {"id":"chatcmpl-tool","object":"chat.completion.chunk","created":1753600000,"model":"glm-5.2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"SF\"}"}}]}}]}

data: {"id":"chatcmpl-tool","object":"chat.completion.chunk","created":1753600000,"model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":8,"total_tokens":28}}

data: [DONE]

`

// fakeChatUpstream 记录每次出站请求体，并按固定 SSE 流响应。
// 用于断言「Anthropic 请求被翻译成了什么 OpenAI body」以及「有没有打上游」。
type fakeChatUpstream struct {
	client *upstream.Client
	mu     sync.Mutex
	bodies [][]byte
}

func newFakeChatUpstream(respBody string) *fakeChatUpstream {
	f := &fakeChatUpstream{}
	f.client = &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			b, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.bodies = append(f.bodies, b)
			f.mu.Unlock()
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(respBody)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	return f
}

func (f *fakeChatUpstream) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *fakeChatUpstream) last() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		return nil
	}
	return f.bodies[len(f.bodies)-1]
}

// anthropicHandler 构造一个带单账号池的 Anthropic 端点 handler。
func anthropicHandler(cfg Config, up *upstream.Client) *Handler {
	cfg.Pool = testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	cfg.Upstream = up
	return NewHandler(cfg)
}

// TestAnthropicAuthAcceptsBearerAndAPIKey 覆盖 /v1/messages 的双头鉴权：
// Anthropic SDK 默认发 x-api-key，OpenAI 兼容客户端复用 Bearer，两者都放行；
// 缺失/错误按 Anthropic 错误信封回 401。
func TestAnthropicAuthAcceptsBearerAndAPIKey(t *testing.T) {
	fake := newFakeChatUpstream(anthropicSSEText)
	h := anthropicHandler(Config{APIKey: "secret", AnthropicDefaultModel: "glm-5.2"}, fake.client)
	body := `{"model":"claude-3-5-sonnet","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`

	cases := []struct {
		name  string
		setup func(*http.Request)
		want  int
	}{
		{"x-api-key", func(r *http.Request) { r.Header.Set("x-api-key", "secret") }, http.StatusOK},
		{"bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer secret") }, http.StatusOK},
		{"missing", func(*http.Request) {}, http.StatusUnauthorized},
		{"wrong", func(r *http.Request) { r.Header.Set("x-api-key", "nope") }, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
			tc.setup(req)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("code=%d want %d body=%s", rec.Code, tc.want, rec.Body)
			}
			if tc.want != http.StatusUnauthorized {
				return
			}
			var env map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("401 body not json: %v (%s)", err, rec.Body)
			}
			if env["type"] != "error" {
				t.Errorf("envelope type=%v want error", env["type"])
			}
			errObj, _ := env["error"].(map[string]any)
			if errObj["type"] != "authentication_error" {
				t.Errorf("error.type=%v want authentication_error", errObj["type"])
			}
		})
	}
}

// TestAnthropicMessagesNonStreamText 覆盖非流式文本往返：请求侧 system/消息/
// max_tokens 翻译成 OpenAI，响应侧聚合结果翻译回 Anthropic Message。
func TestAnthropicMessagesNonStreamText(t *testing.T) {
	fake := newFakeChatUpstream(anthropicSSEText)
	h := anthropicHandler(Config{AnthropicDefaultModel: "glm-5.2"}, fake.client)
	body := `{"model":"claude-3-5-sonnet","max_tokens":64,"system":"be nice","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	// 出站 body：model 已映射，system 提为 messages[0]，max_tokens 保留。
	var out map[string]any
	if err := json.Unmarshal(fake.last(), &out); err != nil {
		t.Fatalf("outbound body not json: %v (%s)", err, fake.last())
	}
	if out["model"] != "glm-5.2" {
		t.Errorf("outbound model=%v want glm-5.2", out["model"])
	}
	if out["max_tokens"] != float64(64) {
		t.Errorf("outbound max_tokens=%v want 64", out["max_tokens"])
	}
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("outbound messages=%v want 2 entries", out["messages"])
	}
	sys, _ := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "be nice" {
		t.Errorf("messages[0]=%v want system/be nice", msgs[0])
	}
	user, _ := msgs[1].(map[string]any)
	if user["role"] != "user" || user["content"] != "hi" {
		t.Errorf("messages[1]=%v want user/hi", msgs[1])
	}

	// 入站响应：Anthropic Message 信封 + text 块 + usage 口径换算。
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not json: %v (%s)", err, rec.Body)
	}
	if resp["type"] != "message" || resp["role"] != "assistant" {
		t.Errorf("response type/role=%v/%v want message/assistant", resp["type"], resp["role"])
	}
	content, _ := resp["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content=%v want 1 block", resp["content"])
	}
	blk, _ := content[0].(map[string]any)
	if blk["type"] != "text" || blk["text"] != "你好" {
		t.Errorf("content[0]=%v want text/你好", content[0])
	}
	if resp["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason=%v want end_turn", resp["stop_reason"])
	}
	usage, _ := resp["usage"].(map[string]any)
	if usage["input_tokens"] != float64(7) || usage["output_tokens"] != float64(3) {
		t.Errorf("usage=%v want input=7 output=3", usage)
	}
}

// TestAnthropicMessagesToolUse 覆盖工具调用双向翻译：请求侧 tools/tool_choice
// → OpenAI function tools；响应侧 tool_calls → tool_use 块 + stop_reason=tool_use。
func TestAnthropicMessagesToolUse(t *testing.T) {
	fake := newFakeChatUpstream(anthropicSSETool)
	h := anthropicHandler(Config{}, fake.client)
	body := `{"model":"glm-5.2","max_tokens":64,` +
		`"tools":[{"name":"get_weather","description":"lookup","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],` +
		`"tool_choice":{"type":"tool","name":"get_weather"},` +
		`"messages":[{"role":"user","content":"weather in SF?"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	var out map[string]any
	if err := json.Unmarshal(fake.last(), &out); err != nil {
		t.Fatalf("outbound body not json: %v (%s)", err, fake.last())
	}
	tools, _ := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("outbound tools=%v want 1", out["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "function" {
		t.Errorf("tool.type=%v want function", tool["type"])
	}
	fn, _ := tool["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["description"] != "lookup" {
		t.Errorf("tool.function=%v want get_weather/lookup", fn)
	}
	if _, ok := fn["parameters"].(map[string]any); !ok {
		t.Errorf("tool.function.parameters missing: %v", fn)
	}
	// 网关出站前会把 tool_choice 对象归一化成上游认的字符串形式
	// （upstream.normalizeToolChoice），这里断言归一化后的最终形态。
	if out["tool_choice"] != "get_weather" {
		t.Errorf("outbound tool_choice=%v want get_weather", out["tool_choice"])
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not json: %v (%s)", err, rec.Body)
	}
	content, _ := resp["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content=%v want 1 tool_use block", resp["content"])
	}
	blk, _ := content[0].(map[string]any)
	if blk["type"] != "tool_use" || blk["name"] != "get_weather" {
		t.Errorf("content[0]=%v want tool_use/get_weather", content[0])
	}
	if blk["id"] != "call_1" {
		t.Errorf("tool_use id=%v want call_1", blk["id"])
	}
	input, _ := blk["input"].(map[string]any)
	if input["city"] != "SF" {
		t.Errorf("tool_use input=%v want city=SF", blk["input"])
	}
	if resp["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason=%v want tool_use", resp["stop_reason"])
	}
}

// TestAnthropicMessagesStream 覆盖流式翻译：OpenAI SSE 帧应产出 Anthropic
// 事件序列（message_start → content_block_* → message_delta → message_stop）。
func TestAnthropicMessagesStream(t *testing.T) {
	fake := newFakeChatUpstream(anthropicSSEText)
	h := anthropicHandler(Config{}, fake.client)
	body := `{"model":"glm-5.2","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("content-type=%q want text/event-stream", ct)
	}
	stream := rec.Body.String()
	order := []string{
		"event: message_start",
		"event: content_block_start",
		"event: content_block_delta",
		"event: content_block_stop",
		"event: message_delta",
		"event: message_stop",
	}
	last := -1
	for _, ev := range order {
		i := strings.Index(stream, ev)
		if i < 0 {
			t.Errorf("missing %q in stream:\n%s", ev, stream)
			continue
		}
		if i < last {
			t.Errorf("%q out of order in stream:\n%s", ev, stream)
		}
		last = i
	}
	if !strings.Contains(stream, "你好") {
		t.Errorf("stream missing text delta:\n%s", stream)
	}
	if strings.Contains(stream, "data: [DONE]") {
		t.Errorf("OpenAI [DONE] sentinel leaked into Anthropic stream:\n%s", stream)
	}
}

// TestAnthropicModelMapping 覆盖模型映射四态：显式映射（含 realm 前缀剥离）、
// default_model 兜底、非 claude* 透传、未映射 claude* 报 400。
func TestAnthropicModelMapping(t *testing.T) {
	cases := []struct {
		name       string
		cfg        Config
		model      string
		wantStatus int
		wantOut    string // 非空时断言出站 model
	}{
		{
			name:       "explicit map with realm",
			cfg:        Config{AnthropicModelMap: map[string]string{"claude-x": "cn:glm-5.2"}},
			model:      "claude-x",
			wantStatus: http.StatusOK,
			wantOut:    "glm-5.2",
		},
		{
			name:       "default fallback",
			cfg:        Config{AnthropicDefaultModel: "glm-5.2"},
			model:      "claude-sonnet-4-5",
			wantStatus: http.StatusOK,
			wantOut:    "glm-5.2",
		},
		{
			name:       "non-claude passthrough",
			cfg:        Config{},
			model:      "glm-5.2",
			wantStatus: http.StatusOK,
			wantOut:    "glm-5.2",
		},
		{
			name:       "unmapped claude",
			cfg:        Config{},
			model:      "claude-sonnet-4-5",
			wantStatus: http.StatusBadRequest,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeChatUpstream(anthropicSSEText)
			h := anthropicHandler(tc.cfg, fake.client)
			body := `{"model":"` + tc.model + `","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))
			if rec.Code != tc.wantStatus {
				t.Fatalf("code=%d want %d body=%s", rec.Code, tc.wantStatus, rec.Body)
			}
			if tc.wantStatus != http.StatusOK {
				if fake.calls() != 0 {
					t.Errorf("unmapped model must not reach upstream, calls=%d", fake.calls())
				}
				var env map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
					t.Fatalf("error body not json: %v (%s)", err, rec.Body)
				}
				errObj, _ := env["error"].(map[string]any)
				if errObj["type"] != "invalid_request_error" {
					t.Errorf("error.type=%v want invalid_request_error", errObj["type"])
				}
				if msg, _ := errObj["message"].(string); !strings.Contains(msg, "anthropic.default_model") {
					t.Errorf("error message should point at config, got %q", msg)
				}
				return
			}
			var out map[string]any
			if err := json.Unmarshal(fake.last(), &out); err != nil {
				t.Fatalf("outbound body not json: %v (%s)", err, fake.last())
			}
			if out["model"] != tc.wantOut {
				t.Errorf("outbound model=%v want %v", out["model"], tc.wantOut)
			}
		})
	}
}

// TestAnthropicMessagesUpstreamError 覆盖上游错误透传：状态码保留、错误信封
// 翻译成 Anthropic error.type（429 → rate_limit_error）。
func TestAnthropicMessagesUpstreamError(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return http.StatusTooManyRequests, `{"code":6004,"msg":"rate limited"}`, false
	})
	h := anthropicHandler(Config{AnthropicDefaultModel: "glm-5.2"}, up)
	body := `{"model":"claude-3-5-sonnet","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429 body=%s", rec.Code, rec.Body)
	}
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("error body not json: %v (%s)", err, rec.Body)
	}
	if env["type"] != "error" {
		t.Errorf("envelope type=%v want error", env["type"])
	}
	errObj, _ := env["error"].(map[string]any)
	if errObj["type"] != "rate_limit_error" {
		t.Errorf("error.type=%v want rate_limit_error", errObj["type"])
	}
}

// TestAnthropicCountTokens 覆盖 count_tokens：本地估算返回正整数，且不发起
// 任何上游调用（Claude Code 用它做上下文预算，本地粗估即可）。
func TestAnthropicCountTokens(t *testing.T) {
	fake := newFakeChatUpstream(anthropicSSEText)
	h := anthropicHandler(Config{}, fake.client)
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hello world this is a longer test sentence"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not json: %v (%s)", err, rec.Body)
	}
	n, ok := resp["input_tokens"].(float64)
	if !ok || n <= 0 {
		t.Errorf("input_tokens=%v want positive number", resp["input_tokens"])
	}
	if fake.calls() != 0 {
		t.Errorf("count_tokens must not call upstream, calls=%d", fake.calls())
	}
}
