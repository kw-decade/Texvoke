package parser

import (
	"strings"
	"testing"

	"github.com/kw-decade/Texvoke/internal/vproto"
)

// 裸文本参数原样解析出来，不做 JSON 校验。
func TestParseArgumentsText(t *testing.T) {
	n := testNonce(t)
	script := "const fs = require('fs')\nfs.readdirSync('.').forEach(f => console.log(f))"

	input := envelopeFor(t, n, vproto.Call{
		ID: "c1", Tool: "client/exec", Freeform: true, ArgumentsText: script,
	})

	// 按 1 字节切碎喂，确保新标签也不依赖读取边界。
	_, res := feedChunks(t, newParser(t), input, 1)
	if res.Outcome != OutcomeCallsParsed {
		t.Fatalf("结局为 %q，错误：%v", res.Outcome, res.Err)
	}
	if len(res.Calls) != 1 {
		t.Fatalf("调用数为 %d", len(res.Calls))
	}
	c := res.Calls[0]
	if !c.Freeform {
		t.Error("没标成裸文本形态，下游会当成 JSON 处理")
	}
	if c.ArgumentsJSON != "" {
		t.Errorf("裸文本不该填进 JSON 槽位：%q", c.ArgumentsJSON)
	}
	if c.ArgumentsText != script {
		t.Errorf("文本原文被改动了\n期望：%q\n实际：%q", script, c.ArgumentsText)
	}
}

// 空脚本是合法的，不能被当成「缺参数」。
func TestParseEmptyArgumentsText(t *testing.T) {
	n := testNonce(t)
	input := n.Signal() + "\n<tool_call_envelope version=\"1\"><call id=\"c1\">" +
		"<tool>a/b</tool><arguments_text><![CDATA[]]></arguments_text></call></tool_call_envelope>"

	_, res := feedChunks(t, newParser(t), input, 7)
	if res.Outcome != OutcomeCallsParsed {
		t.Fatalf("空脚本应解析成功，实际结局 %q：%v", res.Outcome, res.Err)
	}
	if !res.Calls[0].Freeform || res.Calls[0].ArgumentsText != "" {
		t.Errorf("空脚本没被正确识别：%+v", res.Calls[0])
	}
}

// 两种参数标签同时出现是歧义，必须拒绝——取其一等于替模型做决定。
func TestParseRejectsBothArgumentTags(t *testing.T) {
	n := testNonce(t)
	for _, body := range []string{
		"<arguments_json>{}</arguments_json><arguments_text>x</arguments_text>",
		"<arguments_text>x</arguments_text><arguments_json>{}</arguments_json>",
	} {
		input := n.Signal() + "\n<tool_call_envelope version=\"1\"><call id=\"c1\">" +
			"<tool>a/b</tool>" + body + "</call></tool_call_envelope>"
		_, res := feedChunks(t, newParser(t), input, 5)
		if res.Outcome == OutcomeCallsParsed {
			t.Errorf("两份参数应被拒：%s", body)
		}
	}
}

// 一个 envelope 里两种形态的调用可以并存——Codex 的 exec 与 wait 就是这样。
func TestParseMixedForms(t *testing.T) {
	n := testNonce(t)
	input := envelopeFor(t, n,
		vproto.Call{ID: "c1", Tool: "client/exec", Freeform: true, ArgumentsText: "ls()"},
		vproto.Call{ID: "c2", Tool: "client/wait", ArgumentsJSON: `{"ms":100}`},
	)
	_, res := feedChunks(t, newParser(t), input, 3)
	if res.Outcome != OutcomeCallsParsed || len(res.Calls) != 2 {
		t.Fatalf("结局 %q，调用数 %d：%v", res.Outcome, len(res.Calls), res.Err)
	}
	if !res.Calls[0].Freeform || res.Calls[1].Freeform {
		t.Errorf("形态标记错位：%+v", res.Calls)
	}
}

// 字节上限对裸文本同样生效——它防的是内存，与内容形态无关。
func TestParseArgumentsTextRespectsByteLimit(t *testing.T) {
	n := testNonce(t)
	p, err := New(n, Limits{MaxArgumentBytes: 32})
	if err != nil {
		t.Fatal(err)
	}
	input := n.Signal() + "\n<tool_call_envelope version=\"1\"><call id=\"c1\">" +
		"<tool>a/b</tool><arguments_text>" + strings.Repeat("x", 200) +
		"</arguments_text></call></tool_call_envelope>"
	_, res := feedChunks(t, p, input, 16)
	if res.Outcome == OutcomeCallsParsed {
		t.Error("超长裸文本应被拒")
	}
}
