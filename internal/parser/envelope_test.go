package parser

import (
	"strings"
	"testing"

	"github.com/kw-decade/Texvoke/internal/vproto"
)

// wrap 把一段 envelope 内容套上信号行，构造完整输入。
func wrap(n vproto.Nonce, body string) string {
	return n.Signal() + "\n" + body
}

func parseBody(t *testing.T, body string) Result {
	t.Helper()
	n := testNonce(t)
	p := newParser(t)
	_, got := feedChunks(t, p, wrap(n, body), 17)
	return got
}

// envelope 的拒绝清单。每一条都是攻击面或歧义来源——放行任何一条，
// 后果都是让一段没被正确理解的内容进入执行路径。
func TestEnvelopeRejects(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			"未知标签",
			`<tool_call_envelope version="1"><evil>x</evil></tool_call_envelope>`,
			"未知标签",
		},
		{
			"嵌套 envelope",
			`<tool_call_envelope version="1"><tool_call_envelope version="1"></tool_call_envelope></tool_call_envelope>`,
			"嵌套的 envelope",
		},
		{
			"嵌套 call",
			`<tool_call_envelope version="1"><call id="a"><call id="b"></call></call></tool_call_envelope>`,
			"嵌套的 call",
		},
		{
			"call 缺 id",
			`<tool_call_envelope version="1"><call><tool>a/b</tool><arguments_json>{}</arguments_json></call></tool_call_envelope>`,
			"缺少 id",
		},
		{
			// 重复的 call id 会让结果无法唯一关联回调用。
			"call id 重复",
			`<tool_call_envelope version="1">` +
				`<call id="x"><tool>a/b</tool><arguments_json>{}</arguments_json></call>` +
				`<call id="x"><tool>a/b</tool><arguments_json>{}</arguments_json></call>` +
				`</tool_call_envelope>`,
			"重复",
		},
		{
			"call 缺工具名",
			`<tool_call_envelope version="1"><call id="x"><arguments_json>{}</arguments_json></call></tool_call_envelope>`,
			"缺少 tool",
		},
		{
			"call 缺参数",
			`<tool_call_envelope version="1"><call id="x"><tool>a/b</tool></call></tool_call_envelope>`,
			"缺少 arguments_json",
		},
		{
			"工具名为空",
			`<tool_call_envelope version="1"><call id="x"><tool>  </tool><arguments_json>{}</arguments_json></call></tool_call_envelope>`,
			"工具名为空",
		},
		{
			// 工具名逐字匹配，含空白说明模型改写了名字。
			"工具名含空白",
			`<tool_call_envelope version="1"><call id="x"><tool>a b</tool><arguments_json>{}</arguments_json></call></tool_call_envelope>`,
			"含空白字符",
		},
		{
			"参数是数组",
			`<tool_call_envelope version="1"><call id="x"><tool>a/b</tool><arguments_json>[1,2]</arguments_json></call></tool_call_envelope>`,
			"不是 JSON 对象",
		},
		{
			"参数是残缺 JSON",
			`<tool_call_envelope version="1"><call id="x"><tool>a/b</tool><arguments_json>{"a":</arguments_json></call></tool_call_envelope>`,
			"不是 JSON 对象",
		},
		{
			"参数是纯文本",
			`<tool_call_envelope version="1"><call id="x"><tool>a/b</tool><arguments_json>我不能执行</arguments_json></call></tool_call_envelope>`,
			"不是 JSON 对象",
		},
		{
			"参数标签为空",
			`<tool_call_envelope version="1"><call id="x"><tool>a/b</tool><arguments_json></arguments_json></call></tool_call_envelope>`,
			"参数为空",
		},
		{
			// 信号出现了却一个调用也没有，是自相矛盾的输出。
			"空 envelope",
			`<tool_call_envelope version="1"></tool_call_envelope>`,
			"没有任何调用",
		},
		{
			"版本不匹配",
			`<tool_call_envelope version="99"><call id="x"><tool>a/b</tool><arguments_json>{}</arguments_json></call></tool_call_envelope>`,
			"版本",
		},
		{
			// 注释里可以藏任意内容，直接拒绝比悄悄跳过安全。
			"含注释",
			`<tool_call_envelope version="1"><!-- 藏点东西 --><call id="x"><tool>a/b</tool><arguments_json>{}</arguments_json></call></tool_call_envelope>`,
			"注释或处理指令",
		},
		{
			"含处理指令",
			`<tool_call_envelope version="1"><?php evil ?><call id="x"><tool>a/b</tool><arguments_json>{}</arguments_json></call></tool_call_envelope>`,
			"注释或处理指令",
		},
		{
			// call 出现在 envelope 之前。注意闭合标签仍在，否则会先被判成截断。
			"call 在 envelope 之外",
			`<call id="x"><tool>a/b</tool><arguments_json>{}</arguments_json></call>` +
				`<tool_call_envelope version="1"></tool_call_envelope>`,
			"envelope 之外",
		},
		{
			"不是合法 XML",
			`<tool_call_envelope version="1"><call id="x"</tool_call_envelope>`,
			"不是合法 XML",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseBody(t, tc.body)
			if got.Err == nil {
				t.Fatalf("期望报错包含 %q，却解析成功了：%+v", tc.want, got.Calls)
			}
			if !strings.Contains(got.Err.Error(), tc.want) {
				t.Errorf("错误信息 %q 未包含 %q", got.Err.Error(), tc.want)
			}
			// 失败时绝不能同时给出可用的调用——规格三章：
			// 不允许把解析失败的内容塞进兜底字段后继续执行。
			if len(got.Calls) > 0 {
				t.Errorf("解析失败却产出了 %d 个调用", len(got.Calls))
			}
		})
	}
}

// XML 实体是一条经典的放大攻击路径（十亿笑）。本协议不需要实体，
// 所以解析器把实体表清空，遇到未定义实体直接报错。
func TestEnvelopeRejectsEntities(t *testing.T) {
	body := `<!DOCTYPE foo [<!ENTITY lol "haha">]>` +
		`<tool_call_envelope version="1"><call id="x"><tool>a/b</tool>` +
		`<arguments_json>{"a":"&lol;"}</arguments_json></call></tool_call_envelope>`

	got := parseBody(t, body)
	if got.Err == nil {
		t.Fatal("含实体声明的输入必须被拒绝")
	}
}

// 各类上限必须真的生效。没有它们，一段构造的输入就能让解析器
// 无界累积直到进程内存耗尽。
func TestEnvelopeLimits(t *testing.T) {
	n := testNonce(t)

	parseWith := func(t *testing.T, limits Limits, body string) Result {
		t.Helper()
		p, err := New(n, limits)
		if err != nil {
			t.Fatal(err)
		}
		_, got := feedChunks(t, p, wrap(n, body), 64)
		return got
	}

	t.Run("调用数超限", func(t *testing.T) {
		var b strings.Builder
		b.WriteString(`<tool_call_envelope version="1">`)
		for i := 0; i < 10; i++ {
			b.WriteString(`<call id="c` + itoa(i) + `"><tool>a/b</tool><arguments_json>{}</arguments_json></call>`)
		}
		b.WriteString(`</tool_call_envelope>`)

		got := parseWith(t, Limits{MaxCalls: 3}, b.String())
		if got.Err == nil || !strings.Contains(got.Err.Error(), "调用数超过上限") {
			t.Errorf("期望调用数超限报错，实际：%v", got.Err)
		}
	})

	t.Run("参数大小超限", func(t *testing.T) {
		big := `{"a":"` + strings.Repeat("x", 5000) + `"}`
		body := `<tool_call_envelope version="1"><call id="x"><tool>a/b</tool>` +
			`<arguments_json>` + big + `</arguments_json></call></tool_call_envelope>`

		got := parseWith(t, Limits{MaxArgumentBytes: 100}, body)
		if got.Err == nil || !strings.Contains(got.Err.Error(), "超过上限") {
			t.Errorf("期望参数超限报错，实际：%v", got.Err)
		}
	})

	t.Run("JSON 深度超限", func(t *testing.T) {
		// 深嵌套 JSON 是一条经典的栈耗尽路径。
		deep := strings.Repeat(`{"a":`, 50) + "1" + strings.Repeat("}", 50)
		body := `<tool_call_envelope version="1"><call id="x"><tool>a/b</tool>` +
			`<arguments_json>` + deep + `</arguments_json></call></tool_call_envelope>`

		got := parseWith(t, Limits{MaxDepth: 5}, body)
		if got.Err == nil || !strings.Contains(got.Err.Error(), "深度") {
			t.Errorf("期望深度超限报错，实际：%v", got.Err)
		}
	})

	t.Run("envelope 大小超限", func(t *testing.T) {
		body := `<tool_call_envelope version="1"><call id="x"><tool>a/b</tool>` +
			`<arguments_json>{"a":"` + strings.Repeat("y", 3000) + `"}</arguments_json></call></tool_call_envelope>`

		got := parseWith(t, Limits{MaxEnvelopeBytes: 500}, body)
		if got.Err == nil || !strings.Contains(got.Err.Error(), "超过上限") {
			t.Errorf("期望 envelope 超限报错，实际：%v", got.Err)
		}
	})

	t.Run("总字节超限", func(t *testing.T) {
		p, err := New(n, Limits{MaxTotalBytes: 100})
		if err != nil {
			t.Fatal(err)
		}
		var lastErr error
		for i := 0; i < 50; i++ {
			if _, lastErr = p.Write([]byte("普通文本内容填充\n")); lastErr != nil {
				break
			}
		}
		if lastErr == nil {
			t.Fatal("超过总字节上限必须报错")
		}
		if !strings.Contains(lastErr.Error(), "超过上限") {
			t.Errorf("错误信息 %q", lastErr.Error())
		}
	})

	t.Run("单行超限", func(t *testing.T) {
		p, err := New(n, Limits{MaxLineBytes: 50})
		if err != nil {
			t.Fatal(err)
		}
		_, err = p.Write([]byte(strings.Repeat("x", 200)))
		if err == nil {
			t.Fatal("未换行的超长内容必须被拒绝")
		}
		if !strings.Contains(err.Error(), "行长上限") {
			t.Errorf("错误信息 %q", err.Error())
		}
	})
}

// jsonDepth 用扫描而不是递归，因为要防的恰恰是栈耗尽——
// 递归解析器本身就会吃掉调用栈。
func TestJSONDepth(t *testing.T) {
	tests := []struct {
		json string
		want int
	}{
		{`{}`, 1},
		{`{"a":1}`, 1},
		{`{"a":{"b":1}}`, 2},
		{`{"a":[1,2]}`, 2},
		{`{"a":{"b":{"c":{"d":1}}}}`, 4},
		{`{"a":"{{{{{"}`, 1}, // 字符串里的括号不算
		{`{"a":"\"{{{"}`, 1}, // 转义引号后的括号也不算
		{`{"a":"]]]]]]"}`, 1},
	}
	for _, tc := range tests {
		t.Run(tc.json, func(t *testing.T) {
			if got := jsonDepth([]byte(tc.json), 100); got != tc.want {
				t.Errorf("jsonDepth(%s) = %d，期望 %d", tc.json, got, tc.want)
			}
		})
	}

	// 超过上限时提前返回，不必扫完整个输入。
	deep := strings.Repeat("{", 1000)
	if got := jsonDepth([]byte(deep), 10); got <= 10 {
		t.Errorf("深度 %d 应当超过上限 10", got)
	}
}

// 参数里的合法内容不该被误伤。
func TestEnvelopeAcceptsValidVariations(t *testing.T) {
	tests := []struct {
		name string
		args string
	}{
		{"空对象", `{}`},
		{"嵌套对象", `{"a":{"b":{"c":1}}}`},
		{"数组值", `{"list":[1,2,3]}`},
		{"连字符键", `{"content-type":"json"}`},
		{"Unicode 键值", `{"中文键":"中文值"}`},
		{"转义字符", `{"s":"引号\"与反斜杠\\"}`},
		{"数字类型", `{"i":1,"f":1.5,"e":1e10,"neg":-3}`},
		{"布尔与 null", `{"t":true,"f":false,"n":null}`},
		{"含尖括号的值", `{"html":"<div>x</div>"}`},
		{"含 CDATA 结束标记", `{"s":"a]]>b"}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := testNonce(t)
			env, err := vproto.RenderEnvelope(n, []vproto.Call{
				{ID: "c1", Tool: "a/b", ArgumentsJSON: tc.args},
			})
			if err != nil {
				t.Fatalf("渲染失败：%v", err)
			}
			p := newParser(t)
			_, got := feedChunks(t, p, env, 11)
			if got.Outcome != OutcomeCallsParsed {
				t.Fatalf("结局为 %q：%v", got.Outcome, got.Err)
			}
			if got.Calls[0].ArgumentsJSON != tc.args {
				t.Errorf("参数为 %q，期望 %q", got.Calls[0].ArgumentsJSON, tc.args)
			}
		})
	}
}
