package vproto

import (
	"strings"
	"testing"
)

// 修复提示必须与 Instructions 教的说法一致——同一件事两处说法不同，
// 模型会照更近的那个学，两边都开始漂移。
func TestRepairHintSpeaksTheSameLanguage(t *testing.T) {
	// 两种形态的工具都给，Instructions 才会同时教两个标签
	// （全是裸文本工具时它刻意不教 JSON 写法，见 Instructions 的注释）。
	ins, err := Instructions(testNonce(t), []ToolBrief{
		{Name: "client/exec", Freeform: true},
		{Name: "client/wait"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 两边都必须用这些字面标签指称格式元素。
	for _, term := range []string{TagArguments, TagArgumentsText, TagEnvelopeClose} {
		if !strings.Contains(ins, term) {
			t.Fatalf("Instructions 里没有 %q", term)
		}
	}

	h, err := RepairHintFor(&ParseFailure{BadArguments: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, term := range []string{TagArguments, TagArgumentsText} {
		if !strings.Contains(h.Text, term) {
			t.Errorf("修复提示没用 %q 指称标签：%s", term, h.Text)
		}
	}
	// 不得出现 Go 错误的内部词汇——那是给人看的日志，不是给模型的。
	for _, noise := range []string{"parser:", "envelope 闭合后又出现了信号", "存在歧义"} {
		if strings.Contains(h.Text, noise) {
			t.Errorf("修复提示泄漏了内部错误措辞 %q：%s", noise, h.Text)
		}
	}
}

func TestRepairHintCases(t *testing.T) {
	cases := []struct {
		name string
		in   ParseFailure
		want []string // 都要出现
	}{
		{
			name: "截断",
			in:   ParseFailure{Truncated: true},
			want: []string{"闭合之前就中断"},
		},
		{
			name: "双重信号",
			in:   ParseFailure{DoubleSignal: true},
			want: []string{"只能出现一次", "同一个 envelope"},
		},
		{
			name: "参数形状",
			in:   ParseFailure{BadArguments: true},
			want: []string{TagArguments + ">", TagArgumentsText + ">"},
		},
		{
			name: "未知工具",
			in:   ParseFailure{UnknownTool: "exec_command", KnownTools: []string{"exec", "wait"}},
			want: []string{"exec_command", "exec、wait"},
		},
		{
			name: "未知工具但没有名单",
			in:   ParseFailure{UnknownTool: "x"},
			want: []string{"可用工具里选"},
		},
		{
			name: "原因不明",
			in:   ParseFailure{},
			want: []string{"无法解析"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, err := RepairHintFor(&tc.in)
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tc.want {
				if !strings.Contains(h.Text, w) {
					t.Errorf("提示缺 %q：\n%s", w, h.Text)
				}
			}
		})
	}
}

// L2 运行时通知：陈述信号缺失的事实，但不教 XML 细节（那是 L3 示范的事）。
func TestNoCallHint(t *testing.T) {
	n := mustNonce(t)
	h, err := NoCallHint(n, []ToolBrief{{Name: "exec"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[runtime]", "未检测到调用信号", "exec", "已成功送出", "若你判断此任务确实不应执行"} {
		if !strings.Contains(h.Text, want) {
			t.Errorf("运行时通知缺 %q：\n%s", want, h.Text)
		}
	}
	// 关键区别：L2 只陈述事实，不附可照抄的结构——那是 L3 CallExampleHint 的职责。
	if strings.Contains(h.Text, "<call id=") {
		t.Errorf("L2 不该带完整示例（那是 L3）：\n%s", h.Text)
	}
}

// 措辞红线：任何修复提示都不得含施压用语（规格三章）。
func TestRepairHintsContainNoPressure(t *testing.T) {
	var all []string
	h1, _ := RepairHintFor(&ParseFailure{Truncated: true})
	h2, _ := RepairHintFor(&ParseFailure{DoubleSignal: true})
	h3, _ := RepairHintFor(&ParseFailure{})
	n := mustNonce(t)
	h4, _ := NoCallHint(n, []ToolBrief{{Name: "a"}}, false)
	h5, _ := CallExampleHint(n, []ToolBrief{{Name: "a"}})
	h6, _ := DirectActionHint(n, []ToolBrief{{Name: "a"}, {Name: "b"}})
	all = append(all, h1.Text, h2.Text, h3.Text, h4.Text, h5.Text, h6.Text)

	for _, banned := range []string{"必须调用", "一定要", "忽略之前", "你必须", "立刻"} {
		for _, s := range all {
			if strings.Contains(s, banned) {
				t.Errorf("出现施压用语 %q：\n%s", banned, s)
			}
		}
	}
}

// L3 示例的参数必须是占位符：模型照抄占位符会让客户端报错并把错误送回，
// 比编造一个看似合法的假参数更安全。
func TestCallExampleUsesPlaceholders(t *testing.T) {
	n := mustNonce(t)
	h, err := CallExampleHint(n, []ToolBrief{{Name: "exec"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<参数值>", "<tool>exec</tool>", n.Signal()} {
		if !strings.Contains(h.Text, want) {
			t.Errorf("示例缺 %q：\n%s", want, h.Text)
		}
	}
}
