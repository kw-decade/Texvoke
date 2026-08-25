package toolbridge

import (
	"encoding/json"
	"strings"
	"testing"
)

// Codex 那种请求的完整一圈：工具在 additional_tools 里、exec 是裸文本工具，
// 编译进 Prompt、模型按格式输出、解析出来、再渲染回 Responses 协议。
//
// 这是整个改动要解决的那个场景。任何一环断了，这条测试就红。
func TestFreeformToolEndToEnd(t *testing.T) {
	const body = `{"model":"gpt-5","tool_choice":"auto","input":[
		{"type":"additional_tools","role":"developer","tools":[
			{"type":"custom","name":"exec","description":"Accepts raw JavaScript source text, not JSON."},
			{"type":"function","name":"wait","parameters":{"type":"object","properties":{"ms":{"type":"number"}}}}
		]},
		{"type":"message","role":"user","content":"列出当前文件"}
	]}`

	b, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := b.NewSession("sess-1", "req-1")
	if err != nil {
		t.Fatal(err)
	}

	req, err := DecodeRequest(ProtocolResponses, []byte(body), DecodeOptions{
		SessionID: "sess-1", RequestID: "req-1",
	})
	if err != nil {
		t.Fatalf("解码失败：%v", err)
	}

	tools := req.Tools()
	if len(tools) != 2 {
		t.Fatalf("工具数为 %d：%+v", len(tools), tools)
	}
	if !tools[0].Freeform || len(tools[0].InputSchema) != 0 {
		t.Fatalf("exec 应是无 schema 的裸文本工具：%+v", tools[0])
	}

	compiled, err := sess.CompileRequest(req, CompileOptions{})
	if err != nil {
		t.Fatalf("编译失败：%v", err)
	}

	// Prompt 必须教模型用 arguments_text，而不是给 exec 一份空 schema
	// 让它去写 JSON——那是个不报错的静默失败。
	if !strings.Contains(compiled.SystemPrompt, "arguments_text") {
		t.Errorf("Prompt 没教 arguments_text：\n%s", compiled.SystemPrompt)
	}
	if strings.Contains(compiled.SystemPrompt, `{"type":"object","properties":{}}`) {
		t.Errorf("给裸文本工具补了空 schema，模型会照着写 {}：\n%s", compiled.SystemPrompt)
	}

	// 模型按协议输出一次 exec 调用与一次 wait 调用。
	script := "const fs = require('fs')\nconsole.log(fs.readdirSync('.').join('\\n'))"
	output := "我来看一下。\n" + compiled.Signal + `
<tool_call_envelope version="1">
  <call id="c1">
    <tool>exec</tool>
    <arguments_text><![CDATA[` + script + `]]></arguments_text>
  </call>
  <call id="c2">
    <tool>wait</tool>
    <arguments_json><![CDATA[{"ms":500}]]></arguments_json>
  </call>
</tool_call_envelope>`

	res, err := sess.Parse(output)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if res.Outcome != OutcomeCallsParsed || len(res.Calls) != 2 {
		t.Fatalf("结局 %q，调用数 %d", res.Outcome, len(res.Calls))
	}
	if res.Text != "我来看一下。\n" {
		t.Errorf("正文不对：%q", res.Text)
	}

	exec := res.Calls[0]
	if !exec.Freeform {
		t.Fatal("exec 调用没标成裸文本")
	}
	// 存进 Call 的必须仍是合法 JSON——它会被直接序列化进 HTTP 响应。
	var got string
	if err := json.Unmarshal(exec.Arguments, &got); err != nil {
		t.Fatalf("裸文本没包成 JSON 字符串标量：%s", exec.Arguments)
	}
	if got != script {
		t.Errorf("脚本原文被改动了\n期望：%q\n实际：%q", script, got)
	}
	if res.Calls[1].Freeform {
		t.Error("wait 不该是裸文本形态")
	}

	// 渲染回 Responses：exec 必须是 custom_tool_call，wait 必须是 function_call。
	out, err := sess.RenderResponse(req, res, RenderOptions{})
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}
	var rendered struct {
		Output []struct {
			Type      string  `json:"type"`
			Name      string  `json:"name"`
			Input     *string `json:"input"`
			Arguments *string `json:"arguments"`
		} `json:"output"`
	}
	if err := json.Unmarshal(out, &rendered); err != nil {
		t.Fatalf("渲染结果不是合法 JSON：%v\n%s", err, out)
	}

	var sawExec, sawWait bool
	for _, item := range rendered.Output {
		switch item.Name {
		case "exec":
			sawExec = true
			if item.Type != "custom_tool_call" {
				t.Errorf("exec 被渲染成 %q，Codex 会拿一段脚本去 JSON.parse", item.Type)
			}
			if item.Input == nil || *item.Input != script {
				t.Errorf("input 不对：%v", item.Input)
			}
		case "wait":
			sawWait = true
			if item.Type != "function_call" {
				t.Errorf("wait 被渲染成 %q", item.Type)
			}
		}
	}
	if !sawExec || !sawWait {
		t.Errorf("渲染结果缺调用：%s", out)
	}
}

// 流式路径同样要保住形态。
func TestFreeformStreamRender(t *testing.T) {
	b, _ := New(Config{})
	sess, err := b.NewSession("s", "r")
	if err != nil {
		t.Fatal(err)
	}
	req, err := DecodeRequest(ProtocolResponses, []byte(
		`{"model":"m","stream":true,"input":[{"type":"message","role":"user","content":"hi"}]}`),
		DecodeOptions{SessionID: "s", RequestID: "r"})
	if err != nil {
		t.Fatal(err)
	}

	var buf strings.Builder
	sr, err := sess.NewStreamRenderer(req, &buf, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	err = sr.Finish(Result{
		Outcome: OutcomeCallsParsed,
		Calls: []Call{{
			ID: "c1", Name: "exec", Freeform: true,
			Arguments: json.RawMessage(`"ls()"`),
		}},
	})
	if err != nil {
		t.Fatalf("流式渲染失败：%v", err)
	}
	s := buf.String()
	if !strings.Contains(s, "custom_tool_call") {
		t.Errorf("流式没渲染成 custom_tool_call：%s", s)
	}
	if strings.Contains(s, `"function_call"`) {
		t.Errorf("流式渲染成了 function_call：%s", s)
	}
}
