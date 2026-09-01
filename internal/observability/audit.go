package observability

import (
	"context"
	"log/slog"
	"time"

	"github.com/kw-decade/Texvoke/internal/ir"
)

// EventKind 是审计事件的类型。
//
// 规格十一章要求认证失败、策略拒绝与上游错误**与普通模型文本分开上报**。
// 分开的前提是它们有各自的类型——都塞进一条 "error" 日志里，
// 「这次是被拒绝了还是上游挂了」就只能靠读消息正文来猜。
type EventKind string

const (
	// EventCallProposed：模型提出了一次调用。这是本项目的核心产出——
	// 提案交回客户端之后发生什么，本进程看不到，所以没有 admitted /
	// denied / settled 三类后续事件（它们随执行层一起删除）。
	EventCallProposed EventKind = "call_proposed"

	// EventUpstreamError：上游返回错误或传输失败。
	EventUpstreamError EventKind = "upstream_error"

	// EventRefusalClassified：一次拒绝分类的结论。
	EventRefusalClassified EventKind = "refusal_classified"
)

// Level 返回这类事件应有的日志级别。
//
// 做成事件类型的属性而不是让每个调用点自己选：同一类事件在两处记成
// 不同级别，会让「过滤出所有被拒绝的调用」这种最基本的排查动作失效。
func (k EventKind) Level() slog.Level {
	switch k {
	case EventRefusalClassified:
		return slog.LevelWarn
	case EventUpstreamError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Event 是一条审计记录。
//
// 规格十一章：记录「谁、何时、以什么策略、提出了什么调用、最终是否执行」，
// 但不记录不必要的秘密。所以这里有参数的**摘要**而没有参数原文，
// 有错误**分类**而没有错误正文。
//
// 摘要足够回答审计要问的问题——「模型当时提出的参数是不是这一份」——
// 而原文除了泄露之外不提供额外的证明力。
type Event struct {
	Kind EventKind
	At   time.Time

	// 谁：租户与会话。用户身份不进这里——审计要能追责，
	// 但追责走的是租户与会话，不需要把用户标识撒进每一条日志。
	Tenant    string
	SessionID string
	RequestID string

	// 提出了什么调用。
	CallID          string
	Tool            string
	ArgumentsDigest string

	// 以什么策略：判定结果与理由。
	Decision string
	Reason   string

	// 最终是否执行。
	Status     string
	DurationMS int64

	// 错误分类。ErrorCode 是机器可读的，正文不进审计。
	ErrorCode string
}

// LogAttrs 返回可安全落盘的字段。
//
// 空字段一律省略：一条 20 个字段有 14 个是空串的日志，
// 会让真正有值的那几个淹没在里面。
func (e Event) LogAttrs() []slog.Attr {
	attrs := make([]slog.Attr, 0, 12)
	attrs = append(attrs, slog.String("event", string(e.Kind)))
	if !e.At.IsZero() {
		attrs = append(attrs, slog.Time("at", e.At))
	}
	for _, kv := range []struct{ k, v string }{
		{"tenant", e.Tenant},
		{"session_id", e.SessionID},
		{"request_id", e.RequestID},
		{"call_id", e.CallID},
		{"tool", e.Tool},
		{"arguments_digest", e.ArgumentsDigest},
		{"decision", e.Decision},
		{"reason", e.Reason},
		{"status", e.Status},
		{"error_code", e.ErrorCode},
	} {
		if kv.v != "" {
			attrs = append(attrs, slog.String(kv.k, kv.v))
		}
	}
	if e.DurationMS > 0 {
		attrs = append(attrs, slog.Int64("duration_ms", e.DurationMS))
	}
	return attrs
}

// ProposalEvent 从一次调用提案构造审计事件。
//
// 参数只留摘要。提案的 Arguments 是模型写的，其中完全可能有用户粘进来的
// 密码、密钥或个人信息——把它原样记进日志，等于把一次工具调用变成
// 一次数据泄露。
func ProposalEvent(kind EventKind, p ir.ToolCallProposal, tenant string) Event {
	return Event{
		Kind: kind, At: time.Now(), Tenant: tenant,
		SessionID: p.SessionID, RequestID: p.RequestID, CallID: p.CallID,
		Tool:            p.Tool.String(),
		ArgumentsDigest: Digest(string(p.Arguments)),
	}
}

// ResultEvent 随执行结果一起删除：本项目不执行工具，看不到落定状态与耗时。
// （原说明所在的 internal/ir/result.go 已随 2026-08-25 的审计一并删除。）

// Auditor 把审计事件写进日志。
//
// 薄到几乎不存在，但它是那条「分开上报」规则的唯一落点：级别由事件类型
// 决定，调用方选不了。
type Auditor struct {
	logger *slog.Logger
}

// NewAuditor 创建审计器。logger 为 nil 时用 slog 的默认 logger。
func NewAuditor(logger *slog.Logger) *Auditor {
	if logger == nil {
		logger = slog.Default()
	}
	return &Auditor{logger: logger}
}

// Record 写一条审计事件。
func (a *Auditor) Record(ctx context.Context, e Event) {
	a.logger.LogAttrs(ctx, e.Kind.Level(), string(e.Kind), e.LogAttrs()...)
}
