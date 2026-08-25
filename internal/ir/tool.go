package ir

import (
	"encoding/json"
	"fmt"
	"strings"
)

// 工具的风险等级（read/write/destructive/network/code_execution）、副作用
// （none/reversible/irreversible）与来源信任级别（builtin/configured/remote）
// 三套枚举随 4303ee5 的执行层一起删除。
//
// 它们服务的是「Runtime 决定要不要执行」这个判断，而本项目现在不执行任何
// 工具：提案原样交回客户端，风险由客户端的策略层评估。把三套枚举留在这里
// 会给出一个错误暗示——好像本进程做过风险分级。要重新引入它们的前提是
// 先有执行层，而执行层已经明确从路线图移出。

// ToolID 是工具在注册表中的唯一标识，三段都必须逐字匹配。
//
// 规格五章要求工具名精确匹配，不做模糊查找、不做大小写折叠、不做前缀补全——
// 模糊匹配会让模型输出的近似工具名意外命中一个它无权调用的工具。
type ToolID struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Version   string `json:"version"`
}

// String 返回规范形式 namespace/name@version。
func (id ToolID) String() string {
	return id.Namespace + "/" + id.Name + "@" + id.Version
}

// Valid 报告三段是否都非空且不含分隔符。含分隔符会让 String 的输出产生歧义，
// 进而让两个不同的 ToolID 渲染成同一个字符串。
func (id ToolID) Valid() bool {
	for _, part := range []string{id.Namespace, id.Name, id.Version} {
		if part == "" || strings.ContainsAny(part, "/@ \t\n") {
			return false
		}
	}
	return true
}

// 工具契约 ToolSpec（含 InputSchema、ExecutorRef、Timeout、并发资源集合、
// Validate、RequiresApproval）与 ParseToolID 一并删除，理由同上：它们描述的
// 是一个注册表里的可执行工具，而本项目看到的工具全部来自客户端单次请求的
// 声明——没有注册表，没有执行器，也没有需要解析的规范化标识。
//
// 客户端声明的工具由下面的 ToolDeclaration 表达。

// NamespaceClient 是分配给「客户端在请求里声明的工具」的固定命名空间。
//
// 这类工具只存在于单次请求内：本项目不掌握它们的风险等级，也没有对应的
// 执行器，因此绝不执行它们——解析出的调用建议原样转回给客户端，由客户端在
// 自己的权限边界内决定怎么办。用一个固定 namespace 把它们标出来，
// 下游一眼就能看出「这个调用不该在本地执行」。
const NamespaceClient = "client"

// VersionDeclared 是客户端声明工具的占位版本。三种客户端协议都没有版本概念，
// 而 ToolID 要求三段非空，用固定值填充并保持语义可读。
const VersionDeclared = "declared"

// ToolDeclaration 是客户端在请求里声明的一个工具。
//
// 与 ToolSpec 的区别是刻意的：ToolSpec 是 Runtime 注册表里的完整契约，
// 含风险等级、执行器和超时；ToolDeclaration 只有客户端告诉我们的那点信息。
// 不把缺失的字段用「合理默认值」补上，是因为假装知道一个工具的风险等级，
// 比承认不知道危险得多。
type ToolDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`

	// InputForm 说明这个工具的参数长什么样。零值是 InputFormObject，
	// 也就是绝大多数工具的那种「一个 JSON 对象」。
	//
	// 零值刻意选成 object 而不是空值报错：现有三个协议的解码器和
	// pkg/toolbridge 都在构造 ToolDeclaration，让它们一行都不用改，
	// 才谈得上「加了个能力，没动坏已经跑通的路」。
	InputForm InputForm `json:"input_form,omitempty"`
}

// InputForm 是工具参数的形态。
//
// 加它是因为「参数一定是 JSON 对象」这个假设在真实客户端面前不成立：
// Codex 的 exec 工具接收的是一段裸 JavaScript 源码，描述里明写
// 「not JSON, not quoted strings, not markdown code fences」。
// 硬把它塞进 JSON 对象，等于要求一个能力较弱的纯文本模型先把多行代码
// 做一遍 JSON 转义——那正是它最容易写错的地方。
type InputForm string

const (
	// InputFormObject 是参数为 JSON 对象的常规工具。它是零值。
	InputFormObject InputForm = ""
	// InputFormText 是参数为一段裸文本的工具（Responses 协议的 custom 类型）。
	InputFormText InputForm = "text"
)

// Valid 报告 f 是否是已定义的形态。
func (f InputForm) Valid() bool {
	switch f {
	case InputFormObject, InputFormText:
		return true
	}
	return false
}

// Text 报告这个形态的参数是不是裸文本。
func (f InputForm) Text() bool { return f == InputFormText }

// TextArguments 把一段裸文本包成 IR 内部存储用的 JSON 字符串标量。
//
// IR 里的 Arguments 始终保持合法 JSON，即使工具的输入是裸文本。理由很实在：
// cmd/utr-server 的 /v1/parse 会把 Arguments 直接序列化进 HTTP 响应，
// 装一段裸文本会让整个响应变成非法 JSON，接入方在完全不相干的地方炸掉。
func TextArguments(text string) json.RawMessage {
	b, err := json.Marshal(text)
	if err != nil {
		// 一个 Go string 编不成 JSON 字符串是不可能的。
		return json.RawMessage(`""`)
	}
	return b
}

// ID 返回这份声明在 IR 中的工具标识。
func (d ToolDeclaration) ID() ToolID {
	return ToolID{Namespace: NamespaceClient, Name: d.Name, Version: VersionDeclared}
}

// ValidDeclaredName 报告 name 是否符合 OpenAI 对 function name 的约束
// （1--64 个字符，仅限字母、数字、下划线和短横线）。
//
// 三种协议对工具名的约束大体一致，这里取最严的一套。收紧的实际作用是：
// 名字里不可能出现 / 与 @，于是 ToolID 的字符串形式不会产生歧义，
// 也堵掉了用工具名注入协议标记的路子。
func ValidDeclaredName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// Validate 检查这份声明是否可用。
func (d ToolDeclaration) Validate() error {
	if !ValidDeclaredName(d.Name) {
		return fmt.Errorf("ir: 工具名 %q 非法，只允许 1-64 个字母、数字、下划线或短横线", d.Name)
	}
	if !d.InputForm.Valid() {
		return fmt.Errorf("ir: 工具 %q 的 input_form 非法：%q", d.Name, d.InputForm)
	}
	// 裸文本工具必须没有 schema。这不是宽松处理，是把一个静默失败堵死：
	// 若允许它带一份 {"type":"object","properties":{}}，Prompt 就会照着
	// schema 教模型写 JSON，模型老老实实输出 {}，而我们期待的是一段脚本。
	// 没有报错、没有异常，只有一个永远调不动的工具。
	if d.InputForm.Text() {
		if len(d.InputSchema) > 0 {
			return fmt.Errorf("ir: 工具 %q 是裸文本输入，不应带参数 schema", d.Name)
		}
		return nil
	}
	if len(d.InputSchema) == 0 {
		return fmt.Errorf("ir: 工具 %q 缺少参数 schema", d.Name)
	}
	if !json.Valid(d.InputSchema) {
		return fmt.Errorf("ir: 工具 %q 的参数 schema 不是合法 JSON", d.Name)
	}
	return nil
}
