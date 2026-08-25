package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/kw-decade/Texvoke/internal/serving"
	"github.com/kw-decade/Texvoke/pkg/toolbridge"
)

func testServer(t *testing.T) *server {
	t.Helper()
	b, err := toolbridge.New(toolbridge.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return &server{bridge: b, log: slog.New(slog.NewTextHandler(io.Discard, nil)), sessions: serving.NewSessionStore()}
}

func post(t *testing.T, h http.HandlerFunc, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func decodeJSON[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("响应不是合法 JSON：%v\n原文：%s", err, rec.Body.String())
	}
	return v
}

func weatherTool() toolbridge.Tool {
	return toolbridge.Tool{
		Name:        "get_weather",
		Description: "查询天气",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
	}
}

// 完整的接入流程：compile 拿 prompt 与 nonce，parse 带 nonce 换回调用。
// 服务端不保存任何东西——这条测试用两个独立的 handler 调用走完全程。
func TestCompileThenParseIsStateless(t *testing.T) {
	s := testServer(t)

	rec := post(t, s.handleCompile, "/v1/compile", compileRequest{
		SessionID: "sess-1", RequestID: "req-1",
		Tools: []toolbridge.Tool{weatherTool()},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("compile 返回 %d：%s", rec.Code, rec.Body.String())
	}
	compiled := decodeJSON[compileResponse](t, rec)

	if compiled.SystemPrompt == "" {
		t.Fatal("system_prompt 为空")
	}
	if compiled.Nonce == "" {
		t.Fatal("没有返回 nonce，调用方无法重建会话")
	}
	if !strings.Contains(compiled.SystemPrompt, compiled.Signal) {
		t.Error("system_prompt 里没有信号")
	}
	if len(compiled.ToolsIncluded) != 1 {
		t.Errorf("tools_included 为 %v", compiled.ToolsIncluded)
	}

	modelOutput := "我来查一下。\n" + compiled.Signal + `
<tool_call_envelope version="1"><call id="c1"><tool>get_weather</tool>
<arguments_json>{"city":"SF"}</arguments_json></call></tool_call_envelope>`

	rec = post(t, s.handleParse, "/v1/parse", parseRequest{
		SessionID: "sess-1", RequestID: "req-1",
		Nonce: compiled.Nonce, Text: modelOutput,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("parse 返回 %d：%s", rec.Code, rec.Body.String())
	}
	parsed := decodeJSON[parseResponse](t, rec)

	if parsed.Outcome != "calls_parsed" {
		t.Fatalf("outcome 为 %q：%s", parsed.Outcome, parsed.Error)
	}
	if len(parsed.Calls) != 1 || parsed.Calls[0].Name != "get_weather" {
		t.Fatalf("调用解析有误：%+v", parsed.Calls)
	}
	if string(parsed.Calls[0].Arguments) != `{"city":"SF"}` {
		t.Errorf("参数为 %s", parsed.Calls[0].Arguments)
	}
	if !strings.Contains(parsed.Text, "我来查一下") {
		t.Errorf("正文丢失：%q", parsed.Text)
	}
	if strings.Contains(parsed.Text, compiled.Signal) {
		t.Errorf("信号泄漏进正文：%q", parsed.Text)
	}
}

// 换一个 server 实例也能 parse——证明真的无状态，可以随便重启或扩容。
func TestParseWorksOnDifferentInstance(t *testing.T) {
	first := testServer(t)
	rec := post(t, first.handleCompile, "/v1/compile", compileRequest{
		SessionID: "s", RequestID: "r", Tools: []toolbridge.Tool{weatherTool()},
	})
	compiled := decodeJSON[compileResponse](t, rec)

	// 一个全新的实例，没有见过这个会话。
	second := testServer(t)
	output := compiled.Signal + `
<tool_call_envelope version="1"><call id="c1"><tool>get_weather</tool>
<arguments_json>{}</arguments_json></call></tool_call_envelope>`

	rec = post(t, second.handleParse, "/v1/parse", parseRequest{
		SessionID: "s", RequestID: "r", Nonce: compiled.Nonce, Text: output,
	})
	parsed := decodeJSON[parseResponse](t, rec)
	if parsed.Outcome != "calls_parsed" {
		t.Fatalf("换实例后解析失败：%q %s", parsed.Outcome, parsed.Error)
	}
}

// 解析失败返回 200 而不是 4xx：这不是 HTTP 层的错误，而是一次成功的分析，
// 结论是「模型的输出不合格式」。用状态码表达它会让调用方分不清
// 「服务挂了」和「模型写错了」。
func TestParseFailureIsNotAnHTTPError(t *testing.T) {
	s := testServer(t)
	rec := post(t, s.handleCompile, "/v1/compile", compileRequest{
		SessionID: "s", RequestID: "r", Tools: []toolbridge.Tool{weatherTool()},
	})
	compiled := decodeJSON[compileResponse](t, rec)

	bad := compiled.Signal + "\n<tool_call_envelope version=\"1\"><乱七八糟></tool_call_envelope>"
	rec = post(t, s.handleParse, "/v1/parse", parseRequest{
		SessionID: "s", RequestID: "r", Nonce: compiled.Nonce, Text: bad,
	})

	if rec.Code != http.StatusOK {
		t.Errorf("解析失败不该返回 HTTP %d", rec.Code)
	}
	parsed := decodeJSON[parseResponse](t, rec)
	if parsed.Outcome != "malformed" {
		t.Errorf("outcome 为 %q", parsed.Outcome)
	}
	if parsed.ErrorKind == "" {
		t.Error("缺少 error_kind，调用方无从决定是否重试")
	}
	if !parsed.Retryable {
		t.Error("格式错误应当标记为可重试")
	}
}

func TestParseRejectsMissingNonce(t *testing.T) {
	s := testServer(t)
	for _, req := range []parseRequest{
		{RequestID: "r", Nonce: "x", Text: "t"},
		{SessionID: "s", Nonce: "x", Text: "t"},
		{SessionID: "s", RequestID: "r", Text: "t"},
	} {
		rec := post(t, s.handleParse, "/v1/parse", req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("缺字段时应返回 400，实际 %d：%+v", rec.Code, req)
		}
	}
}

func TestParseRejectsBadNonce(t *testing.T) {
	s := testServer(t)
	rec := post(t, s.handleParse, "/v1/parse", parseRequest{
		SessionID: "s", RequestID: "r", Nonce: "不是合法的nonce", Text: "t",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("非法 nonce 应返回 400，实际 %d", rec.Code)
	}
}

func TestCompileWithoutToolsReportsKind(t *testing.T) {
	s := testServer(t)
	rec := post(t, s.handleCompile, "/v1/compile", compileRequest{
		SessionID: "s", RequestID: "r",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("返回 %d", rec.Code)
	}
	body := decodeJSON[map[string]string](t, rec)
	if body["error_kind"] != string(toolbridge.ErrNoTools) {
		t.Errorf("error_kind 为 %q，期望 %q", body["error_kind"], toolbridge.ErrNoTools)
	}
}

// 拒绝未知字段：调用方拼错一个字段名时，静默忽略会让它以为设置生效了，
// 然后困惑于行为不符预期。
func TestRejectsUnknownFields(t *testing.T) {
	s := testServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/compile",
		strings.NewReader(`{"session_id":"s","request_id":"r","tolls":[]}`))
	rec := httptest.NewRecorder()
	s.handleCompile(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("拼错的字段名应被拒绝，实际返回 %d", rec.Code)
	}
}

// 流式：请求体是模型输出流，响应是 NDJSON 事件流。
// 信号之前的文本要能边到边转发，不必等全部内容到齐。
func TestParseStream(t *testing.T) {
	s := testServer(t)
	rec := post(t, s.handleCompile, "/v1/compile", compileRequest{
		SessionID: "s", RequestID: "r", Tools: []toolbridge.Tool{weatherTool()},
	})
	compiled := decodeJSON[compileResponse](t, rec)

	output := "先说两句。\n" + compiled.Signal + `
<tool_call_envelope version="1"><call id="c1"><tool>get_weather</tool>
<arguments_json>{"city":"SF"}</arguments_json></call></tool_call_envelope>`

	url := "/v1/parse/stream?session_id=s&request_id=r&nonce=" + compiled.Nonce
	req := httptest.NewRequest(http.MethodPost, url, strings.NewReader(output))
	w := httptest.NewRecorder()
	s.handleParseStream(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("返回 %d：%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("Content-Type 为 %q", ct)
	}

	var texts []string
	var done streamDoneEvent
	for _, line := range strings.Split(strings.TrimSpace(w.Body.String()), "\n") {
		if line == "" {
			continue
		}
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &probe); err != nil {
			t.Fatalf("事件不是合法 JSON：%v\n行：%s", err, line)
		}
		switch probe.Type {
		case "text":
			var ev streamTextEvent
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				t.Fatal(err)
			}
			texts = append(texts, ev.Text)
		case "done":
			if err := json.Unmarshal([]byte(line), &done); err != nil {
				t.Fatal(err)
			}
		default:
			t.Errorf("未知事件类型 %q", probe.Type)
		}
	}

	if done.Outcome != "calls_parsed" {
		t.Fatalf("outcome 为 %q：%s", done.Outcome, done.Error)
	}
	if len(done.Calls) != 1 {
		t.Fatalf("调用数为 %d", len(done.Calls))
	}
	joined := strings.Join(texts, "")
	if !strings.Contains(joined, "先说两句") {
		t.Errorf("信号前的文本未被转发：%q", joined)
	}
	// 已转发出去的字节里不能有协议内容——它们已经到客户端了，收不回来。
	if strings.Contains(joined, compiled.Signal) {
		t.Errorf("信号被转发给客户端了：%q", joined)
	}
	if strings.Contains(joined, "tool_call_envelope") {
		t.Errorf("协议结构被转发给客户端了：%q", joined)
	}
}

func TestParseStreamRejectsBadNonce(t *testing.T) {
	s := testServer(t)
	req := httptest.NewRequest(http.MethodPost,
		"/v1/parse/stream?session_id=s&request_id=r&nonce=bad", strings.NewReader("x"))
	rec := httptest.NewRecorder()
	s.handleParseStream(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("非法 nonce 应返回 400，实际 %d", rec.Code)
	}
}

// 健康检查不得泄露 nonce、上游地址或工具定义。
func TestHealthLeaksNothing(t *testing.T) {
	s := testServer(t)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	s.handleHealth(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("返回 %d", rec.Code)
	}
	body := rec.Body.String()
	for _, leak := range []string{"UTR-CALL", "nonce", "upstream", "api", "key", "token"} {
		if strings.Contains(strings.ToLower(body), leak) {
			t.Errorf("健康检查泄露了 %q：%s", leak, body)
		}
	}
	if !strings.Contains(body, "ok") {
		t.Errorf("健康检查应报告状态：%s", body)
	}
}

func TestLoadNoiseFilters(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/filters.txt"
	content := "# 这是注释\n^你好\n\n如果你想了解更多.*$\n"
	if err := writeFile(path, content); err != nil {
		t.Fatal(err)
	}

	filters, err := loadNoiseFilters(path)
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if len(filters) != 2 {
		t.Fatalf("加载了 %d 条规则，期望 2 条（注释与空行应跳过）", len(filters))
	}

	if _, err := loadNoiseFilters(""); err != nil {
		t.Errorf("空路径应返回 nil 而非报错：%v", err)
	}

	badPath := dir + "/bad.txt"
	if err := writeFile(badPath, "[未闭合\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := loadNoiseFilters(badPath); err == nil {
		t.Error("非法正则应报错，并指出是第几行")
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
