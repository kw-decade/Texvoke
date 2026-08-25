package observability

import (
	"strings"
	"testing"
)

// 凭证前缀清单是防御的核心。漏掉一个，那家厂商的 token 就会原样落盘。
func TestLooksLikeCredential(t *testing.T) {
	credentials := []struct{ sample, vendor string }{
		{"sk-proj-abc123def456", "OpenAI"},
		{"sk-ant-api03-xyz", "Anthropic"},
		{"ghp_16CharsOfTokenHere", "GitHub PAT"},
		{"gho_OAuthTokenValue", "GitHub OAuth"},
		{"github_pat_11ABCDEFG", "GitHub fine-grained"},
		{"xoxb-123-456-abcdef", "Slack bot"},
		{"AKIAIOSFODNN7EXAMPLE", "AWS access key"},
		{"ASIATEMPORARYCREDS", "AWS 临时凭证"},
		{"AIzaSyD-EXAMPLE-KEY", "Google API key"},
		{"ya29.a0ARrdaM-token", "Google OAuth"},
		{"Bearer abcdefghijk", "通用 Bearer"},
		{"-----BEGIN RSA PRIVATE KEY-----", "PEM 私钥"},
		{"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.x.y", "JWT"},
		{"glpat-xxxxxxxxxxxx", "GitLab"},
		{"npm_abcdefghijklmnop", "npm"},
		{"AccountKey=base64stuff==", "Azure 连接串"},
	}
	for _, c := range credentials {
		t.Run(c.vendor, func(t *testing.T) {
			if !LooksLikeCredential(c.sample) {
				t.Errorf("%s 的凭证未被识别：%q", c.vendor, c.sample)
			}
		})
	}

	// 混在正常文本里也要认出来——凭证很少单独出现。
	embedded := "请求失败，Authorization: Bearer sk-proj-secret 被拒绝"
	if !LooksLikeCredential(embedded) {
		t.Error("嵌在句子里的凭证未被识别")
	}

	benign := []string{
		"", "普通的日志内容", "user=alice", "status=200",
		"file not found: /tmp/data.json",
	}
	for _, s := range benign {
		if LooksLikeCredential(s) {
			t.Errorf("普通文本被误判为凭证：%q", s)
		}
	}
}

// 键名用片段匹配而非完整名单：凭证变量的命名千奇百怪，穷举必然漏掉下一个。
// 宁可误伤一个叫 TOKENIZER_PATH 的变量，也不能漏掉一个真的 token。
func TestSensitiveKey(t *testing.T) {
	sensitive := []string{
		"API_KEY", "api_key", "OPENAI_API_KEY", "GH_TOKEN",
		"AWS_SECRET_ACCESS_KEY", "DB_PASSWORD", "db_passwd",
		"Authorization", "Cookie", "session_id", "PRIVATE_KEY",
		"webhook_signature", "PASSWORD_SALT", "credential_file",
	}
	for _, k := range sensitive {
		if !SensitiveKey(k) {
			t.Errorf("%q 应被视为敏感键名", k)
		}
	}

	benign := []string{"PATH", "HOME", "LANG", "model", "temperature", "city"}
	for _, k := range benign {
		if SensitiveKey(k) {
			t.Errorf("%q 不该被视为敏感键名", k)
		}
	}
}

// 命中凭证特征时整段替换，而不是只遮住那一部分：凭证周围的上下文往往
// 同样敏感，而精确定位边界很容易差一个字符，剩下那截就够用了。
func TestRedact(t *testing.T) {
	secret := "Authorization: Bearer sk-proj-verysecret for tenant acme-prod"
	got := Redact(secret)

	if strings.Contains(got, "sk-proj-verysecret") {
		t.Errorf("凭证未被脱掉：%q", got)
	}
	if strings.Contains(got, "acme-prod") {
		t.Errorf("凭证周围的上下文也应一并脱掉：%q", got)
	}
	if !strings.Contains(got, RedactedMarker) {
		t.Errorf("缺少脱敏标记，读日志的人会以为字段本来就是空的：%q", got)
	}
	// 长度与摘要保留下来，用于比对两条记录是否同源。
	if !strings.Contains(got, "长度=") || !strings.Contains(got, "摘要=") {
		t.Errorf("应保留长度与摘要供比对：%q", got)
	}

	if Redact("") != "" {
		t.Error("空串应原样返回")
	}
	plain := "普通的日志内容"
	if Redact(plain) != plain {
		t.Error("普通文本不该被改动")
	}
}

// 键名说了算：一个叫 API_KEY 的变量即使值看起来平平无奇也要脱掉，
// 它可能是自定义格式的凭证，认不出前缀不代表它安全。
func TestRedactValue(t *testing.T) {
	got := RedactValue("CUSTOM_API_KEY", "plainlookingvalue")
	if strings.Contains(got, "plainlookingvalue") {
		t.Errorf("敏感键名的值未被脱掉：%q", got)
	}

	got = RedactValue("PATH", "sk-proj-secret")
	if strings.Contains(got, "sk-proj-secret") {
		t.Errorf("值本身像凭证时也要脱掉，无论键名如何：%q", got)
	}

	if got := RedactValue("PATH", "/usr/bin"); got != "/usr/bin" {
		t.Errorf("普通键值对不该被改动：%q", got)
	}
	if got := RedactValue("API_KEY", ""); got != "" {
		t.Errorf("空值应保持为空：%q", got)
	}
}

func TestRedactHeaders(t *testing.T) {
	headers := map[string][]string{
		"Authorization": {"Bearer sk-secret"},
		"Cookie":        {"session=abc123"},
		"X-Api-Key":     {"key-value"},
		"Content-Type":  {"application/json"},
		"User-Agent":    {"texvoke/1.0"},
		"X-Custom":      {"value1", "value2"},
	}

	got := RedactHeaders(headers)

	for _, name := range []string{"Authorization", "Cookie", "X-Api-Key"} {
		v := got[name]
		if !strings.Contains(v, RedactedMarker) {
			t.Errorf("%s 未被脱敏：%q", name, v)
		}
	}
	for _, secret := range []string{"sk-secret", "abc123", "key-value"} {
		joined := strings.Join(valuesOf(got), " ")
		if strings.Contains(joined, secret) {
			t.Errorf("敏感值 %q 泄漏：%v", secret, got)
		}
	}
	// 无害的头原样保留——排查问题需要它们。
	if got["Content-Type"] != "application/json" {
		t.Errorf("Content-Type 被改动了：%q", got["Content-Type"])
	}
}

// 一个头出现多次本身有诊断价值（两个 Authorization 说明有代理在插手），
// 但每个值都不该落盘。
func TestRedactHeadersMultiValue(t *testing.T) {
	got := RedactHeaders(map[string][]string{
		"Authorization": {"Bearer a", "Bearer b"},
	})
	v := got["Authorization"]
	if !strings.Contains(v, "2 个值") {
		t.Errorf("多值头应记录个数：%q", v)
	}
	if strings.Contains(v, "Bearer a") || strings.Contains(v, "Bearer b") {
		t.Errorf("多值头的值泄漏了：%q", v)
	}
}

func TestRedactEnv(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		"HOME=/home/u",
		"OPENAI_API_KEY=sk-proj-secret",
		"GH_TOKEN=ghp_tokenvalue",
		"DB_PASSWORD=hunter2",
		"LANG=en_US.UTF-8",
		"格式错误的项",
		"=没有名字",
	}

	got := RedactEnv(env)

	// 键名保留：「哪些变量存在」对排查配置问题很有用。
	for _, k := range []string{"PATH", "OPENAI_API_KEY", "GH_TOKEN", "DB_PASSWORD"} {
		if _, ok := got[k]; !ok {
			t.Errorf("键名 %q 应当保留", k)
		}
	}
	// 值必须脱掉。
	joined := strings.Join(valuesOf(got), " ")
	for _, secret := range []string{"sk-proj-secret", "ghp_tokenvalue", "hunter2"} {
		if strings.Contains(joined, secret) {
			t.Errorf("凭证 %q 泄漏：%v", secret, got)
		}
	}
	// 无害的原样保留。
	if got["PATH"] != "/usr/bin" {
		t.Errorf("PATH 被改动了：%q", got["PATH"])
	}
	// 格式错误的项跳过。
	if len(got) != 6 {
		t.Errorf("解析出 %d 项，期望 6 项（跳过两项格式错误的）：%v", len(got), got)
	}
}

// URL 是最常被完整记进日志的东西，而查询串里塞 token 是常见做法。
func TestRedactURL(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		leaked []string // 不该出现在结果里的片段
		kept   []string // 应当保留的片段
	}{
		{
			"查询串里的 api_key",
			"https://api.example.com/v1/data?api_key=sk-secret&city=SF",
			[]string{"sk-secret"},
			[]string{"api.example.com", "city=SF", "api_key="},
		},
		{
			"查询串里的 token",
			"https://x.com/cb?access_token=ya29.abc&state=xyz",
			[]string{"ya29.abc"},
			[]string{"state=xyz"},
		},
		{
			"用户信息",
			"https://user:pass@api.example.com/path",
			[]string{"user:pass"},
			[]string{"api.example.com", "/path"},
		},
		{
			"值像凭证但键名无害",
			"https://x.com/?data=sk-proj-leak",
			[]string{"sk-proj-leak"},
			[]string{"x.com"},
		},
		{
			"无敏感内容原样保留",
			"https://api.example.com/v1/weather?city=SF&unit=c",
			nil,
			[]string{"city=SF", "unit=c"},
		},
		{
			"带片段标识",
			"https://x.com/?token=abc#section",
			[]string{"token=abc"},
			[]string{"#section"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactURL(tc.in)
			for _, leak := range tc.leaked {
				if strings.Contains(got, leak) {
					t.Errorf("敏感片段 %q 泄漏：%q", leak, got)
				}
			}
			for _, keep := range tc.kept {
				if !strings.Contains(got, keep) {
					t.Errorf("应保留的片段 %q 丢失：%q", keep, got)
				}
			}
		})
	}

	if RedactURL("") != "" {
		t.Error("空 URL 应原样返回")
	}
}

// 摘要用于「证明是这段内容，但不泄露内容」。
func TestDigest(t *testing.T) {
	a := Digest("敏感内容")
	b := Digest("敏感内容")
	c := Digest("别的内容")

	if a != b {
		t.Error("相同输入应得到相同摘要")
	}
	if a == c {
		t.Error("不同输入应得到不同摘要")
	}
	if len(a) != 64 {
		t.Errorf("SHA-256 十六进制应为 64 字符，实际 %d", len(a))
	}
	if strings.Contains(a, "敏感") {
		t.Error("摘要中不应出现原文")
	}
}

// 普通的 == 会在第一个不同的字节处返回，攻击者据此逐字节试探就能在
// 线性次数内还原整个 token。本地网络里几十微秒的差异是可测的。
func TestSecureCompare(t *testing.T) {
	if !SecureCompare("same-token", "same-token") {
		t.Error("相同的 token 应当匹配")
	}
	if SecureCompare("token-a", "token-b") {
		t.Error("不同的 token 不该匹配")
	}
	if SecureCompare("short", "muchlongertoken") {
		t.Error("不等长的输入不该匹配")
	}
	if !SecureCompare("", "") {
		t.Error("两个空串应当匹配")
	}
	if SecureCompare("", "x") {
		t.Error("空串与非空串不该匹配")
	}
}

// 端到端：一份典型的请求上下文经过脱敏后，不该剩下任何凭证。
func TestNoCredentialSurvivesRedaction(t *testing.T) {
	secrets := []string{
		"sk-proj-realkey", "ghp_realtoken", "hunter2",
		"Bearer realbearer", "AKIAREALAWSKEY",
	}

	outputs := []string{
		Redact("Authorization: Bearer realbearer"),
		RedactValue("OPENAI_API_KEY", "sk-proj-realkey"),
		RedactValue("DB_PASSWORD", "hunter2"),
		RedactURL("https://x.com/?api_key=sk-proj-realkey"),
		strings.Join(valuesOf(RedactHeaders(map[string][]string{
			"Authorization": {"Bearer realbearer"},
			"X-Api-Key":     {"AKIAREALAWSKEY"},
		})), " "),
		strings.Join(valuesOf(RedactEnv([]string{
			"OPENAI_API_KEY=sk-proj-realkey",
			"GH_TOKEN=ghp_realtoken",
		})), " "),
	}

	all := strings.Join(outputs, "\n")
	for _, s := range secrets {
		if strings.Contains(all, s) {
			t.Errorf("凭证 %q 在脱敏后仍然可见：\n%s", s, all)
		}
	}
}

func valuesOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
