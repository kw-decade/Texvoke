package protocol

import (
	"strings"
	"testing"

	"github.com/kw-decade/Texvoke/internal/ir"
)

// Codex 的请求形状：顶层没有 tools，全部工具在 input[0] 的
// additional_tools item 里，且有三种 type。
//
// 夹具是从真实抓包裁下来的，不是照着文档编的——这一整条改动的起因就是
// 「照着文档想当然」被真实客户端打脸了两次。
const codexToolsItem = `{
	"type": "additional_tools",
	"role": "developer",
	"tools": [
		{"type": "custom", "name": "exec",
		 "description": "Accepts raw JavaScript source text, not JSON, quoted strings, or markdown code fences.",
		 "format": {"type": "grammar", "syntax": "lark", "definition": "start: SOURCE"}},
		{"type": "function", "name": "wait", "description": "等一会儿",
		 "parameters": {"type": "object", "properties": {"ms": {"type": "number"}}}},
		{"type": "namespace", "name": "collaboration", "description": "多智能体",
		 "tools": [{"type": "function", "name": "spawn_agent", "parameters": {"type": "object"}}]}
	]
}`

func TestCodexAdditionalToolsAreDecoded(t *testing.T) {
	req, err := DecodeResponsesRequest([]byte(`{
		"model": "gpt-5",
		"tool_choice": "auto",
		"input": [`+codexToolsItem+`,
			{"type": "message", "role": "user", "content": "列出当前文件"}
		]
	}`), testOpts)
	if err != nil {
		t.Fatalf("解码失败：%v", err)
	}

	if len(req.Tools) != 2 {
		t.Fatalf("工具数为 %d，期望 2（exec 与 wait）：%+v", len(req.Tools), req.Tools)
	}

	exec := req.Tools[0]
	if exec.Name != "exec" || !exec.InputForm.Text() {
		t.Errorf("exec 应是裸文本工具：%+v", exec)
	}
	// 关键：不能给它补一份空对象 schema。补了，Prompt 就会教模型写 JSON，
	// 模型输出 {} 而不是脚本——不报错，只是永远调不动。
	if len(exec.InputSchema) != 0 {
		t.Errorf("裸文本工具不该有 schema：%s", exec.InputSchema)
	}

	wait := req.Tools[1]
	if wait.Name != "wait" || wait.InputForm.Text() {
		t.Errorf("wait 应是常规 JSON 工具：%+v", wait)
	}
	if len(wait.InputSchema) == 0 {
		t.Error("常规工具的 schema 丢了")
	}

	// namespace 跳过，但必须报上来。零使用的工具静默消失，会变成
	// 「为什么模型从来不用这个功能」这种查不出来的问题。
	if len(req.SkippedTools) != 1 || req.SkippedTools[0] != "collaboration" {
		t.Errorf("被跳过的 namespace 没记下来：%v", req.SkippedTools)
	}

	// item 本身被消化了，不该再留在消息序列里。
	if len(req.Input) != 1 || req.Input[0].Role != RoleUser {
		t.Fatalf("应只剩那条 user 消息：%+v", req.Input)
	}
	// 它也不算「跳过的未知 item」——我们现在认识它了。
	if len(req.SkippedItemTypes) != 0 {
		t.Errorf("additional_tools 不该再记成未知类型：%v", req.SkippedItemTypes)
	}

	// tool_choice 的校验依赖工具总数：顺序写反了，这里会报「没有工具却指定了
	// tool_choice」。这条断言就是钉住那个顺序。
	if req.ToolChoice.Mode != ToolChoiceAuto {
		t.Errorf("tool_choice 解析有误：%+v", req.ToolChoice)
	}
}

// 顶层 tools 与 additional_tools 同时存在时合并；重名报错而不是悄悄覆盖。
func TestToolsFromBothPlacesMerge(t *testing.T) {
	body := `{"model":"m","tools":[{"type":"function","name":"top","parameters":{"type":"object"}}],
		"input":[{"type":"additional_tools","tools":[{"type":"custom","name":"exec"}]},
		         {"type":"message","role":"user","content":"hi"}]}`
	req, err := DecodeResponsesRequest([]byte(body), testOpts)
	if err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	if len(req.Tools) != 2 {
		t.Fatalf("两处的工具应合并，实际 %d 个", len(req.Tools))
	}

	dup := `{"model":"m","tools":[{"type":"function","name":"exec","parameters":{"type":"object"}}],
		"input":[{"type":"additional_tools","tools":[{"type":"custom","name":"exec"}]},
		         {"type":"message","role":"user","content":"hi"}]}`
	if _, err := DecodeResponsesRequest([]byte(dup), testOpts); err == nil {
		t.Error("重名工具应报错——悄悄覆盖会让模型调到另一个工具")
	}
}

// 工具声明本身坏了要报错。跳过一个声明过的工具，症状是「模型忽然不会用
// 这个功能了」，排查方向完全错误。
func TestMalformedToolInAdditionalToolsRejected(t *testing.T) {
	body := `{"model":"m","input":[
		{"type":"additional_tools","tools":[{"name":"没有 type"}]},
		{"type":"message","role":"user","content":"hi"}]}`
	_, err := DecodeResponsesRequest([]byte(body), testOpts)
	if err == nil {
		t.Fatal("缺 type 的工具声明应被拒")
	}
	if !strings.Contains(err.Error(), "additional_tools") {
		t.Errorf("错误信息应指明出错位置：%v", err)
	}
}

// 裸文本工具在 IR 层的约束：有 schema 就是矛盾声明。
func TestTextToolWithSchemaIsInvalid(t *testing.T) {
	d := ir.ToolDeclaration{Name: "exec", InputForm: ir.InputFormText,
		InputSchema: []byte(`{"type":"object"}`)}
	if err := d.Validate(); err == nil {
		t.Error("裸文本工具带 schema 应被拒")
	}
}
