package ir

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// 幂等键在每次调用上都要算一遍：安全门算一次、账本算一次、
// 执行器传给下游又用一次。它在热路径上。
func BenchmarkIdempotencyKey(b *testing.B) {
	p := ToolCallProposal{
		SessionID: "sess-1", RequestID: "req-1", CallID: "call_1",
		Tool:      ToolID{Namespace: "fs", Name: "read_file", Version: "1"},
		Arguments: json.RawMessage(`{"path":"/srv/data/report.csv","encoding":"utf-8"}`),
		Source:    SourceNative, CreatedAt: time.Unix(1700000000, 0),
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = p.IdempotencyKey()
	}
}

// 大参数下的表现单独测：参数是模型写的，长度不可控。
func BenchmarkIdempotencyKeyLargeArguments(b *testing.B) {
	p := ToolCallProposal{
		SessionID: "sess-1", RequestID: "req-1", CallID: "call_1",
		Tool:      ToolID{Namespace: "fs", Name: "write_file", Version: "1"},
		Arguments: json.RawMessage(`{"content":"` + strings.Repeat("x", 64<<10) + `"}`),
		Source:    SourceNative, CreatedAt: time.Unix(1700000000, 0),
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = p.IdempotencyKey()
	}
}

// ToolID.String 被注册表查找、日志和审计反复调用。
func BenchmarkToolIDString(b *testing.B) {
	id := ToolID{Namespace: "fs", Name: "read_file", Version: "1"}
	b.ReportAllocs()
	for b.Loop() {
		_ = id.String()
	}
}

func BenchmarkProposalValidate(b *testing.B) {
	p := ToolCallProposal{
		SessionID: "sess-1", RequestID: "req-1", CallID: "call_1",
		Tool:      ToolID{Namespace: "fs", Name: "read_file", Version: "1"},
		Arguments: json.RawMessage(`{"path":"a.txt"}`),
		Source:    SourceNative, CreatedAt: time.Unix(1700000000, 0),
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = p.Validate()
	}
}
