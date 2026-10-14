package httpauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func req(authz string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	if authz != "" {
		r.Header.Set("Authorization", authz)
	}
	return r
}

func TestVerifyBearer(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		authz string
		want  bool
	}{
		{"空 key 放行（未启用鉴权）", "", "", true},
		{"空 key 也放行任意头", "", "Bearer whatever", true},
		{"正确 key", "sk-abc123", "Bearer sk-abc123", true},
		{"错误 key", "sk-abc123", "Bearer sk-wrong", false},
		{"缺 Authorization 头", "sk-abc123", "", false},
		{"缺 Bearer 前缀", "sk-abc123", "sk-abc123", false},
		{"前缀大小写不符（规范要求精确）", "sk-abc123", "bearer sk-abc123", false},
		{"多余空格", "sk-abc123", "Bearer  sk-abc123", false},
		{"前缀相同但内容短", "sk-abc123", "Bearer sk-abc12", false},
		{"前缀相同但内容长", "sk-abc123", "Bearer sk-abc1234", false},
		{"key 恰好是前缀", "sk-abc", "Bearer sk-abcdef", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := VerifyBearer(req(c.authz), c.key); got != c.want {
				t.Errorf("VerifyBearer(key=%q, authz=%q) = %v, want %v", c.key, c.authz, got, c.want)
			}
		})
	}
}

// TestVerifyBearerWithoutHeaderStillCompares 缺头路径不应因"提前返回"而暴露形状差异：
// 这里只验证它确实返回 false 且不 panic（常量时间的性质无法用单测断言，靠实现保证）。
func TestVerifyBearerWithoutHeaderStillCompares(t *testing.T) {
	if VerifyBearer(req(""), "any-key") {
		t.Error("missing header must not pass")
	}
}

func TestDigestIsFixedLength(t *testing.T) {
	// 不同长度输入摘要后应等长（这是常量时间比较的前提）
	if len(digest("")) != len(digest("a-much-longer-secret-value")) {
		t.Error("digest length must not depend on input length")
	}
	if len(digest("x")) != 32 {
		t.Errorf("sha256 digest length = %d, want 32", len(digest("x")))
	}
}

// reqAPIKey 构造一个只带 x-api-key 头的请求（Anthropic 原生 SDK / Claude Code 口径）。
func reqAPIKey(key string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	if key != "" {
		r.Header.Set("x-api-key", key)
	}
	return r
}

// TestVerifyAPIKey 覆盖 x-api-key 头的常量时间校验：空 key 放行、命中放行、
// 缺头/错值拒绝（含前缀相等但长度不同的边界）。
func TestVerifyAPIKey(t *testing.T) {
	cases := []struct {
		name string
		key  string
		hdr  string
		want bool
	}{
		{"empty key allows any", "", "whatever", true},
		{"correct key", "sk-abc123", "sk-abc123", true},
		{"wrong key", "sk-abc123", "sk-wrong", false},
		{"missing header", "sk-abc123", "", false},
		{"key is prefix of provided", "sk-abc", "sk-abcdef", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := VerifyAPIKey(reqAPIKey(c.hdr), c.key); got != c.want {
				t.Errorf("VerifyAPIKey(key=%q, x-api-key=%q) = %v, want %v", c.key, c.hdr, got, c.want)
			}
		})
	}
}

// TestVerifyBearerOrAPIKey 覆盖 /v1/messages 的双头鉴权：Bearer 或 x-api-key
// 任一命中即放行，两者皆缺/皆错则拒绝。
func TestVerifyBearerOrAPIKey(t *testing.T) {
	cases := []struct {
		name   string
		key    string
		authz  string
		apiKey string
		want   bool
	}{
		{"empty key allows all", "", "", "", true},
		{"bearer accepted", "k", "Bearer k", "", true},
		{"api key accepted", "k", "", "k", true},
		{"both accepted", "k", "Bearer k", "k", true},
		{"neither header", "k", "", "", false},
		{"wrong bearer, right api key", "k", "Bearer x", "k", true},
		{"right bearer, wrong api key", "k", "Bearer k", "x", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := req(c.authz)
			if c.apiKey != "" {
				r.Header.Set("x-api-key", c.apiKey)
			}
			if got := VerifyBearerOrAPIKey(r, c.key); got != c.want {
				t.Errorf("VerifyBearerOrAPIKey(key=%q, authz=%q, x-api-key=%q) = %v, want %v",
					c.key, c.authz, c.apiKey, got, c.want)
			}
		})
	}
}
