package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const responsesSSETextNoModel = `data: {"id":"chatcmpl-responses","object":"chat.completion.chunk","created":1753600000,"choices":[{"index":0,"delta":{"role":"assistant","content":"hello"}}]}

data: {"id":"chatcmpl-responses","object":"chat.completion.chunk","created":1753600000,"choices":[{"index":0,"delta":{"content":" world"}}]}

data: {"id":"chatcmpl-responses","object":"chat.completion.chunk","created":1753600000,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}

data: [DONE]

`

const responsesSSETool = `data: {"id":"chatcmpl-tool","object":"chat.completion.chunk","created":1753600000,"model":"glm-5.2","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}

data: {"id":"chatcmpl-tool","object":"chat.completion.chunk","created":1753600000,"model":"glm-5.2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\""}}]}}]}

data: {"id":"chatcmpl-tool","object":"chat.completion.chunk","created":1753600000,"model":"glm-5.2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"SF\"}"}}]}}]}

data: {"id":"chatcmpl-tool","object":"chat.completion.chunk","created":1753600000,"model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":8,"total_tokens":28}}

data: [DONE]

`

// responsesHandler 构造一个带单账号池的 Responses API handler。
func responsesHandler(cfg Config, up *upstream.Client) *Handler {
	cfg.Pool = testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	cfg.Upstream = up
	return NewHandler(cfg)
}

type responsesEvent struct {
	name string
	data map[string]any
	seq  int
}

// parseResponsesEvents 把 Responses SSE 解析成事件列表，供顺序与序号断言。
func parseResponsesEvents(t *testing.T, body string) []responsesEvent {
	t.Helper()
	var out []responsesEvent
	for _, block := range strings.Split(strings.TrimSpace(body), "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var name, payload string
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				payload = strings.TrimPrefix(line, "data: ")
			}
		}
		if name == "" || payload == "" {
			t.Fatalf("malformed Responses SSE block:\n%s", block)
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(payload), &data); err != nil {
			t.Fatalf("Responses SSE data not JSON: %v (%s)", err, payload)
		}
		seq, _ := data["sequence_number"].(float64)
		out = append(out, responsesEvent{name: name, data: data, seq: int(seq)})
	}
	return out
}

// TestResponsesAuthBearer 覆盖 Responses 端点的 Bearer 鉴权：缺失/错误回
// OpenAI 风格 401，正确密钥才进入上游。
func TestResponsesAuthBearer(t *testing.T) {
	fake := newFakeChatUpstream(responsesSSETextNoModel)
	h := responsesHandler(Config{APIKey: "secret", CodexDefaultModel: "glm-5.2"}, fake.client)
	body := `{"model":"gpt-5-codex","input":"hi"}`

	cases := []struct {
		name  string
		auth  string
		want  int
		calls int
	}{
		{"valid", "Bearer secret", http.StatusOK, 1},
		{"missing", "", http.StatusUnauthorized, 0},
		{"wrong", "Bearer nope", http.StatusUnauthorized, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := fake.calls()
			req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("code=%d want %d body=%s", rec.Code, tc.want, rec.Body)
			}
			if got := fake.calls() - before; got != tc.calls {
				t.Fatalf("upstream calls=%d want %d", got, tc.calls)
			}
			if tc.want != http.StatusUnauthorized {
				return
			}
			var env map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("401 body not JSON: %v (%s)", err, rec.Body)
			}
			errObj, _ := env["error"].(map[string]any)
			if errObj["code"] != "invalid_api_key" {
				t.Errorf("error.code=%v want invalid_api_key", errObj["code"])
			}
		})
	}
}

// TestResponsesNonStreamText 覆盖非流式文本往返与请求字段翻译。
func TestResponsesNonStreamText(t *testing.T) {
	fake := newFakeChatUpstream(responsesSSETextNoModel)
	h := responsesHandler(Config{CodexDefaultModel: "glm-5.2"}, fake.client)
	body := `{
		"model":"gpt-5-codex",
		"instructions":"be concise",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"max_output_tokens":64,
		"temperature":0.2,
		"top_p":0.9,
		"parallel_tool_calls":false,
		"reasoning":{"effort":"high"},
		"prompt_cache_key":"sess-1"
	}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	var out map[string]any
	if err := json.Unmarshal(fake.last(), &out); err != nil {
		t.Fatalf("outbound body not JSON: %v (%s)", err, fake.last())
	}
	if out["model"] != "glm-5.2" {
		t.Errorf("outbound model=%v want glm-5.2", out["model"])
	}
	if out["max_tokens"] != float64(64) || out["reasoning_effort"] != "high" {
		t.Errorf("outbound max_tokens/reasoning_effort=%v/%v", out["max_tokens"], out["reasoning_effort"])
	}
	if out["temperature"] != 0.2 || out["top_p"] != 0.9 || out["parallel_tool_calls"] != false {
		t.Errorf("outbound sampling fields=%v/%v/%v", out["temperature"], out["top_p"], out["parallel_tool_calls"])
	}
	if out["prompt_cache_key"] != "sess-1" {
		t.Errorf("outbound prompt_cache_key=%v want sess-1", out["prompt_cache_key"])
	}
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("outbound messages=%v want 2 entries", out["messages"])
	}
	sys, _ := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "be concise" {
		t.Errorf("messages[0]=%v want system/be concise", msgs[0])
	}
	user, _ := msgs[1].(map[string]any)
	if user["role"] != "user" {
		t.Errorf("messages[1]=%v want user", msgs[1])
	}
	parts, _ := user["content"].([]any)
	if len(parts) != 1 {
		t.Fatalf("user content=%v want one text part", user["content"])
	}
	part, _ := parts[0].(map[string]any)
	if part["type"] != "text" || part["text"] != "hi" {
		t.Errorf("user content[0]=%v want text/hi", parts[0])
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v (%s)", err, rec.Body)
	}
	if resp["object"] != "response" || resp["status"] != "completed" {
		t.Errorf("response object/status=%v/%v", resp["object"], resp["status"])
	}
	output, _ := resp["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output=%v want one message", resp["output"])
	}
	msg, _ := output[0].(map[string]any)
	if msg["type"] != "message" || msg["role"] != "assistant" {
		t.Errorf("output[0]=%v want assistant message", output[0])
	}
	content, _ := msg["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("message content=%v want one output_text", msg["content"])
	}
	textPart, _ := content[0].(map[string]any)
	if textPart["type"] != "output_text" || textPart["text"] != "hello world" {
		t.Errorf("output_text=%v want hello world", content[0])
	}
	usage, _ := resp["usage"].(map[string]any)
	if usage["input_tokens"] != float64(7) || usage["output_tokens"] != float64(2) || usage["total_tokens"] != float64(9) {
		t.Errorf("usage=%v want input=7 output=2 total=9", usage)
	}
}

// TestResponsesInputStringAndInstructions 覆盖 input 字符串简写与 instructions。
func TestResponsesInputStringAndInstructions(t *testing.T) {
	req := &responsesRequest{
		Model:        "gpt-5-codex",
		Instructions: json.RawMessage(`"be brief"`),
		Input:        json.RawMessage(`"hello"`),
	}
	raw, err := responsesToOpenAI(req, "glm-5.2")
	if err != nil {
		t.Fatalf("responsesToOpenAI: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("translated body not JSON: %v (%s)", err, raw)
	}
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages=%v want 2", out["messages"])
	}
	sys, _ := msgs[0].(map[string]any)
	user, _ := msgs[1].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "be brief" {
		t.Errorf("messages[0]=%v want system/be brief", msgs[0])
	}
	if user["role"] != "user" || user["content"] != "hello" {
		t.Errorf("messages[1]=%v want user/hello", msgs[1])
	}
}

// TestResponsesToolTranslation 覆盖工具声明、函数调用历史和函数调用结果的双向翻译。
func TestResponsesToolTranslation(t *testing.T) {
	fake := newFakeChatUpstream(responsesSSETool)
	h := responsesHandler(Config{}, fake.client)
	body := `{
		"model":"glm-5.2",
		"input":[
			{"type":"message","role":"user","content":"weather?"},
			{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"SF\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"sunny"}
		],
		"tools":[{"type":"function","name":"get_weather","description":"lookup","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}],
		"tool_choice":{"type":"function","name":"get_weather"}
	}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	var out map[string]any
	if err := json.Unmarshal(fake.last(), &out); err != nil {
		t.Fatalf("outbound body not JSON: %v (%s)", err, fake.last())
	}
	tools, _ := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("outbound tools=%v want 1", out["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	fn, _ := tool["function"].(map[string]any)
	if tool["type"] != "function" || fn["name"] != "get_weather" || fn["description"] != "lookup" {
		t.Errorf("outbound tool=%v", tools[0])
	}
	if out["tool_choice"] != "get_weather" {
		t.Errorf("outbound tool_choice=%v want get_weather", out["tool_choice"])
	}
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("outbound messages=%v want 3 entries", out["messages"])
	}
	assistant, _ := msgs[1].(map[string]any)
	tcs, _ := assistant["tool_calls"].([]any)
	if assistant["role"] != "assistant" || len(tcs) != 1 {
		t.Errorf("messages[1]=%v want assistant with one tool_call", msgs[1])
	}
	tc, _ := tcs[0].(map[string]any)
	tcFn, _ := tc["function"].(map[string]any)
	if tc["id"] != "call_1" || tcFn["name"] != "get_weather" || tcFn["arguments"] != `{"city":"SF"}` {
		t.Errorf("assistant tool_call=%v", tcs[0])
	}
	toolMsg, _ := msgs[2].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_1" || toolMsg["content"] != "sunny" {
		t.Errorf("messages[2]=%v want tool/call_1/sunny", msgs[2])
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v (%s)", err, rec.Body)
	}
	output, _ := resp["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output=%v want one function_call", resp["output"])
	}
	call, _ := output[0].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call_1" || call["name"] != "get_weather" {
		t.Errorf("output[0]=%v want function_call get_weather", output[0])
	}
	if call["arguments"] != `{"city":"SF"}` || call["status"] != "completed" {
		t.Errorf("function_call arguments/status=%v/%v", call["arguments"], call["status"])
	}
}

// TestResponsesModelMapping 覆盖显式映射、default 兜底和原样透传三态。
func TestResponsesModelMapping(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		model   string
		wantOut string
	}{
		{
			name:    "explicit map with realm",
			cfg:     Config{CodexModelMap: map[string]string{"gpt-5-codex": "cn:deepseek-v4"}},
			model:   "gpt-5-codex",
			wantOut: "deepseek-v4",
		},
		{
			name:    "default fallback",
			cfg:     Config{CodexDefaultModel: "glm-5.2"},
			model:   "gpt-5-codex",
			wantOut: "glm-5.2",
		},
		{
			name:    "passthrough",
			cfg:     Config{},
			model:   "glm-5.2",
			wantOut: "glm-5.2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeChatUpstream(responsesSSETextNoModel)
			h := responsesHandler(tc.cfg, fake.client)
			body := `{"model":"` + tc.model + `","input":"hi"}`
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
			}
			var out map[string]any
			if err := json.Unmarshal(fake.last(), &out); err != nil {
				t.Fatalf("outbound body not JSON: %v (%s)", err, fake.last())
			}
			if out["model"] != tc.wantOut {
				t.Errorf("outbound model=%v want %v", out["model"], tc.wantOut)
			}
		})
	}
}

// TestResponsesInvalidRequests 覆盖本地可判定的 400，且不消耗上游账号。
func TestResponsesInvalidRequests(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing model", `{"input":"hi"}`},
		{"missing input", `{"model":"gpt-5-codex"}`},
		{"invalid input", `{"model":"gpt-5-codex","input":42}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeChatUpstream(responsesSSETextNoModel)
			h := responsesHandler(Config{}, fake.client)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(tc.body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
			}
			if fake.calls() != 0 {
				t.Fatalf("invalid request reached upstream: calls=%d", fake.calls())
			}
			var env map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("error body not JSON: %v (%s)", err, rec.Body)
			}
			errObj, _ := env["error"].(map[string]any)
			if errObj["type"] != "invalid_request_error" {
				t.Errorf("error.type=%v want invalid_request_error", errObj["type"])
			}
		})
	}
}

// TestResponsesStreamTextEvents 覆盖流式文本事件顺序、sequence_number 和 [DONE] 隔离。
func TestResponsesStreamTextEvents(t *testing.T) {
	fake := newFakeChatUpstream(responsesSSETextNoModel)
	h := responsesHandler(Config{CodexDefaultModel: "glm-5.2"}, fake.client)
	body := `{"model":"gpt-5-codex","input":"hi","stream":true}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type=%q want text/event-stream", ct)
	}
	stream := rec.Body.String()
	if strings.Contains(stream, "data: [DONE]") {
		t.Fatalf("OpenAI [DONE] leaked into Responses stream:\n%s", stream)
	}
	events := parseResponsesEvents(t, stream)
	want := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}
	if len(events) != len(want) {
		t.Fatalf("events=%v want %v", events, want)
	}
	for i, ev := range events {
		if ev.name != want[i] {
			t.Errorf("event[%d]=%s want %s", i, ev.name, want[i])
		}
		if ev.seq != i {
			t.Errorf("event[%d].sequence_number=%d want %d", i, ev.seq, i)
		}
	}
	createdResp, _ := events[0].data["response"].(map[string]any)
	if createdResp["model"] != "glm-5.2" {
		t.Errorf("response.created model=%v want mapped model glm-5.2", createdResp["model"])
	}
	completedResp, _ := events[len(events)-1].data["response"].(map[string]any)
	if completedResp["status"] != "completed" {
		t.Errorf("response.completed status=%v want completed", completedResp["status"])
	}
	output, _ := completedResp["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("completed output=%v want one message", completedResp["output"])
	}
	msg, _ := output[0].(map[string]any)
	content, _ := msg["content"].([]any)
	part, _ := content[0].(map[string]any)
	if part["text"] != "hello world" {
		t.Errorf("completed output text=%v want hello world", part["text"])
	}
}

// TestResponsesStreamToolEvents 覆盖函数调用参数增量与 completed output。
func TestResponsesStreamToolEvents(t *testing.T) {
	fake := newFakeChatUpstream(responsesSSETool)
	h := responsesHandler(Config{}, fake.client)
	body := `{"model":"glm-5.2","input":"weather?","stream":true}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	stream := rec.Body.String()
	if strings.Contains(stream, "data: [DONE]") {
		t.Fatalf("OpenAI [DONE] leaked into Responses stream:\n%s", stream)
	}
	events := parseResponsesEvents(t, stream)
	names := map[string]int{}
	for _, ev := range events {
		names[ev.name]++
	}
	if names["response.function_call_arguments.delta"] != 2 {
		t.Errorf("arguments delta count=%d want 2", names["response.function_call_arguments.delta"])
	}
	if names["response.function_call_arguments.done"] != 1 || names["response.completed"] != 1 {
		t.Errorf("event counts=%v want one arguments.done and one completed", names)
	}
	completedResp, _ := events[len(events)-1].data["response"].(map[string]any)
	output, _ := completedResp["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("completed output=%v want one function_call", completedResp["output"])
	}
	call, _ := output[0].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call_1" || call["name"] != "get_weather" {
		t.Errorf("completed function_call=%v", output[0])
	}
	if call["arguments"] != `{"city":"SF"}` {
		t.Errorf("completed arguments=%v want {\"city\":\"SF\"}", call["arguments"])
	}
}

// TestResponsesUpstreamHTTPError 覆盖上游 HTTP 错误的状态码与 Responses 错误信封。
func TestResponsesUpstreamHTTPError(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return http.StatusTooManyRequests, `{"code":6004,"msg":"rate limited"}`, false
	})
	h := responsesHandler(Config{CodexDefaultModel: "glm-5.2", MaxRotate: 1}, up)
	body := `{"model":"gpt-5-codex","input":"hi"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429 body=%s", rec.Code, rec.Body)
	}
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("error body not JSON: %v (%s)", err, rec.Body)
	}
	errObj, _ := env["error"].(map[string]any)
	if errObj["type"] != "rate_limit_error" || errObj["code"] != "rate_limit_error" {
		t.Errorf("error envelope=%v want rate_limit_error", errObj)
	}
	if msg, _ := errObj["message"].(string); !strings.Contains(msg, "rate limited") {
		t.Errorf("error.message=%q should carry upstream text", msg)
	}
}

// TestResponsesStreamUpstreamErrorFrame 覆盖 200 + error 帧 → response.failed。
func TestResponsesStreamUpstreamErrorFrame(t *testing.T) {
	const upstreamError = "data: {\"error\":{\"message\":\"rate limited\",\"type\":\"rate_limit_error\",\"code\":\"6004\"}}\n\ndata: [DONE]\n\n"
	fake := newFakeChatUpstream(upstreamError)
	h := responsesHandler(Config{CodexDefaultModel: "glm-5.2"}, fake.client)
	body := `{"model":"gpt-5-codex","input":"hi","stream":true}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	stream := rec.Body.String()
	if strings.Contains(stream, "event: response.completed") {
		t.Fatalf("error stream must not complete:\n%s", stream)
	}
	if strings.Contains(stream, "data: [DONE]") {
		t.Fatalf("OpenAI [DONE] leaked into Responses stream:\n%s", stream)
	}
	events := parseResponsesEvents(t, stream)
	if len(events) != 3 {
		t.Fatalf("events=%v want created/in_progress/failed", events)
	}
	if events[0].name != "response.created" || events[1].name != "response.in_progress" || events[2].name != "response.failed" {
		t.Errorf("event sequence=%v want created/in_progress/failed", events)
	}
	failedResp, _ := events[2].data["response"].(map[string]any)
	errObj, _ := failedResp["error"].(map[string]any)
	if errObj["code"] != "6004" || errObj["message"] != "rate limited" {
		t.Errorf("response.failed error=%v want code 6004/message rate limited", errObj)
	}
}
