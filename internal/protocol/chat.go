package protocol

import (
	"encoding/json"
	"fmt"

	"github.com/kw-decade/Texvoke/internal/ir"
)

// Role 是 Chat Completions 消息的角色。
type Role string

const (
	RoleSystem    Role = "system"
	RoleDeveloper Role = "developer" // 较新的模型用它取代 system
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Valid 报告 r 是否是已知角色。空值永远无效。
func (r Role) Valid() bool {
	switch r {
	case RoleSystem, RoleDeveloper, RoleUser, RoleAssistant, RoleTool:
		return true
	default:
		return false
	}
}

// Instructional 报告该角色的内容是否被当作指令。
//
// 这是信任边界的判定依据：只有 system 与 developer 的内容是指令，
// user、assistant、tool 的内容一律是数据。规格十一章要求工具结果里的
// “请执行下一条命令”必须当普通文本，判定就落在这里。
func (r Role) Instructional() bool {
	return r == RoleSystem || r == RoleDeveloper
}

// ToolResultBlock 是一条消息里携带的某次调用的结果。
//
// 注意与 ir.ToolResult 区分：ir.ToolResult 是 Runtime 亲自执行工具后的完整
// 记录，带状态、时间戳和幂等键；ToolResultBlock 只是协议消息里传输的那一小
// 部分。历史消息里的结果多半来自客户端自己的执行，Runtime 从未见过对应的
// ir.ToolResult，因此不能用后者来表示。
type ToolResultBlock struct {
	// CallID 关联回提出这次调用的 tool_call_id / tool_use_id。
	CallID string `json:"call_id"`

	// Content 是结果内容，保留原始 JSON 值（可能是字符串，也可能是
	// content part 数组）。
	Content json.RawMessage `json:"content,omitempty"`

	// IsError 对应 Anthropic 的 is_error。Chat Completions 没有这个字段，
	// 但丢掉它会让「工具报错了」和「工具返回了一段恰好像错误的文本」
	// 变得无法区分。
	IsError bool `json:"is_error,omitempty"`

	// Freeform 标明这条结果对应的是一次裸文本工具调用。
	//
	// 必须记住它：Responses 用两种不同的 item 承载结果
	// （function_call_output 与 custom_tool_call_output，后者的 output 还是
	// 数组而不是字符串）。Codex 每一轮都把完整历史重发一遍，形态记错了，
	// 从第二轮起每条历史结果都会被渲染成错误的 item 类型。
	Freeform bool `json:"freeform,omitempty"`
}

// Message 是归一化后的一条对话消息。
type Message struct {
	Role Role `json:"role"`

	// Content 保留原始 JSON 值。Chat Completions 允许它是字符串、
	// content part 数组或 null，这里不提前压平成字符串，否则多模态
	// 内容在往返后会丢失。需要纯文本时用 Text 方法。
	Content json.RawMessage `json:"content,omitempty"`

	Name string `json:"name,omitempty"`

	// ToolCalls 只在 assistant 消息上有意义。
	ToolCalls []ir.ToolCallProposal `json:"tool_calls,omitempty"`

	// ToolResults 携带这条消息里的工具结果。
	//
	// 用切片而不是单个 ID，是被协议差异逼出来的：Chat Completions 的一条
	// tool 消息只装一个结果，而 Anthropic 的一条 user 消息可以同时携带多个
	// tool_result block。单字段装不下后者；把它展开成多条消息又会丢失
	// 「这几个结果原本属于同一轮」这个信息，编码回去时无从还原分组。
	ToolResults []ToolResultBlock `json:"tool_results,omitempty"`

	// Refusal 是 assistant 的显式拒绝内容。保留它是因为拒绝分类需要
	// 区分「模型明确拒绝」和「模型只是没调用工具」。
	Refusal string `json:"refusal,omitempty"`
}

// hasContent 报告 Content 是否携带了实际内容。
//
// 单看长度不够：JSON 的 null 是四个字节，len 不为零，但它表示的正是「没有
// 内容」。assistant 只发工具调用时，Chat Completions 就要求写成 content: null。
func (m Message) hasContent() bool {
	return len(m.Content) > 0 && string(m.Content) != "null"
}

// isEmptyContent 报告 Content 是不是那几种「明确表示什么都没有」的形态。
//
// 比 hasContent 更宽：空字符串和空数组虽然占字节，但同样不携带任何信息。
// 解码时用它把噪声消息丢掉���—真实客户端会发这种消息，Codex 经 CC Switch
// 转协议时就会产生一条。
//
// 只认这几种确定的形态，不去猜：图片、音频这些非文本 content 提不出文字，
// 但绝不是空消息，误丢会让用户发的图凭空消失。
func (m Message) isEmptyContent() bool {
	if !m.hasContent() {
		return true
	}
	switch string(m.Content) {
	case `""`, `[]`:
		return true
	}
	return false
}

// Text 从 Content 中提取纯文本，供 Prompt 编译和拒绝检测使用。
//
// 字符串直接返回；content part 数组只拼接 type 为 text 的部分；
// 其余形态（null、图片、音频）返回空串。提取失败不报错——这个方法的
// 用途是「尽量拿到可读文本」，判断消息是否合法是 Validate 的事。
func (m Message) Text() string {
	if len(m.Content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err != nil {
		return ""
	}
	var buf []byte
	for _, p := range parts {
		// 同一件事在三个协议里有三个名字：Chat 与 Anthropic 叫 text，
		// Responses 的模型输出叫 output_text、用户输入叫 input_text。
		// Message 是三协议的并集——少认一个，那一类消息在这里就静默变成空串。
		//
		// input_text 是漏得最久的一个：扫真实 Codex 会话，它出现 8889 次，
		// 是最常见的用户内容形态。提不出用户原文，工具候选排序就等于没做。
		switch p.Type {
		case "text", "output_text", "input_text":
			buf = append(buf, p.Text...)
		}
	}
	return string(buf)
}

// Validate 检查消息的角色与字段组合是否自洽。
//
// 组合检查不是形式主义：一条带 tool_calls 的 user 消息，或一条没有
// tool_call_id 的 tool 消息，都意味着上游或路由改写过请求——
// 而 router_mutation 正是规格七章要求能诊断出来的拒绝分类之一。
func (m Message) Validate() error {
	if !m.Role.Valid() {
		return fmt.Errorf("protocol: 未知的消息角色 %q", m.Role)
	}
	if len(m.ToolCalls) > 0 && m.Role != RoleAssistant {
		return fmt.Errorf("protocol: 只有 assistant 消息能携带 tool_calls，实际角色为 %q", m.Role)
	}
	// 工具结果的载体在两种协议里不同：Chat Completions 用独立的 tool 消息，
	// Anthropic 把 tool_result 放进 user 消息。内部表示取两者的并集，
	// 各协议的编码器再收窄到自己的形状。
	if len(m.ToolResults) > 0 && m.Role != RoleTool && m.Role != RoleUser {
		return fmt.Errorf("protocol: 只有 tool 或 user 消息能携带工具结果，实际角色为 %q", m.Role)
	}
	if m.Role == RoleTool && len(m.ToolResults) == 0 {
		return fmt.Errorf("protocol: tool 消息必须携带至少一个工具结果")
	}
	for i, r := range m.ToolResults {
		if r.CallID == "" {
			return fmt.Errorf("protocol: 第 %d 个工具结果缺少 call_id，无法关联回具体调用", i)
		}
	}
	// 一条消息必须携带某种内容，空消息进入 Prompt 只会浪费上下文并让
	// 部分上游报错。assistant 可以只有工具调用，tool 可以只有结果。
	if !m.hasContent() && len(m.ToolCalls) == 0 && len(m.ToolResults) == 0 && m.Refusal == "" {
		return fmt.Errorf("protocol: %q 消息没有任何内容", m.Role)
	}
	for i, tc := range m.ToolCalls {
		if err := tc.Validate(); err != nil {
			return fmt.Errorf("protocol: assistant 消息的第 %d 个 tool_call 非法：%w", i, err)
		}
	}
	return nil
}

// ToolChoiceMode 是 tool_choice 的四种语义。
type ToolChoiceMode string

const (
	ToolChoiceAuto     ToolChoiceMode = "auto"     // 允许零个或多个调用
	ToolChoiceNone     ToolChoiceMode = "none"     // 禁止调用
	ToolChoiceRequired ToolChoiceMode = "required" // 至少一个合法调用
	ToolChoiceNamed    ToolChoiceMode = "named"    // 只能调用指定的那个工具
)

// ToolChoice 是归一化后的工具选择约束。
type ToolChoice struct {
	Mode ToolChoiceMode `json:"mode"`
	// Name 只在 Mode 为 named 时有意义。
	Name string `json:"name,omitempty"`
}

// RequiresCall 报告这次请求是否要求至少产生一个工具调用。
//
// 注意规格七章的限定：在虚拟协议模式下这只是一个 requested requirement，
// 不是保证。模型没调用时必须返回 tool_required_but_not_produced 之类的
// 明确状态，绝不能伪造一个调用来满足它。
func (c ToolChoice) RequiresCall() bool {
	return c.Mode == ToolChoiceRequired || c.Mode == ToolChoiceNamed
}

// FinishReason 是一次生成的终止原因。
type FinishReason string

const (
	FinishStop          FinishReason = "stop"
	FinishLength        FinishReason = "length"
	FinishToolCalls     FinishReason = "tool_calls"
	FinishContentFilter FinishReason = "content_filter"
)

// Valid 报告 f 是否是已知的终止原因。空值永远无效。
func (f FinishReason) Valid() bool {
	switch f {
	case FinishStop, FinishLength, FinishToolCalls, FinishContentFilter:
		return true
	default:
		return false
	}
}

// Usage 是 token 用量统计。指针字段用于区分「上游没报告」和「上游报告为 0」。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatRequest 是归一化后的 Chat Completions 请求。
type ChatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`

	// Tools 是客户端声明的工具。为空是一个重要信号：规格七章把
	// client_capability_missing（tools=0）列为第一类拒绝根因，
	// 此时再强的 Prompt 注入也没有意义。
	Tools      []ir.ToolDeclaration `json:"tools,omitempty"`
	ToolChoice ToolChoice           `json:"tool_choice"`

	// ParallelToolCalls 为 nil 表示客户端没有表态，交由下游决定默认值。
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`

	Stream bool `json:"stream,omitempty"`

	// Extra 保留所有未被本结构识别的顶层字段，原样透传给上游。
	// 规格十章要求未知扩展字段要么保留、要么明确丢弃，不能默默吞掉。
	Extra map[string]json.RawMessage `json:"-"`
}

// ChatResponse 是归一化后的 Chat Completions 响应（单 choice）。
//
// 当前只建模一个 choice：n>1 在工具调用场景下语义混乱（几个候选调用里
// 该执行哪个？），本 Runtime 不支持，遇到时在解码处显式报错而非静默取第一个。
type ChatResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Created int64  `json:"created"`

	Content   json.RawMessage       `json:"content,omitempty"`
	Refusal   string                `json:"refusal,omitempty"`
	ToolCalls []ir.ToolCallProposal `json:"tool_calls,omitempty"`

	FinishReason FinishReason `json:"finish_reason"`
	Usage        *Usage       `json:"usage,omitempty"`
}

// rejectFreeform 在协议装不下裸文本参数时显式报错。
//
// Chat Completions 与 Anthropic Messages 都把工具参数定死为 JSON 对象
// （Anthropic 的 tool_use.input 按 API 必须是对象，Chat 的 arguments 虽然
// 是字符串但约定装 JSON）。裸文本在这两条线上无法表达。
//
// 报错而不是降级：把一段脚本硬塞进 arguments，客户端会拿它去 JSON.parse，
// 失败在客户端那边，错误信息与真正的原因隔着一整条链路。而这条路径本来
// 也不该可达——两种协议的客户端都声明不了裸文本工具。
func rejectFreeform(proto string, calls []ir.ToolCallProposal) error {
	for _, c := range calls {
		if c.ArgumentForm.Text() {
			return fmt.Errorf(
				"protocol: 调用 %s 的参数是裸文本，%s 协议只能表达 JSON 对象参数",
				c.CallID, proto)
		}
	}
	return nil
}
