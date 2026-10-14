package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// OpenAI Responses API（Codex）协议适配层（POST /v1/responses）
// ---------------------------------------------------------------------------
//
// 与 /v1/messages（Anthropic）同构：本端点不是第二条上游管线，而是「协议翻译层」。
// 把 Responses API 请求体翻译成网关既有的 OpenAI Chat Completions 请求，复用
// chatCompletions 的选号 / 轮转 / 冷却 / 熔断 / 会话粘性 / 在途租约 / 统计 / 成本
// 记账全链路，再把 OpenAI 响应（非流式 JSON 或 SSE）翻译回 Responses 协议。
//
// 这样 Codex 流量与 OpenAI / Anthropic 流量共享同一套容错与治理语义，不存在两套
// 调度逻辑漂移的风险。适配层只关心「协议形态」，不碰「调度决策」。

// responsesRequest 是 Responses API 请求体中网关需要理解的最小子集。
// 未列出的字段（store / include / text / metadata / previous_response_id /
// truncation / stream_options 等）直接忽略。
type responsesRequest struct {
	Model             string              `json:"model"`
	Instructions      json.RawMessage     `json:"instructions"`
	Input             json.RawMessage     `json:"input"`
	Tools             []responsesTool     `json:"tools"`
	ToolChoice        json.RawMessage     `json:"tool_choice"`
	MaxOutputTokens   *int                `json:"max_output_tokens"`
	Temperature       *float64            `json:"temperature"`
	TopP              *float64            `json:"top_p"`
	ParallelToolCalls *bool               `json:"parallel_tool_calls"`
	Stream            bool                `json:"stream"`
	Reasoning         *responsesReasoning `json:"reasoning"`
	PromptCacheKey    string              `json:"prompt_cache_key"`
}

// responsesReasoning 只取 effort（→ reasoning_effort）；summary 等忽略。
type responsesReasoning struct {
	Effort string `json:"effort"`
}

// responsesTool 是 Responses API 的扁平工具声明（name 在顶层，与 Chat Completions
// 的嵌套 function 形态不同）。非 function 类型（web_search / local_shell 等内建
// 工具）在翻译时跳过。
type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict"`
}

// responses 处理 POST /v1/responses：OpenAI Responses API 端点（Codex）。
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "model: field is required")
		return
	}
	if len(req.Input) == 0 {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "input: at least one input item is required")
		return
	}
	model := h.mapCodexModel(req.Model)
	openaiBody, err := responsesToOpenAI(&req, model)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	// 复用 chatCompletions 全链路：克隆请求以保留 requestTrace（上下文里的 trace
	// 会被 requestTraceFrom 取到），只替换 body 为翻译后的 OpenAI 请求。
	bridge := newResponsesBridge(w, req.Stream, model)
	synth := r.Clone(r.Context())
	synth.Body = io.NopCloser(bytes.NewReader(openaiBody))
	synth.ContentLength = int64(len(openaiBody))
	synth.Header.Set("Content-Type", "application/json")
	h.chatCompletions(bridge, synth)
	bridge.finalize()
}

// mapCodexModel 把 Codex 客户端模型名映射成网关模型名：
//  1. 显式 codex.model_map 命中优先（值可带 cn:/global: realm 前缀）；
//  2. 否则 codex.default_model 非空时兜底；
//  3. 都没有 → 原样透传（用户可直接写网关模型名，如 glm-5.2）。
//
// 为什么不像 Anthropic 那样对未映射的「OpenAI 系」模型名报 400：网关自身的模型
// 目录里就有 gpt-5.5 / gpt-5.6-sol / gpt-6-astra 等以 gpt- 开头的真实模型，前缀
// 判定必然误伤；而 Codex 的模型名固定在配置里，透传未映射名由上游给出真实报错
// 比网关臆测更可靠。需要强制某个模型时配 codex.default_model 即可。
func (h *Handler) mapCodexModel(model string) string {
	if mapped, ok := h.cfg.CodexModelMap[model]; ok && mapped != "" {
		return mapped
	}
	if h.cfg.CodexDefaultModel != "" {
		return h.cfg.CodexDefaultModel
	}
	return model
}

// responsesToOpenAI 把 Responses API 请求翻译成 OpenAI Chat Completions 请求。
func responsesToOpenAI(req *responsesRequest, model string) ([]byte, error) {
	out := map[string]any{"model": model}
	if req.Stream {
		out["stream"] = true
	}
	if req.MaxOutputTokens != nil && *req.MaxOutputTokens > 0 {
		out["max_tokens"] = *req.MaxOutputTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if req.ParallelToolCalls != nil {
		out["parallel_tool_calls"] = *req.ParallelToolCalls
	}
	// prompt_cache_key 透传：session.ExtractKey 认它做会话粘性键（Codex 用它承载
	// 会话级前缀缓存键），带上才能让同一会话尽量落在同一账号。
	if req.PromptCacheKey != "" {
		out["prompt_cache_key"] = req.PromptCacheKey
	}
	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		out["reasoning_effort"] = req.Reasoning.Effort
	}

	msgs := make([]any, 0)
	if sys := responsesInstructionsText(req.Instructions); sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}
	inputMsgs, err := responsesInputToMessages(req.Input)
	if err != nil {
		return nil, err
	}
	msgs = append(msgs, inputMsgs...)
	if len(msgs) == 0 {
		return nil, fmt.Errorf("input: no usable input items")
	}
	out["messages"] = msgs

	if tools := responsesToolsToOpenAI(req.Tools); len(tools) > 0 {
		out["tools"] = tools
		if tc := responsesToolChoice(req.ToolChoice); tc != nil {
			out["tool_choice"] = tc
		}
	}
	return json.Marshal(out)
}

// responsesInstructionsText 取 instructions（字符串形态）。
func responsesInstructionsText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// responsesInputToMessages 把 Responses 的 input（字符串或 input item 数组）翻译成
// OpenAI messages：
//   - message item → 同 role 的 {content}；content part 的 input_text/output_text
//     拍成 text part，input_image 拍成 image_url part；
//   - function_call item → 归并进一条 assistant.tool_calls（连续多条合一条）；
//   - function_call_output item → role=tool 消息（tool_call_id 取 call_id）；
//   - reasoning item → 忽略（上游没有等价结构）。
func responsesInputToMessages(raw json.RawMessage) ([]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	// 字符串简写形态：整段当作一条 user 消息。
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if strings.TrimSpace(s) == "" {
			return nil, nil
		}
		return []any{map[string]any{"role": "user", "content": s}}, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("input must be a string or an array of input items")
	}
	msgs := make([]any, 0, len(items)+1)
	var pending []any
	flushTools := func() {
		if len(pending) > 0 {
			msgs = append(msgs, map[string]any{"role": "assistant", "content": nil, "tool_calls": pending})
			pending = nil
		}
	}
	for _, rawItem := range items {
		var obj map[string]any
		if json.Unmarshal(rawItem, &obj) != nil {
			continue
		}
		switch str(obj["type"]) {
		case "function_call":
			id := str(obj["call_id"])
			if id == "" {
				id = str(obj["id"])
			}
			args := str(obj["arguments"])
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			pending = append(pending, map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      str(obj["name"]),
					"arguments": args,
				},
			})
		case "function_call_output":
			flushTools()
			msgs = append(msgs, map[string]any{
				"role":         "tool",
				"tool_call_id": str(obj["call_id"]),
				"content":      responsesOutputText(obj["output"]),
			})
		case "reasoning":
			// 忽略：上游无等价结构（思维链以 reasoning_content 形式回传，但请求侧
			// 不需要回放）。
		default:
			// message（type 缺省或显式 "message"）：无 role 视为不可用项，跳过。
			role := str(obj["role"])
			if role == "" {
				continue
			}
			flushTools()
			msgs = append(msgs, map[string]any{
				"role":    role,
				"content": responsesContentToOpenAI(obj["content"]),
			})
		}
	}
	flushTools()
	return msgs, nil
}

// responsesContentToOpenAI 翻译 message item 的 content：字符串原样返回；数组按
// part 类型拍平（input_text/output_text/text → text part；input_image → image_url）。
func responsesContentToOpenAI(content any) any {
	if s, ok := content.(string); ok {
		return s
	}
	arr, ok := content.([]any)
	if !ok {
		return ""
	}
	parts := make([]any, 0, len(arr))
	for _, p := range arr {
		pm, _ := p.(map[string]any)
		if pm == nil {
			continue
		}
		switch str(pm["type"]) {
		case "input_text", "output_text", "text":
			parts = append(parts, map[string]any{"type": "text", "text": str(pm["text"])})
		case "input_image":
			if url := str(pm["image_url"]); url != "" {
				parts = append(parts, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": url},
				})
			}
		case "refusal":
			if s := str(pm["refusal"]); s != "" {
				parts = append(parts, map[string]any{"type": "text", "text": s})
			}
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return parts
}

// responsesOutputText 把 function_call_output.output（字符串或 content 数组）拍平成文本。
func responsesOutputText(output any) string {
	switch v := output.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, item := range v {
			m, _ := item.(map[string]any)
			if m == nil {
				continue
			}
			if s := str(m["text"]); s != "" {
				b.WriteString(s)
			}
		}
		return b.String()
	case nil:
		return ""
	}
	raw, _ := json.Marshal(output)
	return string(raw)
}

// responsesToolsToOpenAI 把扁平工具声明翻译成 Chat Completions 的 function tools。
// 非 function 类型（web_search / local_shell 等）跳过——上游没有对应能力。
func responsesToolsToOpenAI(tools []responsesTool) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		if t.Name == "" {
			continue
		}
		fn := map[string]any{"name": t.Name}
		if t.Description != "" {
			fn["description"] = t.Description
		}
		if len(t.Parameters) > 0 {
			var schema any
			if json.Unmarshal(t.Parameters, &schema) == nil {
				fn["parameters"] = schema
			}
		}
		if t.Strict != nil {
			fn["strict"] = *t.Strict
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// responsesToolChoice 把 Responses 的 tool_choice 翻译成 Chat Completions 形态：
// 字符串 auto/none/required 原样；{type:"function",name} → 指定函数对象
// （上游 normalizeToolChoice 会再归一化成字符串名）。
func responsesToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto", "none", "required":
			return s
		}
		return nil
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Type == "function" && obj.Name != "" {
		return map[string]any{"type": "function", "function": map[string]any{"name": obj.Name}}
	}
	return nil
}

// responsesBridge 是 chatCompletions 与客户端之间的写入边界：把 chatCompletions
// 写出的 OpenAI 字节（非流式 JSON 或 SSE）翻译成 Responses 协议。
//
// 与 anthropicBridge 同构：chatCompletions 只通过 http.ResponseWriter 的
// Header/WriteHeader/Write/Flush 对外输出，包一层就能在「不改调度、不改上游调用、
// 不改统计」的前提下完成协议转换。
type responsesBridge struct {
	dst       http.ResponseWriter
	streaming bool
	model     string

	status     int
	gotStatus  bool
	headerSent bool

	// 非流式（或失败）响应先缓冲，finalize 时一次性翻译。
	buf bytes.Buffer

	// 流式翻译状态（仅 streaming=true 时非空）。
	stream *responsesStreamState
}

func newResponsesBridge(dst http.ResponseWriter, streaming bool, model string) *responsesBridge {
	b := &responsesBridge{dst: dst, streaming: streaming, model: model}
	if streaming {
		b.stream = newResponsesStreamState(dst)
		b.stream.model = model
	}
	return b
}

func (b *responsesBridge) Header() http.Header { return b.dst.Header() }

// WriteHeader 只记录状态，不立即下发——流式成功时 StreamHint 根本不显式写头，
// 非流式 / 失败时要等 finalize 翻译完再决定真实状态码与 Content-Type。
func (b *responsesBridge) WriteHeader(status int) {
	if b.gotStatus {
		return
	}
	b.status = status
	b.gotStatus = true
}

func (b *responsesBridge) Write(p []byte) (int, error) {
	// 流式成功路径：status 未定（StreamHint 不显式写头）或 <400 时，把 OpenAI SSE
	// 逐帧翻译成 Responses SSE。
	if b.streaming && (!b.gotStatus || b.status < http.StatusBadRequest) {
		if !b.headerSent {
			b.sendHeader(http.StatusOK)
		}
		if err := b.stream.write(p); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	// 其余情况（非流式成功 / 任意错误）：先缓冲，finalize 统一翻译。
	return b.buf.Write(p)
}

func (b *responsesBridge) Flush() {
	if f, ok := b.dst.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 保留底层 writer 的透传能力（与 logging.responseObserver 同约定）。
func (b *responsesBridge) Unwrap() http.ResponseWriter { return b.dst }

func (b *responsesBridge) sendHeader(status int) {
	b.dst.WriteHeader(status)
	b.headerSent = true
}

// finalize 在 chatCompletions 返回后调用，把缓冲的 OpenAI 响应翻译成 Responses
// 响应；流式成功时无需补写（response.completed 已随 [DONE] 落地）。
func (b *responsesBridge) finalize() {
	if b.streaming && (!b.gotStatus || b.status < http.StatusBadRequest) {
		if !b.headerSent {
			// 理论到不了这里（StreamHint 至少写一帧 + [DONE]）；兜底避免空响应。
			b.sendHeader(http.StatusOK)
		}
		return
	}

	status := b.status
	if status == 0 {
		status = http.StatusOK
	}
	if status >= http.StatusBadRequest {
		code, msg := parseOpenAIErrorBody(b.buf.Bytes())
		if msg == "" {
			msg = http.StatusText(status)
		}
		writeResponsesError(b.dst, status, responsesErrorType(status, code), msg)
		return
	}

	var resp map[string]any
	if err := json.Unmarshal(b.buf.Bytes(), &resp); err != nil {
		writeResponsesError(b.dst, http.StatusBadGateway, "server_error", "failed to decode upstream response: "+err.Error())
		return
	}
	writeJSON(b.dst, http.StatusOK, openAIToResponsesResponse(resp))
}

// openAIToResponsesResponse 把非流式 OpenAI chat.completion 翻译成 Responses response。
func openAIToResponsesResponse(resp map[string]any) map[string]any {
	responseID := "resp_" + randomID(24)
	if id := str(resp["id"]); id != "" {
		responseID = "resp_" + sanitizeID(id)
	}
	created := time.Now().Unix()
	if c, ok := jsonInt(resp["created"]); ok && c > 0 {
		created = int64(c)
	}

	// function_call 条目先收集，文本 message 条目统一排在其前（对齐官方输出顺序）。
	var calls []any
	var text string
	hasText := false
	if choices, ok := resp["choices"].([]any); ok && len(choices) > 0 {
		choice, _ := choices[0].(map[string]any)
		if choice != nil {
			if m, _ := choice["message"].(map[string]any); m != nil {
				if txt := str(m["content"]); txt != "" {
					text, hasText = txt, true
				}
				if tcs, ok := m["tool_calls"].([]any); ok {
					for _, tci := range tcs {
						tc, _ := tci.(map[string]any)
						if tc == nil {
							continue
						}
						fn, _ := tc["function"].(map[string]any)
						callID := str(tc["id"])
						if callID == "" {
							callID = "call_" + randomID(20)
						}
						calls = append(calls, map[string]any{
							"type":      "function_call",
							"id":        "fc_" + randomID(20),
							"call_id":   callID,
							"name":      str(fn["name"]),
							"arguments": str(fn["arguments"]),
							"status":    "completed",
						})
					}
				}
			}
		}
	}
	output := make([]any, 0, len(calls)+1)
	if hasText {
		output = append(output, map[string]any{
			"type":   "message",
			"id":     "msg_" + randomID(24),
			"role":   "assistant",
			"status": "completed",
			"content": []any{
				map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
			},
		})
	}
	output = append(output, calls...)

	final := map[string]any{
		"id":                  responseID,
		"object":              "response",
		"created_at":          created,
		"status":              "completed",
		"model":               str(resp["model"]),
		"output":              output,
		"parallel_tool_calls": true,
		"usage":               responsesUsage(resp["usage"]),
	}
	return final
}

// responsesUsage 把 OpenAI usage 换算成 Responses usage 口径。
func responsesUsage(raw any) map[string]any {
	usage, _ := raw.(map[string]any)
	in, _ := jsonInt(usage["prompt_tokens"])
	out, _ := jsonInt(usage["completion_tokens"])
	total, _ := jsonInt(usage["total_tokens"])
	if total == 0 {
		total = in + out
	}
	return map[string]any{
		"input_tokens":  in,
		"output_tokens": out,
		"total_tokens":  total,
	}
}

// writeResponsesError 写 Responses/OpenAI 风格错误信封：
// {"error":{"message":...,"type":...,"code":...}}。
func writeResponsesError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    code,
			"code":    code,
		},
	})
}

// responsesErrorType 把 HTTP 状态码 / 网关错误码映射成 Responses error.type。
func responsesErrorType(status int, code string) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "invalid_request_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable, http.StatusBadGateway:
		return "server_error"
	}
	if code == "model_unavailable" {
		return "invalid_request_error"
	}
	return "server_error"
}

// ---------------------------------------------------------------------------
// 流式翻译：OpenAI chat.completion.chunk SSE → Responses API SSE
// ---------------------------------------------------------------------------
//
// 事件序列（对齐官方 Responses 流）：
//
//	response.created → response.in_progress
//	→ 文本：response.output_item.added / content_part.added
//	        → output_text.delta ... → output_text.done / content_part.done
//	        → output_item.done
//	→ 工具：response.output_item.added(function_call)
//	        → function_call_arguments.delta ... → function_call_arguments.done
//	        → output_item.done
//	→ response.completed（带完整 output + usage）
//
// 上游 error 帧翻译成 response.failed 事件。OpenAI 的 data: [DONE] 不外泄。
type responsesStreamState struct {
	w http.ResponseWriter

	started  bool
	finished bool
	errored  bool
	seq      int

	responseID string
	createdAt  int64
	model      string

	nextIndex int

	// 文本 message 条目状态（惰性开启）。
	msgIndex int
	msgID    string
	msgOpen  bool
	textBuf  strings.Builder

	// 工具调用条目：OpenAI tool_call index → 累积器；toolOrder 保序。
	toolIndex map[int]int
	tools     map[int]*responsesToolAccum
	toolOrder []int

	// 已完成条目（output_index → item），收尾按 index 排序组装 output 数组。
	items map[int]map[string]any

	inputTokens  int
	outputTokens int
	totalTokens  int

	buf bytes.Buffer
}

// responsesToolAccum 累积单个 function_call 的条目状态。
type responsesToolAccum struct {
	itemID string
	callID string
	name   string
	index  int
	args   strings.Builder
}

func newResponsesStreamState(w http.ResponseWriter) *responsesStreamState {
	return &responsesStreamState{
		w:         w,
		msgIndex:  -1,
		toolIndex: map[int]int{},
		tools:     map[int]*responsesToolAccum{},
		items:     map[int]map[string]any{},
	}
}

// write 累积字节并按空行切分 SSE 帧，逐帧翻译。
func (s *responsesStreamState) write(p []byte) error {
	s.buf.Write(p)
	for {
		data := s.buf.Bytes()
		i := bytes.Index(data, []byte("\n\n"))
		if i < 0 {
			break
		}
		frame := string(data[:i])
		rest := append([]byte(nil), data[i+2:]...)
		s.buf.Reset()
		s.buf.Write(rest)
		if err := s.handleFrame(frame); err != nil {
			return err
		}
	}
	return nil
}

// handleFrame 取出帧里的 data 负载（忽略 event:/注释行）并翻译。
func (s *responsesStreamState) handleFrame(frame string) error {
	var payload strings.Builder
	for _, line := range strings.Split(frame, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		if payload.Len() > 0 {
			payload.WriteString("\n")
		}
		payload.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
	}
	if payload.Len() == 0 {
		return nil
	}
	return s.handleData(payload.String())
}

func (s *responsesStreamState) handleData(payload string) error {
	if payload == "[DONE]" {
		return s.finish()
	}
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		// 非 JSON 帧（上游异常/注释）：协议不同，丢弃。
		return nil
	}
	if errObj, ok := obj["error"].(map[string]any); ok {
		return s.emitError(errObj)
	}
	if err := s.ensureStart(obj); err != nil {
		return err
	}
	if usage, ok := obj["usage"].(map[string]any); ok {
		if v, ok := jsonInt(usage["prompt_tokens"]); ok {
			s.inputTokens = v
		}
		if v, ok := jsonInt(usage["completion_tokens"]); ok {
			s.outputTokens = v
		}
		if v, ok := jsonInt(usage["total_tokens"]); ok {
			s.totalTokens = v
		}
	}
	choices, _ := obj["choices"].([]any)
	for _, ci := range choices {
		ch, _ := ci.(map[string]any)
		if ch == nil {
			continue
		}
		if delta, _ := ch["delta"].(map[string]any); delta != nil {
			if err := s.handleDelta(delta); err != nil {
				return err
			}
		}
	}
	return nil
}

// ensureStart 在首个有效帧上发 response.created + response.in_progress（幂等）。
func (s *responsesStreamState) ensureStart(obj map[string]any) error {
	if s.started {
		return nil
	}
	s.started = true
	if model := str(obj["model"]); model != "" {
		s.model = model
	}
	if id := str(obj["id"]); id != "" {
		s.responseID = "resp_" + sanitizeID(id)
	} else {
		s.responseID = "resp_" + randomID(24)
	}
	s.createdAt = time.Now().Unix()
	if err := s.emit("response.created", map[string]any{
		"type":     "response.created",
		"response": s.responseObject("in_progress", []any{}, nil),
	}); err != nil {
		return err
	}
	return s.emit("response.in_progress", map[string]any{
		"type":     "response.in_progress",
		"response": s.responseObject("in_progress", []any{}, nil),
	})
}

func (s *responsesStreamState) handleDelta(delta map[string]any) error {
	if txt, ok := delta["content"].(string); ok && txt != "" {
		if err := s.ensureMessage(); err != nil {
			return err
		}
		s.textBuf.WriteString(txt)
		return s.emit("response.output_text.delta", map[string]any{
			"type":          "response.output_text.delta",
			"item_id":       s.msgID,
			"output_index":  s.msgIndex,
			"content_index": 0,
			"delta":         txt,
		})
	}
	if tcs, ok := delta["tool_calls"].([]any); ok {
		for _, tci := range tcs {
			tc, _ := tci.(map[string]any)
			if tc == nil {
				continue
			}
			if err := s.handleToolCall(tc); err != nil {
				return err
			}
		}
	}
	return nil
}

// ensureMessage 惰性开启文本 message 条目并发出 added 事件（幂等）。
func (s *responsesStreamState) ensureMessage() error {
	if s.msgOpen {
		return nil
	}
	s.msgIndex = s.nextIndex
	s.nextIndex++
	s.msgID = "msg_" + randomID(24)
	s.msgOpen = true
	if err := s.emit("response.output_item.added", map[string]any{
		"type":         "response.output_item.added",
		"output_index": s.msgIndex,
		"item":         s.messageItem("", "in_progress"),
	}); err != nil {
		return err
	}
	return s.emit("response.content_part.added", map[string]any{
		"type":          "response.content_part.added",
		"item_id":       s.msgID,
		"output_index":  s.msgIndex,
		"content_index": 0,
		"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
}

// closeMessage 收尾文本 message 条目（output_text.done → content_part.done →
// output_item.done）并记入 items。
func (s *responsesStreamState) closeMessage() error {
	if !s.msgOpen {
		return nil
	}
	s.msgOpen = false
	text := s.textBuf.String()
	if err := s.emit("response.output_text.done", map[string]any{
		"type":          "response.output_text.done",
		"item_id":       s.msgID,
		"output_index":  s.msgIndex,
		"content_index": 0,
		"text":          text,
	}); err != nil {
		return err
	}
	part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
	if err := s.emit("response.content_part.done", map[string]any{
		"type":          "response.content_part.done",
		"item_id":       s.msgID,
		"output_index":  s.msgIndex,
		"content_index": 0,
		"part":          part,
	}); err != nil {
		return err
	}
	s.items[s.msgIndex] = s.messageItem(text, "completed")
	return s.emit("response.output_item.done", map[string]any{
		"type":         "response.output_item.done",
		"output_index": s.msgIndex,
		"item":         s.items[s.msgIndex],
	})
}

func (s *responsesStreamState) messageItem(text, status string) map[string]any {
	content := []any{}
	if text != "" {
		content = append(content, map[string]any{"type": "output_text", "text": text, "annotations": []any{}})
	}
	return map[string]any{
		"type":    "message",
		"id":      s.msgID,
		"role":    "assistant",
		"status":  status,
		"content": content,
	}
}

// handleToolCall 把一段 delta.tool_calls 翻译成 function_call 条目：
// 首片开条目（带 id/name），后续 arguments 分片作为 arguments.delta 透传。
func (s *responsesStreamState) handleToolCall(tc map[string]any) error {
	idx := 0
	if v, ok := jsonInt(tc["index"]); ok {
		idx = v
	}
	fn, _ := tc["function"].(map[string]any)
	acc, seen := s.tools[idx]
	if !seen {
		if err := s.closeMessage(); err != nil {
			return err
		}
		acc = &responsesToolAccum{
			itemID: "fc_" + randomID(20),
			callID: str(tc["id"]),
			name:   str(fn["name"]),
			index:  s.nextIndex,
		}
		if acc.callID == "" {
			acc.callID = "call_" + randomID(20)
		}
		s.nextIndex++
		s.tools[idx] = acc
		s.toolOrder = append(s.toolOrder, idx)
		if err := s.emit("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": acc.index,
			"item":         s.toolItem(acc, "in_progress"),
		}); err != nil {
			return err
		}
	}
	args := str(fn["arguments"])
	if args == "" {
		return nil
	}
	acc.args.WriteString(args)
	return s.emit("response.function_call_arguments.delta", map[string]any{
		"type":         "response.function_call_arguments.delta",
		"item_id":      acc.itemID,
		"output_index": acc.index,
		"delta":        args,
	})
}

func (s *responsesStreamState) toolItem(acc *responsesToolAccum, status string) map[string]any {
	return map[string]any{
		"type":      "function_call",
		"id":        acc.itemID,
		"call_id":   acc.callID,
		"name":      acc.name,
		"arguments": acc.args.String(),
		"status":    status,
	}
}

// finish 在收到 [DONE] 时收尾：关闭文本 / 工具条目 → response.completed。
func (s *responsesStreamState) finish() error {
	if s.finished || s.errored {
		return nil
	}
	s.finished = true
	if !s.started {
		if err := s.ensureStart(map[string]any{}); err != nil {
			return err
		}
	}
	if err := s.closeMessage(); err != nil {
		return err
	}
	for _, idx := range s.toolOrder {
		acc := s.tools[idx]
		args := acc.args.String()
		if err := s.emit("response.function_call_arguments.done", map[string]any{
			"type":         "response.function_call_arguments.done",
			"item_id":      acc.itemID,
			"output_index": acc.index,
			"arguments":    args,
		}); err != nil {
			return err
		}
		s.items[acc.index] = s.toolItem(acc, "completed")
		if err := s.emit("response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": acc.index,
			"item":         s.items[acc.index],
		}); err != nil {
			return err
		}
	}
	total := s.totalTokens
	if total == 0 {
		total = s.inputTokens + s.outputTokens
	}
	usage := map[string]any{
		"input_tokens":  s.inputTokens,
		"output_tokens": s.outputTokens,
		"total_tokens":  total,
	}
	return s.emit("response.completed", map[string]any{
		"type":     "response.completed",
		"response": s.responseObject("completed", s.outputArray(), usage),
	})
}

// emitError 把上游 error 帧翻译成 response.failed 事件，并阻止后续收尾。
func (s *responsesStreamState) emitError(errObj map[string]any) error {
	s.errored = true
	if !s.started {
		if err := s.ensureStart(map[string]any{}); err != nil {
			return err
		}
	}
	msg := str(errObj["message"])
	if msg == "" {
		msg = "upstream error"
	}
	code := str(errObj["code"])
	if code == "" {
		code = "server_error"
	}
	resp := s.responseObject("failed", []any{}, nil)
	resp["error"] = map[string]any{"code": code, "message": msg}
	return s.emit("response.failed", map[string]any{
		"type":     "response.failed",
		"response": resp,
	})
}

// responseObject 组装 response 信封（created/in_progress/completed/failed 共用）。
func (s *responsesStreamState) responseObject(status string, output []any, usage map[string]any) map[string]any {
	if output == nil {
		output = []any{}
	}
	return map[string]any{
		"id":                  s.responseID,
		"object":              "response",
		"created_at":          s.createdAt,
		"status":              status,
		"model":               s.model,
		"output":              output,
		"parallel_tool_calls": true,
		"usage":               usage,
	}
}

// outputArray 按 output_index 升序组装 output 数组。
func (s *responsesStreamState) outputArray() []any {
	if len(s.items) == 0 {
		return []any{}
	}
	idxs := make([]int, 0, len(s.items))
	for i := range s.items {
		idxs = append(idxs, i)
	}
	sortInts(idxs)
	out := make([]any, 0, len(idxs))
	for _, i := range idxs {
		out = append(out, s.items[i])
	}
	return out
}

// emit 写一帧 Responses SSE（event: + data:，含 sequence_number）并 flush。
func (s *responsesStreamState) emit(event string, data map[string]any) error {
	data["sequence_number"] = s.seq
	s.seq++
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, raw); err != nil {
		return err
	}
	if f, ok := s.w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}
