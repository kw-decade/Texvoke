package main

// 协议适配端点：让非 Go 的反代也能用上 pkg/toolbridge 的三协议编解码。
//
// /v1/compile 与 /v1/parse 解决的是「工具定义 → prompt」「模型输出 → 调用」，
// 但接入方仍要自己把 Chat / Anthropic / Responses 三种请求体归一化，再把
// 工具调用按各自的形状渲染回去。那部分恰恰是最容易写错、也最不该重复写的。
//
// 这里把它一并收掉：
//
//	POST /v1/adapt         原始请求体 → 可直接发给上游的请求体（+ nonce/signal）
//	POST /v1/render        解析结果   → 客户端协议的完整响应体
//	POST /v1/render/stream 解析结果   → 客户端协议的 SSE 事件序列
//
// 无状态的做法与现有端点一致：nonce 在 /v1/adapt 返回，调用方在
// /v1/render 带回来。服务端不保存任何会话。

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/kw-decade/Texvoke/internal/observability"
	"github.com/kw-decade/Texvoke/internal/serving"
	"github.com/kw-decade/Texvoke/pkg/toolbridge"
)

/* ---------- /v1/adapt ---------- */

type adaptRequest struct {
	// Protocol 是客户端说的话：chat / anthropic / responses。
	Protocol string `json:"protocol"`

	SessionID string `json:"session_id"`
	RequestID string `json:"request_id"`

	// Body 是客户端发来的原始请求体，原样丢进来即可。
	Body json.RawMessage `json:"body"`

	// TargetProtocol 指定发给上游时用哪种协议编码。为空时与 Protocol 相同。
	//
	// 存在这个字段是因为上游经常跟客户端说的不是同一种话——反代的日常。
	TargetProtocol string `json:"target_protocol,omitempty"`

	// UpstreamModel 覆盖发给上游的模型名。为空时保留客户端写的那个。
	UpstreamModel string `json:"upstream_model,omitempty"`

	// 下面三个与 /v1/compile 同义。
	AlwaysInclude []string `json:"always_include,omitempty"`
	RequireCall   bool     `json:"require_call,omitempty"`
	RequiredTool  string   `json:"required_tool,omitempty"`

	// MaxTools 覆盖进入 Prompt 的工具数上限，0 用 Bridge 的默认（24）。
	//
	// Claude Code 这类客户端声明几十个工具，其中混着大量长描述插件工具，
	// 默认上限塞满后协议说明被清单淹没，模型就「忘了自己有工具」。收紧上限
	// 配合 always_include 核心工具，把清单压回模型能消化的规模。
	MaxTools int `json:"max_tools,omitempty"`

	// ExtraInstructions 追加在协议说明之后的补充指令。
	//
	// 用于写客户端特定的「怎么用工具」知识——通用中间件不该内置那些。
	// 注意边界：写的是怎么用已有机制，不是绕过机制。
	ExtraInstructions string `json:"extra_instructions,omitempty"`

	// MaxToolDescBytes 覆盖本次的工具描述上限，0 用启动参数的值。
	//
	// 这个值要按客户端调：调小了模型不知道工具能干什么，调大了协议指令
	// 会被工具描述稀释。全局一刀切对同时接多种客户端的反代不够用。
	MaxToolDescBytes int `json:"max_tool_desc_bytes,omitempty"`

	// SessionKey 是会话级突破阶梯状态的键。同一个对话的所有请求
	// （adapt / parse / recover）带同一个键，阶梯进度与「历史里已有成功
	// 调用」就由服务端自动维护——接入方不再需要自己记状态。
	SessionKey string `json:"session_key,omitempty"`
}

type adaptResponse struct {
	// UpstreamBody 可以直接发给上游：工具定义已从原生字段移除、
	// 协议说明已并进 system。
	UpstreamBody json.RawMessage `json:"upstream_body"`

	// Nonce 与 Signal 与 /v1/compile 同义，/v1/render 与 /v1/parse 要用。
	Nonce  string `json:"nonce"`
	Signal string `json:"signal"`

	// SystemPrompt 单独给出一份，方便调试与日志核对。
	SystemPrompt string `json:"system_prompt"`

	// VirtualProtocol 报告这一轮有没有注入虚拟协议。
	//
	// 客户端没声明工具时它是 false，请求原样转给上游即可。真实 agent
	// 有大量这种辅助请求（提取记忆、压缩上下文、起标题）——实测 Codex
	// 跑一轮任务发的 24 次请求里有 20 次是。
	//
	// 调用方可以据此跳过 /v1/parse。不跳过也安全：没有信号的输出会被
	// 判为 plain_text，原样透传。
	VirtualProtocol bool `json:"virtual_protocol"`

	// 客户端请求里的这几项一并回报，省得调用方再解析一遍请求体。
	Model  string `json:"model"`
	Stream bool   `json:"stream"`

	ToolsIncluded []string `json:"tools_included"`
	ToolsDropped  int      `json:"tools_dropped"`

	// SkippedItemTypes 是被跳过的扩展 item 类型。一并回报，方便接入方
	// 在自己的日志里也留一笔——不认识的东西被丢掉了，该有人知道。
	SkippedItemTypes []string `json:"skipped_item_types,omitempty"`

	// SkippedTools 是被跳过的工具名（目前只有 Responses 的 namespace 类型）。
	//
	// 与上面那个分开：一个说「有段结构我不认识」，另一个说「有个工具你
	// 声明了但模型用不上」。后者直接影响模型能做什么，必须能看见。
	SkippedTools []string `json:"skipped_tools,omitempty"`

	// DroppedContentTypes 是压平 content 时无法传给纯文本上游的 part 类型
	// （图片、音频之类）。它们在文本里留了占位说明，但内容确实没过去——
	// 用户发了一张图而模型不知道有这回事，是最难解释的一类「模型怎么这么笨」。
	DroppedContentTypes []string `json:"dropped_content_types,omitempty"`

	// DedupedInstructions 是被删掉的重复指令消息条数。
	//
	// 真实 agent 会把整套指令按 turn 重发——Codex 的一个请求里同一条 26 KB
	// 沙箱说明出现 4 次。删掉重复的是无损的，但要能被看见：万一判据出错删多了，
	// 症状会是「模型好像忘了某条规则」，那是最难查的一类问题。
	DedupedInstructions int `json:"deduped_instructions,omitempty"`
}

func (s *server) handleAdapt(w http.ResponseWriter, r *http.Request) {
	var req adaptRequest
	if !s.decode(w, r, &req) {
		return
	}
	if req.SessionID == "" || req.RequestID == "" {
		s.fail(w, http.StatusBadRequest, "bad_request", "session_id 与 request_id 必填")
		return
	}
	proto, ok := s.protocol(w, req.Protocol)
	if !ok {
		return
	}
	if len(req.Body) == 0 {
		s.fail(w, http.StatusBadRequest, "bad_request", "body 必填")
		return
	}

	// 会话键存在时登记「历史里已有成功调用」。这个事实由 L2 的运行时通知
	// 使用：用已发生的成功反驳模型「调用接口不可用」的自我错觉。
	if req.SessionKey != "" && serving.HasToolCallInBody(req.Body) {
		s.sessions.MarkCalls(req.SessionKey)
	}

	// adapt 失败时把原始请求记下来。真实客户端发的形状比测试里脏得多，
	// 光看错误信息猜不出对方到底发了什么——有原文才能对着改。
	//
	// 只在 -v 下记录：请求体里有用户的对话内容，不该默认落进日志。
	failed := func(stage string, err error) {
		s.log.Debug("adapt 失败",
			"stage", stage,
			"protocol", string(proto),
			"error", err.Error(),
			"body", string(req.Body))
		s.failErr(w, err)
	}

	decoded, err := toolbridge.DecodeRequest(proto, req.Body, toolbridge.DecodeOptions{
		SessionID: req.SessionID,
		RequestID: req.RequestID,
		MaxBytes:  maxBodyBytes,
	})
	if err != nil {
		failed("decode", err)
		return
	}

	// 跳过的扩展 item 要能被看见：默认日志就记，不等 -v。类型名本身不含
	// 用户内容，而「客户端发了我们不认识的东西」是该被注意到的事。
	if skipped := decoded.SkippedItemTypes(); len(skipped) > 0 {
		s.log.Warn("跳过了不认识的 input item",
			"session", observability.Digest(req.SessionID)[:8],
			"types", skipped)
	}
	// 工具被跳过比 item 被跳过严重：它直接决定模型这轮能做什么。
	if skipped := decoded.SkippedTools(); len(skipped) > 0 {
		s.log.Warn("跳过了无法表达的工具",
			"session", observability.Digest(req.SessionID)[:8],
			"tools", skipped)
	}

	sess, err := s.bridge.NewSession(req.SessionID, req.RequestID)
	if err != nil {
		s.failErr(w, err)
		return
	}

	// 客户端原本要的模型名要留着回报——响应里该显示它，不是上游的那个。
	clientModel := decoded.Model()
	stream := decoded.Stream()

	compiled, err := sess.CompileRequest(decoded, toolbridge.CompileOptions{
		AlwaysInclude:     req.AlwaysInclude,
		RequireCall:       req.RequireCall,
		RequiredTool:      req.RequiredTool,
		MaxToolDescBytes:  req.MaxToolDescBytes,
		MaxTools:          req.MaxTools,
		ExtraInstructions: req.ExtraInstructions,
	})
	if err != nil {
		failed("compile", err)
		return
	}

	if req.UpstreamModel != "" {
		decoded.SetModel(req.UpstreamModel)
	}

	target := proto
	if req.TargetProtocol != "" {
		if target, ok = s.protocol(w, req.TargetProtocol); !ok {
			return
		}
	}
	upstream, err := decoded.EncodeAs(target)
	if err != nil {
		s.failErr(w, err)
		return
	}

	if dropped := decoded.DroppedContentTypes(); len(dropped) > 0 {
		s.log.Warn("有内容无法传给纯文本上游",
			"session", observability.Digest(req.SessionID)[:8],
			"types", dropped)
	}

	if n := decoded.DedupedInstructions(); n > 0 {
		s.log.Debug("删掉了重复的指令消息",
			"session", observability.Digest(req.SessionID)[:8],
			"count", n)
	}

	s.log.Debug("adapt",
		"session", observability.Digest(req.SessionID)[:8],
		"protocol", string(proto),
		"target", string(target),
		"virtual_protocol", compiled.VirtualProtocol,
		"tools_included", len(compiled.ToolsIncluded),
		"tools_dropped", compiled.ToolsDropped)

	// 数组字段绝不返回 JSON null。没有工具时 Go 的 nil slice 会编成 null，
	// 而弱类型的调用方多半直接读 .length——症状是反代侧一个与工具毫无
	// 关系的 TypeError，排查方向完全错误。这是实测踩到的。
	included := compiled.ToolsIncluded
	if included == nil {
		included = []string{}
	}

	s.json(w, http.StatusOK, adaptResponse{
		UpstreamBody:    upstream,
		Nonce:           sess.NonceValue(),
		Signal:          compiled.Signal,
		SystemPrompt:    compiled.SystemPrompt,
		VirtualProtocol: compiled.VirtualProtocol,
		Model:           clientModel,
		Stream:          stream,
		ToolsIncluded:   included,
		ToolsDropped:    compiled.ToolsDropped,

		SkippedItemTypes:    decoded.SkippedItemTypes(),
		SkippedTools:        decoded.SkippedTools(),
		DroppedContentTypes: decoded.DroppedContentTypes(),
		DedupedInstructions: decoded.DedupedInstructions(),
	})
}

/* ---------- /v1/render ---------- */

type renderRequest struct {
	Protocol  string `json:"protocol"`
	SessionID string `json:"session_id"`
	RequestID string `json:"request_id"`
	Nonce     string `json:"nonce"`

	// Body 是客户端最初发来的那个请求体。渲染要用它回报模型名与协议细节。
	Body json.RawMessage `json:"body"`

	// Result 是 /v1/parse 的结果，字段名一致，可以原样转发。
	Result renderResult `json:"result"`

	// ID 覆盖响应标识；为空时按协议生成。
	ID string `json:"id,omitempty"`
	// Model 覆盖响应里回报的模型名；通常该留空，让客户端看到自己问的那个。
	Model string `json:"model,omitempty"`
}

type renderResult struct {
	Text    string            `json:"text"`
	Calls   []toolbridge.Call `json:"calls"`
	Outcome string            `json:"outcome"`
}

// prepare 把 HTTP 上的扁平结构还原成渲染需要的三样东西。
func (s *server) prepare(w http.ResponseWriter, req renderRequest) (*toolbridge.Session, *toolbridge.Request, *toolbridge.Result, bool) {
	if req.SessionID == "" || req.RequestID == "" {
		s.fail(w, http.StatusBadRequest, "bad_request", "session_id 与 request_id 必填")
		return nil, nil, nil, false
	}
	proto, ok := s.protocol(w, req.Protocol)
	if !ok {
		return nil, nil, nil, false
	}
	if len(req.Body) == 0 {
		s.fail(w, http.StatusBadRequest, "bad_request", "body 必填")
		return nil, nil, nil, false
	}

	// 会话用 nonce 重建。渲染本身不需要信号，但 session_id / request_id 要
	// 写进工具调用提案，而它们与 nonce 是绑定的——一起还原才不会对不上。
	sess, ok := s.restore(w, req.SessionID, req.RequestID, req.Nonce)
	if !ok {
		return nil, nil, nil, false
	}

	decoded, err := toolbridge.DecodeRequest(proto, req.Body, toolbridge.DecodeOptions{
		SessionID: req.SessionID,
		RequestID: req.RequestID,
		MaxBytes:  maxBodyBytes,
	})
	if err != nil {
		s.failErr(w, err)
		return nil, nil, nil, false
	}

	outcome, err := parseOutcome(req.Result.Outcome, len(req.Result.Calls))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "bad_request", err.Error())
		return nil, nil, nil, false
	}

	return sess, decoded, &toolbridge.Result{
		Text:    req.Result.Text,
		Calls:   req.Result.Calls,
		Outcome: outcome,
	}, true
}

func (s *server) handleRender(w http.ResponseWriter, r *http.Request) {
	var req renderRequest
	if !s.decode(w, r, &req) {
		return
	}
	sess, decoded, res, ok := s.prepare(w, req)
	if !ok {
		return
	}

	out, err := sess.RenderResponse(decoded, res, toolbridge.RenderOptions{
		ID:    req.ID,
		Model: req.Model,
	})
	if err != nil {
		s.failErr(w, err)
		return
	}

	s.log.Debug("render",
		"session", observability.Digest(req.SessionID)[:8],
		"protocol", req.Protocol,
		"calls", len(req.Result.Calls))

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

/* ---------- /v1/render/stream ---------- */

// handleRenderStream 把同一份结果渲染成客户端协议的 SSE 事件序列。
//
// 注意这里**不是真增量**：调用方把完整文本一次性交过来，服务端一次性发出
// 全部事件。客户端看到的事件形状完全正确（该有的 message_start /
// content_block_delta / [DONE] 一个不少），但第一个字要等模型说完才到。
//
// 需要真流式的反代，目前仍应自己按 /v1/parse/stream 的安全前缀边发边写，
// 只把最终的工具调用交给这里渲染。
func (s *server) handleRenderStream(w http.ResponseWriter, r *http.Request) {
	var req renderRequest
	if !s.decode(w, r, &req) {
		return
	}
	sess, decoded, res, ok := s.prepare(w, req)
	if !ok {
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	sr, err := sess.NewStreamRenderer(decoded, w, toolbridge.RenderOptions{
		ID:    req.ID,
		Model: req.Model,
	})
	if err != nil {
		// 头已经发出去了，这里只能把错误写进事件流。
		fmt.Fprintf(w, "event: error\ndata: {\"error\":%q}\n\n", err.Error())
		return
	}
	if err := sr.Finish(*res); err != nil {
		fmt.Fprintf(w, "event: error\ndata: {\"error\":%q}\n\n", err.Error())
		return
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

/* ---------- 共用 ---------- */

// protocol 把请求里的协议名转成枚举，非法值直接回 400。
func (s *server) protocol(w http.ResponseWriter, name string) (toolbridge.Protocol, bool) {
	p := toolbridge.Protocol(name)
	if !p.Valid() {
		s.fail(w, http.StatusBadRequest, "bad_request",
			fmt.Sprintf("protocol 必须是 chat / anthropic / responses 之一，收到 %q", name))
		return "", false
	}
	return p, true
}

// parseOutcome 把字符串还原成结局枚举。
//
// 允许为空是为了让调用方少写一个字段：有调用就是 calls_parsed，没有就是
// plain_text。写错了则宁可报错——结局直接决定 finish_reason，猜错会让
// 客户端以为模型正常说完了。
func parseOutcome(s string, calls int) (toolbridge.Outcome, error) {
	if s == "" {
		if calls > 0 {
			return toolbridge.OutcomeCallsParsed, nil
		}
		return toolbridge.OutcomePlainText, nil
	}
	for _, o := range []toolbridge.Outcome{
		toolbridge.OutcomePlainText,
		toolbridge.OutcomeCallsParsed,
		toolbridge.OutcomeTruncated,
		toolbridge.OutcomeMalformed,
	} {
		if string(o) == s {
			return o, nil
		}
	}
	return "", fmt.Errorf("outcome 非法：%q", s)
}
