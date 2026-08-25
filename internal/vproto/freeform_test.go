package vproto

import (
	"strings"
	"testing"
)

func testNonce(t *testing.T) Nonce {
	t.Helper()
	n, err := NonceFromValue(strings.Repeat("ab", 16), "s", "r")
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// 裸文本调用渲染成 arguments_text，原文一字不改地进 CDATA。
func TestRenderFreeformCall(t *testing.T) {
	n := testNonce(t)
	// 多行、带引号、带反斜杠——正是让模型手写 JSON 转义时最容易出错的形状。
	script := "const fs = require('fs')\nconsole.log(\"a\\b\")"

	out, err := RenderEnvelope(n, []Call{{
		ID: "call-1", Tool: "client/exec", Freeform: true, ArgumentsText: script,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "<"+TagArgumentsText+">") {
		t.Fatalf("没有用 arguments_text 标签：%s", out)
	}
	if strings.Contains(out, TagArguments+">") {
		t.Errorf("不该同时出现 arguments_json：%s", out)
	}
	if !strings.Contains(out, script) {
		t.Errorf("原文被改写了：%s", out)
	}
	// 版本没变——加标签是向后兼容的扩展，升版本会让解析器拒掉旧输出。
	if !strings.Contains(out, `version="1"`) {
		t.Errorf("协议版本不该变：%s", out)
	}
}

// 空脚本是合法输入，不能因为「文本为空」就当成对象形态或报错。
func TestRenderFreeformAllowsEmptyText(t *testing.T) {
	out, err := RenderEnvelope(testNonce(t), []Call{{
		ID: "call-1", Tool: "client/exec", Freeform: true,
	}})
	if err != nil {
		t.Fatalf("空脚本应当能渲染：%v", err)
	}
	if !strings.Contains(out, "<"+TagArgumentsText+"><![CDATA[]]></"+TagArgumentsText+">") {
		t.Errorf("空文本渲染形状不对：%s", out)
	}
}

// 两种形态的参数同时给出是构造错误，必须报错而不是静默挑一个。
func TestRenderRejectsBothArgumentForms(t *testing.T) {
	_, err := RenderEnvelope(testNonce(t), []Call{{
		ID: "call-1", Tool: "t", Freeform: true,
		ArgumentsText: "x", ArgumentsJSON: `{}`,
	}})
	if err == nil {
		t.Fatal("同时给两种参数必须报错")
	}
}

// 常规调用一字未变：仍然走 arguments_json，仍然校验必须是 JSON 对象。
func TestRenderObjectCallUnchanged(t *testing.T) {
	out, err := RenderEnvelope(testNonce(t), []Call{{
		ID: "call-1", Tool: "client/get_weather", ArgumentsJSON: `{"city":"SF"}`,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "<"+TagArguments+">") {
		t.Errorf("常规调用应走 arguments_json：%s", out)
	}
	if _, err := RenderEnvelope(testNonce(t), []Call{{
		ID: "call-1", Tool: "t", ArgumentsJSON: `"不是对象"`,
	}}); err == nil {
		t.Error("非对象参数仍应被拒")
	}
}

// CDATA 逃逸对裸文本同样生效——脚本里出现 ]]> 完全可能。
func TestFreeformEscapesCDATAClose(t *testing.T) {
	out, err := RenderEnvelope(testNonce(t), []Call{{
		ID: "call-1", Tool: "t", Freeform: true, ArgumentsText: "a]]>b",
	}})
	if err != nil {
		t.Fatal(err)
	}
	inner := out[strings.Index(out, CDATAOpen)+len(CDATAOpen):]
	inner = inner[:strings.LastIndex(inner, CDATAClose)]
	if got := UnescapeCDATA(inner); got != "a]]>b" {
		t.Errorf("往返丢了内容：%q", got)
	}
}
