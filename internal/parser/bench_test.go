package parser

import (
	"strings"
	"testing"

	"github.com/kw-decade/Texvoke/internal/vproto"
)

func benchNonce(b *testing.B) vproto.Nonce {
	b.Helper()
	n, err := vproto.NewNonce("sess-1", "req-1")
	if err != nil {
		b.Fatal(err)
	}
	return n
}

// 纯文本是最常见的情形：绝大多数模型输出里根本没有工具调用，
// 而每一个字节都要过一遍「这会不会是信号的开头」。
func BenchmarkWritePlainText(b *testing.B) {
	n := benchNonce(b)
	chunk := []byte(strings.Repeat("这是一段普通的模型回复，没有任何协议信号。\n", 20))
	b.ReportAllocs()
	b.SetBytes(int64(len(chunk)))
	for b.Loop() {
		p, err := New(n, DefaultLimits())
		if err != nil {
			b.Fatal(err)
		}
		if _, err := p.Write(chunk); err != nil {
			b.Fatal(err)
		}
		p.Close()
	}
}

// 逐字节喂入是流式的最坏情形：每次 Write 都要重新判断提交边界。
func BenchmarkWriteByteByByte(b *testing.B) {
	n := benchNonce(b)
	text := []byte("模型正在正常回答一个问题，中间没有信号。")
	b.ReportAllocs()
	for b.Loop() {
		p, err := New(n, DefaultLimits())
		if err != nil {
			b.Fatal(err)
		}
		for i := range text {
			if _, err := p.Write(text[i : i+1]); err != nil {
				b.Fatal(err)
			}
		}
		p.Close()
	}
}

func BenchmarkParseEnvelope(b *testing.B) {
	n := benchNonce(b)
	env, err := vproto.RenderEnvelope(n, []vproto.Call{
		{ID: "c1", Tool: "fs/read_file", ArgumentsJSON: `{"path":"a.txt"}`},
		{ID: "c2", Tool: "fs/write_file", ArgumentsJSON: `{"path":"b.txt","content":"hello"}`},
	})
	if err != nil {
		b.Fatal(err)
	}
	chunk := []byte("先说一段话。\n" + env)
	b.ReportAllocs()
	b.SetBytes(int64(len(chunk)))
	for b.Loop() {
		p, err := New(n, DefaultLimits())
		if err != nil {
			b.Fatal(err)
		}
		if _, err := p.Write(chunk); err != nil {
			b.Fatal(err)
		}
		if res := p.Close(); res.Outcome != OutcomeCallsParsed {
			b.Fatalf("解析结果不对：%s", res.Outcome)
		}
	}
}
