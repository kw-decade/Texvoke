package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// 裸文本工具的调用与结果，用的是另一套 item：custom_tool_call 与
// custom_tool_call_output，且后者的 output 是数组而不是字符串。
//
// 往返必须保真：Codex 每一轮都把完整历史重发一遍，形态记错了，
// 从第二轮起每条历史都会被渲染成错误的 item 类型。
func TestCustomToolCallRoundTrip(t *testing.T) {
	script := "const fs = require('fs')\nconsole.log(fs.readdirSync('.'))"
	body := `{"model":"gpt-5","input":[
		{"role":"user","content":"列出当前文件"},
		{"type":"custom_tool_call","id":"ctc_1","call_id":"call_a","name":"exec",
		 "input":` + mustQuote(script) + `},
		{"type":"custom_tool_call_output","call_id":"call_a",
		 "output":[{"type":"input_text","text":"a.js\nb.js"}]}
	]}`

	req, err := DecodeResponsesRequest([]byte(body), testOpts)
	if err != nil {
		t.Fatalf("解码失败：%v", err)
	}

	call := req.Input[1].ToolCalls[0]
	if !call.ArgumentForm.Text() {
		t.Fatalf("没标成裸文本形态：%+v", call)
	}
	got, err := call.ArgumentsText()
	if err != nil {
		t.Fatal(err)
	}
	if got != script {
		t.Errorf("脚本原文被改动了\n期望：%q\n实际：%q", script, got)
	}
	if call.ProtocolItemID != "ctc_1" {
		t.Errorf("item id 丢了：%q", call.ProtocolItemID)
	}
	if !req.Input[2].ToolResults[0].Freeform {
		t.Error("结果没标成裸文本形态——重新渲染时会变成 function_call_output")
	}

	encoded, err := EncodeResponsesRequest(*req)
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	s := string(encoded)
	for _, want := range []string{`"custom_tool_call"`, `"custom_tool_call_output"`, `"ctc_1"`, `"call_a"`} {
		if !strings.Contains(s, want) {
			t.Errorf("编码结果缺少 %s：%s", want, s)
		}
	}
	if strings.Contains(s, `"function_call"`) {
		t.Errorf("裸文本调用被渲染成了 function_call：%s", s)
	}
	// output 必须仍是数组。渲染成字符串，客户端拿到的是另一种形状。
	var out struct {
		Input []struct {
			Type   string          `json:"type"`
			Output json.RawMessage `json:"output"`
			Input  *string         `json:"input"`
		} `json:"input"`
	}
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatal(err)
	}
	last := out.Input[len(out.Input)-1]
	if !strings.HasPrefix(string(last.Output), "[") {
		t.Errorf("custom_tool_call_output 的 output 必须是数组：%s", last.Output)
	}

	// 二次解码要回到同一个地方。
	again, err := DecodeResponsesRequest(encoded, testOpts)
	if err != nil {
		t.Fatalf("二次解码失败：%v\n中间结果：%s", err, encoded)
	}
	got2, err := again.Input[1].ToolCalls[0].ArgumentsText()
	if err != nil {
		t.Fatal(err)
	}
	if got2 != script {
		t.Errorf("往返丢了内容：%q", got2)
	}
}

// custom 工具编回线上格式时是 type:"custom" 且不带 parameters。
// 写一份空 schema 上去，上游会以为它收 JSON 对象。
func TestEncodeCustomToolDeclaration(t *testing.T) {
	body := `{"model":"m","input":[
		{"type":"additional_tools","tools":[
			{"type":"custom","name":"exec","description":"裸 JS"},
			{"type":"function","name":"wait","parameters":{"type":"object"}}]},
		{"type":"message","role":"user","content":"hi"}]}`
	req, err := DecodeResponsesRequest([]byte(body), testOpts)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeResponsesRequest(*req)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Tools []map[string]json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 2 {
		t.Fatalf("工具数为 %d", len(out.Tools))
	}
	if string(out.Tools[0]["type"]) != `"custom"` {
		t.Errorf("exec 应编成 custom：%v", out.Tools[0])
	}
	if _, ok := out.Tools[0]["parameters"]; ok {
		t.Errorf("custom 工具不该有 parameters：%v", out.Tools[0])
	}
	if string(out.Tools[1]["type"]) != `"function"` {
		t.Errorf("wait 应编成 function：%v", out.Tools[1])
	}
}

// 响应里的 custom_tool_call 同样要认。
func TestDecodeCustomToolCallInResponse(t *testing.T) {
	resp, err := DecodeResponsesResponse([]byte(`{"id":"r","model":"m","status":"completed",
		"output":[{"type":"custom_tool_call","id":"ctc_1","call_id":"call_a",
		           "name":"exec","input":"ls()"}]}`), testOpts)
	if err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	if len(resp.ToolCalls) != 1 || !resp.ToolCalls[0].ArgumentForm.Text() {
		t.Fatalf("没解出裸文本调用：%+v", resp.ToolCalls)
	}
	encoded, err := EncodeResponsesResponse(*resp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"custom_tool_call"`) {
		t.Errorf("编码结果不对：%s", encoded)
	}
}

func TestCustomToolCallRejectsMissingCallID(t *testing.T) {
	_, err := DecodeResponsesRequest([]byte(`{"model":"m","input":[
		{"type":"custom_tool_call","name":"exec","input":"x"}]}`), testOpts)
	if err == nil {
		t.Fatal("缺 call_id 应被拒——结果无法关联回调用")
	}
}

func mustQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}
