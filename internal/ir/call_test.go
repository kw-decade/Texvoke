package ir

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// 调用生命周期状态机随执行层一起删除（见 call.go 的说明）：本项目只把提案
// 交回客户端，proposed 之后的状态在这个进程里永远不会发生。状态机的穷举
// 转移测试与「proposed 不得直达 executing」的安全门测试一并移除——它们
// 证明的是一道本进程不再设防的门。

// validProposal 返回一份合规的调用提案，供各用例按需改坏其中一项。
func validProposal() ToolCallProposal {
	return ToolCallProposal{
		SessionID: "sess-1",
		RequestID: "req-1",
		CallID:    "call_abc",
		Tool:      ToolID{"fs", "read_file", "1"},
		Arguments: json.RawMessage(`{"path":"/tmp/a.txt"}`),
		Source:    SourceNative,
		CreatedAt: time.Unix(1700000000, 0),
	}
}

func TestToolCallProposalValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ToolCallProposal)
		wantErr string
	}{
		{"合规", func(*ToolCallProposal) {}, ""},
		{"空参数对象也合规", func(p *ToolCallProposal) { p.Arguments = json.RawMessage(`{}`) }, ""},
		{"缺 session_id", func(p *ToolCallProposal) { p.SessionID = "" }, "session_id"},
		{"缺 request_id", func(p *ToolCallProposal) { p.RequestID = "" }, "request_id"},
		{"缺 call_id", func(p *ToolCallProposal) { p.CallID = "" }, "call_id"},
		{"工具标识非法", func(p *ToolCallProposal) { p.Tool.Version = "" }, "工具标识非法"},
		{"source 未设置", func(p *ToolCallProposal) { p.Source = "" }, "source 非法"},
		{"缺 created_at", func(p *ToolCallProposal) { p.CreatedAt = time.Time{} }, "created_at"},

		// 参数必须是成形的 JSON 对象。规格三章禁止把解析不出的内容塞进兜底
		// 字段后继续执行，所以宁可在这里报错。
		{"参数为空", func(p *ToolCallProposal) { p.Arguments = nil }, "缺少 arguments"},
		{"参数是数组", func(p *ToolCallProposal) { p.Arguments = json.RawMessage(`[1,2]`) }, "不是 JSON 对象"},
		{"参数是字符串", func(p *ToolCallProposal) { p.Arguments = json.RawMessage(`"x"`) }, "不是 JSON 对象"},
		{"参数是残缺 JSON", func(p *ToolCallProposal) { p.Arguments = json.RawMessage(`{"a":`) }, "不是 JSON 对象"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := validProposal()
			tc.mutate(&p)
			err := p.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("期望通过，却返回错误：%v", err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("期望报错包含 %q，却通过了", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("错误信息 %q 未包含 %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// 幂等键的核心性质：同一次调用重试得到相同的键（会被账本拦住），
// 任何一个组成部分变化则得到不同的键（属于另一次调用，应当执行）。
func TestIdempotencyKey(t *testing.T) {
	base := validProposal()
	baseKey := base.IdempotencyKey()

	if baseKey == "" {
		t.Fatal("幂等键不应为空")
	}
	if len(baseKey) != 64 {
		t.Errorf("SHA-256 十六进制应为 64 字符，实际 %d", len(baseKey))
	}

	t.Run("同一提案键稳定", func(t *testing.T) {
		if got := base.IdempotencyKey(); got != baseKey {
			t.Error("同一份提案两次计算得到了不同的键")
		}
	})

	// call_id 和 created_at 不参与计算：同一次调用的重试会带着新的时间戳，
	// 若它们参与计算，重试就永远撞不上账本里的旧记录，副作用会被重复执行。
	t.Run("重试改变时间戳不影响键", func(t *testing.T) {
		retry := base
		retry.CreatedAt = base.CreatedAt.Add(time.Minute)
		retry.CallID = "call_def"
		if got := retry.IdempotencyKey(); got != baseKey {
			t.Error("重试时时间戳或 call_id 变化不应改变幂等键")
		}
	})

	differing := []struct {
		name   string
		mutate func(*ToolCallProposal)
	}{
		{"换会话", func(p *ToolCallProposal) { p.SessionID = "sess-2" }},
		{"换工具名", func(p *ToolCallProposal) { p.Tool.Name = "write_file" }},
		{"换工具版本", func(p *ToolCallProposal) { p.Tool.Version = "2" }},
		{"换命名空间", func(p *ToolCallProposal) { p.Tool.Namespace = "net" }},
		{"改参数值", func(p *ToolCallProposal) { p.Arguments = json.RawMessage(`{"path":"/tmp/b.txt"}`) }},
		{"加参数", func(p *ToolCallProposal) { p.Arguments = json.RawMessage(`{"path":"/tmp/a.txt","n":1}`) }},
	}
	for _, tc := range differing {
		t.Run(tc.name+"应改变键", func(t *testing.T) {
			p := base
			tc.mutate(&p)
			if got := p.IdempotencyKey(); got == baseKey {
				t.Errorf("%s 后幂等键未改变，会被账本误判为同一次调用", tc.name)
			}
		})
	}

	// 分隔符测试：若各段之间不加分隔就直接拼接哈希，"ab"+"c" 与 "a"+"bc"
	// 会撞成同一个键，两次不同的调用会被账本误认为重试。
	t.Run("段间有分隔符防止拼接歧义", func(t *testing.T) {
		a := validProposal()
		a.SessionID = "s"
		a.Tool = ToolID{"n", "x", "1"}

		b := validProposal()
		b.SessionID = "sn"
		b.Tool = ToolID{"", "x", "1"} // 注意：这份提案本身不合规，只用来验哈希输入
		if a.IdempotencyKey() == b.IdempotencyKey() {
			t.Error("不同的字段切分得到了相同的幂等键")
		}
	})
}

func TestDigestRawCandidate(t *testing.T) {
	a := DigestRawCandidate([]byte("hello"))
	b := DigestRawCandidate([]byte("hello"))
	c := DigestRawCandidate([]byte("hellp"))

	if a != b {
		t.Error("相同输入应得到相同摘要")
	}
	if a == c {
		t.Error("不同输入应得到不同摘要")
	}
	if len(a) != 64 {
		t.Errorf("SHA-256 十六进制应为 64 字符，实际 %d", len(a))
	}
	if strings.Contains(a, "hello") {
		t.Error("摘要中不应出现原文")
	}
}
