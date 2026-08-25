package ir

import (
	"encoding/json"
	"testing"
	"time"
)

func textProposal(args json.RawMessage) ToolCallProposal {
	return ToolCallProposal{
		SessionID:    "s",
		RequestID:    "r",
		CallID:       "call_1",
		Tool:         ToolID{Namespace: NamespaceClient, Name: "exec", Version: VersionDeclared},
		Arguments:    args,
		ArgumentForm: InputFormText,
		Source:       SourceVirtual,
		CreatedAt:    time.Now(),
	}
}

// 零值必须是 object。这是整个改动的向后兼容支点：三个协议的解码器和
// pkg/toolbridge 都在裸构造 ToolDeclaration / ToolCallProposal，
// 零值一旦不是 object，它们全都会当场失效。
func TestZeroInputFormIsObject(t *testing.T) {
	var f InputForm
	if f != InputFormObject || f.Text() {
		t.Fatalf("零值应是 object，实际 %q", f)
	}
	if !f.Valid() {
		t.Fatal("零值必须合法，否则所有现存构造点都会失效")
	}
	if InputForm("array").Valid() {
		t.Error("未定义的形态不该合法")
	}
}

// 裸文本工具必须没有 schema。带一份空对象 schema 会让 Prompt 照着教模型
// 写 JSON，模型输出 {} 而不是脚本——不报错、不异常，只是永远调不动。
func TestTextToolMustNotCarrySchema(t *testing.T) {
	d := ToolDeclaration{Name: "exec", InputForm: InputFormText}
	if err := d.Validate(); err != nil {
		t.Fatalf("裸文本工具没有 schema 是正常的：%v", err)
	}

	d.InputSchema = json.RawMessage(`{"type":"object","properties":{}}`)
	if err := d.Validate(); err == nil {
		t.Fatal("裸文本工具带 schema 必须报错，否则是个查不出来的静默失败")
	}
}

// 常规工具的校验一字未改。
func TestObjectToolValidationUnchanged(t *testing.T) {
	if err := (ToolDeclaration{Name: "f"}).Validate(); err == nil {
		t.Error("对象形态缺 schema 仍应报错")
	}
	d := ToolDeclaration{Name: "f", InputSchema: json.RawMessage(`{"type":"object"}`)}
	if err := d.Validate(); err != nil {
		t.Errorf("常规工具不该受影响：%v", err)
	}
}

func TestTextArgumentsRoundTrip(t *testing.T) {
	// 多行、带引号、带反斜杠——正是让模型手写 JSON 转义时最容易出错的那种。
	script := "const fs = require('fs')\nconsole.log(\"a\\\\b\")\n"

	p := textProposal(TextArguments(script))
	if err := p.Validate(); err != nil {
		t.Fatalf("裸文本提案应合法：%v", err)
	}
	// 存进 IR 的必须仍是合法 JSON——它会被直接序列化进 HTTP 响应。
	if !json.Valid(p.Arguments) {
		t.Fatalf("IR 里的 arguments 必须是合法 JSON：%s", p.Arguments)
	}
	got, err := p.ArgumentsText()
	if err != nil {
		t.Fatal(err)
	}
	if got != script {
		t.Errorf("原文被改动了\n期望：%q\n实际：%q", script, got)
	}
}

func TestTextProposalRejectsNonString(t *testing.T) {
	if err := textProposal(json.RawMessage(`{"a":1}`)).Validate(); err == nil {
		t.Error("裸文本形态收到 JSON 对象应报错")
	}
	if err := textProposal(nil).Validate(); err == nil {
		t.Error("空 arguments 仍应报错")
	}
}

// 形态是对象时 ArgumentsText 报错，而不是把对象字节当文本交出去。
func TestArgumentsTextRefusesObjectForm(t *testing.T) {
	p := textProposal(TextArguments("x"))
	p.ArgumentForm = InputFormObject
	p.Arguments = json.RawMessage(`{}`)
	if _, err := p.ArgumentsText(); err == nil {
		t.Error("对象形态不该能取出文本")
	}
}

// 幂等键对同一段脚本稳定，对不同脚本不同。
func TestIdempotencyKeyCoversText(t *testing.T) {
	a := textProposal(TextArguments("ls"))
	b := textProposal(TextArguments("ls"))
	c := textProposal(TextArguments("rm -rf /"))
	if a.IdempotencyKey() != b.IdempotencyKey() {
		t.Error("同一段脚本应得到同一个键")
	}
	if a.IdempotencyKey() == c.IdempotencyKey() {
		t.Error("不同脚本必须是不同的调用")
	}
}
