package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

func benchDecodeOpts() DecodeOptions {
	return DecodeOptions{
		SessionID: "sess-1", RequestID: "req-1",
		Now: time.Unix(1700000000, 0), MaxBytes: DefaultMaxRequestBytes,
	}
}

// 带很多工具的请求是真实场景：Claude Code 一次带 80 多个工具定义，
// 而请求解码在每次调用上都要跑一遍。
func benchChatRequestJSON(tools int) []byte {
	var b strings.Builder
	b.WriteString(`{"model":"gpt-4o","messages":[{"role":"user","content":"读一下文件"}],"tools":[`)
	for i := range tools {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"type":"function","function":{"name":"tool_%d",`+
			`"description":"第 %d 个工具，用来做某件事",`+
			`"parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}`, i, i)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

func BenchmarkDecodeChatRequest(b *testing.B) {
	raw := benchChatRequestJSON(80)
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	for b.Loop() {
		if _, err := DecodeChatRequest(raw, benchDecodeOpts()); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeChatResponseWithToolCalls(b *testing.B) {
	raw := []byte(`{"id":"chatcmpl-1","model":"gpt-4o","created":1700000000,
		"choices":[{"index":0,"finish_reason":"tool_calls","message":{
			"role":"assistant","tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.txt\"}"}},
				{"id":"call_2","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"b.txt\"}"}}]}}]}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	for b.Loop() {
		if _, err := DecodeChatResponse(raw, benchDecodeOpts()); err != nil {
			b.Fatal(err)
		}
	}
}

func benchChatResponse() ChatResponse {
	return ChatResponse{
		ID: "chatcmpl-1", Model: "gpt-4o", Created: 1700000000,
		Content:      json.RawMessage(`"` + strings.Repeat("这是一段回复。", 40) + `"`),
		FinishReason: FinishStop,
	}
}

func BenchmarkEncodeChatResponse(b *testing.B) {
	r := benchChatResponse()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := EncodeChatResponse(r); err != nil {
			b.Fatal(err)
		}
	}
}

// SSE 渲染在流式路径上每次响应都要跑。
func BenchmarkEncodeChatStream(b *testing.B) {
	r := benchChatResponse()
	b.ReportAllocs()
	for b.Loop() {
		enc := NewSSEEncoder(io.Discard)
		if err := EncodeChatStream(enc, r); err != nil {
			b.Fatal(err)
		}
	}
}

// SSE 解码是转发上游流时的热路径，逐行扫描。
func BenchmarkSSEDecode(b *testing.B) {
	var buf bytes.Buffer
	for i := range 200 {
		fmt.Fprintf(&buf, "event: message\ndata: {\"i\":%d,\"text\":\"增量内容\"}\n\n", i)
	}
	raw := buf.Bytes()
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	for b.Loop() {
		dec := NewSSEDecoder(bytes.NewReader(raw), SSEDecoderOptions{})
		for {
			_, err := dec.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
		}
	}
}
