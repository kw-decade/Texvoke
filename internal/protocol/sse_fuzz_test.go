package protocol

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// FuzzSSEDecode 对 SSE 解析器做模糊测试。
//
// 断言的是性质而不是具体输出：解析器直接吃上游的原始字节，而上游可能是
// 任何东西——被截断的流、恶意构造的输入、代理改写过的响应。这里要证明的是
//
//  1. 永不 panic；
//  2. 永不无界增长（受限于配置的上限）；
//  3. 要么产出事件，要么给出可诊断的错误，不会静默吞掉输入。
//
// 跑法：go test -run=Fuzz -fuzz=FuzzSSEDecode -fuzztime=30s ./internal/protocol/
func FuzzSSEDecode(f *testing.F) {
	seeds := []string{
		"",
		"\n",
		"\r",
		"\r\n",
		"data: x\n\n",
		"event: a\ndata: b\nid: c\nretry: 1\n\n",
		": ping\n\n",
		"data: 中文\n\n",
		"data\n\n",
		"data:\n\n",
		"data: a\ndata: b\n\n",
		":\r:\r\n:\n",
		"event: a\r\ndata: {\"k\":\"v\"}\r\n\r\n",
		"data: [DONE]\n\n",
		"unknown_field: x\ndata: y\n\n",
		"retry: 不是数字\ndata: x\n\n",
		"id: \x00nul\ndata: x\n\n",
		strings.Repeat("data: x\n", 100) + "\n",
		"data: " + strings.Repeat("长", 500) + "\n\n",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		// 上限压得很小，让「超限必须报错而不是继续吃」这条性质更容易被触发。
		const maxLine = 4096
		const maxEvent = 8192

		d := NewSSEDecoder(bytes.NewReader(data), SSEDecoderOptions{
			MaxLineBytes:  maxLine,
			MaxEventBytes: maxEvent,
		})

		for i := 0; ; i++ {
			// 事件数不可能超过输入字节数：每个事件至少要吃掉一个换行。
			// 超过就说明解析器在原地打转，是一个不会自己停下的死循环。
			if i > len(data)+2 {
				t.Fatalf("产出的事件数超过输入长度，解析器未推进：输入 %d 字节", len(data))
			}

			ev, err := d.Next()
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				// 报错必须可诊断——空错误信息等于没报。
				if err.Error() == "" {
					t.Fatal("返回了空的错误信息")
				}
				return
			}
			if ev == nil {
				t.Fatal("同时返回了 nil 事件与 nil 错误")
			}

			// 上限必须真的生效。
			if len(ev.Data) > maxEvent {
				t.Fatalf("事件数据 %d 字节超过上限 %d", len(ev.Data), maxEvent)
			}
			// 解析出的字段不该含行终止符：含了就说明切分逻辑漏了一种行尾，
			// 再渲染出去会伪造出额外的字段行。
			if strings.ContainsAny(ev.Type, "\r\n") {
				t.Fatalf("事件类型含行终止符：%q", ev.Type)
			}
			if strings.ContainsAny(ev.ID, "\r\n") {
				t.Fatalf("事件 id 含行终止符：%q", ev.ID)
			}
		}
	})
}

// FuzzSSERoundTrip 验证「渲染出去的一定能原样读回来」。
//
// 这条性质是 Bridge 转发流式响应的基础：任何一次渲染若产出了自己都解析不回
// 的字节，客户端拿到的就是一份被悄悄改写过的流。
func FuzzSSERoundTrip(f *testing.F) {
	f.Add("message", "hello", "e1")
	f.Add("", "", "")
	f.Add("a\tb", "多行\n内容", "id-1")
	f.Add("x", "含\r回车", "")
	f.Add("y", ": 冒号开头", "")
	f.Add("z", strings.Repeat("长", 100), "")

	f.Fuzz(func(t *testing.T, evType, data, id string) {
		ev := Event{Type: evType, Data: []byte(data), ID: id}

		var buf bytes.Buffer
		err := NewSSEEncoder(&buf).Write(ev)
		if err != nil {
			// 渲染器只在字段含换行或 NUL 时报错，那是有意的防护：
			// 前者会伪造出额外的字段行，后者会被接收端按规范忽略。
			if !strings.ContainsAny(evType, "\r\n") && !strings.ContainsAny(id, "\r\n\x00") {
				t.Fatalf("不含换行或 NUL 的字段却渲染失败：%v", err)
			}
			return
		}
		if buf.Len() == 0 {
			// 完全空的事件不写出，这是有意的。
			return
		}

		d := NewSSEDecoder(bytes.NewReader(buf.Bytes()), SSEDecoderOptions{})
		got, err := d.Next()
		if err != nil {
			t.Fatalf("渲染出的字节自己解析不回来：%v\n渲染结果：%q", err, buf.String())
		}

		if got.Type != evType {
			t.Errorf("事件类型往返后为 %q，期望 %q", got.Type, evType)
		}
		if got.ID != id {
			t.Errorf("事件 id 往返后为 %q，期望 %q", got.ID, id)
		}
		// data 里的 \r 会被规范化成 \n：SSE 的行终止符无法在 data 中表达，
		// 这是协议本身的限制，不是实现缺陷。
		wantData := strings.ReplaceAll(data, "\r\n", "\n")
		wantData = strings.ReplaceAll(wantData, "\r", "\n")
		if string(got.Data) != wantData {
			t.Errorf("data 往返后为 %q，期望 %q", got.Data, wantData)
		}
	})
}
