package parser

import (
	"strings"
	"testing"

	"github.com/kw-decade/Texvoke/internal/vproto"
)

func testNonce(t *testing.T) vproto.Nonce {
	t.Helper()
	n, err := vproto.NonceFromValue(strings.Repeat("ab", 16), "sess-1", "req-1")
	if err != nil {
		t.Fatalf("构造 nonce 失败：%v", err)
	}
	return n
}

func newParser(t *testing.T) *Parser {
	t.Helper()
	p, err := New(testNonce(t), Limits{})
	if err != nil {
		t.Fatalf("创建解析器失败：%v", err)
	}
	return p
}

// feedChunks 按给定大小把输入切碎喂进解析器，返回提交的文本与最终结果。
func feedChunks(t *testing.T, p *Parser, input string, size int) (string, Result) {
	t.Helper()
	var committed strings.Builder
	data := []byte(input)
	for i := 0; i < len(data); i += size {
		end := i + size
		if end > len(data) {
			end = len(data)
		}
		out, err := p.Write(data[i:end])
		if err != nil {
			return committed.String(), p.Close()
		}
		committed.Write(out)
	}
	return committed.String(), p.Close()
}

func envelopeFor(t *testing.T, n vproto.Nonce, calls ...vproto.Call) string {
	t.Helper()
	s, err := vproto.RenderEnvelope(n, calls)
	if err != nil {
		t.Fatalf("渲染 envelope 失败：%v", err)
	}
	return s
}

// 同一份输入按任意边界切分，解析结果必须一致。这是规格十三章要求的
// 「每个 chunk 一个字符、随机拆分」，也是旧版实测撞过的坑：
// 上游 delta 约 8-9 个字符一块，触发字符串经常被切断。
func TestParseIsChunkBoundaryAgnostic(t *testing.T) {
	n := testNonce(t)
	input := "我先说明一下思路。\n" +
		envelopeFor(t, n, vproto.Call{
			ID: "c1", Tool: "fs/read_file", ArgumentsJSON: `{"path":"/tmp/a.txt"}`,
		})

	var want Result
	for _, size := range []int{1, 2, 3, 5, 7, 8, 13, 64, 4096} {
		t.Run("chunk="+itoa(size), func(t *testing.T) {
			p := newParser(t)
			committed, got := feedChunks(t, p, input, size)

			if got.Outcome != OutcomeCallsParsed {
				t.Fatalf("结局为 %q，期望解析出调用：%v", got.Outcome, got.Err)
			}
			if len(got.Calls) != 1 {
				t.Fatalf("调用数为 %d", len(got.Calls))
			}
			if got.Calls[0].Tool != "fs/read_file" {
				t.Errorf("工具名为 %q", got.Calls[0].Tool)
			}
			if got.Calls[0].ArgumentsJSON != `{"path":"/tmp/a.txt"}` {
				t.Errorf("参数为 %q", got.Calls[0].ArgumentsJSON)
			}
			// 信号之前的文本要原样提交，信号之后的协议内容一个字都不能漏出去。
			if !strings.Contains(committed, "我先说明一下思路") {
				t.Errorf("信号前的文本未提交：%q", committed)
			}
			if strings.Contains(committed, n.Signal()) {
				t.Errorf("信号本身被当成普通文本提交了：%q", committed)
			}
			for _, leak := range []string{"tool_call_envelope", "CDATA", "fs/read_file"} {
				if strings.Contains(committed, leak) {
					t.Errorf("协议内容 %q 泄漏进了普通文本：%q", leak, committed)
				}
			}

			if want.Outcome == "" {
				want = got
				return
			}
			if got.Text != want.Text || len(got.Calls) != len(want.Calls) {
				t.Errorf("与 chunk=1 的结果不同\n实际：%+v\n期望：%+v", got, want)
			}
		})
	}
}

// 提交边界：一旦确定这半行不可能是信号，就该立即提交，不必等换行。
// 但只要还有可能，就一个字节都不能放出去——发到客户端屏幕上的字符收不回来。
func TestCommitBoundary(t *testing.T) {
	n := testNonce(t)
	sig := n.Signal()

	t.Run("确定不是信号就立即提交", func(t *testing.T) {
		p := newParser(t)
		// 第一个字节就与信号前缀不符。
		out, err := p.Write([]byte("你"))
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != "你" {
			t.Errorf("应立即提交，实际提交了 %q", out)
		}
	})

	t.Run("可能是信号时一个字节都不提交", func(t *testing.T) {
		p := newParser(t)
		// 逐字节喂入信号的前缀，每一步都不该有输出。
		for i := 0; i < len(sig)-1; i++ {
			out, err := p.Write([]byte{sig[i]})
			if err != nil {
				t.Fatal(err)
			}
			if len(out) > 0 {
				t.Fatalf("喂入信号前 %d 字节时提交了 %q——这些字节可能属于信号", i+1, out)
			}
		}
	})

	t.Run("前缀相同但后来分叉，提交整段", func(t *testing.T) {
		p := newParser(t)
		// "[[UTR-CALL:" 是信号前缀，但后面跟的不是十六进制。
		fake := "[[UTR-CALL:not-hex-at-all]] 这是普通文本"
		out, err := p.Write([]byte(fake))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out)+p.CommittedText(), "普通文本") {
			t.Errorf("分叉后应提交，实际提交了 %q", out)
		}
	})

	t.Run("待定窗口不超过信号长度加空白", func(t *testing.T) {
		p := newParser(t)
		// 喂入一长串空格加信号前缀。
		input := strings.Repeat(" ", 10) + sig[:20]
		if _, err := p.Write([]byte(input)); err != nil {
			t.Fatal(err)
		}
		// pending 里最多就是这些，不会无界增长。
		if len(p.buf) != len(input) {
			t.Errorf("待定窗口为 %d 字节，输入 %d 字节", len(p.buf), len(input))
		}
	})
}

// 信号可以出现在行内任意位置。实测模型约一半的调用是把信号直接接在正文
// 后面、不换行；要求它独占一行会让这些调用被判成普通文本，于是整段协议
// XML 原样吐给客户端——比误判严重得多。
//
// 换来的判据是「信号 + 其后必须是合法 envelope」：不合法就是 truncated 或
// malformed，模型单纯复述协议说明时不会紧跟一个结构完整的信封。
func TestSignalAnywhereInLine(t *testing.T) {
	n := testNonce(t)
	env := envelopeFor(t, n, vproto.Call{ID: "c1", Tool: "a/b", ArgumentsJSON: "{}"})
	body := env[strings.Index(env, "\n")+1:] // envelope 部分，不含信号行

	t.Run("信号接在正文后面也触发", func(t *testing.T) {
		p := newParser(t)
		input := "我来查一下。" + n.Signal() + "\n" + body
		committed, got := feedChunks(t, p, input, 7)
		if got.Outcome != OutcomeCallsParsed {
			t.Fatalf("结局为 %q：%v", got.Outcome, got.Err)
		}
		if len(got.Calls) != 1 {
			t.Fatalf("应解析出 1 个调用，得到 %d", len(got.Calls))
		}
		// 信号之前的正文要留住。
		if !strings.Contains(committed, "我来查一下。") {
			t.Errorf("信号前的正文丢了：%q", committed)
		}
		// 信号与其后的结构一个字节都不该漏给客户端。
		if strings.Contains(committed, n.Signal()) {
			t.Errorf("信号泄漏进正文：%q", committed)
		}
		if strings.Contains(committed, "tool_call_envelope") {
			t.Errorf("envelope 泄漏进正文：%q", committed)
		}
	})

	t.Run("信号与 envelope 同处一行不换行", func(t *testing.T) {
		p := newParser(t)
		// 最坏情况：整段输出一个换行都没有。
		p2 := strings.ReplaceAll(body, "\n", "")
		input := "好的" + n.Signal() + p2
		committed, got := feedChunks(t, p, input, 3)
		if got.Outcome != OutcomeCallsParsed {
			t.Fatalf("结局为 %q：%v", got.Outcome, got.Err)
		}
		if strings.Contains(committed, n.Signal()) || strings.Contains(committed, "tool_call_envelope") {
			t.Errorf("协议内容泄漏：%q", committed)
		}
	})

	t.Run("信号独占一行照旧触发", func(t *testing.T) {
		p := newParser(t)
		_, got := feedChunks(t, p, env, 7)
		if got.Outcome != OutcomeCallsParsed {
			t.Errorf("结局为 %q：%v", got.Outcome, got.Err)
		}
	})

	t.Run("信号行两侧的空白允许", func(t *testing.T) {
		p := newParser(t)
		input := "   " + n.Signal() + "  \n" + body
		_, got := feedChunks(t, p, input, 5)
		if got.Outcome != OutcomeCallsParsed {
			t.Errorf("结局为 %q：%v", got.Outcome, got.Err)
		}
	})

	t.Run("信号后没有合法结构则不是调用", func(t *testing.T) {
		p := newParser(t)
		input := "要调用工具就输出 " + n.Signal() + " 这一行\n然后正常结束。\n"
		committed, got := feedChunks(t, p, input, 7)
		if len(got.Calls) != 0 {
			t.Errorf("不应解析出调用：%+v", got.Calls)
		}
		// 结局不是 plain_text——信号确实出现了，装作没看见会让问题查不出来。
		if got.Outcome == OutcomeCallsParsed {
			t.Errorf("结局为 %q，不该算成功解析", got.Outcome)
		}
		// 但信号之前的正文必须留住。
		if !strings.Contains(committed, "要调用工具就输出") {
			t.Errorf("信号前的正文丢了：%q", committed)
		}
	})
}

// think 区域内不检测信号。模型在思考里复述协议格式是常见的，
// 把那当成发起调用会凭空造出一个模型并未打算执行的工具调用。
func TestThinkRegionDoesNotTrigger(t *testing.T) {
	n := testNonce(t)

	p := newParser(t)
	input := "<think>\n我应该输出 " + n.Signal() + "\n然后写 envelope。\n</think>\n最终答案是 42。\n"
	committed, got := feedChunks(t, p, input, 3)

	if got.Outcome != OutcomePlainText {
		t.Errorf("结局为 %q，期望普通文本：%v", got.Outcome, got.Err)
	}
	if len(got.Calls) != 0 {
		t.Errorf("think 区内的信号不应触发调用：%+v", got.Calls)
	}
	// 思维内容原样透传，不改写也不吞掉——规格九章要求不得为了解析
	// 而泄漏或伪造隐藏思维内容，原样搬运是唯一不做手脚的选择。
	if !strings.Contains(committed, "<think>") || !strings.Contains(committed, "</think>") {
		t.Errorf("think 标签被吞掉了：%q", committed)
	}
	if !strings.Contains(committed, "最终答案是 42") {
		t.Errorf("think 之后的正文丢失：%q", committed)
	}
}

// think 结束之后信号恢复生效。
func TestSignalWorksAfterThink(t *testing.T) {
	n := testNonce(t)
	env := envelopeFor(t, n, vproto.Call{ID: "c1", Tool: "a/b", ArgumentsJSON: "{}"})

	p := newParser(t)
	input := "<think>先想想</think>\n" + env
	_, got := feedChunks(t, p, input, 4)

	if got.Outcome != OutcomeCallsParsed {
		t.Errorf("结局为 %q：%v", got.Outcome, got.Err)
	}
}

// 四种结局必须能区分。混为一谈会让「模型正常回答」和「模型的调用被截断」
// 得到同样的处理。
func TestOutcomeClassification(t *testing.T) {
	n := testNonce(t)

	t.Run("普通文本", func(t *testing.T) {
		p := newParser(t)
		_, got := feedChunks(t, p, "旧金山今天 18 度。\n", 3)
		if got.Outcome != OutcomePlainText {
			t.Errorf("结局为 %q", got.Outcome)
		}
		if got.Err != nil {
			t.Errorf("不该有错误：%v", got.Err)
		}
	})

	t.Run("解析出调用", func(t *testing.T) {
		p := newParser(t)
		_, got := feedChunks(t, p, envelopeFor(t, n, vproto.Call{
			ID: "c1", Tool: "a/b", ArgumentsJSON: "{}",
		}), 3)
		if got.Outcome != OutcomeCallsParsed {
			t.Errorf("结局为 %q：%v", got.Outcome, got.Err)
		}
	})

	t.Run("信号后结构截断", func(t *testing.T) {
		p := newParser(t)
		full := envelopeFor(t, n, vproto.Call{ID: "c1", Tool: "a/b", ArgumentsJSON: "{}"})
		// 砍掉闭合标签。
		truncated := full[:len(full)-len(vproto.TagEnvelopeClose)-5]
		_, got := feedChunks(t, p, truncated, 3)
		if got.Outcome != OutcomeTruncated {
			t.Errorf("结局为 %q，期望截断", got.Outcome)
		}
		if got.Err == nil {
			t.Error("截断必须带错误说明")
		}
	})

	t.Run("信号后结构非法", func(t *testing.T) {
		p := newParser(t)
		input := n.Signal() + "\n<tool_call_envelope version=\"1\">\n  <这不是合法标签>\n</tool_call_envelope>"
		_, got := feedChunks(t, p, input, 5)
		if got.Outcome != OutcomeMalformed {
			t.Errorf("结局为 %q，期望非法", got.Outcome)
		}
	})
}

// 重复信号按歧义拒绝，不取「最后一个」。两个信号意味着模型给出了
// 两套互相矛盾的调用，选任何一个都是替调用方做了它没授权的决定。
func TestDuplicateSignalIsRejected(t *testing.T) {
	n := testNonce(t)
	p := newParser(t)

	// 第一个信号进入 envelope 状态；在 envelope 内再出现信号会被当成内容，
	// 所以这里构造的是「envelope 闭合后又来一个信号」。
	env := envelopeFor(t, n, vproto.Call{ID: "c1", Tool: "a/b", ArgumentsJSON: "{}"})
	input := env + "\n" + n.Signal() + "\n"

	_, got := feedChunks(t, p, input, 5)
	if got.Err == nil {
		t.Fatal("闭合后再出现信号必须报错")
	}
	if !strings.Contains(got.Err.Error(), "闭合后") {
		t.Errorf("错误信息 %q 未说明原因", got.Err.Error())
	}
}

// envelope 闭合后不该再有输出，但纯文本模型经常做不到。
//
// 一开始这里是报错。实测后改掉了：模型最常见的两种收尾——补一句
// 「我已经调用了工具」，或者把整段 envelope 包在 markdown 代码块里
// 于是末尾多一行围栏——都会让一个**已经完整解析出来**的调用被判失败，
// agent 卡在一句无害的废话上。
//
// 现在容忍它，但不静默：内容记进 Result.Trailing，也不会并进要转发给
// 客户端的正文。
func TestOutputAfterEnvelopeCloseIsTolerated(t *testing.T) {
	n := testNonce(t)
	env := envelopeFor(t, n, vproto.Call{ID: "c1", Tool: "a/b", ArgumentsJSON: "{}"})

	t.Run("闭合后有正文", func(t *testing.T) {
		_, got := feedChunks(t, newParser(t), env+"\n还有话说\n", 5)
		if got.Outcome != OutcomeCallsParsed {
			t.Fatalf("结局为 %q：%v", got.Outcome, got.Err)
		}
		if len(got.Calls) != 1 {
			t.Fatalf("调用数为 %d", len(got.Calls))
		}
		// 违规要可见。
		if !strings.Contains(got.Trailing, "还有话说") {
			t.Errorf("尾部内容没记下来：%q", got.Trailing)
		}
		// 但不能混进要发给客户端的正文——那些多半是协议相关的自言自语。
		if strings.Contains(got.Text, "还有话说") {
			t.Errorf("尾部内容混进了正文：%q", got.Text)
		}
	})

	t.Run("markdown 围栏收尾", func(t *testing.T) {
		_, got := feedChunks(t, newParser(t), "```xml\n"+env+"\n```", 3)
		if got.Outcome != OutcomeCallsParsed || len(got.Calls) != 1 {
			t.Fatalf("被 markdown 包住的调用应当仍能解析：%q %v", got.Outcome, got.Err)
		}
	})

	t.Run("闭合后只有空白是允许的", func(t *testing.T) {
		_, got := feedChunks(t, newParser(t), env+"\n\n  \n", 5)
		if got.Outcome != OutcomeCallsParsed {
			t.Errorf("结局为 %q：%v", got.Outcome, got.Err)
		}
		if got.Trailing != "" {
			t.Errorf("纯空白不该记成违规：%q", got.Trailing)
		}
	})

	t.Run("闭合后又出现信号仍然拒绝", func(t *testing.T) {
		// 那不是收尾的废话，是模型想发起第二次调用。两个调用意图摆在
		// 一起，取哪个都是替调用方做了它没授权的决定。
		_, got := feedChunks(t, newParser(t), env+"\n"+n.Signal()+"\n", 5)
		if got.Err == nil {
			t.Fatal("闭合后再出现信号必须报错")
		}
	})
}

func TestMultipleCalls(t *testing.T) {
	n := testNonce(t)
	env := envelopeFor(t, n,
		vproto.Call{ID: "c1", Tool: "fs/read", ArgumentsJSON: `{"p":"/a"}`},
		vproto.Call{ID: "c2", Tool: "fs/read", ArgumentsJSON: `{"p":"/b"}`},
		vproto.Call{ID: "c3", Tool: "net/get", ArgumentsJSON: `{"u":"x"}`},
	)

	p := newParser(t)
	_, got := feedChunks(t, p, env, 6)
	if got.Outcome != OutcomeCallsParsed {
		t.Fatalf("结局为 %q：%v", got.Outcome, got.Err)
	}
	if len(got.Calls) != 3 {
		t.Fatalf("调用数为 %d，期望 3", len(got.Calls))
	}
	for i, want := range []string{"c1", "c2", "c3"} {
		if got.Calls[i].ID != want {
			t.Errorf("第 %d 个调用 id 为 %q，期望 %q", i, got.Calls[i].ID, want)
		}
	}
}

// CDATA 里含 ]]> 是规格十三章的必测项。
func TestCDATAWithCloseMarker(t *testing.T) {
	n := testNonce(t)
	args := `{"text":"这里有 ]]> 结束标记"}`
	env := envelopeFor(t, n, vproto.Call{ID: "c1", Tool: "a/b", ArgumentsJSON: args})

	p := newParser(t)
	_, got := feedChunks(t, p, env, 3)
	if got.Outcome != OutcomeCallsParsed {
		t.Fatalf("结局为 %q：%v", got.Outcome, got.Err)
	}
	if got.Calls[0].ArgumentsJSON != args {
		t.Errorf("参数为 %q\n期望 %q", got.Calls[0].ArgumentsJSON, args)
	}
}

func TestUnicodeArguments(t *testing.T) {
	n := testNonce(t)
	args := `{"文本":"中文与 emoji 🎉","嵌套":{"键":["值1","值2"]}}`
	env := envelopeFor(t, n, vproto.Call{ID: "c1", Tool: "a/b", ArgumentsJSON: args})

	// 逐字节喂入，多字节字符会被劈开。
	p := newParser(t)
	_, got := feedChunks(t, p, env, 1)
	if got.Outcome != OutcomeCallsParsed {
		t.Fatalf("结局为 %q：%v", got.Outcome, got.Err)
	}
	if got.Calls[0].ArgumentsJSON != args {
		t.Errorf("参数为 %q\n期望 %q", got.Calls[0].ArgumentsJSON, args)
	}
}

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
