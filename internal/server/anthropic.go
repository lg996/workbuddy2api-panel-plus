package server

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Anthropic Messages 协议适配层（/v1/messages、/v1/messages/count_tokens）
// ---------------------------------------------------------------------------
//
// 这两个端点不是独立的上游管线，而是「协议翻译层」：把 Anthropic Messages
// 请求体翻译成网关既有的 OpenAI Chat Completions 请求，复用 chatCompletions
// 的选号 / 轮转 / 冷却 / 熔断 / 会话粘性 / 在途租约 / 统计 / 成本记账全链路，
// 再把 OpenAI 响应（非流式 JSON 或 SSE）翻译回 Anthropic 协议。
//
// 这样做的原因：调度与容错语义都长在 chatCompletions 上，另起一条 Messages
// 管线等于复制一整套并发 / 冷却 / 粘性逻辑，两处必然漂移。适配层只关心
// 「协议形态」，不碰「调度决策」。

// anthropicRequest 是 Anthropic Messages 请求体中网关需要理解的最小子集。
// 未列出的字段（metadata / service_tier 等）直接忽略。
type anthropicRequest struct {
	Model         string             `json:"model"`
	System        json.RawMessage    `json:"system"`
	Messages      []anthropicMessage `json:"messages"`
	MaxTokens     int                `json:"max_tokens"`
	Temperature   *float64           `json:"temperature"`
	TopP          *float64           `json:"top_p"`
	TopK          *int               `json:"top_k"`
	StopSequences []string           `json:"stop_sequences"`
	Stream        bool               `json:"stream"`
	Tools         []anthropicTool    `json:"tools"`
	ToolChoice    json.RawMessage    `json:"tool_choice"`
	Thinking      *anthropicThinking `json:"thinking"`
}

// anthropicThinking 表示扩展思考开关、预算与显式档位。
type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
	Effort       string `json:"effort,omitempty"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// anthropicMessages 处理 POST /v1/messages：Anthropic 原生 Messages 端点。
func (h *Handler) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "messages: at least one message is required")
		return
	}
	model, ok := h.mapAnthropicModel(req.Model)
	if !ok {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("model %q is not mapped: configure anthropic.default_model or anthropic.model_map", req.Model))
		return
	}
	openaiBody, err := anthropicToOpenAI(&req, model)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	// 复用 chatCompletions 全链路：克隆请求以保留 requestTrace（上下文里的
	// trace 会被 requestTraceFrom 取到），只替换 body 为翻译后的 OpenAI 请求。
	bridge := newAnthropicBridge(w, req.Stream)
	synth := r.Clone(r.Context())
	synth.Body = io.NopCloser(bytes.NewReader(openaiBody))
	synth.ContentLength = int64(len(openaiBody))
	synth.Header.Set("Content-Type", "application/json")
	h.chatCompletions(bridge, synth)
	bridge.finalize()
}

// anthropicCountTokens 处理 POST /v1/messages/count_tokens。
// 上游没有对应接口，这里按字符数本地粗估（见 estimateAnthropicTokens），
// 不发起网络调用——Claude Code 用它做上下文预算，精确值不是必需的。
func (h *Handler) anthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": estimateAnthropicTokens(&req)})
}

// mapAnthropicModel 把客户端模型名映射成网关模型名：
//  1. 显式 anthropic.model_map 命中优先（值可带 cn:/global: realm 前缀）；
//  2. 非 claude* 名称原样透传（用户可能直接写网关模型名，如 deepseek-v4）；
//  3. claude* 名称回落到 anthropic.default_model；
//  4. 都没有 → false，由调用方报 400 并提示配置。
//
// 为什么不给 claude* 硬编码默认映射：网关模型目录会变，静默把 claude 打到某个
// 具体模型上会让用户拿到与预期不符的输出。宁可报错要求显式配置。
func (h *Handler) mapAnthropicModel(model string) (string, bool) {
	if mapped, ok := h.cfg.AnthropicModelMap[model]; ok && mapped != "" {
		return mapped, true
	}
	if !strings.HasPrefix(strings.ToLower(model), "claude") {
		return model, true
	}
	if h.cfg.AnthropicDefaultModel != "" {
		return h.cfg.AnthropicDefaultModel, true
	}
	return "", false
}

// anthropicToOpenAI 把 Anthropic Messages 请求翻译成 OpenAI Chat Completions 请求。
func anthropicToOpenAI(req *anthropicRequest, model string) ([]byte, error) {
	out := map[string]any{"model": model}
	if req.Stream {
		out["stream"] = true
	}
	if req.MaxTokens > 0 {
		out["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	// top_k 无 OpenAI 等价字段，忽略（不报错：Anthropic 客户端默认会带）。
	if len(req.StopSequences) > 0 {
		out["stop"] = req.StopSequences
	}

	msgs := make([]any, 0, len(req.Messages)+1)
	if sys := anthropicSystemText(req.System); sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}
	for _, m := range req.Messages {
		translated, err := anthropicMessageToOpenAI(m)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, translated...)
	}
	out["messages"] = msgs

	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			fn := map[string]any{"name": t.Name}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if len(t.InputSchema) > 0 {
				var schema any
				if json.Unmarshal(t.InputSchema, &schema) == nil {
					fn["parameters"] = schema
				}
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		out["tools"] = tools
	}
	if tc := anthropicToolChoice(req.ToolChoice); tc != nil {
		out["tool_choice"] = tc
	}
	applyAnthropicThinking(out, model, req.Thinking)
	return json.Marshal(out)
}

// applyAnthropicThinking 把 Anthropic 的思考配置翻译成 Chat 的 reasoning_effort。
// 禁用思考在 DeepSeek 上需要显式 thinking.type=disabled；其他上游没有通用的
// “关闭思考”字段，因此不注入 off，避免被档位降级逻辑误认为最低档。
func applyAnthropicThinking(out map[string]any, model string, thinking *anthropicThinking) {
	if thinking == nil {
		return
	}
	typ := strings.ToLower(strings.TrimSpace(thinking.Type))
	switch typ {
	case "disabled":
		if isDeepSeekLikeModel(model) {
			out["thinking"] = map[string]any{"type": "disabled"}
			delete(out, "reasoning_effort")
			delete(out, "reasoningEffort")
		}
		return
	case "enabled", "adaptive":
	default:
		return
	}
	effort := strings.TrimSpace(thinking.Effort)
	if effort == "" {
		effort = anthropicBudgetToEffort(thinking.BudgetTokens)
	}
	out["reasoning_effort"] = effort
}

// anthropicBudgetToEffort 按预算区间选择档位。Anthropic 的 budget_tokens
// 与产品 effort 没有官方一一对应，这里采用单调区间，再由上游支持表降级。
func anthropicBudgetToEffort(budget int) string {
	switch {
	case budget <= 0:
		return "high"
	case budget < 4096:
		return "low"
	case budget < 8192:
		return "medium"
	case budget < 16384:
		return "high"
	case budget < 32000:
		return "xhigh"
	default:
		return "max"
	}
}

// isDeepSeekLikeModel 剥掉 cn:/global: 路由前缀后判断上游是否 DeepSeek 系。
func isDeepSeekLikeModel(model string) bool {
	_, bare := resolveModel(model)
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(bare)), "deepseek")
}

// anthropicSystemText 把 system 字段（字符串或 text block 数组）拍平成一段文本。
func anthropicSystemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Type == "text" && blk.Text != "" {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(blk.Text)
			}
		}
		return b.String()
	}
	return ""
}

// anthropicMessageToOpenAI 翻译单条消息。user 消息里的 tool_result 会拆成独立
// 的 role=tool 消息，因此返回的是「一条或多条」OpenAI 消息。
func anthropicMessageToOpenAI(m anthropicMessage) ([]any, error) {
	var plain string
	if json.Unmarshal(m.Content, &plain) == nil {
		if plain == "" {
			return nil, nil
		}
		return []any{map[string]any{"role": m.Role, "content": plain}}, nil
	}
	var blocks []map[string]any
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil, fmt.Errorf("messages content must be a string or an array of content blocks")
	}
	if m.Role == "assistant" {
		return assistantBlocksToOpenAI(blocks)
	}
	return userBlocksToOpenAI(m.Role, blocks)
}

// userBlocksToOpenAI 翻译 user 消息的 content block 数组。
func userBlocksToOpenAI(role string, blocks []map[string]any) ([]any, error) {
	parts := []any{}
	var toolMsgs []any
	for _, blk := range blocks {
		switch str(blk["type"]) {
		case "text":
			parts = append(parts, map[string]any{"type": "text", "text": str(blk["text"])})
		case "image":
			if url := anthropicImageURL(blk); url != "" {
				parts = append(parts, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": url},
				})
			}
		case "tool_result":
			toolMsgs = append(toolMsgs, map[string]any{
				"role":         "tool",
				"tool_call_id": str(blk["tool_use_id"]),
				"content":      anthropicToolResultText(blk["content"]),
			})
		}
	}
	out := []any{}
	if len(parts) > 0 {
		out = append(out, map[string]any{"role": role, "content": parts})
	}
	out = append(out, toolMsgs...)
	return out, nil
}

// assistantBlocksToOpenAI 翻译 assistant 消息的 content block 数组：
// text → content，tool_use → tool_calls（input 序列化成 arguments）。
func assistantBlocksToOpenAI(blocks []map[string]any) ([]any, error) {
	var text strings.Builder
	var toolCalls []any
	for _, blk := range blocks {
		switch str(blk["type"]) {
		case "text":
			text.WriteString(str(blk["text"]))
		case "tool_use":
			args, err := json.Marshal(blk["input"])
			if err != nil || blk["input"] == nil {
				args = []byte("{}")
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   str(blk["id"]),
				"type": "function",
				"function": map[string]any{
					"name":      str(blk["name"]),
					"arguments": string(args),
				},
			})
		}
	}
	msg := map[string]any{"role": "assistant"}
	if text.Len() > 0 {
		msg["content"] = text.String()
	} else {
		msg["content"] = nil
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	return []any{msg}, nil
}

// anthropicImageURL 把 Anthropic 的 image source 转成 OpenAI 可用的 URL：
// base64 源拼成 data URL，url 源直接透传。
func anthropicImageURL(blk map[string]any) string {
	src, _ := blk["source"].(map[string]any)
	if src == nil {
		return ""
	}
	switch str(src["type"]) {
	case "base64":
		media := str(src["media_type"])
		if media == "" {
			media = "image/png"
		}
		return "data:" + media + ";base64," + str(src["data"])
	case "url":
		return str(src["url"])
	}
	return ""
}

// anthropicToolResultText 把 tool_result 的 content（字符串或 block 数组）拍平成文本。
func anthropicToolResultText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, item := range v {
			blk, _ := item.(map[string]any)
			if blk == nil {
				continue
			}
			if str(blk["type"]) == "text" {
				b.WriteString(str(blk["text"]))
			}
		}
		return b.String()
	case nil:
		return ""
	}
	raw, _ := json.Marshal(content)
	return string(raw)
}

// anthropicToolChoice 把 Anthropic tool_choice 翻译成 OpenAI tool_choice。
func anthropicToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto":
			return "auto"
		case "any":
			return "required"
		case "none":
			return "none"
		}
		return nil
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Type == "tool" && obj.Name != "" {
		return map[string]any{"type": "function", "function": map[string]any{"name": obj.Name}}
	}
	return nil
}

// anthropicBridge 是 chatCompletions 与客户端之间的写入边界：把 chatCompletions
// 写出的 OpenAI 字节（非流式 JSON 或 SSE）翻译成 Anthropic 协议。
//
// 之所以选这个边界：chatCompletions 只通过 http.ResponseWriter 的
// Header/WriteHeader/Write/Flush 对外输出，包一层就能在「不改调度、不改上游
// 调用、不改统计」的前提下完成协议转换。
type anthropicBridge struct {
	dst       http.ResponseWriter
	streaming bool

	status     int
	gotStatus  bool
	headerSent bool

	// 非流式（或失败）响应先缓冲，finalize 时一次性翻译。
	buf bytes.Buffer

	// 流式翻译状态（仅 streaming=true 时非空）。
	stream *anthropicStreamState
}

func newAnthropicBridge(dst http.ResponseWriter, streaming bool) *anthropicBridge {
	b := &anthropicBridge{dst: dst, streaming: streaming}
	if streaming {
		b.stream = newAnthropicStreamState(dst)
	}
	return b
}

func (b *anthropicBridge) Header() http.Header { return b.dst.Header() }

// WriteHeader 只记录状态，不立即下发——流式成功时 StreamHint 根本不显式写头，
// 非流式 / 失败时要等 finalize 翻译完再决定真实状态码与 Content-Type。
func (b *anthropicBridge) WriteHeader(status int) {
	if b.gotStatus {
		return
	}
	b.status = status
	b.gotStatus = true
}

func (b *anthropicBridge) Write(p []byte) (int, error) {
	// 流式成功路径：status 未定（StreamHint 不显式写头）或 <400 时，把 OpenAI SSE
	// 逐帧翻译成 Anthropic SSE。
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

func (b *anthropicBridge) Flush() {
	if f, ok := b.dst.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 保留底层 writer 的透传能力（与 logging.responseObserver 同约定）。
func (b *anthropicBridge) Unwrap() http.ResponseWriter { return b.dst }

func (b *anthropicBridge) sendHeader(status int) {
	b.dst.WriteHeader(status)
	b.headerSent = true
}

// finalize 在 chatCompletions 返回后调用，把缓冲的 OpenAI 响应翻译成 Anthropic
// 响应；流式成功时无需补写（message_stop 已随 [DONE] 落地）。
func (b *anthropicBridge) finalize() {
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
		writeAnthropicError(b.dst, status, anthropicErrorType(status, code), msg)
		return
	}

	var resp map[string]any
	if err := json.Unmarshal(b.buf.Bytes(), &resp); err != nil {
		writeAnthropicError(b.dst, http.StatusBadGateway, "api_error", "failed to decode upstream response: "+err.Error())
		return
	}
	writeJSON(b.dst, http.StatusOK, openAIToAnthropicMessage(resp))
}

// openAIToAnthropicMessage 把非流式 OpenAI chat.completion 翻译成 Anthropic Message。
func openAIToAnthropicMessage(resp map[string]any) map[string]any {
	msg := map[string]any{
		"id":            "msg_" + randomID(24),
		"type":          "message",
		"role":          "assistant",
		"model":         str(resp["model"]),
		"content":       []any{},
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
	}
	if id := str(resp["id"]); id != "" {
		msg["id"] = "msg_" + sanitizeID(id)
	}

	var content []any
	stopReason := "end_turn"
	hasToolUse := false
	if choices, ok := resp["choices"].([]any); ok && len(choices) > 0 {
		choice, _ := choices[0].(map[string]any)
		if choice != nil {
			if fr := str(choice["finish_reason"]); fr != "" {
				stopReason = anthropicStopReason(fr, false)
			}
			m, _ := choice["message"].(map[string]any)
			if m != nil {
				if txt := str(m["content"]); txt != "" {
					content = append(content, map[string]any{"type": "text", "text": txt})
				}
				if tcs, ok := m["tool_calls"].([]any); ok {
					for _, tci := range tcs {
						tc, _ := tci.(map[string]any)
						if tc == nil {
							continue
						}
						fn, _ := tc["function"].(map[string]any)
						id := str(tc["id"])
						if id == "" {
							id = "toolu_" + randomID(20)
						}
						content = append(content, map[string]any{
							"type":  "tool_use",
							"id":    id,
							"name":  str(fn["name"]),
							"input": parseJSONArgs(str(fn["arguments"])),
						})
						hasToolUse = true
					}
				}
			}
		}
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	msg["content"] = content
	if hasToolUse && stopReason == "end_turn" {
		stopReason = "tool_use"
	}
	msg["stop_reason"] = stopReason
	if usage, ok := resp["usage"].(map[string]any); ok {
		in, _ := jsonInt(usage["prompt_tokens"])
		out, _ := jsonInt(usage["completion_tokens"])
		msg["usage"] = map[string]any{"input_tokens": in, "output_tokens": out}
	}
	return msg
}

// anthropicStopReason 把 OpenAI finish_reason 映射成 Anthropic stop_reason。
func anthropicStopReason(finish string, hasToolUse bool) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "stop", "content_filter", "":
		if hasToolUse {
			return "tool_use"
		}
		return "end_turn"
	default:
		return "end_turn"
	}
}

// parseJSONArgs 把 tool_call.arguments 的 JSON 字符串解析成对象，失败回退空对象。
func parseJSONArgs(s string) any {
	if strings.TrimSpace(s) == "" {
		return map[string]any{}
	}
	var v any
	if json.Unmarshal([]byte(s), &v) != nil {
		return map[string]any{}
	}
	return v
}

// writeAnthropicError 写 Anthropic 风格错误信封：{"type":"error","error":{...}}。
func writeAnthropicError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, anthropicErrorBody(code, msg))
}

func anthropicErrorBody(code, msg string) map[string]any {
	return map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    code,
			"message": msg,
		},
	}
}

// anthropicErrorType 把 HTTP 状态码 / 网关错误码映射成 Anthropic error.type。
func anthropicErrorType(status int, code string) string {
	switch status {
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusServiceUnavailable, http.StatusBadGateway:
		return "overloaded_error"
	case http.StatusInternalServerError:
		return "api_error"
	}
	switch code {
	case "invalid_request", "bad_params":
		return "invalid_request_error"
	case "rate_limit_exceeded":
		return "rate_limit_error"
	}
	return "api_error"
}

// parseOpenAIErrorBody 从 OpenAI 错误信封里取 code / message，非 JSON 时原样当 message。
func parseOpenAIErrorBody(body []byte) (code, msg string) {
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) != nil {
		return "", strings.TrimSpace(string(body))
	}
	return env.Error.Code, env.Error.Message
}

// estimateAnthropicTokens 是 count_tokens 的本地估算：按字符数 / 4 粗算。
func estimateAnthropicTokens(req *anthropicRequest) int {
	total := 0
	add := func(s string) {
		total += (len([]rune(s)) + 3) / 4
	}
	add(anthropicSystemText(req.System))
	for _, m := range req.Messages {
		var plain string
		if json.Unmarshal(m.Content, &plain) == nil {
			add(plain)
			continue
		}
		var blocks []map[string]any
		if json.Unmarshal(m.Content, &blocks) == nil {
			for _, blk := range blocks {
				switch str(blk["type"]) {
				case "text":
					add(str(blk["text"]))
				case "tool_use":
					add(str(blk["name"]))
					raw, _ := json.Marshal(blk["input"])
					add(string(raw))
				case "tool_result":
					add(anthropicToolResultText(blk["content"]))
				}
			}
		}
	}
	for _, t := range req.Tools {
		add(t.Name)
		add(t.Description)
		add(string(t.InputSchema))
	}
	if total < 1 {
		total = 1
	}
	return total
}

// randomID 生成 n 个十六进制字符的随机 ID（crypto/rand；失败回退时间戳）。
func randomID(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)[:n]
}

// sanitizeID 只保留 [A-Za-z0-9_-]，把上游 id 派生为合法 Anthropic id 片段。
func sanitizeID(id string) string {
	var b strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return randomID(24)
	}
	return b.String()
}

// str 取任意值为字符串（非字符串返回空串），供 map[string]any 解包时降噪。
func str(v any) string {
	s, _ := v.(string)
	return s
}

// jsonInt 把 JSON 解码出的数值（float64 / json.Number / int）取成 int。
func jsonInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return int(i), true
	}
	return 0, false
}

// sortInts 对 content block 索引排序（tool 块可能乱序到达，收尾要按 index 递增）。
func sortInts(v []int) { sort.Ints(v) }

// ---------------------------------------------------------------------------
// 流式翻译：OpenAI chat.completion.chunk SSE → Anthropic Messages SSE
// ---------------------------------------------------------------------------

// anthropicStreamState 累积 OpenAI SSE 字节，产出 Anthropic 事件序列：
//
//	message_start → content_block_start/delta/stop... → message_delta → message_stop
//
// OpenAI 没有与 Anthropic content block 完全对应的边界，这里按「文本块」与
// 「每个 tool_call 一个 tool_use 块」建模；块 index 从 0 递增。
type anthropicStreamState struct {
	w http.ResponseWriter

	started  bool
	finished bool
	errored  bool

	messageID string
	model     string

	nextIndex int
	textIdx   int // 当前文本块 index；-1 = 未开启
	textOpen  bool
	toolIdx   map[int]int // OpenAI tool_call index → Anthropic content block index

	stopReason   string
	inputTokens  int
	outputTokens int

	buf bytes.Buffer
}

func newAnthropicStreamState(w http.ResponseWriter) *anthropicStreamState {
	return &anthropicStreamState{w: w, textIdx: -1, toolIdx: map[int]int{}}
}

// write 累积字节并按空行切分 SSE 帧，逐帧翻译。
func (s *anthropicStreamState) write(p []byte) error {
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
func (s *anthropicStreamState) handleFrame(frame string) error {
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

func (s *anthropicStreamState) handleData(payload string) error {
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
		if fr := str(ch["finish_reason"]); fr != "" {
			s.stopReason = anthropicStopReason(fr, len(s.toolIdx) > 0)
		}
	}
	return nil
}

// ensureStart 在首个有效帧上发 message_start（幂等）。
func (s *anthropicStreamState) ensureStart(obj map[string]any) error {
	if s.started {
		return nil
	}
	s.started = true
	s.model = str(obj["model"])
	if id := str(obj["id"]); id != "" {
		s.messageID = "msg_" + sanitizeID(id)
	} else {
		s.messageID = "msg_" + randomID(24)
	}
	return s.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            s.messageID,
			"type":          "message",
			"role":          "assistant",
			"model":         s.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			// 上游 usage 通常只在末帧给出，这里先按 0；message_delta 再补 output_tokens。
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

func (s *anthropicStreamState) handleDelta(delta map[string]any) error {
	if txt, ok := delta["content"].(string); ok && txt != "" {
		if err := s.ensureTextBlock(); err != nil {
			return err
		}
		return s.emit("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": s.textIdx,
			"delta": map[string]any{"type": "text_delta", "text": txt},
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

func (s *anthropicStreamState) ensureTextBlock() error {
	if s.textOpen {
		return nil
	}
	s.textIdx = s.nextIndex
	s.nextIndex++
	s.textOpen = true
	return s.emit("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         s.textIdx,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
}

func (s *anthropicStreamState) closeTextBlock() error {
	if !s.textOpen {
		return nil
	}
	s.textOpen = false
	return s.emit("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": s.textIdx,
	})
}

// handleToolCall 把一段 delta.tool_calls 翻译成 tool_use 块：
// 首片开块（带 id/name），后续 arguments 分片作为 input_json_delta 透传。
func (s *anthropicStreamState) handleToolCall(tc map[string]any) error {
	idx := 0
	if v, ok := jsonInt(tc["index"]); ok {
		idx = v
	}
	fn, _ := tc["function"].(map[string]any)
	blockIdx, seen := s.toolIdx[idx]
	if !seen {
		if err := s.closeTextBlock(); err != nil {
			return err
		}
		blockIdx = s.nextIndex
		s.nextIndex++
		s.toolIdx[idx] = blockIdx
		id := str(tc["id"])
		if id == "" {
			id = "toolu_" + randomID(20)
		}
		if err := s.emit("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": blockIdx,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    id,
				"name":  str(fn["name"]),
				"input": map[string]any{},
			},
		}); err != nil {
			return err
		}
	}
	args := str(fn["arguments"])
	if args == "" {
		return nil
	}
	return s.emit("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": blockIdx,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
	})
}

// finish 在收到 [DONE] 时收尾：关闭所有块 → message_delta → message_stop。
func (s *anthropicStreamState) finish() error {
	if s.finished || s.errored {
		return nil
	}
	s.finished = true
	if !s.started {
		if err := s.ensureStart(map[string]any{}); err != nil {
			return err
		}
	}
	if err := s.closeTextBlock(); err != nil {
		return err
	}
	// 未显式 stop 的 tool 块也要收尾，按 index 递增顺序发 content_block_stop。
	idxs := make([]int, 0, len(s.toolIdx))
	for _, bi := range s.toolIdx {
		idxs = append(idxs, bi)
	}
	sortInts(idxs)
	for _, bi := range idxs {
		if err := s.emit("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": bi,
		}); err != nil {
			return err
		}
	}
	reason := s.stopReason
	if reason == "" {
		reason = "end_turn"
	}
	if err := s.emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": reason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": s.outputTokens},
	}); err != nil {
		return err
	}
	return s.emit("message_stop", map[string]any{"type": "message_stop"})
}

// emitError 把上游 error 帧翻译成 Anthropic error 事件，并阻止后续 message_stop。
func (s *anthropicStreamState) emitError(errObj map[string]any) error {
	s.errored = true
	msg := str(errObj["message"])
	if msg == "" {
		msg = "upstream error"
	}
	return s.emit("error", map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "api_error", "message": msg},
	})
}

// emit 写一帧 Anthropic SSE（event: + data:）并 flush。
func (s *anthropicStreamState) emit(event string, data map[string]any) error {
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
