// Package httpauth 网关与面板共用的 Bearer 鉴权原语。
//
// 单独成包的原因：server（/v1/*、/status）与 panel（/panel/api/*）两处鉴权
// 必须完全同口径——此前各自复制了一份"字符串直接比较"的实现，既容易漂移，
// 又都带计时侧信道。统一到这里后，口径只有一份，且天然常量时间比较。
package httpauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// bearerPrefix 认证方案前缀（大小写敏感，与 HTTP 规范及既有实现一致）。
const bearerPrefix = "Bearer "

// apiKeyHeader Anthropic 原生 SDK / Claude Code 使用的密钥头。
//
// 为什么单独支持它：OpenAI 生态统一走 `Authorization: Bearer`，而 Anthropic
// SDK 默认发 `x-api-key`。网关两个入口共用同一把 api_key，这里把「读哪个头」
// 收敛成一处，server 侧 `/v1/messages` 与 `/v1/chat/completions` 才能共享鉴权口径。
const apiKeyHeader = "x-api-key"

// VerifyBearer 校验请求头是否携带正确的 Bearer 密钥。
//
// key 为空表示"未启用鉴权"，恒返回 true（调用方据此放行）。
// 比较用 SHA-256 摘要 + subtle.ConstantTimeCompare：
//   - 常量时间，不因前缀匹配长度而泄露信息；
//   - 先摘要再比较，长度差异被吸收进摘要（不会因长度不同提前返回）；
//   - 摘要本身不可逆，即便有侧信道也拿不到密钥原文。
func VerifyBearer(r *http.Request, key string) bool {
	if key == "" {
		return true
	}
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, bearerPrefix) {
		// 缺头/方案不对：仍走一次摘要比较，保持耗时形状一致。
		subtle.ConstantTimeCompare(digest(""), digest(key))
		return false
	}
	tok := authz[len(bearerPrefix):]
	return subtle.ConstantTimeCompare(digest(tok), digest(key)) == 1
}

// VerifyAPIKey 校验 Anthropic 风格的 `x-api-key` 头（常量时间比较，口径同
// VerifyBearer）。key 为空表示未启用鉴权，恒返回 true。
func VerifyAPIKey(r *http.Request, key string) bool {
	if key == "" {
		return true
	}
	return subtle.ConstantTimeCompare(digest(r.Header.Get(apiKeyHeader)), digest(key)) == 1
}

// VerifyBearerOrAPIKey 接受 `Authorization: Bearer <key>` 或 `x-api-key: <key>`
// 任一形态。`/v1/messages` 用本函数：Anthropic SDK 发 x-api-key，但很多用户把
// 网关当 OpenAI 兼容端点、复用同一把 key 时也会发 Bearer，两种都放行。
//
// 两个头都缺失时：VerifyBearer 已做一次常量时间比较（保持耗时形状），
// 这里再比一次 x-api-key（空串），行为与单头校验一致。
func VerifyBearerOrAPIKey(r *http.Request, key string) bool {
	if key == "" {
		return true
	}
	return VerifyBearer(r, key) || VerifyAPIKey(r, key)
}

// digest 返回 s 的 SHA-256（定长 32 字节，供常量时间比较）。
func digest(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
