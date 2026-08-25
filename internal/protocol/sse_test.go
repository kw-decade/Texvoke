package protocol

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// chunkedReader 按固定大小切分数据，用来验证解析器不假设读取边界。
//
// 这是这组测试的核心手法：真实上游的 delta 会以任意边界到达（旧版实测约
// 8-9 个字符一块），触发字符串和 JSON 经常被劈成两半。用 size=1 就是最极端
// 的情形——每个字节单独到达，包括被劈开的多字节 UTF-8 字符。
type chunkedReader struct {
	data []byte
	pos  int
	size int
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := r.size
	if n > len(p) {
		n = len(p)
	}
	if r.pos+n > len(r.data) {
		n = len(r.data) - r.pos
	}
	copy(p, r.data[r.pos:r.pos+n])
	r.pos += n
	return n, nil
}

func collectEvents(t *testing.T, r io.Reader) []Event {
	t.Helper()
	d := NewSSEDecoder(r, SSEDecoderOptions{})
	var out []Event
	for {
		ev, err := d.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("解析失败：%v（已解析 %d 个事件）", err, len(out))
		}
		out = append(out, *ev)
	}
}

// 同一份数据按各种边界切分，解析结果必须完全一致。
// 这条测试直接对应规格十三章要求的「每个 chunk 一个字符、随机拆分 SSE」。
func TestSSEDecodeIsChunkBoundaryAgnostic(t *testing.T) {
	stream := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\"}}\n" +
		"\n" +
		"event: content_block_delta\n" +
		"data: {\"delta\":{\"text\":\"你好世界\"}}\n" +
		"\n" +
		"event: message_stop\n" +
		"data: {}\n" +
		"\n"

	var want []Event
	for _, size := range []int{1, 2, 3, 5, 7, 8, 13, 64, 4096} {
		t.Run(strings.Repeat("", 0)+"chunk="+itoa(size), func(t *testing.T) {
			got := collectEvents(t, &chunkedReader{data: []byte(stream), size: size})
			if len(got) != 3 {
				t.Fatalf("事件数为 %d，期望 3", len(got))
			}
			if want == nil {
				want = got
				return
			}
			for i := range got {
				if got[i].Type != want[i].Type || !bytes.Equal(got[i].Data, want[i].Data) {
					t.Errorf("第 %d 个事件与 chunk=1 时不同\n实际：%+v\n期望：%+v", i, got[i], want[i])
				}
			}
		})
	}
}

// 多字节字符被劈开时不能出错或产生替换字符。
// 行边界只看 \r 与 \n 这两个 ASCII 字节，UTF-8 的后续字节不会与它们冲突，
// 但只有逐字节喂进去才能证明实现真的没做「一次读取 = 一行」的假设。
func TestSSEDecodeSplitUTF8(t *testing.T) {
	text := "中文内容与 emoji 🎉 混排"
	stream := "data: " + text + "\n\n"

	for _, size := range []int{1, 2, 3} {
		got := collectEvents(t, &chunkedReader{data: []byte(stream), size: size})
		if len(got) != 1 {
			t.Fatalf("chunk=%d：事件数为 %d，期望 1", size, len(got))
		}
		if string(got[0].Data) != text {
			t.Errorf("chunk=%d：内容为 %q，期望 %q", size, got[0].Data, text)
		}
	}
}

// CRLF、LF、裸 CR 三种行尾都要认。bufio.ScanLines 只认 \n，
// 上游用裸 \r 分行时会把整个流当成一行，最终撞上行长上限。
func TestSSEDecodeLineEndings(t *testing.T) {
	tests := []struct {
		name   string
		stream string
	}{
		{"LF", "event: a\ndata: x\n\n"},
		{"CRLF", "event: a\r\ndata: x\r\n\r\n"},
		{"CR", "event: a\rdata: x\r\r"},
		{"混用", "event: a\r\ndata: x\n\r"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// 逐字节喂：CRLF 被劈在 \r 与 \n 之间是最容易出错的情形，
			// 处理不当会把一个 CRLF 当成两个空行，凭空多切出一个事件。
			for _, size := range []int{1, 2, 4096} {
				got := collectEvents(t, &chunkedReader{data: []byte(tc.stream), size: size})
				if len(got) != 1 {
					t.Fatalf("chunk=%d：事件数为 %d，期望 1：%+v", size, len(got), got)
				}
				if got[0].Type != "a" || string(got[0].Data) != "x" {
					t.Errorf("chunk=%d：解析为 %+v", size, got[0])
				}
			}
		})
	}
}

// 没有 event 行的事件是合法的（Chat Completions 的流就不带 event 行）。
// 这里保持空串而不是补上规范的默认值 "message"，是为了让上层能区分
// 「上游显式写了 message」与「上游根本没写 event 行」——后者可能意味着
// 中间的代理剥掉了字段。
func TestSSEDecodeWithoutEventLine(t *testing.T) {
	stream := "data: {\"a\":1}\n\ndata: [DONE]\n\n"
	got := collectEvents(t, strings.NewReader(stream))

	if len(got) != 2 {
		t.Fatalf("事件数为 %d，期望 2", len(got))
	}
	for i, ev := range got {
		if ev.Type != "" {
			t.Errorf("第 %d 个事件的类型为 %q，期望空串", i, ev.Type)
		}
	}
	if string(got[1].Data) != "[DONE]" {
		t.Errorf("终止标记为 %q", got[1].Data)
	}
}

// 注释行是心跳。丢掉它，「连接还活着」与「上游卡住了」就分不开了。
func TestSSEDecodeComments(t *testing.T) {
	stream := ": ping\n\n" + "event: real\ndata: x\n\n" + ":\n\n"
	got := collectEvents(t, strings.NewReader(stream))

	if len(got) != 3 {
		t.Fatalf("事件数为 %d，期望 3（含两个心跳）", len(got))
	}
	if !got[0].IsComment() || got[0].Comment != "ping" {
		t.Errorf("第一个事件应是注释心跳：%+v", got[0])
	}
	if got[1].IsComment() {
		t.Error("带字段的事件不应被判为注释")
	}
	// 空注释（只有一个冒号）也是合法心跳。
	if got[2].Comment != "" || got[2].Type != "" {
		t.Errorf("空注释解析为 %+v", got[2])
	}
}

// 多行 data 用 \n 连接，这是规范定义的行为。
func TestSSEDecodeMultilineData(t *testing.T) {
	stream := "data: 第一行\ndata: 第二行\ndata: 第三行\n\n"
	got := collectEvents(t, strings.NewReader(stream))

	if len(got) != 1 {
		t.Fatalf("事件数为 %d，期望 1", len(got))
	}
	if string(got[0].Data) != "第一行\n第二行\n第三行" {
		t.Errorf("多行 data 拼接为 %q", got[0].Data)
	}
}

func TestSSEDecodeFieldParsing(t *testing.T) {
	tests := []struct {
		name   string
		stream string
		check  func(*testing.T, Event)
	}{
		{
			"值前只去掉一个空格",
			"data:  两个空格开头\n\n",
			func(t *testing.T, ev Event) {
				if string(ev.Data) != " 两个空格开头" {
					t.Errorf("data 为 %q，应只去掉一个前导空格", ev.Data)
				}
			},
		},
		{
			"没有空格也能解析",
			"data:x\n\n",
			func(t *testing.T, ev Event) {
				if string(ev.Data) != "x" {
					t.Errorf("data 为 %q", ev.Data)
				}
			},
		},
		{
			"值里含冒号",
			"data: {\"url\":\"https://example.com\"}\n\n",
			func(t *testing.T, ev Event) {
				if !strings.Contains(string(ev.Data), "https://example.com") {
					t.Errorf("只应在第一个冒号处切分，实际 data 为 %q", ev.Data)
				}
			},
		},
		{
			"没有冒号的行整行是字段名",
			"data\n\n",
			func(t *testing.T, ev Event) {
				// data 字段值为空，但 dataWasSet 为真，所以 Data 是空切片而非 nil。
				if ev.Data == nil || len(ev.Data) != 0 {
					t.Errorf("data 应为空值，实际 %q（nil=%v）", ev.Data, ev.Data == nil)
				}
			},
		},
		{
			"id 与 retry",
			"id: evt-7\nretry: 3000\ndata: x\n\n",
			func(t *testing.T, ev Event) {
				if ev.ID != "evt-7" || ev.Retry != 3000 {
					t.Errorf("id/retry 解析为 %q/%d", ev.ID, ev.Retry)
				}
			},
		},
		{
			"非法 retry 按规范忽略",
			"retry: 不是数字\ndata: x\n\n",
			func(t *testing.T, ev Event) {
				if ev.Retry != 0 {
					t.Errorf("retry 应保持 0，实际 %d", ev.Retry)
				}
			},
		},
		{
			"未知字段忽略而不报错",
			"unknown: whatever\nvendor_ext: 1\ndata: x\n\n",
			func(t *testing.T, ev Event) {
				if string(ev.Data) != "x" {
					t.Errorf("未知字段影响了解析：%+v", ev)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := collectEvents(t, strings.NewReader(tc.stream))
			if len(got) != 1 {
				t.Fatalf("事件数为 %d，期望 1", len(got))
			}
			tc.check(t, got[0])
		})
	}
}

func TestSSEDecodeEmptyAndBlankLines(t *testing.T) {
	t.Run("空流", func(t *testing.T) {
		if got := collectEvents(t, strings.NewReader("")); len(got) != 0 {
			t.Errorf("空流不应产出事件，实际 %d 个", len(got))
		}
	})
	t.Run("只有空行", func(t *testing.T) {
		if got := collectEvents(t, strings.NewReader("\n\n\n\n")); len(got) != 0 {
			t.Errorf("连续空行不应产出空事件，实际 %d 个", len(got))
		}
	})
	t.Run("事件间的多余空行", func(t *testing.T) {
		got := collectEvents(t, strings.NewReader("data: a\n\n\n\ndata: b\n\n"))
		if len(got) != 2 {
			t.Fatalf("事件数为 %d，期望 2", len(got))
		}
	})
}

// 流断在事件中途与正常结束是两回事，必须能区分。规格九章要求流结束时
// 能分辨「普通文本完成」与「信号后结构截断」。
func TestSSEDecodeTruncatedStream(t *testing.T) {
	// 最后一个事件缺少结尾空行。
	d := NewSSEDecoder(strings.NewReader("data: a\n\nevent: b\ndata: partial\n"), SSEDecoderOptions{})

	first, err := d.Next()
	if err != nil {
		t.Fatalf("第一个事件应正常解析：%v", err)
	}
	if string(first.Data) != "a" {
		t.Errorf("第一个事件为 %+v", first)
	}

	_, err = d.Next()
	if err == nil {
		t.Fatal("截断的流必须报错，不能当成正常结束")
	}
	if errors.Is(err, io.EOF) {
		t.Error("截断不应报成 io.EOF——那会让上层以为流正常走完了")
	}
	if !strings.Contains(err.Error(), "不完整") {
		t.Errorf("错误信息 %q 未说明是截断", err.Error())
	}
}

// 没有上限的行与事件是内存耗尽路径。
func TestSSEDecodeLimits(t *testing.T) {
	t.Run("单行超限", func(t *testing.T) {
		stream := "data: " + strings.Repeat("x", 1000) + "\n\n"
		d := NewSSEDecoder(strings.NewReader(stream), SSEDecoderOptions{MaxLineBytes: 100})
		if _, err := d.Next(); err == nil {
			t.Fatal("超长行必须被拒绝")
		} else if !strings.Contains(err.Error(), "长度上限") {
			t.Errorf("错误信息 %q 未说明是行超限", err.Error())
		}
	})

	t.Run("事件累积超限", func(t *testing.T) {
		// 每行都不超限，但多行 data 累积起来超限。
		var b strings.Builder
		for i := 0; i < 50; i++ {
			b.WriteString("data: " + strings.Repeat("y", 40) + "\n")
		}
		b.WriteString("\n")

		d := NewSSEDecoder(strings.NewReader(b.String()), SSEDecoderOptions{
			MaxLineBytes:  1000,
			MaxEventBytes: 500,
		})
		if _, err := d.Next(); err == nil {
			t.Fatal("累积超限的事件必须被拒绝")
		} else if !strings.Contains(err.Error(), "超过上限") {
			t.Errorf("错误信息 %q 未说明是事件超限", err.Error())
		}
	})
}

func TestSSEEncode(t *testing.T) {
	tests := []struct {
		name  string
		ev    Event
		want  string
		isErr bool
	}{
		{
			name: "完整事件",
			ev:   Event{Type: "message", Data: []byte(`{"a":1}`), ID: "e1"},
			want: "event: message\nid: e1\ndata: {\"a\":1}\n\n",
		},
		{
			name: "只有 data",
			ev:   Event{Data: []byte("x")},
			want: "data: x\n\n",
		},
		{
			name: "注释心跳",
			ev:   Event{Comment: "ping"},
			want: ": ping\n\n",
		},
		{
			name: "retry",
			ev:   Event{Data: []byte("x"), Retry: 5000},
			want: "retry: 5000\ndata: x\n\n",
		},
		{
			// data 里的换行必须拆成多个 data 行，否则接收端会把换行后的
			// 内容当成新字段，JSON 就断成了两截。
			name: "多行 data",
			ev:   Event{Data: []byte("第一行\n第二行")},
			want: "data: 第一行\ndata: 第二行\n\n",
		},
		{
			// data 里的 \r 同样会被接收端当成行尾。
			name: "data 含 CR",
			ev:   Event{Data: []byte("a\r\nb\rc")},
			want: "data: a\ndata: b\ndata: c\n\n",
		},
		{
			name: "空数据",
			ev:   Event{Data: []byte("")},
			want: "data: \n\n",
		},
		{
			name: "完全空的事件不写出",
			ev:   Event{},
			want: "",
		},
		{
			name:  "事件类型含换行",
			ev:    Event{Type: "a\nb", Data: []byte("x")},
			isErr: true,
		},
		{
			name:  "id 含换行",
			ev:    Event{ID: "a\nb", Data: []byte("x")},
			isErr: true,
		},
		{
			// 由 FuzzSSERoundTrip 发现：解析器按规范忽略含 NUL 的 id，
			// 而渲染器原本照写不误——写出去的字节自己读不回来。
			name:  "id 含 NUL",
			ev:    Event{ID: "a\x00b", Data: []byte("x")},
			isErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := NewSSEEncoder(&buf).Write(tc.ev)
			if tc.isErr {
				if err == nil {
					t.Fatal("必须报错——换行会伪造出额外的字段行")
				}
				return
			}
			if err != nil {
				t.Fatalf("渲染失败：%v", err)
			}
			if buf.String() != tc.want {
				t.Errorf("渲染为 %q，期望 %q", buf.String(), tc.want)
			}
		})
	}
}

// 渲染再解析必须回到原样，这是 Bridge 转发流式响应的基本要求。
func TestSSERoundTrip(t *testing.T) {
	events := []Event{
		{Type: "message_start", Data: []byte(`{"type":"message_start"}`), ID: "1"},
		{Comment: "ping"},
		{Type: "content_block_delta", Data: []byte(`{"text":"多行\n内容"}`)},
		{Data: []byte("[DONE]")},
	}

	var buf bytes.Buffer
	enc := NewSSEEncoder(&buf)
	for _, ev := range events {
		if err := enc.Write(ev); err != nil {
			t.Fatalf("渲染失败：%v", err)
		}
	}

	// 逐字节读回，顺便验证往返也不依赖读取边界。
	got := collectEvents(t, &chunkedReader{data: buf.Bytes(), size: 1})
	if len(got) != len(events) {
		t.Fatalf("事件数为 %d，期望 %d：%+v", len(got), len(events), got)
	}
	for i := range events {
		if got[i].Type != events[i].Type {
			t.Errorf("第 %d 个事件类型为 %q，期望 %q", i, got[i].Type, events[i].Type)
		}
		if string(got[i].Data) != string(events[i].Data) {
			t.Errorf("第 %d 个事件数据为 %q，期望 %q", i, got[i].Data, events[i].Data)
		}
		if got[i].ID != events[i].ID {
			t.Errorf("第 %d 个事件 id 为 %q，期望 %q", i, got[i].ID, events[i].ID)
		}
		if got[i].Comment != events[i].Comment {
			t.Errorf("第 %d 个事件注释为 %q，期望 %q", i, got[i].Comment, events[i].Comment)
		}
	}
}

// itoa 避免为了拼测试名而引入 strconv。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
