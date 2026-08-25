package toolbridge

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

func weatherTool() Tool {
	return Tool{
		Name:        "get_weather",
		Description: "查询指定城市的天气",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
	}
}

func newSession(t *testing.T, cfg Config) *Session {
	t.Helper()
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("创建 Bridge 失败：%v", err)
	}
	s, err := b.NewSession("sess-1", "req-1")
	if err != nil {
		t.Fatalf("创建会话失败：%v", err)
	}
	return s
}

// 门面的核心承诺：编译出的格式与解析认的格式来自同一份契约，
// 接入方不需要知道协议长什么样。
func TestCompileParseRoundTrip(t *testing.T) {
	s := newSession(t, Config{})

	compiled, err := s.Compile([]Tool{weatherTool()}, CompileOptions{})
	if err != nil {
		t.Fatalf("编译失败：%v", err)
	}
	if compiled.SystemPrompt == "" {
		t.Fatal("编译结果为空")
	}
	if !strings.Contains(compiled.SystemPrompt, "get_weather") {
		t.Error("system prompt 里没有工具名")
	}
	if !strings.Contains(compiled.SystemPrompt, compiled.Signal) {
		t.Error("system prompt 里没有协议信号，模型无从知道该输出什么")
	}

	// 模拟模型按 prompt 里教的格式输出。这里手工拼出来，
	// 正是为了验证「照着 prompt 写就能被解析」——如果两边分叉，这个测试会红。
	modelOutput := "我来查一下。\n" + compiled.Signal + `
<tool_call_envelope version="1">
  <call id="c1">
    <tool>get_weather</tool>
    <arguments_json><![CDATA[{"city":"SF"}]]></arguments_json>
  </call>
</tool_call_envelope>`

	res, err := s.Parse(modelOutput)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if res.Outcome != OutcomeCallsParsed {
		t.Fatalf("结局为 %q", res.Outcome)
	}
	if len(res.Calls) != 1 {
		t.Fatalf("调用数为 %d", len(res.Calls))
	}
	c := res.Calls[0]
	if c.Name != "get_weather" || c.ID != "c1" {
		t.Errorf("调用标识有误：%+v", c)
	}
	if string(c.Arguments) != `{"city":"SF"}` {
		t.Errorf("参数为 %s", c.Arguments)
	}
	// 信号与协议结构一个字都不该出现在要转发给客户端的文本里。
	if strings.Contains(res.Text, compiled.Signal) {
		t.Errorf("信号泄漏进正文：%q", res.Text)
	}
	if strings.Contains(res.Text, "tool_call_envelope") {
		t.Errorf("协议结构泄漏进正文：%q", res.Text)
	}
	if !strings.Contains(res.Text, "我来查一下") {
		t.Errorf("信号前的正文丢失：%q", res.Text)
	}
}

func TestParsePlainText(t *testing.T) {
	s := newSession(t, Config{})
	if _, err := s.Compile([]Tool{weatherTool()}, CompileOptions{}); err != nil {
		t.Fatal(err)
	}

	res, err := s.Parse("旧金山今天 18 度，天气不错。")
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if res.Outcome != OutcomePlainText {
		t.Errorf("结局为 %q，期望纯文本", res.Outcome)
	}
	if len(res.Calls) != 0 {
		t.Errorf("不该解析出调用：%+v", res.Calls)
	}
	if res.Text != "旧金山今天 18 度，天气不错。" {
		t.Errorf("正文被改动了：%q", res.Text)
	}
}

// 会话内信号不变，这是历史回灌能对上的前提。
// 一个会话内换信号，模型看到的历史示例与当前规范就打架了。
func TestSignalStableWithinSession(t *testing.T) {
	s := newSession(t, Config{})

	first, err := s.Compile([]Tool{weatherTool()}, CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Compile([]Tool{weatherTool()}, CompileOptions{Query: "别的查询"})
	if err != nil {
		t.Fatal(err)
	}

	if first.Signal != second.Signal {
		t.Errorf("同一会话内信号变了：%q → %q", first.Signal, second.Signal)
	}
	if s.Signal() != first.Signal {
		t.Error("Signal() 与编译时用的不一致")
	}
}

// 不同会话必须用不同信号，否则上一轮回显的输出会触发这一轮的调用。
func TestSignalDiffersAcrossSessions(t *testing.T) {
	b, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}

	seen := make(map[string]bool, 50)
	for i := 0; i < 50; i++ {
		s, err := b.NewSession("sess", "req")
		if err != nil {
			t.Fatal(err)
		}
		if seen[s.Signal()] {
			t.Fatalf("第 %d 个会话的信号与之前重复", i)
		}
		seen[s.Signal()] = true
	}
}

// 一个会话的信号不该被另一个会话的输出触发。
func TestCrossSessionSignalDoesNotTrigger(t *testing.T) {
	b, _ := New(Config{})
	a, _ := b.NewSession("s1", "r1")
	other, _ := b.NewSession("s2", "r2")

	if _, err := a.Compile([]Tool{weatherTool()}, CompileOptions{}); err != nil {
		t.Fatal(err)
	}

	// 用另一个会话的信号构造输出。
	output := other.Signal() + `
<tool_call_envelope version="1"><call id="c1"><tool>get_weather</tool>
<arguments_json>{}</arguments_json></call></tool_call_envelope>`

	res, err := a.Parse(output)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(res.Calls) != 0 {
		t.Errorf("另一个会话的信号触发了调用：%+v", res.Calls)
	}
	if res.Outcome != OutcomePlainText {
		t.Errorf("结局为 %q，期望当成普通文本", res.Outcome)
	}
}

// 没有工具就不该注入协议：注入了模型会以为自己有工具可用，然后编一个出来。
func TestCompileWithoutToolsIsAnError(t *testing.T) {
	s := newSession(t, Config{})

	_, err := s.Compile(nil, CompileOptions{})
	if err == nil {
		t.Fatal("没有工具时必须报错")
	}
	if KindOf(err) != ErrNoTools {
		t.Errorf("错误分类为 %q，期望 %q", KindOf(err), ErrNoTools)
	}
}

func TestCompileRejectsInvalidTool(t *testing.T) {
	s := newSession(t, Config{})

	bad := Tool{Name: "有 空格", InputSchema: json.RawMessage(`{}`)}
	_, err := s.Compile([]Tool{bad}, CompileOptions{})
	if err == nil {
		t.Fatal("不合规的工具必须报错")
	}
	if KindOf(err) != ErrInvalidTool {
		t.Errorf("错误分类为 %q，期望 %q", KindOf(err), ErrInvalidTool)
	}
}

// 工具太多时截断要显式报告：模型「没用那个工具」的原因可能是它没看见。
func TestCompileReportsDroppedTools(t *testing.T) {
	s := newSession(t, Config{Upstream: UpstreamProfile{MaxToolsInPrompt: 3}})

	var tools []Tool
	for i := 0; i < 10; i++ {
		tools = append(tools, Tool{
			Name:        "tool_" + string(rune('a'+i)),
			InputSchema: json.RawMessage(`{"type":"object"}`),
		})
	}

	res, err := s.Compile(tools, CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ToolsIncluded) != 3 {
		t.Errorf("进入 Prompt 的工具数为 %d，期望 3", len(res.ToolsIncluded))
	}
	if res.ToolsDropped != 7 {
		t.Errorf("报告丢弃 %d 个，期望 7 个", res.ToolsDropped)
	}
}

// tool_choice 只能变成 Prompt 里的一句预期，不是保证。
// 措辞是陈述而非命令——强硬措辞在模型确实受政策约束时会变成绕过尝试。
func TestCompileToolChoiceIsAdvisory(t *testing.T) {
	s := newSession(t, Config{})

	res, err := s.Compile([]Tool{weatherTool()}, CompileOptions{RequireCall: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.SystemPrompt, "预期") {
		t.Errorf("缺少 tool_choice 约束：%s", res.SystemPrompt)
	}
	for _, coercive := range []string{"必须调用", "你必须", "MUST"} {
		if strings.Contains(res.SystemPrompt, coercive) {
			t.Errorf("使用了命令式措辞 %q", coercive)
		}
	}

	named, err := s.Compile([]Tool{weatherTool()}, CompileOptions{RequiredTool: "get_weather"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(named.SystemPrompt, "get_weather") {
		t.Error("具名约束里没有工具名")
	}
}

// 上游品牌噪声过滤。这是上游特定的，所以由接入方配置——
// 把某一家的正则硬编码进核心，等于宣告这个框架只服务那一家。
func TestNoiseFilter(t *testing.T) {
	s := newSession(t, Config{Upstream: UpstreamProfile{
		NoiseFilters: []*regexp.Regexp{
			regexp.MustCompile(`(?m)^👋\s*你好！我是 p5\.js 助手\n?`),
			regexp.MustCompile(`(?s)\n+如果你想了解更多.*$`),
		},
	}})
	if _, err := s.Compile([]Tool{weatherTool()}, CompileOptions{}); err != nil {
		t.Fatal(err)
	}

	noisy := "👋 你好！我是 p5.js 助手\n旧金山 18 度。\n如果你想了解更多 p5.js 的用法，随时问我。"
	res, err := s.Parse(noisy)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(res.Text, "p5.js 助手") {
		t.Errorf("首部噪声未被过滤：%q", res.Text)
	}
	if strings.Contains(res.Text, "了解更多") {
		t.Errorf("尾部噪声未被过滤：%q", res.Text)
	}
	if !strings.Contains(res.Text, "旧金山 18 度") {
		t.Errorf("正文被误伤：%q", res.Text)
	}
}

// 错误分类要能让接入方决定重试还是停手。
func TestErrorClassification(t *testing.T) {
	s := newSession(t, Config{})
	if _, err := s.Compile([]Tool{weatherTool()}, CompileOptions{}); err != nil {
		t.Fatal(err)
	}

	t.Run("结构非法可重试", func(t *testing.T) {
		bad := s.Signal() + "\n<tool_call_envelope version=\"1\"><不是合法标签></tool_call_envelope>"
		res, err := s.Parse(bad)
		if err == nil {
			t.Fatal("非法结构必须报错")
		}
		if res.Outcome != OutcomeMalformed {
			t.Errorf("结局为 %q", res.Outcome)
		}
		if KindOf(err) != ErrParseFailed {
			t.Errorf("分类为 %q，期望 %q", KindOf(err), ErrParseFailed)
		}
		var e *Error
		if !asError(err, &e) || !e.Retryable() {
			t.Error("格式错误应当被判为值得重试")
		}
	})

	t.Run("截断与格式错误分开", func(t *testing.T) {
		// 截断通常是上游断流或达到 token 上限，该查链路；
		// 格式错误是模型不会写，该调 Prompt。混为一谈会指错排查方向。
		truncated := s.Signal() + "\n<tool_call_envelope version=\"1\"><call id=\"c1\"><tool>get_weather</tool>"
		res, err := s.Parse(truncated)
		if err == nil {
			t.Fatal("截断必须报错")
		}
		if res.Outcome != OutcomeTruncated {
			t.Errorf("结局为 %q，期望 truncated", res.Outcome)
		}
		if KindOf(err) != ErrTruncated {
			t.Errorf("分类为 %q，期望 %q", KindOf(err), ErrTruncated)
		}
	})
}

// 流式路径：信号出现之前的文本要能边到边转发，不必等全部内容到齐。
func TestStreamParserCommitsEarly(t *testing.T) {
	s := newSession(t, Config{})
	compiled, err := s.Compile([]Tool{weatherTool()}, CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}

	sp, err := s.NewStreamParser()
	if err != nil {
		t.Fatal(err)
	}

	full := "我来查一下。\n" + compiled.Signal + `
<tool_call_envelope version="1"><call id="c1"><tool>get_weather</tool>
<arguments_json>{"city":"SF"}</arguments_json></call></tool_call_envelope>`

	var committed strings.Builder
	data := []byte(full)
	for i := 0; i < len(data); i += 3 {
		end := i + 3
		if end > len(data) {
			end = len(data)
		}
		out, err := sp.Write(data[i:end])
		if err != nil {
			t.Fatalf("第 %d 字节处失败：%v", i, err)
		}
		committed.Write(out)
	}

	res := sp.Close()
	if res.Outcome != OutcomeCallsParsed {
		t.Fatalf("结局为 %q", res.Outcome)
	}
	if len(res.Calls) != 1 {
		t.Fatalf("调用数为 %d", len(res.Calls))
	}
	// 已提交的字节里不能有协议内容——它们已经发到客户端了，收不回来。
	sent := committed.String()
	if strings.Contains(sent, compiled.Signal) {
		t.Errorf("信号被提交给客户端了：%q", sent)
	}
	if strings.Contains(sent, "tool_call_envelope") {
		t.Errorf("协议结构被提交给客户端了：%q", sent)
	}
	if !strings.Contains(sent, "我来查一下") {
		t.Errorf("正文没有被及时提交：%q", sent)
	}
}

func TestStreamParserPlainText(t *testing.T) {
	s := newSession(t, Config{})
	if _, err := s.Compile([]Tool{weatherTool()}, CompileOptions{}); err != nil {
		t.Fatal(err)
	}

	sp, err := s.NewStreamParser()
	if err != nil {
		t.Fatal(err)
	}
	var committed strings.Builder
	for _, chunk := range []string{"旧金山", "今天", "18 度。"} {
		out, err := sp.Write([]byte(chunk))
		if err != nil {
			t.Fatal(err)
		}
		committed.Write(out)
	}
	res := sp.Close()

	if res.Outcome != OutcomePlainText {
		t.Errorf("结局为 %q", res.Outcome)
	}
	if res.Text != "旧金山今天18 度。" {
		t.Errorf("累积文本为 %q", res.Text)
	}
}

// 流式路径上的噪声过滤。
//
// 这条测试来自跑 examples/go_inprocess.go 时发现的真缺陷：噪声正则要匹配
// 完整的一行，而解析器为了低延迟会提交半行，于是一行被切成几段分别过正则，
// 一条也匹配不上——过滤在流式路径上等于没配。
func TestStreamNoiseFilterNeedsLineBuffering(t *testing.T) {
	s := newSession(t, Config{Upstream: UpstreamProfile{
		NoiseFilters: []*regexp.Regexp{
			regexp.MustCompile(`(?m)^👋\s*你好！我是[^\n]*助手\n?`),
		},
	}})
	if _, err := s.Compile([]Tool{weatherTool()}, CompileOptions{}); err != nil {
		t.Fatal(err)
	}

	input := "👋 你好！我是天气助手\n旧金山 18 度。\n"

	sp, err := s.NewStreamParser()
	if err != nil {
		t.Fatal(err)
	}
	var forwarded strings.Builder
	data := []byte(input)
	// 按 3 字节切碎，保证噪声那一行被劈成好几段。
	for i := 0; i < len(data); i += 3 {
		end := i + 3
		if end > len(data) {
			end = len(data)
		}
		out, err := sp.Write(data[i:end])
		if err != nil {
			t.Fatal(err)
		}
		forwarded.Write(out)
	}
	forwarded.Write(sp.Flush())
	sp.Close()

	got := forwarded.String()
	if strings.Contains(got, "天气助手") {
		t.Errorf("流式路径上噪声未被过滤：%q", got)
	}
	if !strings.Contains(got, "旧金山 18 度") {
		t.Errorf("正文被误伤：%q", got)
	}
}

// 没配置噪声过滤时不该有额外延迟：Write 应当立即返回可提交的字节，
// 而不是等到行边界。
func TestStreamHasNoBufferingWithoutFilters(t *testing.T) {
	s := newSession(t, Config{})
	if _, err := s.Compile([]Tool{weatherTool()}, CompileOptions{}); err != nil {
		t.Fatal(err)
	}

	sp, err := s.NewStreamParser()
	if err != nil {
		t.Fatal(err)
	}
	// 一段确定不是信号开头的文本，应当立刻被提交。
	out, err := sp.Write([]byte("旧金山"))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Error("没有噪声过滤时不该缓冲，应当立即提交")
	}
	if len(sp.Flush()) != 0 {
		t.Error("没有噪声过滤时 Flush 应当为空")
	}
}

func TestBridgeIsReusableAcrossSessions(t *testing.T) {
	b, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		s, err := b.NewSession("sess", "req")
		if err != nil {
			t.Fatalf("第 %d 个会话创建失败：%v", i, err)
		}
		if _, err := s.Compile([]Tool{weatherTool()}, CompileOptions{}); err != nil {
			t.Fatalf("第 %d 个会话编译失败：%v", i, err)
		}
	}
}

func TestDefaultsAreApplied(t *testing.T) {
	b, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	// 默认值来自实测：24 个工具、2000 字节描述。
	//
	// 描述上限原本是 200，实测后提到 2000：现代 agent 的工具描述普遍几 KB，
	// 真实 Codex 的 exec 有 11 KB，截到 200 字节模型会说「我没有这个工具」。
	if b.profile.MaxToolsInPrompt != 24 {
		t.Errorf("默认工具上限为 %d，期望 24", b.profile.MaxToolsInPrompt)
	}
	if b.profile.MaxToolDescBytes != 2000 {
		t.Errorf("默认描述上限为 %d，期望 2000", b.profile.MaxToolDescBytes)
	}
}

// asError 是 errors.As 的薄封装，避免测试文件引入 errors 包只为一处调用。
func asError(err error, target **Error) bool {
	for err != nil {
		if e, ok := err.(*Error); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
