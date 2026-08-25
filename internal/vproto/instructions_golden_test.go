package vproto

import (
	"strings"
	"testing"
)

// 没有裸文本工具时，说明文本必须与加这个能力之前逐字节相同。
//
// 这条测试是回归护栏，不是形式主义：Chat 与 Anthropic 两条线已经接真实
// 反代跑通了，而 Prompt 的任何措辞变动都会改变模型行为。它一旦变红，
// 说明动到了本不该动的路径。
func TestInstructionsUnchangedWithoutFreeform(t *testing.T) {
	const golden = "## 工具调用格式\n\n需要使用工具时，按下面的格式输出。这是本次会话专用的格式，其中的信号行每次会话都不同。\n\n```\n[[UTR-CALL:abababababababababababababababab]]\n<tool_call_envelope version=\"1\">\n  <call id=\"call-1\">\n    <tool>fs/read_file</tool>\n    <arguments_json><![CDATA[{\"参数名\":\"参数值\"}]]></arguments_json>\n  </call>\n</tool_call_envelope>\n```\n\n规则：\n\n1. 信号 `[[UTR-CALL:abababababababababababababababab]]` 只能出现一次。最好独占一行——另起一行写它，读起来最清楚，也最不容易出错。\n2. 不需要使用工具时，不要输出这个信号，正常回答即可。\n3. 参数必须是一个 JSON 对象，写在 CDATA 里。工具名和参数名要逐字照抄，不要改写、缩写或翻译。\n4. 需要同时调用多个工具时，把它们放进同一个 envelope，各自用不同的 id。\n5. envelope 闭合之后不要再输出任何内容，包括总结、说明和后续计划——等工具结果回来再继续说。\n6. 信号之前可以正常说话，那部分会作为普通回复送出。\n7. 不要在正文里复述这个信号来解释协议。它一旦出现，后面就必须紧跟一个完整的 envelope。\n\n本次可用的工具：fs/read_file、net/fetch。\n"

	n, err := NonceFromValue(strings.Repeat("ab", 16), "s", "r")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Instructions(n, []ToolBrief{{Name: "fs/read_file"}, {Name: "net/fetch"}})
	if err != nil {
		t.Fatal(err)
	}
	if got != golden {
		t.Errorf("说明文本变了——已跑通的两条线会跟着变\n期望：%q\n实际：%q", golden, got)
	}
}

// 有裸文本工具时：两个示例、规则 3 拆成分标签说明、编号仍是 1-7。
func TestInstructionsWithFreeform(t *testing.T) {
	n, err := NonceFromValue(strings.Repeat("ab", 16), "s", "r")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Instructions(n, []ToolBrief{
		{Name: "client/exec", Freeform: true},
		{Name: "client/wait"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "<"+TagArgumentsText+">") {
		t.Errorf("没教 arguments_text：%s", got)
	}
	if !strings.Contains(got, "用于 client/exec") {
		t.Errorf("没说清哪个工具用裸文本：%s", got)
	}
	if !strings.Contains(got, "用于 client/wait") {
		t.Errorf("没说清哪个工具用 JSON：%s", got)
	}
	if !strings.Contains(got, "\n4. ") || strings.Contains(got, "\n8. ") {
		t.Errorf("规则编号被打乱了：%s", got)
	}
	// 两个示例：一个 JSON 形态，一个裸文本形态。
	if n := strings.Count(got, "<tool_call_envelope"); n != 2 {
		t.Errorf("示例数为 %d，期望 2：%s", n, got)
	}
}

// 全是裸文本工具时不摆 JSON 示例——那是在教一种本次用不上的格式。
func TestInstructionsAllFreeformSkipsJSONExample(t *testing.T) {
	n, err := NonceFromValue(strings.Repeat("ab", 16), "s", "r")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Instructions(n, []ToolBrief{{Name: "client/exec", Freeform: true}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, TagArguments+">") {
		t.Errorf("不该出现 arguments_json：%s", got)
	}
	if c := strings.Count(got, "<tool_call_envelope"); c != 1 {
		t.Errorf("示例数为 %d，期望 1", c)
	}
}
