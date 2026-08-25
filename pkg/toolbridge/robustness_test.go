package toolbridge

import (
	"strings"
	"testing"
)

func robustSession(t *testing.T) (*Session, *CompileResult) {
	t.Helper()
	b, _ := New(Config{})
	sess, err := b.NewSession("s", "r")
	if err != nil {
		t.Fatal(err)
	}
	res, err := sess.Compile([]Tool{
		{Name: "exec", Description: "跑 JS", Freeform: true},
		{Name: "wait", InputSchema: []byte(`{"type":"object"}`)},
	}, CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return sess, res
}

func envelopeOf(signal, inner string) string {
	return signal + "\n<tool_call_envelope version=\"1\">" + inner + "</tool_call_envelope>"
}

const execCall = `<call id="c1"><tool>exec</tool><arguments_text><![CDATA[ls()]]></arguments_text></call>`

// 纯文本模型最常见的两种收尾——补一句话、用 markdown 围栏包住——
// 不该让一个已经完整解析出来的调用作废。
func TestTrailingChatterDoesNotKillTheCall(t *testing.T) {
	t.Run("补一句总结", func(t *testing.T) {
		sess, c := robustSession(t)
		res, err := sess.Parse(envelopeOf(c.Signal, execCall) + "\n我已经调用了工具。")
		if err != nil {
			t.Fatalf("不该失败：%v", err)
		}
		if res.Outcome != OutcomeCallsParsed || len(res.Calls) != 1 {
			t.Fatalf("结局 %q，调用数 %d", res.Outcome, len(res.Calls))
		}
		// 违规要可见，但不能混进要转发给客户端的正文。
		if !strings.Contains(res.Trailing, "我已经调用了工具") {
			t.Errorf("尾部内容没记下来：%q", res.Trailing)
		}
		if strings.Contains(res.Text, "我已经调用了工具") {
			t.Errorf("尾部内容混进了正文：%q", res.Text)
		}
	})

	t.Run("markdown 围栏", func(t *testing.T) {
		sess, c := robustSession(t)
		res, err := sess.Parse("```xml\n" + envelopeOf(c.Signal, execCall) + "\n```")
		if err != nil {
			t.Fatalf("不该失败：%v", err)
		}
		if res.Outcome != OutcomeCallsParsed || len(res.Calls) != 1 {
			t.Fatalf("结局 %q，调用数 %d：这是模型最爱的写法", res.Outcome, len(res.Calls))
		}
	})

	t.Run("闭合后又发信号仍然拒绝", func(t *testing.T) {
		sess, c := robustSession(t)
		res, _ := sess.Parse(envelopeOf(c.Signal, execCall) + "\n" + c.Signal + "\n<tool_call_envelope version=\"1\">" + execCall + "</tool_call_envelope>")
		if res.Outcome == OutcomeCallsParsed {
			t.Error("两次调用意图摆在一起，取哪个都是替调用方做决定")
		}
	})
}

// 模型写错工具名是常见的，而错了之后没有任何人会报错——调用原样转给
// 客户端，客户端说「没有这个工具」，症状出现在链路另一端。
func TestToolNameIsCrossChecked(t *testing.T) {
	t.Run("剥掉协议前缀", func(t *testing.T) {
		sess, c := robustSession(t)
		// Codex 的 developer message 用 to=functions.xxx 引用工具，
		// 模型很容易照着写。
		res, err := sess.Parse(envelopeOf(c.Signal,
			`<call id="c1"><tool>functions.exec</tool><arguments_text><![CDATA[ls()]]></arguments_text></call>`))
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Calls) != 1 || res.Calls[0].Name != "exec" {
			t.Errorf("前缀没剥掉：%+v", res.Calls)
		}
		if len(res.UnknownTools) != 0 {
			t.Errorf("剥掉前缀后就对上了，不该报未知：%v", res.UnknownTools)
		}
	})

	t.Run("大小写不一致", func(t *testing.T) {
		sess, c := robustSession(t)
		res, err := sess.Parse(envelopeOf(c.Signal,
			`<call id="c1"><tool>Exec</tool><arguments_text><![CDATA[ls()]]></arguments_text></call>`))
		if err != nil {
			t.Fatal(err)
		}
		if res.Calls[0].Name != "exec" {
			t.Errorf("大小写没还原：%q", res.Calls[0].Name)
		}
	})

	t.Run("编出来的工具要报告", func(t *testing.T) {
		sess, c := robustSession(t)
		res, err := sess.Parse(envelopeOf(c.Signal,
			`<call id="c1"><tool>delete_everything</tool><arguments_json><![CDATA[{}]]></arguments_json></call>`))
		if err != nil {
			t.Fatal(err)
		}
		// 调用仍然原样给出——不猜测、不改写。
		if len(res.Calls) != 1 || res.Calls[0].Name != "delete_everything" {
			t.Fatalf("不该改写模型写的名字：%+v", res.Calls)
		}
		// 但必须报告，否则接入方会把一个不存在的工具转给客户端。
		if len(res.UnknownTools) != 1 || res.UnknownTools[0] != "delete_everything" {
			t.Errorf("未知工具没报告：%v", res.UnknownTools)
		}
	})

	t.Run("不做模糊匹配", func(t *testing.T) {
		sess, c := robustSession(t)
		// 近似匹配会让一个错名字意外命中另一个工具，比调用失败危险得多。
		res, err := sess.Parse(envelopeOf(c.Signal,
			`<call id="c1"><tool>exe</tool><arguments_json><![CDATA[{}]]></arguments_json></call>`))
		if err != nil {
			t.Fatal(err)
		}
		if res.Calls[0].Name != "exe" || len(res.UnknownTools) != 1 {
			t.Errorf("不该猜成 exec：%+v %v", res.Calls, res.UnknownTools)
		}
	})

	t.Run("会话重建时不假装能判断", func(t *testing.T) {
		// 无状态 sidecar 的常态：手上没有工具名单。
		b, _ := New(Config{})
		sess, c := robustSession(t)
		restored, err := b.RestoreSession("s", "r", sess.NonceValue())
		if err != nil {
			t.Fatal(err)
		}
		res, err := restored.Parse(envelopeOf(c.Signal,
			`<call id="c1"><tool>随便什么</tool><arguments_json><![CDATA[{}]]></arguments_json></call>`))
		if err != nil {
			t.Fatal(err)
		}
		if len(res.UnknownTools) != 0 {
			t.Errorf("没有名单就不该判断：%v", res.UnknownTools)
		}
	})
}
