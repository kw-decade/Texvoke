package main

import (
	"net/http"
	"testing"

	"github.com/kw-decade/Texvoke/pkg/toolbridge"
)

func recoverReq(nonce string, outcome toolbridge.Outcome, text, parseKind string, handshakeDone bool) recoverRequest {
	req := recoverRequest{
		SessionID: "s", RequestID: "r", Nonce: nonce,
		Result:         renderResult{Outcome: string(outcome), Text: text},
		Tools:          []string{"exec"},
		ParseErrorKind: parseKind,
	}
	if parseKind != "" {
		req.ParseError = "测试错误"
	}
	req.Evidence.ToolChoice = "auto"
	req.Evidence.HandshakeDone = handshakeDone
	return req
}

// 端点是门面：诊断与恢复的语义在包测试里已经钉死，这里只验 HTTP 契约——
// 字段名、handshake 往返、以及调用方能拿到明确的停手信号。
func TestRecoverEndpoint(t *testing.T) {
	s := testServer(t)
	n := mustNonceValue(t)

	t.Run("人格拒绝 → 能力说明 + handshake_done", func(t *testing.T) {
		rec := post(t, s.handleRecover, "/v1/recover", recoverReq(
			n, toolbridge.OutcomePlainText, "我无法直接读取文件系统，请自行运行", "", false))
		if rec.Code != http.StatusOK {
			t.Fatalf("HTTP %d：%s", rec.Code, rec.Body)
		}
		out := decodeJSON[struct {
			Kind        string `json:"kind"`
			Terminal    bool   `json:"terminal"`
			ShouldRetry bool   `json:"should_retry"`
			Messages    []struct {
				Role string `json:"role"`
				Text string `json:"text"`
			} `json:"messages"`
			HandshakeDone bool     `json:"handshake_done"`
			Reasons       []string `json:"reasons"`
		}](t, rec)

		if out.Kind != "persona_refusal" || !out.ShouldRetry || len(out.Messages) != 1 {
			t.Fatalf("诊断或恢复不对：%+v", out)
		}
		if out.Terminal {
			t.Error("人格误会不是硬拒绝")
		}
		if !out.HandshakeDone {
			t.Error("handshake_done 必须为 true，下一轮要带回来")
		}
	})

	t.Run("第二次带 handshake_done → 换运行时通知继续（阶梯不重复握手）", func(t *testing.T) {
		rec := post(t, s.handleRecover, "/v1/recover", recoverReq(
			n, toolbridge.OutcomePlainText, "我不能执行", "", true))
		out := decodeJSON[struct {
			ShouldRetry bool   `json:"should_retry"`
			Reason      string `json:"reason"`
			Messages    []struct {
				Text string `json:"text"`
			} `json:"messages"`
		}](t, rec)
		if !out.ShouldRetry || len(out.Messages) == 0 {
			t.Fatalf("握手后应换 L2 手段而不是停手：%+v", out)
		}
		if out.Reason == "" {
			t.Error("恢复动作要说明原因，接入方才能记日志")
		}
	})

	t.Run("正常回答 → 不追问", func(t *testing.T) {
		rec := post(t, s.handleRecover, "/v1/recover", recoverReq(
			n, toolbridge.OutcomePlainText, "今天天气不错", "", false))
		out := decodeJSON[struct {
			Kind        string `json:"kind"`
			ShouldRetry bool   `json:"should_retry"`
		}](t, rec)
		if out.Kind != "" || out.ShouldRetry {
			t.Errorf("正常回答不该触发恢复：%+v", out)
		}
	})
}

// 无状态：换个实例也能完成诊断——与 /v1/render 的约定一致。
func TestRecoverWorksOnDifferentInstance(t *testing.T) {
	s1 := testServer(t)
	rec := post(t, s1.handleCompile, "/v1/compile", map[string]any{
		"session_id": "s2", "request_id": "r2",
		"tools": []map[string]any{{"name": "f"}},
	})
	c := decodeJSON[struct {
		Nonce string `json:"nonce"`
	}](t, rec)

	s2 := testServer(t)
	rec2 := post(t, s2.handleRecover, "/v1/recover", recoverReq(
		c.Nonce, toolbridge.OutcomePlainText, "我无法直接读取文件系统", "", false))
	out := decodeJSON[struct {
		ShouldRetry bool `json:"should_retry"`
	}](t, rec2)
	if !out.ShouldRetry {
		t.Errorf("换实例后应照常工作：%s", rec2.Body)
	}
}

// mustNonceValue 编译一次拿到合法 nonce。
func mustNonceValue(t *testing.T) string {
	t.Helper()
	s := testServer(t)
	rec := post(t, s.handleCompile, "/v1/compile", map[string]any{
		"session_id": "s", "request_id": "r",
		"tools": []map[string]any{{"name": "f"}},
	})
	return decodeJSON[struct {
		Nonce string `json:"nonce"`
	}](t, rec).Nonce
}
