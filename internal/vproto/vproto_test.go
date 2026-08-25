package vproto

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustNonce(t *testing.T) Nonce {
	t.Helper()
	n, err := NewNonce("sess-1", "req-1")
	if err != nil {
		t.Fatalf("生成 nonce 失败：%v", err)
	}
	return n
}

// 每次生成的 nonce 必须不同。可预测的信号等于没有信号——攻击者只要猜中，
// 就能让 Runtime 把一段普通文本解析成工具调用。
func TestNonceIsUnpredictable(t *testing.T) {
	seen := make(map[string]bool, 200)
	for i := 0; i < 200; i++ {
		n := mustNonce(t)
		if seen[n.Value()] {
			t.Fatalf("第 %d 次生成撞上了已有的 nonce：%s", i, n.Value())
		}
		seen[n.Value()] = true
		if len(n.Value()) != nonceBytes*2 {
			t.Fatalf("nonce 长度为 %d，期望 %d", len(n.Value()), nonceBytes*2)
		}
	}
}

func TestNonceRequiresBinding(t *testing.T) {
	// 规格要求 nonce 绑定 session、request 与协议版本。
	if _, err := NewNonce("", "req-1"); err == nil {
		t.Error("缺少 session 时必须报错")
	}
	if _, err := NewNonce("sess-1", ""); err == nil {
		t.Error("缺少 request 时必须报错")
	}

	n := mustNonce(t)
	if n.SessionID() != "sess-1" || n.RequestID() != "req-1" {
		t.Errorf("绑定信息丢失：%+v", n)
	}
	if n.Version() != Version {
		t.Errorf("协议版本为 %q，期望 %q", n.Version(), Version)
	}
}

func TestNonceZeroValue(t *testing.T) {
	var n Nonce
	if !n.Zero() {
		t.Error("零值应被判为未初始化")
	}
	// 未初始化的 nonce 不能匹配任何东西，否则一个忘了初始化的解析器
	// 会把所有文本都当成信号。
	if n.Matches("") || n.Matches(n.Signal()) {
		t.Error("未初始化的 nonce 不应匹配任何行")
	}
	if _, err := RenderEnvelope(n, []Call{{ID: "1", Tool: "a/b", ArgumentsJSON: "{}"}}); err == nil {
		t.Error("用未初始化的 nonce 渲染必须报错")
	}
	if _, err := Instructions(n, nil); err == nil {
		t.Error("用未初始化的 nonce 生成说明必须报错")
	}
}

// 信号必须整行逐字匹配。子串匹配会让模型在正文里提到信号时
// 被误判成发起调用——而它完全可能在解释「我该怎么调用工具」时提到。
func TestNonceMatchesWholeLineOnly(t *testing.T) {
	n := mustNonce(t)
	sig := n.Signal()

	shouldMatch := []string{
		sig,
		"  " + sig,
		sig + "  ",
		"\t" + sig + "\n",
	}
	for _, line := range shouldMatch {
		if !n.Matches(line) {
			t.Errorf("应当匹配：%q", line)
		}
	}

	shouldNotMatch := []string{
		"",
		"接下来我会输出 " + sig + " 这个信号",
		sig + " 后面还有别的",
		"前面有别的 " + sig,
		strings.ToUpper(sig),
		sig[:len(sig)-1],
		"[[UTR-CALL:0000000000000000000000000000000f]]", // 另一个合法格式的 nonce
	}
	for _, line := range shouldNotMatch {
		if n.Matches(line) {
			t.Errorf("不应匹配：%q", line)
		}
	}
}

// 长得像信号但不是本轮的，说明模型回放了历史轮次，或者有人在尝试注入。
// 两种都值得记一笔，但都不该触发调用。
func TestLooksLikeSignal(t *testing.T) {
	n := mustNonce(t)

	if !LooksLikeSignal(n.Signal()) {
		t.Error("本轮信号应当被识别为信号形状")
	}
	// 另一轮的信号：形状对，但 Matches 会拒绝。
	other := "[[UTR-CALL:" + strings.Repeat("ab", nonceBytes) + "]]"
	if !LooksLikeSignal(other) {
		t.Error("其他轮次的信号也应被识别为信号形状")
	}
	if n.Matches(other) {
		t.Error("其他轮次的信号不得触发本轮的调用")
	}

	notSignals := []string{
		"",
		"普通文本",
		"[[UTR-CALL:太短]]",
		"[[UTR-CALL:" + strings.Repeat("z", nonceBytes*2) + "]]", // 非十六进制
		"[[UTR-CALL:" + strings.Repeat("a", nonceBytes*2+1) + "]]",
		"UTR-CALL:" + strings.Repeat("a", nonceBytes*2),
	}
	for _, s := range notSignals {
		if LooksLikeSignal(s) {
			t.Errorf("不应被识别为信号：%q", s)
		}
	}
}

func TestNonceFromValue(t *testing.T) {
	valid := strings.Repeat("a1", nonceBytes)
	n, err := NonceFromValue(valid, "s", "r")
	if err != nil {
		t.Fatalf("重建失败：%v", err)
	}
	if n.Value() != valid {
		t.Errorf("值为 %q", n.Value())
	}

	bad := []string{"", "xyz", strings.Repeat("a", nonceBytes*2-1), strings.Repeat("A", nonceBytes*2)}
	for _, v := range bad {
		if _, err := NonceFromValue(v, "s", "r"); err == nil {
			t.Errorf("非法值 %q 应当被拒绝", v)
		}
	}
	if _, err := NonceFromValue(valid, "", "r"); err == nil {
		t.Error("缺少绑定信息应当被拒绝")
	}
}

func TestSignalLength(t *testing.T) {
	n := mustNonce(t)
	if n.SignalLength() != len(n.Signal()) {
		t.Error("信号长度与实际不符")
	}
	// 流式解析器在拿到 nonce 之前也要预留安全窗口，所以需要一个
	// 不依赖具体实例的上限。
	if n.SignalLength() > MaxSignalLength {
		t.Errorf("信号长度 %d 超过声称的上限 %d", n.SignalLength(), MaxSignalLength)
	}
}

func TestRenderEnvelope(t *testing.T) {
	n := mustNonce(t)
	out, err := RenderEnvelope(n, []Call{
		{ID: "c1", Tool: "fs/read_file", ArgumentsJSON: `{"path":"/tmp/a.txt"}`},
		{ID: "c2", Tool: "net/fetch", ArgumentsJSON: `{"url":"https://example.com"}`},
	})
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}

	// 信号独占第一行。
	lines := strings.Split(out, "\n")
	if lines[0] != n.Signal() {
		t.Errorf("首行为 %q，期望信号独占一行", lines[0])
	}
	// 信号只出现一次。
	if strings.Count(out, n.Signal()) != 1 {
		t.Errorf("信号出现了 %d 次：%s", strings.Count(out, n.Signal()), out)
	}
	// 多个调用共用一个 envelope。
	if strings.Count(out, TagEnvelopeOpen) != 1 || strings.Count(out, TagEnvelopeClose) != 1 {
		t.Errorf("多个调用应共用一个 envelope：%s", out)
	}
	if strings.Count(out, TagCallOpen) != 2 {
		t.Errorf("应有两个 call：%s", out)
	}
	for _, want := range []string{"fs/read_file", "net/fetch", `{"path":"/tmp/a.txt"}`, "https://example.com"} {
		if !strings.Contains(out, want) {
			t.Errorf("渲染结果中缺少 %q：%s", want, out)
		}
	}
}

func TestRenderEnvelopeRejects(t *testing.T) {
	n := mustNonce(t)
	tests := []struct {
		name  string
		calls []Call
		want  string
	}{
		{"没有调用", nil, "不得输出信号"},
		{"缺 id", []Call{{Tool: "a/b", ArgumentsJSON: "{}"}}, "缺少 id"},
		{"缺工具名", []Call{{ID: "c1", ArgumentsJSON: "{}"}}, "缺少工具名"},
		{
			"id 重复",
			[]Call{{ID: "c1", Tool: "a/b", ArgumentsJSON: "{}"}, {ID: "c1", Tool: "a/b", ArgumentsJSON: "{}"}},
			"重复",
		},
		{"id 含引号", []Call{{ID: `c"1`, Tool: "a/b", ArgumentsJSON: "{}"}}, "需要转义"},
		{"工具名含尖括号", []Call{{ID: "c1", Tool: "a/<b>", ArgumentsJSON: "{}"}}, "需要转义"},
		{"参数是数组", []Call{{ID: "c1", Tool: "a/b", ArgumentsJSON: "[1]"}}, "不是 JSON 对象"},
		{"参数是残缺 JSON", []Call{{ID: "c1", Tool: "a/b", ArgumentsJSON: "{"}}, "不是 JSON 对象"},
		{"参数为空", []Call{{ID: "c1", Tool: "a/b"}}, "不是 JSON 对象"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := RenderEnvelope(n, tc.calls)
			if err == nil {
				t.Fatalf("期望报错包含 %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息 %q 未包含 %q", err.Error(), tc.want)
			}
		})
	}
}

// CDATA 没有转义机制，参数里出现 ]]> 会让它提前闭合，后半截参数变成裸文本。
// 规格十三章把这一项列为必测，因为坑很隐蔽：正常的 JSON 结构里不会有它，
// 但用户输入的字符串值里完全可能有。
func TestCDATAEscaping(t *testing.T) {
	n := mustNonce(t)
	args := `{"text":"结束标记 ]]> 就在这里"}`

	out, err := RenderEnvelope(n, []Call{{ID: "c1", Tool: "a/b", ArgumentsJSON: args}})
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}

	// 提取 CDATA 之间的内容，还原后必须与原文一致。
	start := strings.Index(out, CDATAOpen)
	end := strings.LastIndex(out, CDATAClose)
	if start < 0 || end < 0 {
		t.Fatalf("渲染结果里没有 CDATA：%s", out)
	}
	// 中间可能有多段 CDATA（被 ]]> 拆开的），把整段取出来还原。
	inner := out[start+len(CDATAOpen) : end]
	restored := UnescapeCDATA(inner)
	if restored != args {
		t.Errorf("CDATA 往返后为 %q\n期望 %q\n渲染结果：%s", restored, args, out)
	}

	// 还原后仍是合法 JSON 对象。
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(restored), &probe); err != nil {
		t.Errorf("还原后不是合法 JSON：%v", err)
	}
}

func TestCDATAEscapeIsNoopWhenSafe(t *testing.T) {
	safe := `{"a":"普通内容"}`
	if got := escapeCDATA(safe); got != safe {
		t.Errorf("无需转义时不应改动内容：%q", got)
	}
	if got := UnescapeCDATA(safe); got != safe {
		t.Errorf("还原不应改动无转义的内容：%q", got)
	}
}

// 说明文本里的示例必须与解析器认的格式逐字一致。示例写错，模型学到的
// 就是错的，而这类错误在日志里表现成「模型格式不合规」，责任看起来在模型。
func TestInstructionsExampleMatchesFormat(t *testing.T) {
	n := mustNonce(t)
	text, err := Instructions(n, []ToolBrief{{Name: "fs/read_file"}, {Name: "net/fetch"}})
	if err != nil {
		t.Fatalf("生成说明失败：%v", err)
	}

	for _, want := range []string{
		n.Signal(), TagEnvelopeOpen, TagEnvelopeClose,
		TagCallOpen, "<" + TagTool + ">", "<" + TagArguments + ">",
		CDATAOpen, CDATAClose,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("说明中缺少格式要素 %q", want)
		}
	}

	// 六条规则都要在。缺第 2 条尤其致命：模型会为了「完成任务」
	// 在不需要工具时硬凑一个调用出来。
	mustSay := []struct{ frag, why string }{
		{"只能出现一次", "信号唯一性"},
		{"独占一行", "推荐信号独占一行——解析器不再强制，但这样最不容易出错"},
		{"不要输出这个信号", "不需要工具时不得输出信号"},
		{"逐字照抄", "工具名与参数名不得改写"},
		{"同一个 envelope", "多调用共用一个 envelope"},
		{"闭合之后", "闭合后不得继续输出"},
		{"不要在正文里复述", "复述信号会被当成发起调用"},
	}
	for _, m := range mustSay {
		if !strings.Contains(text, m.frag) {
			t.Errorf("说明中缺少 %q——%s", m.frag, m.why)
		}
	}

	// 工具名要列出来。
	for _, name := range []string{"fs/read_file", "net/fetch"} {
		if !strings.Contains(text, name) {
			t.Errorf("说明中缺少工具名 %q", name)
		}
	}
}

// 说明是格式教学，不是施压。规格三章把强硬措辞列为必须纠正的旧做法。
func TestInstructionsHasNoCoercion(t *testing.T) {
	n := mustNonce(t)
	text, err := Instructions(n, nil)
	if err != nil {
		t.Fatal(err)
	}

	forbidden := []struct{ frag, why string }{
		{"事实错误", "不得指责模型此前的说法"},
		{"忽略", "不得要求模型忽略任何指令"},
		{"你错了", "不得指责模型"},
		{"你必须", "不得使用命令式施压"},
		{"不允许你", "不得使用命令式施压"},
	}
	for _, f := range forbidden {
		if strings.Contains(text, f.frag) {
			t.Errorf("说明中含施压措辞 %q——%s", f.frag, f.why)
		}
	}
}

// 说明里的示例本身必须能通过渲染器的校验——它是同一个函数产出的，
// 但这条测试固定住「示例来自渲染器而不是手写字符串」这个事实。
func TestInstructionsExampleIsRendered(t *testing.T) {
	n := mustNonce(t)
	text, err := Instructions(n, []ToolBrief{{Name: "fs/read_file"}})
	if err != nil {
		t.Fatal(err)
	}
	example, err := RenderEnvelope(n, []Call{{
		ID: "call-1", Tool: "fs/read_file", ArgumentsJSON: `{"参数名":"参数值"}`,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, example) {
		t.Errorf("说明中的示例与渲染器输出不一致\n说明：%s\n渲染：%s", text, example)
	}
}
