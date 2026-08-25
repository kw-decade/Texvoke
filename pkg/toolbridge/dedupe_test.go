package toolbridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 真实 agent 会把整套指令按 turn 重发。抓包统计：Codex 一个请求里同一条
// 26 KB 沙箱说明出现 4 次，140 KB 消息里 66% 是重复内容。扫 34 个会话，
// 6 个重复超过 15%，最高 79%——这直接让协议指令的占比在不同会话间差 3 倍。
func TestDedupeInstructionsOnRealFixture(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..",
		"tests", "fixtures", "eval", "codex-multi-turn.json"))
	if err != nil {
		t.Skipf("缺 fixture：%v", err)
	}

	b, _ := New(Config{})
	sess, _ := b.NewSession("s", "r")
	req, err := DecodeRequest(ProtocolResponses, body,
		DecodeOptions{SessionID: "s", RequestID: "r"})
	if err != nil {
		t.Fatalf("真实 fixture 解码失败：%v", err)
	}

	before := len(req.msgs())
	userBefore := countRole(req, "user")

	if _, err := sess.CompileRequest(req, CompileOptions{}); err != nil {
		t.Fatal(err)
	}

	if n := req.DedupedInstructions(); n == 0 {
		t.Fatal("这份 fixture 有 66% 重复内容，一条都没删说明去重没生效")
	}
	// user 消息一条都不能少——它们是对话轮次，用户可能真的说了两次同样的话。
	if got := countRole(req, "user"); got != userBefore {
		t.Errorf("user 消息被删了：%d → %d", userBefore, got)
	}
	t.Logf("消息数 %d → %d（删了 %d 条重复指令）",
		before, len(req.msgs()), req.DedupedInstructions())
}

func countRole(r *Request, role string) int {
	n := 0
	for _, m := range r.msgs() {
		if string(m.Role) == role {
			n++
		}
	}
	return n
}

// 去重的两条判据都要钉住：只动指令消息，且保留第一次出现。
func TestDedupeInstructionsRules(t *testing.T) {
	t.Run("只动指令消息", func(t *testing.T) {
		body := `{"model":"m","messages":[
			{"role":"system","content":"规则 A"},
			{"role":"user","content":"继续"},
			{"role":"system","content":"规则 A"},
			{"role":"user","content":"继续"}
		],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`

		req := mustPrepare(t, ProtocolChat, body)
		if req.DedupedInstructions() != 1 {
			t.Errorf("应删掉 1 条重复 system，实际 %d", req.DedupedInstructions())
		}
		// 两条「继续」都要留着：用户真的说了两次。
		if n := countRole(req, "user"); n != 2 {
			t.Errorf("重复的 user 消息被删了，剩 %d 条", n)
		}
	})

	t.Run("developer 也算指令", func(t *testing.T) {
		// 真实 Codex 的指令全是 developer 角色，只认 system 等于白做。
		body := `{"model":"m","input":[
			{"type":"message","role":"developer","content":"规则 A"},
			{"type":"message","role":"user","content":"干活"},
			{"type":"message","role":"developer","content":"规则 A"}
		],"tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]}`

		req := mustPrepare(t, ProtocolResponses, body)
		if req.DedupedInstructions() != 1 {
			t.Errorf("developer 消息没被去重，实际删了 %d 条", req.DedupedInstructions())
		}
	})

	t.Run("保留第一次不重排", func(t *testing.T) {
		body := `{"model":"m","messages":[
			{"role":"system","content":"规则 A"},
			{"role":"user","content":"甲"},
			{"role":"system","content":"规则 A"},
			{"role":"user","content":"乙"}
		],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`

		req := mustPrepare(t, ProtocolChat, body)
		up, err := req.Encode()
		if err != nil {
			t.Fatal(err)
		}
		s := string(up)
		// 顺序必须是 规则A → 甲 → 乙。保留最后一次会把「甲」挤到规则之前。
		iA, i甲, i乙 := strings.Index(s, "规则 A"), strings.Index(s, "甲"), strings.Index(s, "乙")
		if !(iA < i甲 && i甲 < i乙) {
			t.Errorf("对话顺序被改了：%s", s)
		}
	})

	t.Run("只差一个字不算重复", func(t *testing.T) {
		// 两条相似的指令可能是刻意的修订，合并等于替客户端改写业务逻辑。
		body := `{"model":"m","messages":[
			{"role":"system","content":"规则 A"},
			{"role":"system","content":"规则 B"},
			{"role":"user","content":"干活"}
		],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`

		req := mustPrepare(t, ProtocolChat, body)
		if req.DedupedInstructions() != 0 {
			t.Errorf("不同内容被当成重复删了 %d 条", req.DedupedInstructions())
		}
	})

	t.Run("没有重复时不动任何东西", func(t *testing.T) {
		body := `{"model":"m","messages":[
			{"role":"system","content":"规则"},
			{"role":"user","content":"干活"}
		],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`

		req := mustPrepare(t, ProtocolChat, body)
		if req.DedupedInstructions() != 0 {
			t.Errorf("无重复却删了 %d 条", req.DedupedInstructions())
		}
	})
}

func mustPrepare(t *testing.T, proto Protocol, body string) *Request {
	t.Helper()
	b, _ := New(Config{})
	sess, _ := b.NewSession("s", "r")
	req, err := DecodeRequest(proto, []byte(body),
		DecodeOptions{SessionID: "s", RequestID: "r"})
	if err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	if _, err := sess.CompileRequest(req, CompileOptions{}); err != nil {
		t.Fatalf("编译失败：%v", err)
	}
	return req
}

// 去重不能把带工具调用或结果的消息删掉——那不是纯指令，删了会丢关联。
func TestDedupeKeepsMessagesWithCalls(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"model": "gpt-4o",
		"messages": []any{
			map[string]any{"role": "system", "content": "规则"},
			map[string]any{"role": "user", "content": "跑"},
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
				map[string]any{"id": "c1", "type": "function",
					"function": map[string]any{"name": "f", "arguments": `{}`}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "c1", "content": "结果"},
		},
		"tools": []any{map[string]any{"type": "function",
			"function": map[string]any{"name": "f", "parameters": map[string]any{"type": "object"}}}},
	})

	req := mustPrepare(t, ProtocolChat, string(raw))
	up, err := req.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(up), "结果") {
		t.Errorf("工具结果丢了：%s", up)
	}
}
