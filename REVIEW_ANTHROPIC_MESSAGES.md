# Review：原生 Anthropic Messages 端点与思考档位适配

本文记录两阶段工作：先为网关补上**原生 Anthropic Messages** 协议入口（Claude Code /
Anthropic SDK 可直接对接），随后把 Anthropic `thinking` 配置接入既有 Chat
`reasoning_effort` 档位体系。OpenAI Chat Completions 既有行为不变；Responses API
（Codex）见 `REVIEW_CODEX_RESPONSES.md`。

> 阅读提示：第 2、3 节中的 `/v1/messages`、鉴权与响应翻译属于**已落库的基线实现**；
> 标题含「本次增量」的部分才是当前工作区相对基线新增的 `thinking` 适配。

---

## 1. 背景与结论

改前仓库只有 OpenAI Chat Completions 一个协议入口（`POST /v1/chat/completions`）：

- 上游 CodeBuddy 是「OpenAI chat 形态 + SSE」；
- 网关自身也只对外暴露 OpenAI 形态，Anthropic SDK / Claude Code 无法直接接入。

改后新增两条路由：

| 端点 | 作用 |
|---|---|
| `POST /v1/messages` | 原生 Anthropic Messages（流式 / 非流式） |
| `POST /v1/messages/count_tokens` | Anthropic 令牌计数（本地粗估） |

**设计要点：这两个端点不是第二条上游管线，而是「协议翻译层」**——把 Anthropic 请求体
翻译成既有 OpenAI chat 请求，复用 `chatCompletions` 的选号 / 轮转 / 冷却 / 熔断 /
会话粘性 / 在途租约 / 统计 / 成本记账全链路，再把 OpenAI 响应翻译回 Anthropic 协议。
这样 Anthropic 流量与 OpenAI 流量共享同一套容错与治理语义，不存在两套调度逻辑漂移的
风险。

---

## 2. 实现文件

### `internal/server/anthropic.go`（约 1030 行）

协议适配层全部实现，分四块：

1. **请求类型**：`anthropicRequest` / `anthropicThinking` / `anthropicMessage` /
   `anthropicTool`——只声明网关需要理解的字段子集，未列出字段（`metadata` /
   `service_tier` 等）直接忽略。`anthropicThinking` 及 `reasoning_effort` 翻译属于
   本次增量，其余请求类型属于基线实现。
2. **请求翻译**：`anthropicToOpenAI` 及一组 helper（`anthropicSystemText`、
   `anthropicMessageToOpenAI`、`userBlocksToOpenAI`、`assistantBlocksToOpenAI`、
   `anthropicImageURL`、`anthropicToolResultText`、`anthropicToolChoice`、
   `applyAnthropicThinking`、`anthropicBudgetToEffort`）。
3. **响应翻译**：
   - `anthropicBridge`：包在 `chatCompletions` 与客户端之间的 `http.ResponseWriter`，
     把网关写出的 OpenAI 字节（非流式 JSON / SSE）翻译成 Anthropic 协议；
   - `openAIToAnthropicMessage`：非流式 `chat.completion` → Anthropic `message`；
   - `anthropicStreamState`：流式 OpenAI SSE → Anthropic SSE 事件序列；
   - `writeAnthropicError` / `anthropicErrorBody` / `anthropicErrorType`：Anthropic
     错误信封；
   - `estimateAnthropicTokens`：`count_tokens` 的本地估算；
   - 小工具 `randomID` / `sanitizeID` / `str` / `jsonInt` / `sortInts` /
     `parseJSONArgs` / `parseOpenAIErrorBody` / `anthropicStopReason`。
4. **Handler 入口**：`anthropicMessages`、`anthropicCountTokens`、
   `mapAnthropicModel`。

### `internal/server/anthropic_test.go`

共 10 个测试（含子用例）：基线 7 个覆盖双头鉴权、非流式文本往返、工具调用双向翻译、
流式事件序列、模型映射四态、上游错误透传、`count_tokens` 不打上游；本次增量新增 3 个，
覆盖 `thinking` 预算分档 / 显式档位 / 关闭思考、关闭意图穿过上游注入管线，以及
`budget_tokens=32000 → max` 经完整请求链路按 global 模型能力降级为 `high`。

---

## 3. 文件清单（区分基线实现与本次增量）

下表先给出**本次工作区改动**；随后各小节保留 Anthropic 端点的完整基线说明，便于后
续维护。基线文件不是本次重新实现的，Review 中不再把它们描述成本次新增。

| 文件 | 本次增量 |
|---|---|
| `internal/server/anthropic.go` | 新增 `anthropicThinking`；在请求翻译末尾调用 `applyAnthropicThinking`；新增预算区间映射 `anthropicBudgetToEffort`、DeepSeek 识别 `isDeepSeekLikeModel`，处理 `enabled` / `adaptive` / `disabled` 与显式 `effort` |
| `internal/server/anthropic_test.go` | 新增 `TestAnthropicThinkingToReasoningEffort`、`TestAnthropicThinkingDisabledReachesUpstream`、`TestAnthropicThinkingEffortDowngradedOnUpstream` |
| `README.md` | 特性表补充 Anthropic thinking → `reasoning_effort`；字段翻译表新增 thinking 行；已知边界更新为「档位映射已支持，thinking block 尚未回传」 |
| `REVIEW_ANTHROPIC_MESSAGES.md` | 补充本次增量说明、预算分档、关闭语义、降级验证与限制，并区分基线实现与后续适配 |

**本次没有改调度链路**：Anthropic 请求仍在 `anthropicToOpenAI` 后直接进入既有
`chatCompletions`，账号选择、会话粘性、重试、冷却、统计等行为均复用原链路。档位最终
由既有 `normalizeReasoningEffort` 按模型 `supportedEfforts` 降级，避免给不支持高档位的
模型发送超范围值。

### 基线文件总览

以下文件是 Anthropic Messages 端点首次落地时引入或修改的，当前 Review 保留说明仅供
理解全貌；它们不属于本次 `thinking` 档位增量的工作区 diff。

### `internal/httpauth/httpauth.go`

**新增**：

- `apiKeyHeader = "x-api-key"` 常量；
- `VerifyAPIKey(r, key)`：校验 Anthropic 风格 `x-api-key` 头（常量时间比较）；
- `VerifyBearerOrAPIKey(r, key)`：`Authorization: Bearer` 或 `x-api-key` 任一命中即放行。

**为什么**：OpenAI 生态统一走 `Authorization: Bearer`，Anthropic SDK / Claude Code
默认发 `x-api-key`。网关两个入口共用同一把 `api_key`，把「读哪个头」收敛成一处，两个
入口才能共享鉴权口径。既有 `VerifyBearer` / `digest` 原样保留，OpenAI 入口行为不变。

### `internal/httpauth/httpauth_test.go`

**新增**：`reqAPIKey` helper、`TestVerifyAPIKey`、`TestVerifyBearerOrAPIKey`。

### `internal/server/handler.go`

**新增**：

- 路由注册 `POST /v1/messages`、`POST /v1/messages/count_tokens`（经
  `withAuthAnthropic` 包装）；
- `withAuthAnthropic`：`/v1/messages` 的鉴权包装，接受 Bearer 或 `x-api-key`，
  失败按 Anthropic 错误信封回 401；
- `isChatPath(p)`：报告路径是否走 chat 请求归档（OpenAI chat / Anthropic messages /
  count_tokens）；
- `Config` 新增两个字段：`AnthropicDefaultModel string`、`AnthropicModelMap map[string]string`。

**修改**：

- `ServeHTTP` 的请求归档判定从 `r.URL.Path == "/v1/chat/completions"` 改为
  `isChatPath(r.URL.Path)`，并把真实路径写进 `requestTrace.path`。

**为什么**：Anthropic 端点要复用 chat 的请求归档与统计出口，但归档事件里应记录真实
路径，而不是把每条请求都记成 `/v1/chat/completions`。

### `internal/server/logging.go`

**新增**：`requestTrace` 增加 `path string` 字段。

**修改**：`requestTrace.event(status)` 用 `t.path`（空则回落
`/v1/chat/completions`）作为事件 `Path`。

**为什么**：同上——归档按真实路由落账。

### `cmd/server/config.go`

**新增**：`Config.Anthropic` 结构体（`default_model` / `model_map`），紧随 `PromptText`
之后。

### `cmd/server/main.go`

**修改**：`server.NewHandler(server.Config{...})` 透传
`AnthropicDefaultModel: cfg.Anthropic.DefaultModel`、
`AnthropicModelMap: cfg.Anthropic.ModelMap`。

### `config.example.json`

**新增**：顶层 `"anthropic": {"default_model": "", "model_map": {}}` 配置块。

### `README.md`

**新增/修改**：

- API 端点表新增 `/v1/messages`、`/v1/messages/count_tokens` 两行；
- 鉴权规则行补充「`/v1/messages` 额外接受 `x-api-key`」；
- 新增「Anthropic Messages 端点」小节：设计说明、请求/响应翻译表、流式事件序列、
  已知边界；
- 字段速查表新增 `anthropic.default_model` / `anthropic.model_map` 两行；
- 「验证」段新增 Anthropic curl 示例。

---

## 4. 协议映射明细

### 4.1 鉴权

| 入口 | 接受的密钥头 |
|---|---|
| `/v1/chat/completions`、`/v1/models`、`/status` | `Authorization: Bearer <api_key>` |
| `/v1/messages`、`/v1/messages/count_tokens` | `Authorization: Bearer <api_key>` **或** `x-api-key: <api_key>` |
| `/healthz` | 无鉴权 |

`api_key` 为空 = 不鉴权直接放行（与既有语义一致）。鉴权失败按 Anthropic 错误信封回
`401 {"type":"error","error":{"type":"authentication_error",...}}`。

### 4.2 请求翻译（Anthropic → OpenAI）

| Anthropic | OpenAI（网关内部） |
|---|---|
| `system`（字符串或 text block 数组） | 首条 `role=system` 消息（多个 text block 用 `\n` 连接） |
| `messages[].content` 字符串 | 同 role 的字符串 content |
| `messages[].content[].type=text` | `content` part `{"type":"text","text":...}` |
| `messages[].content[].type=image` | `{"type":"image_url","image_url":{"url":...}}`；`base64` 源拼成 `data:<media_type>;base64,<data>` |
| user 消息里的 `tool_result` block | 独立 `role=tool` 消息（`tool_call_id` 取 `tool_use_id`，content 拍平成文本） |
| assistant 消息里的 `tool_use` block | `tool_calls[]`（`input` 序列化为 `arguments`） |
| `tools[].input_schema` | `tools[].function.parameters` |
| `tool_choice`：`auto` / `any` / `none` / `{type:"tool",name}` | `auto` / `required` / `none` / 指定函数 |
| `stop_sequences` | `stop` |
| `max_tokens` / `temperature` / `top_p` | 同名透传 |
| `stream` | `stream` |
| `top_k`、`metadata` 等 | 忽略 |

**思考档位适配（2026-10 新增）**：`thinking` 不再被忽略，而是先映射为 Chat 的
`reasoning_effort`，随后复用既有 `normalizeReasoningEffort` 按模型
`supportedEfforts` 选择不超过请求档位的最高支持档。Anthropic 的
`budget_tokens` 采用单调区间映射：

| `budget_tokens` | `reasoning_effort` |
|---|---|
| `<= 0` / 缺失 | `high` |
| `1–4095` | `low` |
| `4096–8191` | `medium` |
| `8192–16383` | `high` |
| `16384–31999` | `xhigh` |
| `>= 32000` | `max` |

- `thinking.effort` 显式给出时优先于预算映射，不经过二次换算；
- `thinking.type=adaptive` 无预算时默认 `high`；`enabled` 无预算同样默认 `high`；
- `thinking.type=disabled`：DeepSeek 系发送 `thinking.type=disabled` 且删除 effort，
  防止既有注入逻辑重新开启思考；其他模型不注入关闭字段，避免上游不识别；
- `thinking` 缺失、type 为空或未知时保持旧行为，不注入 `reasoning_effort`。

> 说明：网关出站前会走既有 `upstream.PrepareBodyOpt`，其中 `tool_choice` 对象会被归一化
> 成上游认的字符串形式（`{type:function,function:{name:X}}` → `"X"`）。这是既有行为，
> 不是本次新增。

### 4.3 响应翻译（OpenAI → Anthropic）

**非流式**（`openAIToAnthropicMessage`）：

| OpenAI | Anthropic |
|---|---|
| `choices[0].message.content` | `content[].{type:"text",text}` |
| `choices[0].message.tool_calls[]` | `content[].{type:"tool_use",id,name,input}`（`arguments` JSON 解析回对象，失败回 `{}`） |
| `finish_reason` | `stop_reason`：`stop`/`content_filter`/空 → `end_turn`；`tool_calls`/`function_call` → `tool_use`；`length` → `max_tokens` |
| `usage.prompt_tokens` / `completion_tokens` | `usage.input_tokens` / `output_tokens` |
| `id` | `id` 派生为 `msg_<sanitize(id)>` |

空 content 兜底补一个空 text 块；有 tool_use 但 `finish_reason` 为 stop 时，`stop_reason`
收敛为 `tool_use`。

**流式**（`anthropicStreamState`）：

```
message_start
  content_block_start (text)
    content_block_delta (text_delta) ...
  content_block_stop
  content_block_start (tool_use)        ← 每个 tool_call 一个块
    content_block_delta (input_json_delta) ...
  content_block_stop
message_delta (stop_reason + output_tokens)
message_stop
```

- 块 index 从 0 递增；文本块与工具块按到达顺序交替。
- 上游 `error` 帧翻译成 Anthropic `error` 事件，并阻止后续 `message_stop`。
- OpenAI 的 `data: [DONE]` 哨兵不外泄，转为 `message_stop`。
- 事件帧格式为 `event: <name>\ndata: <json>\n\n`，每帧 flush。

### 4.4 错误翻译

| HTTP 状态 | Anthropic `error.type` |
|---|---|
| 401 | `authentication_error` |
| 403 | `permission_error` |
| 429 | `rate_limit_error` |
| 400 | `invalid_request_error` |
| 404 | `not_found_error` |
| 413 | `request_too_large` |
| 502 / 503 | `overloaded_error` |
| 500 | `api_error` |
| 其他 | `api_error` |

错误信封统一为 `{"type":"error","error":{"type":"<type>","message":"<msg>"}}`；message
尽量取上游原文（经 `parseOpenAIErrorBody` 提取 `error.message`）。

### 4.5 模型映射（`mapAnthropicModel`）

优先级：

1. `anthropic.model_map[客户端模型名]` 命中（值可带 `cn:` / `global:` realm 前缀）；
2. 非 `claude*` 名称原样透传（用户可直接写网关模型名，如 `deepseek-v4`）；
3. `claude*` 名称回落到 `anthropic.default_model`；
4. 都没有 → 400，message 提示配置 `anthropic.default_model` 或 `anthropic.model_map`。

**为什么不对 `claude*` 硬编码默认映射**：网关模型目录会变，静默把 claude 打到某个具体
模型上会让用户拿到与预期不符的输出。宁可报错要求显式配置。

---

## 5. 配置与使用

`config.json` 新增（示例见 `config.example.json`）：

```json
{
  "anthropic": {
    "default_model": "deepseek-v4",
    "model_map": {
      "claude-sonnet-4-5": "cn:deepseek-v4",
      "claude-haiku-4-5": "global:glm-5.2"
    }
  }
}
```

调用：

```bash
curl -sN http://localhost:7863/v1/messages \
  -H "x-api-key: your-api-key" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{"model":"claude-sonnet-4-5","max_tokens":256,"stream":true,
       "messages":[{"role":"user","content":"hi"}]}'
```

Anthropic SDK 只需把 `base_url` 指向 `http://localhost:7863`（SDK 会自动打
`/v1/messages` 并带 `x-api-key`）。

---

## 6. 验证

- `gofmt -w internal/server/anthropic.go internal/server/anthropic_test.go` 已执行；
- `go build ./...` 通过（2026-10-19）；
- `go test ./internal/server -run TestAnthropicThinking -count=1 -v` 通过（0.704s），
  日志确认 `reasoning_effort downgraded model=deepseek-v4.1-flash max -> high`；
- `go test ./internal/server -count=1` 通过（8.493s）；
- `go test ./... -count=1` 全部包通过（2026-10-19；`internal/server` 9.562s），
  含 `anthropic_test.go` 共 10 个测试。

`anthropic_test.go` 测试覆盖：

| 测试 | 覆盖点 |
|---|---|
| `TestAnthropicAuthAcceptsBearerAndAPIKey` | `x-api-key` / Bearer 放行；缺失/错误回 401 且信封为 `authentication_error` |
| `TestAnthropicMessagesNonStreamText` | 出站 body 的 model 映射 / system 提位 / max_tokens；入站 message 信封 + text 块 + usage 换算 |
| `TestAnthropicMessagesToolUse` | tools / tool_choice 翻译；tool_calls → tool_use + `stop_reason=tool_use` |
| `TestAnthropicMessagesStream` | 流式事件序列与顺序、`[DONE]` 不外泄 |
| `TestAnthropicModelMapping` | 显式映射（含 realm 前缀剥离）/ default 兜底 / 非 claude 透传 / 未映射 400 且不打上游 |
| `TestAnthropicThinkingToReasoningEffort` | 预算边界 `1/4095/4096/8191/8192/16383/16384/31999/32000`、缺预算、`adaptive`、显式 effort 优先、未知 type 忽略、DeepSeek / 非 DeepSeek 的 disabled 处理 |
| `TestAnthropicThinkingDisabledReachesUpstream` | DeepSeek 的 `thinking.type=disabled` 穿过既有注入管线且不带 effort；GLM 不凭空获得 thinking 对象 |
| `TestAnthropicThinkingEffortDowngradedOnUpstream` | `global:deepseek-v4.1-flash` + `budget_tokens=32000` 经 Anthropic → Chat → upstream 全链路，`max` 按模型能力降级为 `high`，且出站模型名已剥掉 realm 前缀 |
| `TestAnthropicMessagesUpstreamError` | 上游 429 → Anthropic `rate_limit_error` |
| `TestAnthropicCountTokens` | 本地估算返回正整数且不调用上游 |

---

## 7. 已知边界 / 未做

- `count_tokens` 为**本地粗估**（按字符数 / 4），不请求上游；Claude Code 用它做上下文
  预算，精确值非必需。
- `top_k`、`metadata`、`service_tier` 等字段仍被忽略。`thinking` 的
  `type` / `budget_tokens` / `effort` 已映射到 Chat 的 `reasoning_effort`，
  最终档位按模型 `supportedEfforts` 降级；但上游 `reasoning_content` 仍**不会**
  以 Anthropic `thinking` content block 形式回传。
- `thinking.budget_tokens` 到档位是网关定义的区间映射，不是 Anthropic 官方
  一一对应；不同上游的实际推理强度仍受模型能力表约束。
- 流式 `message_start` 的 `input_tokens` 固定为 0（上游 usage 通常只在末帧给出），
  `output_tokens` 在 `message_delta` 补齐。
- `claude-*` 模型名必须显式配置映射，否则 400（设计选择，见 4.5）。
- Responses API（Codex）本次未做，已由后续改动补上，见 `REVIEW_CODEX_RESPONSES.md`。
