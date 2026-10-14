# Review：新增原生 OpenAI Responses API 端点（`/v1/responses`，Codex）

本次改动为网关补上**原生 OpenAI Responses API** 协议入口（Codex CLI / Responses SDK
可直接对接），OpenAI Chat Completions 与 Anthropic Messages 既有行为零改动。

---

## 1. 背景与结论

改前仓库有两个协议入口：OpenAI Chat Completions（`POST /v1/chat/completions`）与原生
Anthropic Messages（`POST /v1/messages`）。Codex CLI 走的是 **Responses API**
（`POST /v1/responses`），协议形态与 Chat Completions 不同（`input` item 数组、
`output` item 数组、扁平工具声明、`sequence_number` 流式事件），因此无法直接复用
`/v1/chat/completions` 入口。

改后新增一条路由：

| 端点 | 作用 |
|---|---|
| `POST /v1/responses` | 原生 OpenAI Responses API（流式 / 非流式） |

**设计要点：这个端点不是第二条上游管线，而是「协议翻译层」**——把 Responses 请求体翻译
成既有 OpenAI chat 请求，复用 `chatCompletions` 的选号 / 轮转 / 冷却 / 熔断 / 会话粘性 /
在途租约 / 统计 / 成本记账全链路，再把 OpenAI 响应（非流式 JSON 或 SSE）翻译回 Responses
协议。这样 Codex 流量与 OpenAI / Anthropic 流量共享同一套容错与治理语义，不存在两套调度
逻辑漂移的风险。适配层只关心「协议形态」，不碰「调度决策」。

---

## 2. 新增文件

### `internal/server/responses.go`（约 1000 行）

协议适配层全部实现，与 `anthropic.go` 同构，分四块：

1. **请求类型**：`responsesRequest` / `responsesReasoning` / `responsesTool`——只声明
   网关需要理解的字段子集，未列出字段（`store` / `include` / `text` / `metadata` /
   `previous_response_id` / `truncation` 等）直接忽略。
2. **请求翻译**：`responsesToOpenAI` 及一组 helper（`responsesInstructionsText`、
   `responsesInputToMessages`、`responsesContentToOpenAI`、`responsesOutputText`、
   `responsesToolsToOpenAI`、`responsesToolChoice`）。
3. **响应翻译**：
   - `responsesBridge`：包在 `chatCompletions` 与客户端之间的 `http.ResponseWriter`，
     把网关写出的 OpenAI 字节（非流式 JSON / SSE）翻译成 Responses 协议；
   - `openAIToResponsesResponse`：非流式 `chat.completion` → Responses `response`；
   - `responsesStreamState`：流式 OpenAI SSE → Responses SSE 事件序列；
   - `writeResponsesError` / `responsesErrorType`：Responses 错误信封。
4. **Handler 入口**：`responses`、`mapCodexModel`。

复用 `anthropic.go` 同包 helper：`randomID` / `sanitizeID` / `str` / `jsonInt` /
`sortInts` / `parseOpenAIErrorBody`（两文件必须一起保留 / 提交 / 部署）。

### `internal/server/responses_test.go`

新增 9 个测试（含子用例），覆盖：Bearer 鉴权、非流式文本往返、字符串 input + instructions、
工具双向翻译、模型映射三态、非法请求不打上游、流式文本事件序列、流式工具事件、上游 HTTP
错误、上游 error 帧。

### `cmd/server/config_test.go`

新增 `TestCodexConfigParsed`：验证 `codex.default_model` / `codex.model_map` 解析。

---

## 3. 修改文件（逐文件说明改了什么、为什么）

### `internal/server/handler.go`

**新增**：

- 路由注册 `POST /v1/responses`（经 `withAuth` 包装，走 Bearer 鉴权）；
- `Config` 新增两个字段：`CodexDefaultModel string`、`CodexModelMap map[string]string`。

**修改**：

- `isChatPath(p)` 的路径集合加入 `/v1/responses`，让 Responses 请求进入 chat 请求归档 /
  统计出口。

**为什么**：Responses 端点要复用 chat 的请求归档与统计出口，且归档事件里记录真实路径
`/v1/responses`。`/v1/messages` 的 `withAuthAnthropic` 双头鉴权路径**未改**——Responses
只用 `Authorization: Bearer`（Codex CLI 默认头）。

### `cmd/server/config.go`

**新增**：`Config.Codex` 结构体（`default_model` / `model_map`，JSON 键 `codex`），紧随
`Config.Anthropic` 之后。

### `cmd/server/main.go`

**修改**：`server.NewHandler(server.Config{...})` 透传
`CodexDefaultModel: cfg.Codex.DefaultModel`、`CodexModelMap: cfg.Codex.ModelMap`。

### `config.example.json`

**新增**：顶层 `"codex": {"default_model": "", "model_map": {}}` 配置块。

### `README.md`

**新增/修改**：

- API 端点表新增 `/v1/responses` 一行；
- 鉴权规则行补充 `/v1/responses` 走 `Authorization: Bearer`；
- 新增「Responses API（Codex）端点」小节：设计说明、请求/响应翻译表、流式事件序列、
  已知边界；
- 字段速查表新增 `codex.default_model` / `codex.model_map` 两行；
- 「验证」段新增 Responses curl 示例。

---

## 4. 协议映射明细

### 4.1 鉴权

| 入口 | 接受的密钥头 |
|---|---|
| `/v1/chat/completions`、`/v1/responses`、`/v1/models`、`/status` | `Authorization: Bearer <api_key>` |
| `/v1/messages`、`/v1/messages/count_tokens` | `Authorization: Bearer <api_key>` **或** `x-api-key: <api_key>` |
| `/healthz` | 无鉴权 |

`api_key` 为空 = 不鉴权直接放行（与既有语义一致）。Responses 鉴权失败按 OpenAI 风格错误
信封回 `401 {"error":{"message":...,"type":"invalid_api_key",...}}`（复用既有 `withAuth`）。

### 4.2 请求翻译（Responses → OpenAI）

| Responses | OpenAI（网关内部） |
|---|---|
| `instructions`（字符串） | 首条 `role=system` 消息 |
| `input` 字符串简写 | 一条 `role=user` 消息 |
| `input[].type=message` 的 `content` 字符串 | 同 role 的字符串 content |
| `input[].type=message` 的 `content[].type=input_text`/`output_text` | `{"type":"text","text":...}` part |
| `input[].type=message` 的 `content[].type=input_image` | `{"type":"image_url","image_url":{"url":...}}` part |
| `input[].type=function_call` | 归并进 `assistant.tool_calls[]`（连续多条合一条；`arguments` 空则补 `{}`） |
| `input[].type=function_call_output` | 独立 `role=tool` 消息（`tool_call_id` 取 `call_id`，`output` 拍平成文本） |
| `input[].type=reasoning` | 忽略（上游无等价结构） |
| `tools[]`（扁平 `name`/`description`/`parameters`/`strict`） | `tools[].function`（非 `function` 类型跳过） |
| `tool_choice`：`auto` / `none` / `required` / `{type:function,name}` | `auto` / `none` / `required` / 指定函数 |
| `max_output_tokens` | `max_tokens` |
| `temperature` / `top_p` / `parallel_tool_calls` | 同名透传 |
| `reasoning.effort` | `reasoning_effort` |
| `prompt_cache_key` | 同名透传（`session.ExtractKey` 认它做会话粘性键） |
| `stream` | `stream` |
| `store` / `include` / `text` / `metadata` / `previous_response_id` / `truncation` 等 | 忽略 |

### 4.3 响应翻译（OpenAI → Responses）

**非流式**（`openAIToResponsesResponse`）：

| OpenAI | Responses |
|---|---|
| `choices[0].message.content` | `output[].{type:"message",content:[{type:"output_text",text}]}` |
| `choices[0].message.tool_calls[]` | `output[].{type:"function_call",id,call_id,name,arguments,status:"completed"}` |
| `usage.prompt_tokens` / `completion_tokens` / `total_tokens` | `usage.input_tokens` / `output_tokens` / `total_tokens` |
| `id` | `id` 派生为 `resp_<sanitize(id)>` |
| `created` | `created_at` |

文本 message 条目排在 function_call 条目之前（对齐官方输出顺序）。

**流式**（`responsesStreamState`）：

```
response.created
response.in_progress
  response.output_item.added (message)
  response.content_part.added
    response.output_text.delta ...
  response.output_text.done
  response.content_part.done
  response.output_item.done
  response.output_item.added (function_call)   ← 每个 tool_call 一个条目
    response.function_call_arguments.delta ...
  response.function_call_arguments.done
  response.output_item.done
response.completed（带完整 output + usage）
```

- `sequence_number` 从 0 起逐帧递增（对齐官方流）。
- `response.created` / `response.in_progress` 在首个有效帧上惰性发出（幂等）；模型名优先取
  上游 chunk 的 `model`，缺失时回落到映射后的模型名。
- 上游 `error` 帧翻译成 `response.failed` 事件，并阻止后续 `response.completed`。
- OpenAI 的 `data: [DONE]` 哨兵不外泄，触发 `response.completed` 收尾。
- 事件帧格式为 `event: <name>\ndata: <json>\n\n`，每帧 flush。

### 4.4 错误翻译

| HTTP 状态 | Responses `error.type` |
|---|---|
| 401 | `authentication_error` |
| 403 | `permission_error` |
| 404 | `not_found_error` |
| 429 | `rate_limit_error` |
| 400 / 413 | `invalid_request_error` |
| 502 / 503 | `server_error` |
| 其他 | `server_error` |

错误信封统一为 `{"error":{"message":"<msg>","type":"<type>","code":"<type>"}}`；message
尽量取上游原文（经 `parseOpenAIErrorBody` 提取 `error.message`）。

### 4.5 模型映射（`mapCodexModel`）

优先级：

1. `codex.model_map[客户端模型名]` 命中（值可带 `cn:` / `global:` realm 前缀）；
2. `codex.default_model` 非空时兜底；
3. 都没有 → **原样透传**（用户可直接写网关模型名，如 `glm-5.2`）。

**为什么不像 Anthropic 那样对未映射的「OpenAI 系」模型名报 400**：网关自身的模型目录里
就有 `gpt-5.5` / `gpt-5.6-sol` / `gpt-6-astra` 等以 `gpt-` 开头的真实模型，前缀判定必然
误伤；而 Codex 的模型名固定在配置里，透传未映射名由上游给出真实报错比网关臆测更可靠。
需要强制某个模型时配 `codex.default_model` 即可。

---

## 5. 配置与使用

`config.json` 新增（示例见 `config.example.json`）：

```json
{
  "codex": {
    "default_model": "glm-5.2",
    "model_map": {
      "gpt-5-codex": "cn:deepseek-v4",
      "gpt-5.1-codex": "global:glm-5.2"
    }
  }
}
```

调用：

```bash
curl -sN http://localhost:7863/v1/responses \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-5-codex","input":"hi","stream":true}'
```

Codex CLI / Responses SDK 只需把 base URL 指向 `http://localhost:7863`（SDK 会自动打
`/v1/responses` 并带 `Authorization: Bearer`）。

---

## 6. 验证

- `gofmt -l` 对改动文件无输出（格式干净）；
- `go build ./...` 通过；
- `go vet ./...` 通过；
- `go test ./...` 全绿（含新增 `responses_test.go` 9 个测试与 `TestCodexConfigParsed`）。

新增测试覆盖：

| 测试 | 覆盖点 |
|---|---|
| `TestResponsesAuthBearer` | Bearer 放行；缺失 / 错误回 401 且不打上游 |
| `TestResponsesNonStreamText` | 出站 body 的 model 映射 / system 提位 / max_tokens / 采样字段 / reasoning_effort / prompt_cache_key；入站 response 信封 + output_text + usage 换算 |
| `TestResponsesInputStringAndInstructions` | `input` 字符串简写 + `instructions` |
| `TestResponsesToolTranslation` | tools / tool_choice / function_call / function_call_output 翻译；tool_calls → function_call 响应 |
| `TestResponsesModelMapping` | 显式映射（含 realm 前缀剥离）/ default 兜底 / 原样透传三态 |
| `TestResponsesInvalidRequests` | 缺 model / 缺 input / 非法 input 回 400 且不打上游 |
| `TestResponsesStreamTextEvents` | 流式事件顺序、`sequence_number` 从 0、`[DONE]` 不外泄、映射模型注入、completed |
| `TestResponsesStreamToolEvents` | 工具参数 delta / done、completed output |
| `TestResponsesUpstreamHTTPError` | 上游 429 → Responses `rate_limit_error` |
| `TestResponsesStreamUpstreamErrorFrame` | 上游 error 帧 → `response.failed`（不产生 completed） |

---

## 7. 已知边界 / 未做

- 非 `function` 类型工具（`web_search` / `local_shell` 等内建工具）跳过——上游没有对应能力。
- `store` / `include` / `text` / `metadata` / `previous_response_id` / `truncation` 等字段
  被忽略；`previous_response_id` 不回放历史，多轮上下文需由客户端在 `input` 里带全。
- 未映射模型名原样透传（设计选择，见 4.5）；需要强制某模型时配 `codex.default_model`。
- `reasoning` item 在请求侧被忽略；上游思维链仍以 `reasoning_content` 形式回传，但不以
  Responses `reasoning` item 形式暴露。
- Anthropic Messages 与 OpenAI Chat Completions 端点行为**未改动**（本次只新增
  `/v1/responses`）。
