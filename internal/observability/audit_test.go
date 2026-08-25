package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/kw-decade/Texvoke/internal/ir"
)

func capture(t *testing.T) (*Auditor, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return NewAuditor(slog.New(h)), &buf
}

func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var m map[string]any
	line := strings.TrimSpace(buf.String())
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("日志不是 JSON：%v（%q）", err, line)
	}
	return m
}

func sampleProposal() ir.ToolCallProposal {
	return ir.ToolCallProposal{
		SessionID: "sess-1", RequestID: "req-1", CallID: "call_1",
		Tool:      ir.ToolID{Namespace: "fs", Name: "read_file", Version: "1"},
		Arguments: json.RawMessage(`{"path":"/home/u/.aws/credentials","token":"sk-live-9"}`),
		Source:    ir.SourceNative, CreatedAt: time.Unix(1700000000, 0),
	}
}

func TestLevelsSeparateRefusalFromUpstreamError(t *testing.T) {
	// 都塞进一条 error 日志里，「这次是模型不肯调用还是上游挂了」
	// 就只能靠读消息正文来猜。
	for kind, want := range map[EventKind]slog.Level{
		EventCallProposed:      slog.LevelInfo,
		EventRefusalClassified: slog.LevelWarn,
		EventUpstreamError:     slog.LevelError,
	} {
		if got := kind.Level(); got != want {
			t.Errorf("%s 的级别应为 %s，得到 %s", kind, want, got)
		}
	}
}

func TestProposalEventKeepsOnlyDigest(t *testing.T) {
	p := sampleProposal()
	e := ProposalEvent(EventCallProposed, p, "acme")

	// 参数是模型写的，其中完全可能有用户粘进来的密钥。
	blob := e.ArgumentsDigest + e.Reason + e.Tool
	for _, leak := range []string{"sk-live-9", ".aws/credentials"} {
		if strings.Contains(blob, leak) {
			t.Errorf("审计事件泄露了 %q", leak)
		}
	}
	// 摘要要足以回答「模型当时提出的是不是这一份参数」。
	if e.ArgumentsDigest != Digest(string(p.Arguments)) {
		t.Error("摘要应可复算")
	}
	if e.SessionID != "sess-1" || e.CallID != "call_1" || e.Tenant != "acme" {
		t.Errorf("谁、哪次调用要记全：%+v", e)
	}
	if e.Tool != "fs/read_file@1" {
		t.Errorf("工具标识应含版本，得到 %q", e.Tool)
	}
}

func TestDifferentArgumentsGiveDifferentDigest(t *testing.T) {
	a := ProposalEvent(EventCallProposed, sampleProposal(), "acme")
	p := sampleProposal()
	p.Arguments = json.RawMessage(`{"path":"/etc/passwd"}`)
	b := ProposalEvent(EventCallProposed, p, "acme")
	if a.ArgumentsDigest == b.ArgumentsDigest {
		t.Error("不同参数必须有不同摘要，否则摘要证明不了任何事")
	}
}

// 执行结果的审计事件随 ir.ToolResult 一起删除：本项目不执行工具，
// 看不到落定状态、错误码与耗时。见 internal/ir/result.go 的说明。

func TestRecordWritesStructuredFields(t *testing.T) {
	a, buf := capture(t)
	a.Record(context.Background(), Event{
		Kind: EventRefusalClassified, At: time.Unix(1700000000, 0),
		Tenant: "acme", SessionID: "sess-1", CallID: "call_1",
		Tool: "client/exec@declared", Decision: "persona_refusal", Reason: "模型声称没有工具",
	})

	m := decode(t, buf)
	if m["event"] != "refusal_classified" {
		t.Errorf("事件类型不对：%v", m["event"])
	}
	if m["level"] != "WARN" {
		t.Errorf("拒绝分类应是 WARN，得到 %v", m["level"])
	}
	for _, k := range []string{"tenant", "session_id", "call_id", "tool", "decision", "reason"} {
		if _, ok := m[k]; !ok {
			t.Errorf("缺字段 %q", k)
		}
	}
}

func TestRecordOmitsEmptyFields(t *testing.T) {
	// 20 个字段里 14 个是空串，会让真正有值的那几个淹没在里面。
	a, buf := capture(t)
	a.Record(context.Background(), Event{Kind: EventUpstreamError})

	m := decode(t, buf)
	for _, k := range []string{"tenant", "session_id", "call_id", "tool", "reason", "duration_ms", "at"} {
		if _, ok := m[k]; ok {
			t.Errorf("为空时不该出现 %q", k)
		}
	}
	if m["event"] != "upstream_error" {
		t.Errorf("事件类型必须始终存在，得到 %v", m["event"])
	}
}

func TestNewAuditorFallsBackToDefaultLogger(t *testing.T) {
	// 传 nil 不该 panic——审计器缺席比审计器崩溃更容易被发现。
	NewAuditor(nil).Record(context.Background(), Event{Kind: EventCallProposed})
}
