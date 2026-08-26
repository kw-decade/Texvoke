package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kw-decade/Texvoke/internal/serving"
	"github.com/kw-decade/Texvoke/pkg/toolbridge"
)

// 会话阶梯的核心承诺：同一键自动递进、封顶停手、成功归位。
// 接入方只传一个 session_key，其余全部由服务端维护。
func TestSessionLadder(t *testing.T) {
	s := testServer(t)

	refusal := func() toolbridge.Result {
		return toolbridge.Result{
			Text:    "我会先读取 AGENTS.md 再创建文件。",
			Outcome: toolbridge.OutcomePlainText,
		}
	}
	recoverOnce := func(key string, attempt int, hsDone bool) recoverResponse {
		rec := post(t, s.handleRecover, "/v1/recover", recoverRequest{
			SessionID: "s", RequestID: "r", Nonce: mustNonceValue(t),
			Tools:      []string{"exec"},
			Result:     renderResult{Outcome: string(toolbridge.OutcomePlainText), Text: refusal().Text},
			SessionKey: key,
			Evidence: struct {
				ToolChoice           string `json:"tool_choice,omitempty"`
				HTTPStatus           int    `json:"http_status,omitempty"`
				UpstreamErrType      string `json:"upstream_err_type,omitempty"`
				UpstreamErrCode      string `json:"upstream_err_code,omitempty"`
				TransportError       bool   `json:"transport_error,omitempty"`
				HandshakeDone        bool   `json:"handshake_done,omitempty"`
				Attempt              int    `json:"attempt,omitempty"`
				HasSuccessfulHistory bool   `json:"has_successful_history,omitempty"`
				AgentMode            *bool  `json:"agent_mode,omitempty"`
			}{Attempt: attempt, HandshakeDone: hsDone},
		})
		if rec.Code != 200 {
			t.Fatalf("recover 失败 %d：%s", rec.Code, rec.Body.String())
		}
		return decodeJSON[recoverResponse](t, rec)
	}

	key := "sess-A"
	seen := map[int]bool{}
	for level := 1; level <= serving.MaxLadderLevel; level++ {
		out := recoverOnce(key, level, false)
		if !out.ShouldRetry {
			t.Fatalf("第 %d 级应给出追问：%+v", level, out)
		}
		if out.Level != level {
			t.Errorf("第 %d 次追问的 level=%d", level, out.Level)
		}
		if seen[out.Level] {
			t.Errorf("级数 %d 重复出现——阶梯没有递进", out.Level)
		}
		seen[out.Level] = true
	}

	// 封顶：再问就停手，如实上报而不是继续骚扰。
	out := recoverOnce(key, serving.MaxLadderLevel, true)
	if out.ShouldRetry {
		t.Errorf("超过最高级后必须停手：%+v", out)
	}
	if !strings.Contains(out.Reason, "已用尽") {
		t.Errorf("停手要说明原因：%s", out.Reason)
	}
}

// 成功调用把阶梯归位：同一个键在 parse 出调用后再卡住，从 L1 重新开始。
func TestSessionLadderResetsOnSuccess(t *testing.T) {
	s := testServer(t)

	compileBody := `{"session_id":"sr","request_id":"rr","tools":[{"name":"exec","input_schema":{"type":"object"}}]}`
	rec := post(t, s.handleCompile, "/v1/compile", json.RawMessage(compileBody))
	nonce := decodeJSON[struct {
		Nonce string `json:"nonce"`
	}](t, rec).Nonce

	const key = "sess-reset"
	// 先推两级。
	for i := 0; i < 2; i++ {
		post(t, s.handleRecover, "/v1/recover", recoverRequest{
			SessionID: "sr", RequestID: "rr", Nonce: nonce,
			Result:     renderResult{Outcome: string(toolbridge.OutcomePlainText), Text: "我会先读取 AGENTS.md 再创建文件。"},
			Tools:      []string{"exec"},
			SessionKey: key,
		})
	}

	// parse 出调用 → 归零。
	pr := post(t, s.handleParse, "/v1/parse", parseRequest{
		SessionID: "sr", RequestID: "rr", Nonce: nonce,
		Text:       "[[UTR-CALL:" + nonce + "]]\n<tool_call_envelope version=\"1\">\n  <call id=\"c1\">\n    <tool>exec</tool>\n    <arguments_json><![CDATA[{\"cmd\":\"ls\"}]]></arguments_json>\n  </call>\n</tool_call_envelope>",
		Tools:      []string{"exec"},
		SessionKey: key,
	})
	parsed := decodeJSON[parseResponse](t, pr)
	if parsed.Outcome != "calls_parsed" {
		t.Fatalf("前置条件：应解析出调用，实际 %s（%s）", parsed.Outcome, parsed.Error)
	}

	// 再卡住：应从 L1 重新开始（能力说明），而不是接着 L3。
	rc := post(t, s.handleRecover, "/v1/recover", recoverRequest{
		SessionID: "sr", RequestID: "rr", Nonce: nonce,
		Result:     renderResult{Outcome: string(toolbridge.OutcomePlainText), Text: "我会先读取 AGENTS.md 再创建文件。"},
		Tools:      []string{"exec"},
		SessionKey: key,
	})
	out := decodeJSON[recoverResponse](t, rc)
	if !out.HandshakeDone || out.Level != 1 {
		t.Errorf("归位后应从 L1 开始（level=%d handshake=%v）：%s", out.Level, out.HandshakeDone, out.Reason)
	}
}

// adapt 带会话键时登记「历史里已有成功调用」，recover 的 L2 文案随之引用它。
func TestAdaptRegistersCallHistory(t *testing.T) {
	s := testServer(t)
	body := `{
		"model": "m", "max_tokens": 100,
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": [
				{"type": "text", "text": "好的"},
				{"type": "tool_use", "id": "t1", "name": "f", "input": {}}
			]},
			{"role": "user", "content": [{"type":"tool_result","tool_use_id":"t1","content":"done"}]}
		],
		"tools": [{"name": "f", "input_schema": {"type": "object"}}]
	}`
	rec := post(t, s.handleAdapt, "/v1/adapt", adaptRequest{
		Protocol: "anthropic", SessionID: "sa", RequestID: "ra",
		Body: json.RawMessage(body), SessionKey: "hist-key",
	})
	if rec.Code != 200 {
		t.Fatalf("adapt 失败 %d：%s", rec.Code, rec.Body.String())
	}
	if st := s.sessions.Snapshot("hist-key"); !st.HasCalls {
		t.Error("历史里有 tool_use 却没登记")
	}

	// 无历史的请求不登记。
	rec2 := post(t, s.handleAdapt, "/v1/adapt", adaptRequest{
		Protocol: "anthropic", SessionID: "sb", RequestID: "rb",
		Body:       json.RawMessage(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"g","input_schema":{"type":"object"}}]}`),
		SessionKey: "nohist-key",
	})
	if rec2.Code != 200 {
		t.Fatalf("adapt 失败 %d", rec2.Code)
	}
	if st := s.sessions.Snapshot("nohist-key"); st.HasCalls {
		t.Error("无调用的历史被误登记")
	}
}
