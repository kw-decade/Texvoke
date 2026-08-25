// Package observability 提供脱敏日志、审计事件与指标。
//
// 红线（规格十一章）：工具结果、Prompt、Authorization、Cookie、环境变量
// 与文件内容默认脱敏，日志只保留摘要、哈希和错误分类。
package observability

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
)

// RedactedMarker 是脱敏后的占位符。
//
// 保留一个可见的标记而不是直接删除，是为了让读日志的人知道「这里原本有
// 东西，被脱掉了」。空着会让人以为字段本来就是空的，于是去别处找问题。
const RedactedMarker = "[已脱敏]"

// sensitiveHeaderNames 是必须脱敏的 HTTP 头。
//
// 用小写比对。这份清单只增不减——删掉任何一项都需要一个明确的理由，
// 而「我们不用那个头」不是理由：清单是防御，不是文档。
var sensitiveHeaderNames = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"cookie":              true,
	"set-cookie":          true,
	"x-api-key":           true,
	"api-key":             true,
	"x-auth-token":        true,
	"x-goog-api-key":      true,
	"anthropic-api-key":   true,
	"openai-api-key":      true,
}

// sensitiveKeyFragments 出现在环境变量名或配置键里就视为敏感。
//
// 用片段匹配而不是完整名单：凭证变量的命名千奇百怪
// （OPENAI_API_KEY、GH_TOKEN、AWS_SECRET_ACCESS_KEY、DB_PASSWORD），
// 穷举名单必然漏掉下一个。宁可误伤一个叫 TOKENIZER_PATH 的变量，
// 也不能漏掉一个真的 token。
var sensitiveKeyFragments = []string{
	"key", "token", "secret", "password", "passwd",
	"credential", "auth", "session", "cookie", "private",
	"signature", "salt",
}

// credentialPrefixes 是各家凭证的可识别前缀。
//
// 命中它们说明这段文本里躺着一个真实凭证——不管它出现在什么字段里。
var credentialPrefixes = []string{
	"sk-",         // OpenAI / Anthropic
	"sk-ant-",     // Anthropic
	"ghp_",        // GitHub personal access token
	"gho_",        // GitHub OAuth
	"ghs_",        // GitHub server-to-server
	"github_pat_", // GitHub fine-grained
	"xoxb-",       // Slack bot
	"xoxp-",       // Slack user
	"AKIA",        // AWS access key id
	"ASIA",        // AWS 临时凭证
	"AIza",        // Google API key
	"ya29.",       // Google OAuth
	"Bearer ",     // 通用
	"-----BEGIN ", // PEM 私钥
	"eyJhbGciOi",  // JWT（base64 的 {"alg":）
	"glpat-",      // GitLab
	"npm_",        // npm token
	"dop_v1_",     // DigitalOcean
	"AccountKey=", // Azure 连接串
	"PRIVATE KEY", //
}

// SensitiveKey 报告一个键名是否应当被视为敏感。
func SensitiveKey(name string) bool {
	l := strings.ToLower(name)
	for _, frag := range sensitiveKeyFragments {
		if strings.Contains(l, frag) {
			return true
		}
	}
	return false
}

// LooksLikeCredential 报告一段文本里是否含可识别的凭证前缀。
func LooksLikeCredential(s string) bool {
	for _, p := range credentialPrefixes {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// Redact 把一段可能含凭证的文本转成可安全记录的形式。
//
// 命中凭证特征时整段替换而不是只遮住那一部分：凭证周围的上下文往往
// 同样敏感（"Authorization: Bearer xxx for tenant acme-prod"），
// 而精确定位边界很容易差一个字符，剩下的那一截就够用了。
func Redact(s string) string {
	if s == "" {
		return ""
	}
	if LooksLikeCredential(s) {
		return fmt.Sprintf("%s(长度=%d, 摘要=%s)", RedactedMarker, len(s), shortDigest(s))
	}
	return s
}

// RedactValue 按键名决定是否脱敏一个值。
//
// 与 Redact 的区别：这里由键名做主。一个叫 API_KEY 的变量即使值看起来
// 平平无奇也要脱掉——它可能是个自定义格式的凭证，认不出前缀不代表它安全。
func RedactValue(key, value string) string {
	if SensitiveKey(key) || LooksLikeCredential(value) {
		if value == "" {
			return ""
		}
		return fmt.Sprintf("%s(长度=%d, 摘要=%s)", RedactedMarker, len(value), shortDigest(value))
	}
	return value
}

// RedactHeaders 脱敏一组 HTTP 头，返回可安全记录的映射。
//
// 多值头只记录值的个数：一个头出现多次本身是有诊断价值的信息
// （比如两个 Authorization 说明有代理在插手），但每个值都不该落盘。
func RedactHeaders(h map[string][]string) map[string]string {
	out := make(map[string]string, len(h))
	for name, values := range h {
		lower := strings.ToLower(name)
		switch {
		case sensitiveHeaderNames[lower] || SensitiveKey(lower):
			if len(values) == 1 {
				out[name] = fmt.Sprintf("%s(长度=%d)", RedactedMarker, len(values[0]))
			} else {
				out[name] = fmt.Sprintf("%s(%d 个值)", RedactedMarker, len(values))
			}
		case len(values) == 1:
			out[name] = Redact(values[0])
		default:
			joined := strings.Join(values, ", ")
			out[name] = Redact(joined)
		}
	}
	return out
}

// RedactEnv 脱敏一组 "KEY=VALUE" 形式的环境变量。
//
// 返回的是键名到脱敏值的映射：键名本身几乎总是可以记录的，
// 而「哪些变量存在」对排查配置问题很有用。
func RedactEnv(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, kv := range env {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		k, v := kv[:i], kv[i+1:]
		out[k] = RedactValue(k, v)
	}
	return out
}

// Digest 返回内容的 SHA-256 十六进制摘要。
//
// 用途是「证明是这段内容，但不泄露内容」：审计要能回答「模型当时提出的
// 参数是不是这一份」，而不需要把参数原文存进日志。
func Digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// shortDigest 返回前 8 个十六进制字符，够用于日志里比对两条记录是否同源。
func shortDigest(s string) string {
	return Digest(s)[:8]
}

// SecureCompare 以常量时间比较两个字符串。
//
// 规格十一章要求「用常量时间比较敏感 token」。普通的 == 会在第一个不同的
// 字节处返回，攻击者据此逐字节试探就能在线性次数内还原整个 token——
// 这不是理论攻击，本地网络里几十微秒的差异是可测的。
func SecureCompare(a, b string) bool {
	// 长度不同直接返回 false 会泄露长度，但 token 长度通常是公开的
	// （格式固定），而 subtle.ConstantTimeCompare 对不等长输入本就返回 0。
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// RedactURL 脱敏一个 URL，保留结构但去掉可能含凭证的部分。
//
// 查询串里塞 token 是常见做法（?api_key=xxx），而 URL 又是最常被完整
// 记进日志的东西。用户信息部分同理。
func RedactURL(raw string) string {
	if raw == "" {
		return ""
	}

	// 用户信息：scheme://user:pass@host
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		if at := strings.IndexByte(rest, '@'); at >= 0 {
			if slash := strings.IndexByte(rest, '/'); slash < 0 || at < slash {
				raw = raw[:i+3] + RedactedMarker + "@" + rest[at+1:]
			}
		}
	}

	// 查询串：逐个参数按键名判断。
	q := strings.IndexByte(raw, '?')
	if q < 0 {
		return raw
	}
	base, query := raw[:q], raw[q+1:]

	frag := ""
	if f := strings.IndexByte(query, '#'); f >= 0 {
		frag, query = query[f:], query[:f]
	}

	parts := strings.Split(query, "&")
	for i, p := range parts {
		eq := strings.IndexByte(p, '=')
		if eq < 0 {
			continue
		}
		k, v := p[:eq], p[eq+1:]
		if SensitiveKey(k) || LooksLikeCredential(v) {
			parts[i] = k + "=" + RedactedMarker
		}
	}
	return base + "?" + strings.Join(parts, "&") + frag
}
