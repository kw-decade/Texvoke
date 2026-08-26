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
// **真流式**：WriteText 收到的每个增量立刻编成客户端协议的事件写出并
// Flush。首个增量触发各协议的起始事件（Chat 的 role 首包、Anthropic 的
// message_start + content_block_start、Responses 的 response.created +
// output_item.added），Finish 负责工具调用与终止事件——终止事件一个不能少
// （不变量 16）：Chat 的 [DONE]、Anthropic 的 content_block_stop +
// message_stop、Responses 的 output_item.done + response.completed。
//
// 文本与调用的分工来自协议本身的形状：模型被教成「先输出信号再写
// envelope」，所以调用（如果有）永远出现在文本之后、输出末尾；且调用参数
// 必须完整到达才能发。于是文本属于流式阶段，调用属于收尾阶段。
//
// 一致性约束（不变量 8）：Finish 收到的 Result.Text 必须与已发增量的拼接
// 一致。调用方（gateway）的权威来源是同一个 StreamParser，天然满足；
// Finish 里对已发过的文本不再重复渲染，只补 tool_calls 与收尾。
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

	// streamed 记录是否已发出过增量（决定 Finish 走哪条路径）。
	streamed bool
	// text 累计已发出的内容：Finish 时的对账依据，也是未发过任何增量时
	// 一次性渲染的数据源（回落伪流式，行为与历史版本一致）。
	text []byte
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

// WriteText 收下一段文本增量并立即发给客户端。
//
// 第一个非空增量先触发协议起始事件。返回错误时流已损坏（写失败说明客户端
// 断开），后续 Write 与 Finish 仍可调用但不会再写字节。
func (sr *StreamRenderer) WriteText(b []byte) error {
	if len(b) == 0 || sr.finished {
		return nil
	}
	if !sr.streamed {
		if err := sr.begin(); err != nil {
			return err
		}
		sr.streamed = true
	}
	sr.text = append(sr.text, b...)
	return sr.delta(string(b))
}

// begin 发出协议起始事件。只在第一个增量前调用一次。
func (sr *StreamRenderer) begin() error {
	switch sr.req.Protocol() {
	case ProtocolChat:
		// Chat 的首包只带 role——OpenAI 的既定形态，客户端据此知道消息开始。
		b, err := json.Marshal(map[string]any{
			"id": sr.respID("chatcmpl_"), "object": "chat.completion.chunk",
			"created": time.Now().Unix(), "model": sr.modelName(),
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"role": "assistant"},
				"finish_reason": nil,
			}},
		})
		if err != nil {
			return err
		}
		return sr.enc.Write(protocol.Event{Data: b})
	case ProtocolResponses:
		return protocol.EncodeResponsesBegin(sr.enc, protocol.ResponsesResponse{
			ID: sr.respID("resp_"), Model: sr.modelName(),
			CreatedAt:     time.Now().Unix(),
			Status:        protocol.ResponseInProgress,
			MessageItemID: sr.itemID(),
		})
	default:
		startUsage := &protocol.AnthropicUsage{}
		return protocol.EncodeAnthropicMessageStart(sr.enc, protocol.MessagesResponse{
			ID: sr.respID("msg_"), Model: sr.modelName(),
		}, startUsage)
	}
}

// delta 发出一段文本增量事件。
func (sr *StreamRenderer) delta(text string) error {
	switch sr.req.Protocol() {
	case ProtocolChat:
		return protocol.EncodeChatTextDelta(sr.enc, sr.respID("chatcmpl_"), sr.modelName(), time.Now().Unix(), text)
	case ProtocolResponses:
		return protocol.EncodeResponsesTextDelta(sr.enc, sr.itemID(), text)
	default:
		return protocol.EncodeAnthropicTextDelta(sr.enc, 0, text)
	}
}

// respID 返回响应标识（opts.ID 优先）。同一渲染器内多次取值必须一致，
// 所以第一次调用时生成后存进 opts.ID。
func (sr *StreamRenderer) respID(prefix string) string {
	if sr.opts.ID == "" {
		sr.opts.ID = prefix + randomID()
	}
	return sr.opts.ID
}

// modelName 返回响应里回报的模型名。
func (sr *StreamRenderer) modelName() string { return modelOr(sr.opts.Model, sr.req) }

// itemID 返回 Responses 协议的 message item 标识。
//
// 用会话 nonce 算摘要而不是随机数：增量事件的 item_id 与收尾 output_item.done
// 里的 id 必须逐字相同，客户端才能把文本拼回同一个 item（不变量 5 的同类
// 要求）。同一轮内多次取值稳定，跨轮 nonce 不同则必然不同。
func (sr *StreamRenderer) itemID() string {
	return "msg_" + observability.Digest(sr.sess.nonce.Value())[:24]
}

// Finish 收尾：发出工具调用与终止事件。
//
// 必须调用，否则客户端会一直等——Chat 等 [DONE]，Anthropic 等
// message_stop，Responses 等 response.completed（不变量 16）。
//
// 已流式发出过文本时：文本部分跳过重发（增量已实时到达），只补 tool_calls
// 与各协议的终止事件。未流式过（上游全程 envelope 或零输出）时：一次性
// 渲染完整序列，行为与历史版本一致。
func (sr *StreamRenderer) Finish(res Result) error {
	if sr.finished {
		return nil
	}
	sr.finished = true

	if sr.streamed {
		return sr.finishStreamed(res)
	}

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

// finishStreamed 补齐真流式的收尾：tool_calls 事件 + 终止事件。
//
// 文本不再重发。三协议的收尾形状各自不同：
//   - Chat：每个调用一个 tool_calls delta chunk → finish_reason chunk →
//     usage 尾包 → [DONE]。复用 EncodeChatStream 会重发文本，所以这里
//     手工补齐——但事件形状必须与它逐一对齐。
//   - Anthropic：content_block_stop → 每个调用的 content_block_start/delta/
//     stop（tool_use 块）→ message_delta(stop_reason) → message_stop。
//   - Responses：output_item.done（message item 收口）→ 每个调用的
//     function_call item → response.completed（output 数组含全部 item）。
func (sr *StreamRenderer) finishStreamed(res Result) error {
	proto := sr.req.Protocol()
	switch proto {
	case ProtocolChat:
		return protocol.EncodeChatStreamTail(sr.enc, chatResponse(sr.sess, sr.req, &res, sr.opts), true)
	case ProtocolResponses:
		resp := responsesResponse(sr.sess, sr.req, &res, sr.opts)
		// item_id 必须与已发增量里的完全一致，否则客户端拼不回同一个 item。
		resp.MessageItemID = sr.itemID()
		return protocol.EncodeResponsesStreamTail(sr.enc, resp, true)
	default:
		return protocol.EncodeAnthropicStreamTail(sr.enc, messagesResponse(sr.sess, sr.req, &res, sr.opts), true)
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
