package toolbridge

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kw-decade/Texvoke/internal/ir"
	"github.com/kw-decade/Texvoke/internal/prompt"
	"github.com/kw-decade/Texvoke/internal/protocol"
	"github.com/kw-decade/Texvoke/internal/vproto"
)

// Protocol 是客户端或上游说的线上协议。
type Protocol string

const (
	// ProtocolChat 是 OpenAI Chat Completions。
	ProtocolChat Protocol = "chat"
	// ProtocolResponses 是 OpenAI Responses。
	ProtocolResponses Protocol = "responses"
	// ProtocolAnthropic 是 Anthropic Messages。
	ProtocolAnthropic Protocol = "anthropic"
)

// Valid 报告 p 是否是已支持的协议。
func (p Protocol) Valid() bool {
	switch p {
	case ProtocolChat, ProtocolResponses, ProtocolAnthropic:
		return true
	}
	return false
}

// DecodeOptions 是解码请求时需要的上下文。
type DecodeOptions struct {
	// SessionID 与 RequestID 会绑进解析出来的调用，用于关联与诊断。
	SessionID string
	RequestID string

	// MaxBytes 是请求体上限，0 取默认值 32 MB。
	//
	// 必须有上限：没有它，一个构造的大请求就能吃光内存。
	MaxBytes int64
}

// Request 是解码后的客户端请求。
//
// 它是不透明的：内部持有三协议各自的结构，外部只通过方法访问。
// 这样做是为了让 pkg/toolbridge 保持稳定 API——internal/protocol
// 的类型会随协议演进变化，而你的反代不该跟着改。
type Request struct {
	proto Protocol

	chat      *protocol.ChatRequest
	responses *protocol.ResponsesRequest
	messages  *protocol.MessagesRequest

	// dropped 记录压平 content 时无法表达的 part 类型，见 DroppedContentTypes。
	dropped []string

	// deduped 记录删掉的重复指令消息条数，见 DedupedInstructions。
	deduped int

	// maxTokens 是显式设定的输出上限，覆盖协议自带的字段。
	//
	// 单独存一份是因为 Chat 协议压根没有这个字段，而转成 Anthropic 时
	// 它是必填的。借用别的协议的槽位来存会让「这个值从哪来」变成一个
	// 需要读三处代码才能回答的问题。
	maxTokens int
}

// DecodeRequest 把客户端发来的原始请求体解成 Request。
//
// 这一步替你做完了三协议的差异：Chat 的 function 包装、Anthropic 的
// input_schema、Responses 的扁平结构，以及顶层 system 与消息里的
// system 的区别，出来之后都是同一套访问方法。
func DecodeRequest(proto Protocol, raw []byte, opts DecodeOptions) (*Request, error) {
	if !proto.Valid() {
		return nil, fmt.Errorf("toolbridge: 未知协议 %q", proto)
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = protocol.DefaultMaxRequestBytes
	}
	popts := protocol.DecodeOptions{
		SessionID: opts.SessionID, RequestID: opts.RequestID,
		Now: time.Now(), MaxBytes: maxBytes,
	}

	r := &Request{proto: proto}
	var err error
	switch proto {
	case ProtocolChat:
		r.chat, err = protocol.DecodeChatRequest(raw, popts)
	case ProtocolResponses:
		r.responses, err = protocol.DecodeResponsesRequest(raw, popts)
	case ProtocolAnthropic:
		r.messages, err = protocol.DecodeMessagesRequest(raw, popts)
	}
	if err != nil {
		return nil, fmt.Errorf("toolbridge: 解码 %s 请求失败：%w", proto, err)
	}
	return r, nil
}

// Protocol 返回这个请求用的协议。
func (r *Request) Protocol() Protocol { return r.proto }

// SkippedItemTypes 返回解码时跳过的扩展 item 类型（目前只有 Responses 有）。
//
// 跳过不认识的 item 是为了不把整轮对话挡在门外，但跳过本身要能被看见——
// 接入方应该把它记进日志。工具声明的标准位置是 tools 字段，所以真要是所有
// 工具都藏在这类 item 里，请求会直接变成「没有工具」而报错，不会静默降级。
func (r *Request) SkippedItemTypes() []string {
	if r.responses == nil {
		return nil
	}
	return r.responses.SkippedItemTypes
}

// SkippedTools 返回解码时被跳过的工具名。
//
// 目前只有 Responses 的 namespace 类型会落进来：它嵌套的工具真名是
// <namespace>.<tool>，而点号在工具名里不被允许（那道约束防的是 ToolID
// 的字符串形式产生歧义）。
//
// 接入方应该把它记进日志。一个声明了却用不了的工具，症状是「模型从来
// 不用这个功能」——不报错，也查不出来，除非这里有一行记录。
func (r *Request) SkippedTools() []string {
	if r.responses == nil {
		return nil
	}
	return r.responses.SkippedTools
}

// Model 返回客户端请求的模型名。
func (r *Request) Model() string {
	switch r.proto {
	case ProtocolChat:
		return r.chat.Model
	case ProtocolResponses:
		return r.responses.Model
	default:
		return r.messages.Model
	}
}

// SetModel 改写模型名，用于上游模型与客户端模型不同名的情形。
func (r *Request) SetModel(m string) {
	switch r.proto {
	case ProtocolChat:
		r.chat.Model = m
	case ProtocolResponses:
		r.responses.Model = m
	default:
		r.messages.Model = m
	}
}

// Stream 报告客户端是否要求流式。
func (r *Request) Stream() bool {
	switch r.proto {
	case ProtocolChat:
		return r.chat.Stream
	case ProtocolResponses:
		return r.responses.Stream
	default:
		return r.messages.Stream
	}
}

// MaxTokens 返回客户端给出的输出上限，0 表示没给。
//
// Anthropic 把它定为必填，另外两个协议可选——EncodeAs 转成 Anthropic
// 时如果这里是 0，会显式报错而不是替你填一个数：模型在谁也没决定过的
// 长度上被截断，是最难查的一类问题。
func (r *Request) MaxTokens() int {
	if r.maxTokens > 0 {
		return r.maxTokens
	}
	switch r.proto {
	case ProtocolChat:
		return 0
	case ProtocolResponses:
		return r.responses.MaxOutputTokens
	default:
		return r.messages.MaxTokens
	}
}

// Tools 返回客户端声明的工具，已归一化。
//
// 这正是你原本要为每种协议写一遍的那段代码。
func (r *Request) Tools() []Tool {
	var decls []ir.ToolDeclaration
	switch r.proto {
	case ProtocolChat:
		decls = r.chat.Tools
	case ProtocolResponses:
		decls = r.responses.Tools
	default:
		decls = r.messages.Tools
	}
	out := make([]Tool, 0, len(decls))
	for _, d := range decls {
		out = append(out, Tool{
			Name: d.Name, Description: d.Description, InputSchema: d.InputSchema,
			Freeform: d.InputForm.Text(),
		})
	}
	return out
}

// RequireCall 报告客户端是否要求必须调用工具（tool_choice=required/named）。
//
// 它只是一个**请求**，不是保证：模型没调用时你必须返回明确状态，
// 绝不能伪造一个调用来满足它。
func (r *Request) RequireCall() bool {
	return r.toolChoice().RequiresCall()
}

// RequiredTool 返回客户端指名要调用的工具，为空表示没指名。
func (r *Request) RequiredTool() string { return r.toolChoice().Name }

func (r *Request) toolChoice() protocol.ToolChoice {
	switch r.proto {
	case ProtocolChat:
		return r.chat.ToolChoice
	case ProtocolResponses:
		return r.responses.ToolChoice
	default:
		return r.messages.ToolChoice
	}
}

// LastUserText 返回最后一条用户消息的纯文本，供工具候选排序使用。
func (r *Request) LastUserText() string {
	msgs := r.msgs()
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == protocol.RoleUser {
			return msgs[i].Text()
		}
	}
	return ""
}

func (r *Request) msgs() []protocol.Message {
	switch r.proto {
	case ProtocolChat:
		return r.chat.Messages
	case ProtocolResponses:
		return r.responses.Input
	default:
		return r.messages.Messages
	}
}

// Messages 返回归一化后的消息序列。
//
// 导出它是给进程内编排层（internal/gateway）用的：适配器接口直接吃
// []protocol.Message，不需要再经过一次 JSON 编码解码。HTTP sidecar 的
// 接入方走不到这里——它们拿到的是 EncodeAs 之后的字节。
func (r *Request) Messages() []protocol.Message { return r.msgs() }

// AppendMessages 把救援循环产出的消息追加到对话末尾。
//
// 与 Messages 同理，服务进程内编排层。追加而不是替换：针对性追问的语义
// 就是「在原对话之后补一句话」，重发整个历史是前身实现之前的错误做法。
func (r *Request) AppendMessages(msgs []RecoveryMessage) {
	if len(msgs) == 0 {
		return
	}
	cur := r.msgs()
	for _, m := range msgs {
		role := protocol.RoleUser
		if m.Role == "system" {
			role = protocol.RoleSystem
		}
		b, err := json.Marshal(m.Text)
		if err != nil {
			continue
		}
		cur = append(cur, protocol.Message{Role: role, Content: b})
	}
	r.setMsgs(cur)
}

func (r *Request) setMsgs(m []protocol.Message) {
	switch r.proto {
	case ProtocolChat:
		r.chat.Messages = m
	case ProtocolResponses:
		r.responses.Input = m
	default:
		r.messages.Messages = m
	}
}

func (r *Request) setTools(t []ir.ToolDeclaration) {
	switch r.proto {
	case ProtocolChat:
		r.chat.Tools = t
	case ProtocolResponses:
		r.responses.Tools = t
	default:
		r.messages.Tools = t
	}
}

// PrepareForTextUpstream 把请求改造成纯文本上游能接受的形状。
//
// 做三件事：
//
//  1. 把历史里的工具调用与结果**改写成文本**（见 inlineToolHistory）；
//  2. 把编译好的协议说明加进 system 消息（已有 system 就追加在后面，
//     而不是替换——客户端的 system prompt 可能有几万字，那是它的业务
//     逻辑，覆盖掉等于把它的产品功能删了）；
//  3. 清空 tools 与 tool_choice——上游不认识它们，留着有些中转站会直接报 400。
//
// signal 是本会话的协议信号，用于渲染历史里的调用。systemPrompt 为空
// （这一轮没有工具）时，前一步仍然要做：历史该长什么样，与这一轮有没有
// 工具无关。
//
// 这个方法就地修改 r。改完用 Encode 或 EncodeAs 拿到发给上游的请求体。
func (r *Request) PrepareForTextUpstream(systemPrompt, signal string) {
	r.prepare(systemPrompt, signal, "")
}

// PrepareForTextUpstreamWithReminder 与 PrepareForTextUpstream 相同，
// 外加把一句格式提醒追加到对话末尾。
//
// 分成两个方法而不是加参数，是为了不动已有调用方的签名。推荐路径
// CompileRequest 走的是带提醒的这个——协议说明被十几 KB 的工具清单
// 稀释是实测确认的失败模式，默认就该防住。
func (r *Request) PrepareForTextUpstreamWithReminder(systemPrompt, signal, reminder string) {
	r.prepare(systemPrompt, signal, reminder)
}

func (r *Request) prepare(systemPrompt, signal, reminder string) {
	if signal != "" {
		r.setMsgs(inlineToolHistory(r.msgs(), signal))
	}
	// content 压平成字符串。纯文本上游只吃字符串——原样把 content part
	// 数组丢过去，中转站会对它做字符串化，模型看到的是「[object Object]」。
	//
	// 这不是假想：真实 Codex 的每条消息 content 都是
	// [{"type":"input_text","text":"..."}]，整条链路的上游收到的就是那六个字。
	// 这一步放在中间件里而不是让接入方各自处理，因为它是「为纯文本上游
	// 准备」这件事的一部分——每个接入方都会踩同一个坑。
	msgs, dropped := flattenContent(r.msgs())
	r.setMsgs(msgs)
	r.dropped = dropped

	// 删掉重复的指令消息。
	//
	// 真实 agent 会把整套指令按 turn 重发：抓包统计 Codex 的一个请求里，
	// 同一条 26 KB 的沙箱说明出现 4 次，140 KB 消息里 66% 是重复内容。
	// 扫 34 个会话，6 个重复超过 15%，最高 79%。
	//
	// 这直接影响协议指令的占比——同样的改动在不同会话里效果能差 3 倍，
	// 而我一度以为自己在测同一件事。去重是无损变换：完全相同的指令重复
	// 声明是幂等的，保留一份不损失任何信息。
	msgs, deduped := dedupeInstructions(r.msgs())
	r.setMsgs(msgs)
	r.deduped = deduped

	// 协议说明独立成一条消息放末尾，不再拼进客户端的指令消息——被 31 KB
	// 的 hook 注入淹没是实测确认的失败模式，见 appendSystem 的注释。
	if systemPrompt != "" {
		r.setMsgs(appendSystemFor(r.msgs(), systemPrompt, r.proto))
	}
	// 提醒再追加一次，落在说明之后——它是最后一句话，离生成点最近。
	if reminder != "" {
		r.setMsgs(appendReminder(r.msgs(), reminder, r.proto))
	}

	r.setTools(nil)
	// tool_choice 也要清掉。留着 "required" 而没有 tools，OpenAI 兼容的
	// 上游会直接报 400——工具已经不在原生字段里了，约束却还指着它们。
	r.setToolChoice(protocol.ToolChoice{Mode: protocol.ToolChoiceAuto})
} // DroppedContentTypes 返回压平 content 时无法表达的 part 类型（去重后）。
// 纯文本上游收不了图片和音频。丢弃是必然的，但必须能被看见——用户发了
// 一张图而模型完全不知道有这回事，是最难解释的一类「模型怎么这么笨」。
//
// 压平时这些 part 会留下一个占位说明，所以模型至少知道「有东西我看不到」。
func (r *Request) DroppedContentTypes() []string { return r.dropped }

// DedupedInstructions 返回被删掉的重复指令消息条数。
//
// 接入方应该把它记进日志。静默压缩上下文是不可接受的：万一判据出错删多了，
// 症状会是「模型好像忘了某条规则」——那是最难查的一类问题，除非这里有一个数。
func (r *Request) DedupedInstructions() int { return r.deduped }

// dedupeInstructions 删掉内容完全相同的重复指令消息，保留第一次出现。
//
// 两个判据都刻意写严：
//
//   - **只动指令性消息**（system / developer）。指令的语义是「约束」，
//     重复声明同一约束是幂等的；user 消息的语义是「对话轮次」，用户完全
//     可能真的说了两次同样的话（「继续」「再来一次」），删掉就改变了历史。
//     代价是 Codex 那 4 份用 user 角色发的 AGENTS.md（11.7 KB）留着。
//   - **保留第一次，纯删除不重排**。试过保留最后一次（越靠后约束越强），
//     实测那样会把 user 消息挤到 developer 之前，改变了对话结构。而末尾
//     提醒已经解决了「协议说明离生成点太远」的问题，不必为此重排。
//
// 判据是内容**完全相同**，不做任何近似合并——两条只差一个字的指令可能
// 是刻意的修订，合并它们等于替客户端改写业务逻辑。
func dedupeInstructions(msgs []protocol.Message) ([]protocol.Message, int) {
	seen := make(map[string]bool, len(msgs))
	out := make([]protocol.Message, 0, len(msgs))
	removed := 0

	for _, m := range msgs {
		if !m.Role.Instructional() {
			out = append(out, m)
			continue
		}
		// 带工具调用或结果的消息不参与去重：那不是纯指令，删掉会丢关联。
		if len(m.ToolCalls) > 0 || len(m.ToolResults) > 0 {
			out = append(out, m)
			continue
		}
		key := string(m.Role) + "\x00" + m.Text()
		if seen[key] {
			removed++
			continue
		}
		seen[key] = true
		out = append(out, m)
	}

	if removed == 0 {
		return msgs, 0
	}
	return out, removed
}

// flattenContent 把 content part 数组压平成纯字符串。
func flattenContent(msgs []protocol.Message) ([]protocol.Message, []string) {
	var dropped []string
	out := make([]protocol.Message, len(msgs))
	copy(out, msgs)

	for i, m := range out {
		if len(m.Content) == 0 || string(m.Content) == "null" {
			continue
		}
		// 已经是字符串就别动——重新序列化只会引入差异。
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			continue
		}
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(m.Content, &parts) != nil {
			// 认不出来的形状原样留着。猜错了比不动更糟。
			continue
		}

		var b strings.Builder
		for _, p := range parts {
			switch p.Type {
			case "text", "output_text", "input_text":
				b.WriteString(p.Text)
			case "":
				// 没有 type 的 part，形状不明，跳过但记一笔。
				if !slices.Contains(dropped, "(无 type)") {
					dropped = append(dropped, "(无 type)")
				}
			default:
				// 留一个占位：让模型知道「有东西我看不到」，
				// 而不是让那条消息看起来只有半句话。
				b.WriteString("[无法传给上游的内容：" + p.Type + "]")
				if !slices.Contains(dropped, p.Type) {
					dropped = append(dropped, p.Type)
				}
			}
		}
		out[i] = textMessage(m.Role, b.String())
		out[i].ToolCalls, out[i].ToolResults = m.ToolCalls, m.ToolResults
		out[i].Name, out[i].Refusal = m.Name, m.Refusal
	}
	return out, dropped
}

// inlineToolHistory 把历史消息里的工具调用与结果改写成纯文本。
//
// 为什么必须做：上游是**纯文本模型**，它看不见 assistant.tool_calls 和
// tool 消息这些结构化字段——中转站把消息拼成 prompt 时，这些字段通常
// 直接被丢掉。不改写的后果是模型第二轮完全不知道自己上一轮做过什么，
// agent loop 断在第二步。
//
// 这是实测出来的：上游收到的请求体里 tool_calls 与工具结果都在，
// 模型的回答却是「本轮未提供可用的执行工具」。
//
// 改写规则：
//
//   - assistant 的调用 → 用同一个信号渲染成 envelope，接在原文后面。
//     信号必须一致，否则模型看到的历史与当前规范自相矛盾。
//   - 工具结果 → 用 prompt.RenderToolResult 包成带信任边界的区块，
//     并转成 user 消息。结果内容来自文件、网页或另一个服务，其中的
//     「忽略之前的指示」必须被当成数据而不是指令。
func inlineToolHistory(msgs []protocol.Message, signal string) []protocol.Message {
	need := false
	for _, m := range msgs {
		if len(m.ToolCalls) > 0 || len(m.ToolResults) > 0 {
			need = true
			break
		}
	}
	if !need {
		return msgs
	}

	// 结果只带 call_id，不带工具名。从调用那边建一张表，让模型能看到
	// 「这段结果是哪个工具返回的」——只有 id 的话它得自己去历史里对。
	toolOf := make(map[string]string, 8)
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			toolOf[c.CallID] = c.Tool.Name
		}
	}

	out := make([]protocol.Message, 0, len(msgs))
	for _, m := range msgs {
		switch {
		case len(m.ToolCalls) > 0:
			text := m.Text()
			if env, err := historyEnvelope(signal, m.ToolCalls); err == nil {
				if text != "" {
					text += "\n"
				}
				text += env
			}
			// 渲染失败时保留原文本、丢掉调用：这一轮的历史会少一段，
			// 但不该因此让整个请求发不出去。
			out = append(out, textMessage(m.Role, text))

		case len(m.ToolResults) > 0:
			var b strings.Builder
			if t := m.Text(); t != "" {
				b.WriteString(t)
				b.WriteByte('\n')
			}
			for i, res := range m.ToolResults {
				if i > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(prompt.RenderToolResult(
					res.CallID, toolOf[res.CallID], resultText(res.Content), res.IsError))
			}
			// 一律转成 user：工具结果是数据不是模型的话，而 tool 角色
			// 在清空 ToolResults 之后已经编码不出去了（Chat 的 tool 消息
			// 必须带 tool_call_id）。
			out = append(out, textMessage(protocol.RoleUser, b.String()))

		default:
			out = append(out, m)
		}
	}
	return out
}

// historyEnvelope 把历史里的一组调用渲染成 envelope 文本。
func historyEnvelope(signal string, calls []ir.ToolCallProposal) (string, error) {
	vc := make([]vproto.Call, 0, len(calls))
	for _, c := range calls {
		call := vproto.Call{ID: safeCallID(c.CallID), Tool: c.Tool.Name}
		if c.ArgumentForm.Text() {
			text, err := c.ArgumentsText()
			if err != nil {
				return "", err
			}
			call.Freeform, call.ArgumentsText = true, text
		} else {
			call.ArgumentsJSON = string(c.Arguments)
		}
		vc = append(vc, call)
	}
	return vproto.RenderEnvelopeSignal(signal, vc)
}

// safeCallID 去掉 id 里会破坏 XML 属性的字符。
//
// 历史 envelope 里的 id 只是给模型看的关联标记，替换掉不影响正确性；
// 而一个含引号的 id 会让渲染失败，代价是整段历史消失。
func safeCallID(id string) string {
	if id == "" {
		return "call"
	}
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(`"<>&`, r) {
			return '_'
		}
		return r
	}, id)
}

// resultText 从工具结果里提取可读文本。
//
// 三种形态都要认：Chat 的字符串、Anthropic 的 content block 数组、
// Responses 裸文本工具的 input_text 数组。认不出来就把原始 JSON 交出去——
// 那总比交出一个空串好，模型至少还能看到内容。
func resultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			switch p.Type {
			case "text", "output_text", "input_text":
				b.WriteString(p.Text)
			}
		}
		if b.Len() > 0 {
			return b.String()
		}
	}
	return string(raw)
}

// textMessage 造一条纯文本消息。
func textMessage(role protocol.Role, text string) protocol.Message {
	content, err := json.Marshal(text)
	if err != nil {
		// 一个 Go string 编不成 JSON 字符串是不可能的。
		content = json.RawMessage(`""`)
	}
	return protocol.Message{Role: role, Content: content}
}

func (r *Request) setToolChoice(c protocol.ToolChoice) {
	switch r.proto {
	case ProtocolChat:
		r.chat.ToolChoice = c
	case ProtocolResponses:
		r.responses.ToolChoice = c
	default:
		r.messages.ToolChoice = c
	}
}

// appendReminder 把格式提醒放到对话末尾，尽量靠近模型开始生成的位置。
//
// 两种协议两种做法，是被 Anthropic 的约束逼出来的：它只允许一条 system
// 消息，末尾再追加一条会直接被拒。所以那条线上把提醒接在最后一条消息的
// 文本后面——不改变消息数量与角色交替，Anthropic 对这两件事很挑。
//
// 另两种协议用独立的 system 消息：它是格式约束，不是用户说的话。混进
// user 消息会让模型以为用户在要求什么。
func appendReminder(msgs []protocol.Message, reminder string, proto Protocol) []protocol.Message {
	if len(msgs) == 0 {
		return msgs
	}
	if proto != ProtocolAnthropic {
		return append(msgs, textMessage(protocol.RoleSystem, reminder))
	}

	last := msgs[len(msgs)-1]
	// 最后一条是 assistant 说明客户端在做 prefill——那是它精心构造的
	// 开头，接一句提醒进去会改写模型该续写的内容。这种情况就不加了。
	if last.Role == protocol.RoleAssistant {
		return msgs
	}
	text := last.Text()
	if text != "" {
		text += "\n\n"
	}
	out := make([]protocol.Message, len(msgs))
	copy(out, msgs)
	merged := textMessage(last.Role, text+reminder)
	merged.ToolCalls, merged.ToolResults = last.ToolCalls, last.ToolResults
	merged.Name, merged.Refusal = last.Name, last.Refusal
	out[len(out)-1] = merged
	return out
}

// maxMergeHostBytes 是「协议说明仍可合并进宿主指令消息」的大小上限。
//
// 2026-08-24 实测的两个端点：Codex 的最后一条指令只有 186 字节，合并在
// 里面工作良好（4/4）；Claude Code 被 hook 插件注入的最后一条指令有
// 31 KB，合并进去后模型完全忽略说明（0/4，说「没有提供文件读取或终端
// 工具」；删掉那条注入同一请求立刻正常调用，把注入移到 user 前面也没用
// ——是绝对体量的问题，不是相对位置的问题）。
// 中间没有数据点，8 KB 是保守插值——协议说明本身约 2 KB，宿主超过 8 KB
// 时占比已不足 1:4。这个值该随更多客户端实测修正。
const maxMergeHostBytes = 8 * 1024

// appendSystemFor 把协议说明放进对话，位置与形态按宿主指令的形状自适应：
//
//   - prefill（最后一条是 assistant）：合并进最后一条指令消息。不能在
//     prefill 后面接任何东西——接了就等于改写客户端精心构造的开头。
//   - 最后一条指令消息很小（≤ maxMergeHostBytes）且是普通指令：合并进去。
//     说明贴着一段小指令，读起来是一体的规则；Codex 全部指令都是这种形状。
//   - 其余情况：独立成一条消息追加在末尾。两类宿主不能合并——
//     太大的（Claude Code 的 hook 插件注入几十 KB 行为规则，实测说明拼进去
//     被淹没，0/4）；以及客户端的状态注入（turn_aborted 之类）。后者尤其
//     要避开：Codex 在用户打断一轮后注入「工具可能被中断、可能只执行了一半」，
//     协议说明紧贴着它，弱模型会把两段读成一件事——2026-08-26 实测有模型
//     因此声称「当前运行时阻止了工具调用」并停手。状态注入描述的是运行时的
//     异常，协议说明描述的是怎么用工具；拼在一起就是给「工具通道坏了」
//     这个错误结论递证据。
//
// 独立成条时的角色：Chat / Responses 用 system（允许序列里多条 system，
// 位置保持在末尾）；Anthropic 用 user——它的编码器会把 system 摘回顶层
// system 字段（协议只有一个 system 位置），等于又回到被淹没的老问题。
//
// 用 user 角色不违反「不可信数据不升级为指令」：这段内容是 Runtime 自己
// 生成的协议规则，不是任何外部输入。红线管的是别人的数据不能变成指令，
// 不是我们自己的指令不能放在 user 位置上。
func appendSystemFor(msgs []protocol.Message, add string, proto Protocol) []protocol.Message {
	if len(msgs) > 0 && msgs[len(msgs)-1].Role == protocol.RoleAssistant {
		return mergeIntoLastInstruction(msgs, add)
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if !msgs[i].Role.Instructional() {
			continue
		}
		if len(msgs[i].Text()) <= maxMergeHostBytes && !isClientStateNotice(msgs[i].Text()) {
			return mergeIntoLastInstruction(msgs, add)
		}
		break
	}
	role := protocol.RoleSystem
	if proto == ProtocolAnthropic {
		role = protocol.RoleUser
	}
	return append(msgs, textMessage(role, add))
}

// isClientStateNotice 报告一段宿主指令是不是客户端注入的运行时状态通告。
//
// 这类文本描述的是「刚才发生了什么异常」，与「怎么用工具」毫无关系；
// 协议说明合并进去会让两个主题在模型眼里搅成一团。已知的形态都带显式
// 标签（<turn_aborted> 等），按标签识别而不是按措辞猜——客户端加新形态
// 时在这里补一行，比误判一段正常指令安全。
func isClientStateNotice(text string) bool {
	for _, tag := range []string{"<turn_aborted>", "<turn_diff>"} {
		if strings.Contains(text, tag) {
			return true
		}
	}
	return false
}

// mergeIntoLastInstruction 把内容追加进最后一条指令消息，只在 prefill
// 场景下使用（见 appendSystemFor）。
//
// 定位逻辑里有三条实测教训：
//
//   - 认 developer 而不只认 system。真实 Codex 的全部指令都是 developer
//     角色——只认 system 时一条都找不到，于是把协议说明插在整个对话的
//     **最前面**，被后面 29 KB 的业务指令彻底淹没。
//   - 追加到最后一条而不是第一条，让说明离生成点更近。
//   - 保留原角色。把一条 developer 消息改写成 system 会改变它的语义。
func mergeIntoLastInstruction(msgs []protocol.Message, add string) []protocol.Message {
	for i := len(msgs) - 1; i >= 0; i-- {
		if !msgs[i].Role.Instructional() {
			continue
		}
		merged := msgs[i].Text()
		if merged != "" {
			merged += "\n\n"
		}
		merged += add
		content, err := json.Marshal(merged)
		if err != nil {
			// 一个 Go string 编不成 JSON 字符串是不可能的。
			return msgs
		}
		out := make([]protocol.Message, len(msgs))
		copy(out, msgs)
		out[i] = protocol.Message{Role: msgs[i].Role, Content: content}
		return out
	}

	// 一条指令消息都没有，只能自己开一条。放在开头是因为它是所有内容的
	// 前提，而不是对某条消息的补充。
	content, err := json.Marshal(add)
	if err != nil {
		return msgs
	}
	head := protocol.Message{Role: protocol.RoleSystem, Content: content}
	return append([]protocol.Message{head}, msgs...)
}

// Encode 把请求编回它原本的协议。
func (r *Request) Encode() ([]byte, error) { return r.EncodeAs(r.proto) }

// EncodeAs 把请求编成指定协议，用于客户端与上游协议不同的反代。
//
// 跨协议转换几乎是免费的：解码时已经归一化到中性表示，这里只是换一种
// 写法。装不下的东西会显式报错而不是静默丢弃。
func (r *Request) EncodeAs(target Protocol) ([]byte, error) {
	if !target.Valid() {
		return nil, fmt.Errorf("toolbridge: 未知协议 %q", target)
	}
	switch target {
	case ProtocolChat:
		return protocol.EncodeChatRequest(protocol.ChatRequest{
			Model: r.Model(), Messages: r.msgs(),
			Tools: r.toolDecls(), ToolChoice: r.toolChoice(),
			Stream: r.Stream(),
		})
	case ProtocolResponses:
		return protocol.EncodeResponsesRequest(protocol.ResponsesRequest{
			Model: r.Model(), Input: r.msgs(),
			Tools: r.toolDecls(), ToolChoice: r.toolChoice(),
			MaxOutputTokens: r.MaxTokens(), Stream: r.Stream(),
		})
	default:
		mt := r.MaxTokens()
		if mt <= 0 {
			return nil, fmt.Errorf(
				"toolbridge: 编成 anthropic 需要 max_tokens，请先调用 SetMaxTokens")
		}
		return protocol.EncodeMessagesRequest(protocol.MessagesRequest{
			Model: r.Model(), MaxTokens: mt, Messages: r.msgs(),
			Tools: r.toolDecls(), ToolChoice: r.toolChoice(),
			Stream: r.Stream(),
		})
	}
}

// SetMaxTokens 设置输出上限，用于转成 Anthropic 协议时补上必填字段。
func (r *Request) SetMaxTokens(n int) {
	r.maxTokens = n
	switch r.proto {
	case ProtocolResponses:
		r.responses.MaxOutputTokens = n
	case ProtocolAnthropic:
		r.messages.MaxTokens = n
	}
}

func (r *Request) toolDecls() []ir.ToolDeclaration {
	switch r.proto {
	case ProtocolChat:
		return r.chat.Tools
	case ProtocolResponses:
		return r.responses.Tools
	default:
		return r.messages.Tools
	}
}

// CompileRequest 一步完成「读工具定义 → 编译协议说明 → 改造请求」。
//
// 这是三个方法（Tools / Compile / PrepareForTextUpstream）的组合，
// 单独提供是因为它们几乎总是一起用，而顺序写反了不会报错、只会静默
// 少一层保护：先 PrepareForTextUpstream 再 Tools，拿到的就是空列表。
//
// opts.Query 为空时自动取最后一条用户消息——工具候选排序需要它，
// 而每个调用方都手写一遍 LastUserText 是没必要的重复。
func (s *Session) CompileRequest(req *Request, opts CompileOptions) (*CompileResult, error) {
	if req == nil {
		return nil, fmt.Errorf("toolbridge: 需要请求")
	}
	tools := req.Tools()
	if len(tools) == 0 {
		// 客户端没声明工具不是错误，是「这轮不需要虚拟协议」。
		//
		// 与 Compile 的区别是刻意的：那个方法收的是一份明确的工具列表，
		// 给它空列表确实是调用错误；而一个请求本来就可能不带工具——
		// 真实 agent 有大量这种辅助请求（提取记忆、压缩上下文、起标题）。
		// 把它们当错误打回，整个 agent 当场卡死，这是实测出来的。
		//
		// 仍然调用 PrepareForTextUpstream：它会把历史里的工具调用与结果
		// 文本化。上游是纯文本模型，看不见结构化字段——不做这一步，
		// 模型第二轮不知道自己上一轮做过什么。
		req.PrepareForTextUpstream("", s.nonce.Signal())
		return &CompileResult{Signal: s.nonce.Signal()}, nil
	}

	if opts.Query == "" {
		opts.Query = req.LastUserText()
	}
	if !opts.RequireCall && req.RequireCall() {
		// 客户端要求必须调用时把这个约束带进 Prompt。
		// 它只是一句约束，不是保证——模型没调用时你必须返回明确状态。
		opts.RequireCall = true
		opts.RequiredTool = req.RequiredTool()
	}

	res, err := s.Compile(tools, opts)
	if err != nil {
		return nil, err
	}
	req.PrepareForTextUpstreamWithReminder(res.SystemPrompt, s.nonce.Signal(), res.Reminder)
	return res, nil
}
