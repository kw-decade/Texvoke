package main

// /v1/recover：诊断「为什么没拿到调用」并给出下一步动作。
//
// 调用方在 parse 之后发现没有可用的调用时，把观测到的事实带过来，这里
// 返回两样东西：根因分类（供日志与审计）与要不要再问一次、问什么。
//
// 无状态约定与 /v1/render 相同：会话用 nonce 重建，handshake 状态由调用方
// 保管——上一轮响应里的 handshake_done，下一轮请求里原样带回来。

import (
	"net/http"

	"github.com/kw-decade/Texvoke/internal/observability"
	"github.com/kw-decade/Texvoke/internal/serving"
	"github.com/kw-decade/Texvoke/pkg/toolbridge"
)

/* ---------- /v1/recover ---------- */

type recoverRequest struct {
	Protocol  string `json:"protocol"`
	SessionID string `json:"session_id"`
	RequestID string `json:"request_id"`
	Nonce     string `json:"nonce"`

	// Result 是上一次 /v1/parse 的结果。解析成功但没有调用时它描述那次
	// 成功的解析；解析失败时 Outcome 是 truncated / malformed。
	Result renderResult `json:"result"`

	// ParseError 是 /v1/parse 报的错误原文（error 字段）。用于区分
	// 「解析失败」与「解析成功但零调用」这两种不同的失败。
	ParseError     string `json:"parse_error,omitempty"`
	ParseErrorKind string `json:"parse_error_kind,omitempty"`

	// Tools 是本轮声明的工具名（/v1/adapt 的 tools_included 原样传回）。
	Tools []string `json:"tools,omitempty"`

	// SessionKey 是会话级突破阶梯状态的键（与 /v1/adapt、/v1/parse 同一个）。
	//
	// 带了它时，阶梯深度由服务端状态决定并自动递进——接入方不再需要自己记
	// attempt 或 handshake_done，响应里的 level 是本次实际使用的级数。
	// 不带时退回无状态模式：attempt / handshake_done 由 evidence 提供。
	SessionKey string `json:"session_key,omitempty"`

	Evidence struct {
		ToolChoice      string `json:"tool_choice,omitempty"`
		HTTPStatus      int    `json:"http_status,omitempty"`
		UpstreamErrType string `json:"upstream_err_type,omitempty"`
		UpstreamErrCode string `json:"upstream_err_code,omitempty"`
		TransportError  bool   `json:"transport_error,omitempty"`
		HandshakeDone   bool   `json:"handshake_done,omitempty"`
		// Attempt 是调用方的尝试计数（1 起），突破阶梯按它选强度。
		Attempt int `json:"attempt,omitempty"`
		// HasSuccessfulHistory：请求历史里已有成功送出的调用。
		HasSuccessfulHistory bool `json:"has_successful_history,omitempty"`
	} `json:"evidence"`
}

type recoverResponse struct {
	Kind       string   `json:"kind"`
	Confidence string   `json:"confidence"`
	Remedy     string   `json:"remedy"`
	Terminal   bool     `json:"terminal"`
	Reasons    []string `json:"reasons"`

	ShouldRetry   bool             `json:"should_retry"`
	Messages      []recoverMessage `json:"messages,omitempty"`
	HandshakeDone bool             `json:"handshake_done"`
	// Level 是本次实际使用的阶梯级数（带 session_key 时由服务端状态给出）。
	Level  int    `json:"level,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type recoverMessage struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

func (s *server) handleRecover(w http.ResponseWriter, r *http.Request) {
	var req recoverRequest
	if !s.decode(w, r, &req) {
		return
	}
	if req.SessionID == "" || req.RequestID == "" {
		s.fail(w, http.StatusBadRequest, "bad_request", "session_id 与 request_id 必填")
		return
	}

	// 会话在这里只用来提供方法接收者；诊断本身是纯函数。
	sess, ok := s.restore(w, req.SessionID, req.RequestID, req.Nonce)
	if !ok {
		return
	}

	var res *toolbridge.Result
	if req.Result.Outcome != "" || len(req.Result.Calls) > 0 {
		outcome, err := parseOutcome(req.Result.Outcome, len(req.Result.Calls))
		if err != nil {
			s.fail(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		res = &toolbridge.Result{
			Text:    req.Result.Text,
			Calls:   req.Result.Calls,
			Outcome: outcome,
		}
	}

	var parseErr error
	if req.ParseError != "" {
		parseErr = toolbridge.WrapKind(toolbridge.ErrorKind(req.ParseErrorKind), req.ParseError)
	}

	ev := toolbridge.Evidence{
		// 工具数以声明为准：反代不删工具。真要支持「上游剥工具」的场景，
		// 让调用方把两个数分开传，而不是在这里猜。
		ToolsDeclared:        len(req.Tools),
		ToolsSent:            len(req.Tools),
		ToolChoice:           req.Evidence.ToolChoice,
		HTTPStatus:           req.Evidence.HTTPStatus,
		UpstreamErrType:      req.Evidence.UpstreamErrType,
		UpstreamErrCode:      req.Evidence.UpstreamErrCode,
		TransportError:       req.Evidence.TransportError,
		HandshakeDone:        req.Evidence.HandshakeDone,
		ModelText:            req.Result.Text,
		Attempt:              req.Evidence.Attempt,
		HasSuccessfulHistory: req.Evidence.HasSuccessfulHistory,
	}

	// 带会话键时阶梯由服务端状态主导：深度自动递进、封顶后停手。
	// 不带键则维持无状态模式（attempt 等由 evidence 提供），旧调用方不受影响。
	ladderExhausted := false
	level := 0
	if req.SessionKey != "" {
		lv, snap := s.sessions.Advance(req.SessionKey)
		level = lv
		if level > serving.MaxLadderLevel {
			ladderExhausted = true
		} else {
			ev.Attempt = level
			// 服务端状态与调用方带回的事实取并集：无状态调用方仍然有效。
			ev.HandshakeDone = snap.HandshakeDone || req.Evidence.HandshakeDone
			ev.HasSuccessfulHistory = snap.HasCalls || ev.HasSuccessfulHistory
		}
	}

	d := sess.Diagnose(res, parseErr, ev)

	out := recoverResponse{
		Kind:       d.Kind,
		Confidence: d.Confidence,
		Remedy:     d.Remedy,
		Terminal:   d.Terminal,
		Reasons:    d.Reasons,
		Level:      level,
	}

	if ladderExhausted {
		out.Reason = "突破阶梯已用尽：本会话已连续多轮拒绝调用，如实上报而不是继续追问"
		s.log.Info("recover",
			"session", observability.Digest(req.SessionID)[:8],
			"kind", d.Kind,
			"retry", false,
			"reason", out.Reason)
		s.json(w, http.StatusOK, out)
		return
	}

	rc, err := sess.Recover(d, res, parseErr, req.Tools, ev)
	if err != nil {
		s.failErr(w, err)
		return
	}
	// L1 发出后把状态记进会话：同一会话不再重复能力说明。
	if req.SessionKey != "" && rc.HandshakeDone {
		s.sessions.MarkHandshake(req.SessionKey)
	}

	s.log.Info("recover",
		"session", observability.Digest(req.SessionID)[:8],
		"kind", d.Kind,
		"remedy", d.Remedy,
		"retry", rc.ShouldRetry)

	out.ShouldRetry = rc.ShouldRetry
	out.HandshakeDone = rc.HandshakeDone
	out.Reason = rc.Reason
	for _, m := range rc.Messages {
		out.Messages = append(out.Messages, recoverMessage{Role: m.Role, Text: m.Text})
	}
	s.json(w, http.StatusOK, out)
}
