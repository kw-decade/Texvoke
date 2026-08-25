package toolbridge

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/kw-decade/Texvoke/internal/ir"
	"github.com/kw-decade/Texvoke/internal/observability"
	"github.com/kw-decade/Texvoke/internal/protocol"
)

// RenderOptions 调整响应渲染。
type RenderOptions struct {
	// ID 是响应标识。为空时按协议生成一个（chatcmpl_xxx / resp_xxx / msg_xxx）。
	//
	// 允许指定是因为有些客户端会拿它做去重或关联；不指定也完全能用。
	ID string

	// Model 覆盖响应里回报的模型名。为空时用请求里的那个。
	//
	// 通常应该留空：客户端问的是哪个模型，就该看到哪个模型。
	// 把上游的真实模型名透出去，等于泄露了你的路由配置。
	Model string
}

// RenderResponse 把解析结果编成客户端要的协议。
//
// 工具调用会以客户端原本期待的形状回去（Chat 的 tool_calls、
// Anthropic 的 tool_use 块、Responses 的 function_call item），
// 所以客户端感觉不到中间有一层——它照常执行、照常回填结果。
func (s *Session) RenderResponse(req *Request, res *Result, opts RenderOptions) ([]byte, error) {
	if req == nil || res == nil {
		return nil, fmt.Errorf("toolbridge: 渲染响应需要请求与结果")
	}
	switch req.Protocol() {
	case ProtocolChat:
		return protocol.EncodeChatResponse(chatResponse(s, req, res, opts))
	case ProtocolResponses:
		return protocol.EncodeResponsesResponse(responsesResponse(s, req, res, opts))
	default:
		return protocol.EncodeMessagesResponse(messagesResponse(s, req, res, opts))
	}
}

// StreamRenderer 按客户端协议输出 SSE 事件。
//
// **当前是伪流式**：WriteText 收下的增量会攒起来，直到 Finish 才一次性
// 渲染成完整的事件序列。客户端看到的事件形状完全正确（该有的
// message_start / content_block_delta / [DONE] 一个不少），但没有逐 token
// 的时间分布——第一个字要等模型说完才到。
//
// 为什么先这样：三协议的增量事件序列各有各的状态机（Anthropic 要
// content_block_start/stop 配对，Responses 要 output_item 与 content_part
// 两层嵌套），手写容易与 internal/protocol 的整体渲染分叉。正确的做法是
// 给 protocol 加增量编码器，让两条路径共用同一份序列知识。
//
// API 形状已经是流式的，所以那次升级不会影响你的调用代码——
// WriteText 会从「攒着」变成「立刻发出」，其余不变。
//
// 配合 StreamParser 使用——后者负责判断哪些字节确定不是协议信号、
// 可以安全发出去，这里负责把它们编成客户端认识的事件。
//
// 典型用法：
//
//	sp, _ := sess.NewStreamParser()
//	sr, _ := sess.NewStreamRenderer(req, w, toolbridge.RenderOptions{})
//	for chunk := range upstreamChunks {
//	    safe, err := sp.Write(chunk)
//	    if err != nil { break }
//	    sr.WriteText(safe)
//	}
//	sr.WriteText(sp.Flush())
//	sr.Finish(sp.Close())
type StreamRenderer struct {
	sess *Session
	req  *Request
	opts RenderOptions
	enc  *protocol.SSEEncoder
	w    io.Writer

	finished bool
	text     []byte
}

// NewStreamRenderer 创建流式渲染器。
func (s *Session) NewStreamRenderer(req *Request, w io.Writer, opts RenderOptions) (*StreamRenderer, error) {
	if req == nil {
		return nil, fmt.Errorf("toolbridge: 流式渲染需要请求")
	}
	if w == nil {
		return nil, fmt.Errorf("toolbridge: 流式渲染需要写入器")
	}
	return &StreamRenderer{sess: s, req: req, opts: opts, enc: protocol.NewSSEEncoder(w), w: w}, nil
}

// WriteText 收下一段文本增量。空输入是空操作。
//
// ponytail: 当前只累积不发送，Finish 时一次性渲染（见类型文档）。
// 升级到真增量时改这里，调用方不用动。
func (sr *StreamRenderer) WriteText(b []byte) error {
	if len(b) == 0 || sr.finished {
		return nil
	}
	sr.text = append(sr.text, b...)
	return nil
}

// Finish 收尾：发出工具调用与结束事件。
//
// 必须调用，否则客户端会一直等——Chat 等 [DONE]，Anthropic 等
// message_stop，Responses 等 response.completed。
func (sr *StreamRenderer) Finish(res Result) error {
	if sr.finished {
		return nil
	}
	sr.finished = true

	merged := res
	if merged.Text == "" {
		merged.Text = string(sr.text)
	}
	switch sr.req.Protocol() {
	case ProtocolChat:
		return protocol.EncodeChatStream(sr.enc, chatResponse(sr.sess, sr.req, &merged, sr.opts))
	case ProtocolResponses:
		return protocol.EncodeResponsesStream(sr.enc, responsesResponse(sr.sess, sr.req, &merged, sr.opts))
	default:
		return protocol.EncodeAnthropicStream(sr.enc, messagesResponse(sr.sess, sr.req, &merged, sr.opts))
	}
}

/* ---------- 三协议的响应构造 ---------- */

func chatResponse(s *Session, req *Request, res *Result, opts RenderOptions) protocol.ChatResponse {
	return protocol.ChatResponse{
		ID: idOr(opts.ID, "chatcmpl_"), Model: modelOr(opts.Model, req),
		Created: time.Now().Unix(),
		// Chat 的 content 是字符串。
		Content:      textContent(res.Text),
		ToolCalls:    proposals(s, res),
		FinishReason: chatFinish(res),
	}
}

func responsesResponse(s *Session, req *Request, res *Result, opts RenderOptions) protocol.ResponsesResponse {
	status := protocol.ResponseCompleted
	if res.Outcome == OutcomeTruncated || res.Outcome == OutcomeMalformed {
		status = protocol.ResponseIncomplete
	}
	return protocol.ResponsesResponse{
		ID: idOr(opts.ID, "resp_"), Model: modelOr(opts.Model, req),
		CreatedAt: time.Now().Unix(), Status: status,
		// Responses 的文本块叫 output_text。
		Content:          blockContent(res.Text, "output_text"),
		ToolCalls:        withItemIDs(s, proposals(s, res)),
		IncompleteReason: incompleteReason(res),
	}
}

// withItemIDs 给每个调用补上 Responses 专用的 item 标识。
//
// 只有 Responses 有这个概念，而且只有它需要：流式事件里的 item_id 与最终
// response.output 里的 id 必须相同，客户端才能把 arguments 增量拼回同一个
// item。缺了它，Codex 这类客户端拿到的是一串 item_id 为空的事件。
//
// 上游是纯文本模型，不会给出 fc_xxx，所以这里必须自己造一个。用会话信号
// 加调用 ID 算摘要而不是随机数：同一个调用重新渲染一次，得到的仍是同一个
// item id——规格三章把「每次渲染重新生成 ID」列为必须纠正的问题。
func withItemIDs(s *Session, calls []ir.ToolCallProposal) []ir.ToolCallProposal {
	for i := range calls {
		if calls[i].ProtocolItemID == "" {
			seed := s.nonce.Value() + "|" + calls[i].CallID
			calls[i].ProtocolItemID = "fc_" + observability.Digest(seed)[:24]
		}
	}
	return calls
}

func messagesResponse(s *Session, req *Request, res *Result, opts RenderOptions) protocol.MessagesResponse {
	stop := protocol.StopEndTurn
	if len(res.Calls) > 0 {
		stop = protocol.StopToolUse
	}
	return protocol.MessagesResponse{
		ID: idOr(opts.ID, "msg_"), Model: modelOr(opts.Model, req),
		// Anthropic 的 content 永远是块数组，它的流式渲染器会直接
		// 拒绝字符串形式。
		Content:    blockContent(res.Text, "text"),
		ToolCalls:  proposals(s, res),
		StopReason: stop,
	}
}

// proposals 把解析出的调用转成协议层的提案。
//
// 命名空间固定为 client：这些工具是客户端声明的，执行发生在客户端的
// 权限边界内。这个标记让下游一眼看出「这个调用不该被本地执行」。
//
// session_id / request_id 取自会话绑定的信号——协议层会校验它们非空，
// 而且审计要靠这两个字段把调用归回具体某一轮请求。
func proposals(s *Session, res *Result) []ir.ToolCallProposal {
	if len(res.Calls) == 0 {
		return nil
	}
	out := make([]ir.ToolCallProposal, 0, len(res.Calls))
	for i, c := range res.Calls {
		out = append(out, ir.ToolCallProposal{
			SessionID: s.nonce.SessionID(),
			RequestID: s.nonce.RequestID(),
			CallID:    deterministicCallID(s, c.ID, i),
			Tool: ir.ToolID{
				Namespace: ir.NamespaceClient,
				Name:      c.Name,
				Version:   ir.VersionDeclared,
			},
			Arguments: c.Arguments,
			// 形态必须跟着走：丢了它，一次 exec 调用会被渲染成
			// function_call，Codex 拿到后按 JSON 去解一段脚本。
			ArgumentForm: argumentForm(c.Freeform),
			Source:       ir.SourceVirtual,
			CreatedAt:    time.Now(),
		})
	}
	return out
}

// deterministicCallID 把模型在 envelope 里写的本地标识转成跨轮唯一的 call_id。
//
// 为什么不原样用模型写的 id：Instructions 只要求 id 在**同一个 envelope 内**
// 互不相同（call-1、call-2），跨轮没有约束——而 Codex 每一轮都把完整历史
// 重发，第二轮的 call-1 与第一轮的 call-1 在解码时撞车，整个请求被拒
// （protocol: call_id "call-1" 重复，2026-08-24 实测，agent loop 跑两轮必现）。
//
// 用会话 nonce + 序号 + 模型 id 算确定性摘要：同一轮重渲染得到同一个 id
// （与 withItemIDs 的稳定性要求一致），跨轮 nonce 不同则必然不同。这不违反
// 「不凭空造 ID」的不变量——envelope 的 id 是模型的自由文本，不是任何上游
// 系统发的标识；真正要保的关联是「客户端回传的 call_id 与我们发出的完全
// 一致」，由确定性保证。
func deterministicCallID(s *Session, modelID string, idx int) string {
	if modelID == "" {
		modelID = fmt.Sprintf("idx%d", idx)
	}
	seed := s.nonce.Value() + "|" + strconv.Itoa(idx) + "|" + modelID
	return "call_" + observability.Digest(seed)[:24]
}

func chatFinish(res *Result) protocol.FinishReason {
	switch {
	case len(res.Calls) > 0:
		return protocol.FinishToolCalls
	case res.Outcome == OutcomeTruncated:
		return protocol.FinishLength
	default:
		return protocol.FinishStop
	}
}

// incompleteReason 把解析结局告诉客户端。
//
// Responses 协议正好有个字段能装它，于是「为什么没说完」不必只存在于
// 服务端日志里。
func incompleteReason(res *Result) string {
	switch res.Outcome {
	case OutcomeTruncated:
		return "truncated_after_signal"
	case OutcomeMalformed:
		return "malformed_after_signal"
	default:
		return ""
	}
}

func textContent(s string) json.RawMessage {
	if s == "" {
		return nil
	}
	b, err := json.Marshal(s)
	if err != nil {
		return nil
	}
	return b
}

func blockContent(s, blockType string) json.RawMessage {
	if s == "" {
		return nil
	}
	b, err := json.Marshal([]map[string]string{{"type": blockType, "text": s}})
	if err != nil {
		return nil
	}
	return b
}

func idOr(id, prefix string) string {
	if id != "" {
		return id
	}
	return prefix + randomID()
}

func modelOr(m string, req *Request) string {
	if m != "" {
		return m
	}
	return req.Model()
}

// randomID 生成响应标识的随机部分。
//
// 它只需唯一，不承担任何安全职责——所以随机源不可用时用时间兜底，
// 而不是让整个请求失败。
func randomID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// argumentForm 把布尔标记转成 IR 的形态枚举。
func argumentForm(freeform bool) ir.InputForm {
	if freeform {
		return ir.InputFormText
	}
	return ir.InputFormObject
}
