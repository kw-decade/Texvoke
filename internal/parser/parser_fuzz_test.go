package parser

import (
	"strings"
	"testing"

	"github.com/kw-decade/Texvoke/internal/vproto"
)

// FuzzParse 对虚拟协议解析器做模糊测试。
//
// 这是本项目最需要 fuzz 的地方：解析器直接吃模型的输出，而模型的输出是
// 不可信输入的典型——它可能被 prompt injection 影响，可能被中间的代理改写，
// 也可能只是模型在胡言乱语。
//
// 断言的是性质而非具体输出：
//
//  1. 永不 panic；
//  2. 永不无界增长；
//  3. 解析失败时**绝不产出可用的调用**——这是规格三章那条硬要求的
//     模糊测试版本：不允许把解析失败的内容塞进兜底字段后继续执行；
//  4. 提交为普通文本的字节里，绝不含信号或协议结构。
//
// 跑法：go test -run=Fuzz -fuzz=FuzzParse -fuzztime=60s ./internal/parser/
func FuzzParse(f *testing.F) {
	n, err := vproto.NonceFromValue(strings.Repeat("ab", 16), "s", "r")
	if err != nil {
		f.Fatal(err)
	}
	sig := n.Signal()

	seeds := []string{
		"",
		"普通回答。",
		sig,
		sig + "\n",
		sig + "\n<tool_call_envelope version=\"1\"></tool_call_envelope>",
		sig + "\n<tool_call_envelope version=\"1\"><call id=\"c1\"><tool>a/b</tool><arguments_json><![CDATA[{}]]></arguments_json></call></tool_call_envelope>",
		"前面有话\n" + sig + "\n<tool_call_envelope version=\"1\"><call id=\"c1\"><tool>a/b</tool><arguments_json>{\"k\":\"v\"}</arguments_json></call></tool_call_envelope>",
		"<think>" + sig + "</think>",
		"<think>没闭合的思考 " + sig,
		sig + "\n" + sig,
		sig + "\n<tool_call_envelope><call><tool></tool></call></tool_call_envelope>",
		sig + "\n<tool_call_envelope version=\"1\"><call id=\"a\"><tool>x/y</tool><arguments_json>]]>",
		"  " + sig + "  \n<tool_call_envelope version=\"1\"></tool_call_envelope>",
		"[[UTR-CALL:0000000000000000000000000000000f]]\n",
		"[[UTR-CALL:",
		"[[UTR",
		strings.Repeat("a", 500),
		"a\rb\r\nc\nd",
		"{\"json\":\"但不是协议\"}",
		sig + "\n<tool_call_envelope version=\"1\"><call id=\"c1\"><tool>a/b</tool><arguments_text><![CDATA[console.log(\"hi\")]]></arguments_text></call></tool_call_envelope>",
		sig + "\n<tool_call_envelope version=\"1\"><call id=\"c1\"><tool>a/b</tool><arguments_text><![CDATA[]]></arguments_text></call></tool_call_envelope>",
		sig + "\n<tool_call_envelope version=\"1\"><call id=\"c1\"><tool>a/b</tool><arguments_text>x</arguments_text><arguments_json>{}</arguments_json></call></tool_call_envelope>",
	}
	for _, s := range seeds {
		f.Add([]byte(s), 1)
	}

	f.Fuzz(func(t *testing.T, data []byte, chunkSize int) {
		// 把切分粒度也纳入模糊范围：解析器不该依赖任何读取边界。
		if chunkSize < 1 {
			chunkSize = 1
		}
		if chunkSize > 64 {
			chunkSize = 64
		}

		limits := Limits{
			MaxLineBytes:     4096,
			MaxTotalBytes:    1 << 16,
			MaxEnvelopeBytes: 1 << 15,
			MaxCalls:         8,
			MaxArgumentBytes: 4096,
			MaxDepth:         6,
		}
		p, err := New(n, limits)
		if err != nil {
			t.Fatal(err)
		}

		var committed []byte
		for i := 0; i < len(data); i += chunkSize {
			end := i + chunkSize
			if end > len(data) {
				end = len(data)
			}
			out, err := p.Write(data[i:end])
			committed = append(committed, out...)
			if err != nil {
				if err.Error() == "" {
					t.Fatal("返回了空的错误信息")
				}
				break
			}
			// 提交的字节数不可能超过喂入的字节数。超过说明有内容被重复发送，
			// 客户端会看到重复的文本。
			if len(committed) > len(data) {
				t.Fatalf("提交了 %d 字节，超过喂入的 %d 字节——有内容被重复发送",
					len(committed), len(data))
			}
		}

		res := p.Close()

		// 结局必须是四种之一。
		switch res.Outcome {
		case OutcomePlainText, OutcomeCallsParsed, OutcomeTruncated, OutcomeMalformed:
		default:
			t.Fatalf("未知结局 %q", res.Outcome)
		}

		// 只有解析成功才允许有调用。这是规格三章那条硬要求：
		// 不允许把解析失败的内容塞进兜底字段后继续执行。
		if res.Outcome != OutcomeCallsParsed && len(res.Calls) > 0 {
			t.Fatalf("结局为 %q 却产出了 %d 个调用", res.Outcome, len(res.Calls))
		}

		// 成功解析出的调用必须各项齐备且参数是合法 JSON 对象——
		// 半成品的提案流到下游，安全检查会基于残缺信息做判断。
		if res.Outcome == OutcomeCallsParsed {
			if len(res.Calls) == 0 {
				t.Fatal("结局为解析成功却没有调用")
			}
			seen := map[string]bool{}
			for _, c := range res.Calls {
				if c.ID == "" || c.Tool == "" {
					t.Fatalf("调用字段不全：%+v", c)
				}
				if seen[c.ID] {
					t.Fatalf("调用 id %q 重复", c.ID)
				}
				seen[c.ID] = true
				// 两种参数形态永远不能同时带值——那意味着解析器把两份互斥的
				// 参数都收下了，下游取哪一份都是猜。
				if c.ArgumentsJSON != "" && c.ArgumentsText != "" {
					t.Fatalf("调用 %q 同时带着两种形态的参数：%+v", c.ID, c)
				}
				if c.Freeform {
					// 裸文本不做 JSON 校验，但它绝不能反过来填进 JSON 槽位。
					if c.ArgumentsJSON != "" {
						t.Fatalf("裸文本调用 %q 却填了 arguments_json", c.ID)
					}
					continue
				}
				if c.ArgumentsJSON == "" {
					t.Fatalf("调用字段不全：%+v", c)
				}
				if err := validateArgumentsJSON(c.ArgumentsJSON, limits.MaxDepth); err != nil {
					t.Fatalf("成功解析的调用带着非法参数：%v", err)
				}
			}
			if len(res.Calls) > limits.MaxCalls {
				t.Fatalf("调用数 %d 超过上限 %d", len(res.Calls), limits.MaxCalls)
			}
		}

		// 独占一行的信号绝不能被当成普通文本提交——它一旦漏出去，
		// 客户端屏幕上会显示一串本该被消化掉的协议标记，且无法撤回。
		//
		// 注意断言的是「独占一行」而非「出现过」：模型在正文里顺带写出
		// 信号（前后还有别的字）是合法的普通文本，理应原样透传。
		for _, line := range strings.Split(string(committed), "\n") {
			if strings.TrimSpace(line) == sig {
				t.Fatalf("独占一行的信号被当成普通文本提交了：%q", committed)
			}
		}
		for _, line := range strings.Split(res.Text, "\n") {
			if strings.TrimSpace(line) == sig {
				t.Fatalf("独占一行的信号进了累积文本：%q", res.Text)
			}
		}
	})
}

// FuzzPendingSignalPrefix 单独模糊提交边界的判据。
//
// 它是整个解析器里最要紧的一个数：留少了，信号的前半截会被当成普通文本
// 发给客户端且无法撤回；留多了，普通文本会被无限期扣住不发。
func FuzzPendingSignalPrefix(f *testing.F) {
	n, err := vproto.NonceFromValue(strings.Repeat("cd", 16), "s", "r")
	if err != nil {
		f.Fatal(err)
	}
	sig := n.Signal()

	f.Add("")
	f.Add("[")
	f.Add(sig)
	f.Add(sig + " ")
	f.Add(sig + "x")
	f.Add("  " + sig)
	f.Add("正文" + sig)
	f.Add("正文" + sig[:10])
	f.Add("你好")
	f.Add(sig[:10])

	f.Fuzz(func(t *testing.T, s string) {
		got := pendingSignalPrefix([]byte(s), []byte(sig))

		if got < 0 || got > len(s) || got > len(sig) {
			t.Fatalf("%q 的待定长度 %d 越界（len(s)=%d len(sig)=%d）", s, got, len(s), len(sig))
		}

		// 性质一：留下的那段必须真的是信号的前缀。留一段不可能长成信号的
		// 字节，等于把已经确定安全的正文无限期扣住。
		if got > 0 && !strings.HasPrefix(sig, s[len(s)-got:]) {
			t.Fatalf("%q 留了 %d 字节，但 %q 不是信号的前缀", s, got, s[len(s)-got:])
		}

		// 性质二：必须取最长的那个匹配。留短了，信号的头部就会被当正文
		// 发出去——已经发到客户端屏幕上的字符收不回来。
		for n := len(sig); n > got; n-- {
			if n <= len(s) && strings.HasPrefix(sig, s[len(s)-n:]) {
				t.Fatalf("%q 有更长的候选后缀（%d 字节）却只留了 %d", s, n, got)
			}
		}

		// 性质三：完整的信号出现在末尾时，必须整个扣住。少扣一个字节，
		// 那个字节就漏出去了。
		if strings.HasSuffix(s, sig) && got != len(sig) {
			t.Fatalf("%q 以完整信号结尾，必须扣住全部 %d 字节，只扣了 %d", s, len(sig), got)
		}
	})
}
