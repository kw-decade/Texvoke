package toolbridge

import (
	"regexp"
	"strings"
	"testing"
)

// legacyNoise 是从前身项目抄来的真实规则，
// 用它测才有意义——自己编一条好过的规则等于没测。
func legacyNoise() []*regexp.Regexp {
	return []*regexp.Regexp{
		// 首部品牌横幅
		regexp.MustCompile(`(?m)^\s*(?:👋\s*)?(?:你好|Hi|Hello)[\s!！]*我是[^\n]*?p5\.?js[^\n]*\n+`),
		// 尾部锚定——这是难点所在
		regexp.MustCompile(`(?s)\n+如果你.{0,40}?p5\.?js.{0,200}?$`),
		regexp.MustCompile(`(?s)\n+想要.{0,20}?p5\.?js.{0,200}?$`),
	}
}

// feedStream 把输入按给定粒度切碎喂进去，返回转发出去的全部内容。
func feedStream(t *testing.T, sp *StreamParser, input string, chunkSize int) string {
	t.Helper()
	var out strings.Builder
	data := []byte(input)
	for i := 0; i < len(data); i += chunkSize {
		end := i + chunkSize
		if end > len(data) {
			end = len(data)
		}
		safe, err := sp.Write(data[i:end])
		if err != nil {
			t.Fatalf("第 %d 字节处失败：%v", i, err)
		}
		out.Write(safe)
	}
	out.Write(sp.Flush())
	return out.String()
}

// 尾部噪声在流式路径上一个字都不能外泄。
//
// 这曾被认为无法解决——`$` 锚定输出结尾，流式时不知道哪里是结尾。
// 解法是在累积文本上反复跑正则（`$` 匹配当前结尾），一旦匹配就扣住
// 从匹配起点起的全部内容；再用保留窗口兜住规则的回溯距离。
func TestStreamTailNoiseNeverLeaks(t *testing.T) {
	input := "👋 你好！我是 p5.js 助手\n" +
		"旧金山今天 18 度，适合出门。\n" +
		"如果你想了解更多 p5.js 的绘图用法，随时问我。"

	// 各种切分粒度都不能漏。逐字节是最极端的情形。
	for _, size := range []int{1, 2, 3, 7, 13, 64, 4096} {
		t.Run("chunk="+itoa(size), func(t *testing.T) {
			s := newSession(t, Config{Upstream: UpstreamProfile{NoiseFilters: legacyNoise()}})
			if _, err := s.Compile([]Tool{weatherTool()}, CompileOptions{}); err != nil {
				t.Fatal(err)
			}
			sp, err := s.NewStreamParser()
			if err != nil {
				t.Fatal(err)
			}

			forwarded := feedStream(t, sp, input, size)
			res := sp.Close()

			// 转发出去的内容里不能有任何噪声痕迹。
			for _, leak := range []string{"p5.js 助手", "如果你想了解更多", "随时问我"} {
				if strings.Contains(forwarded, leak) {
					t.Errorf("噪声 %q 泄漏给了客户端：%q", leak, forwarded)
				}
			}
			// 正文必须完整送达。
			if !strings.Contains(forwarded, "旧金山今天 18 度") {
				t.Errorf("正文丢失：%q", forwarded)
			}
			// 流式转发的内容与最终文本应当一致。
			if strings.TrimSpace(forwarded) != strings.TrimSpace(res.Text) {
				t.Errorf("流式转发与最终文本不一致\n转发：%q\n最终：%q", forwarded, res.Text)
			}
		})
	}
}

// 回溯场景：规则的前半句先到，后半句晚到才构成匹配。
// 保留窗口就是为这种情况存在的。
func TestStreamTailNoiseWithBacktrack(t *testing.T) {
	s := newSession(t, Config{Upstream: UpstreamProfile{
		NoiseFilters:  legacyNoise(),
		TailHoldBytes: 512,
	}})
	if _, err := s.Compile([]Tool{weatherTool()}, CompileOptions{}); err != nil {
		t.Fatal(err)
	}
	sp, err := s.NewStreamParser()
	if err != nil {
		t.Fatal(err)
	}

	// 分三次喂：第一段是正文，第二段单独看不构成匹配（缺 p5.js），
	// 第三段到了才让整条规则成立。
	pieces := []string{
		"这是正文内容。\n",
		"如果你想深入一点，",
		"可以看看 p5.js 的官方文档。",
	}
	var forwarded strings.Builder
	for _, p := range pieces {
		safe, err := sp.Write([]byte(p))
		if err != nil {
			t.Fatal(err)
		}
		forwarded.Write(safe)
	}
	forwarded.Write(sp.Flush())
	sp.Close()

	got := forwarded.String()
	if strings.Contains(got, "如果你想深入一点") {
		t.Errorf("回溯匹配失败，噪声前半句已泄漏：%q", got)
	}
	if !strings.Contains(got, "这是正文内容") {
		t.Errorf("正文丢失：%q", got)
	}
}

// 保留窗口设得比噪声短时会漏——这是配置问题，不是实现问题。
// 测试固定这个行为，让「窗口要设多大」有据可依。
func TestStreamTailHoldTooSmallLeaks(t *testing.T) {
	s := newSession(t, Config{Upstream: UpstreamProfile{
		NoiseFilters: legacyNoise(),
		// 只留 4 字节，远小于噪声长度。
		TailHoldBytes: 4,
	}})
	if _, err := s.Compile([]Tool{weatherTool()}, CompileOptions{}); err != nil {
		t.Fatal(err)
	}
	sp, err := s.NewStreamParser()
	if err != nil {
		t.Fatal(err)
	}

	// 噪声跨越多次 Write，窗口太小兜不住。
	pieces := []string{"正文。\n", "如果你想深入，", "看看 p5.js 文档。"}
	var forwarded strings.Builder
	for _, p := range pieces {
		safe, _ := sp.Write([]byte(p))
		forwarded.Write(safe)
	}
	forwarded.Write(sp.Flush())
	res := sp.Close()

	// 最终文本一定是干净的——Close 时在全文上判定，永远正确。
	if strings.Contains(res.Text, "如果你想深入") {
		t.Errorf("最终文本仍有噪声，说明过滤本身坏了：%q", res.Text)
	}
	// 但流式转发可能已经漏了。这里不断言必漏（取决于切分），
	// 只记录：窗口不足时最终文本与转发内容会不一致。
	if strings.TrimSpace(forwarded.String()) != strings.TrimSpace(res.Text) {
		t.Logf("窗口不足导致转发与最终文本不一致（预期行为）\n转发：%q\n最终：%q",
			forwarded.String(), res.Text)
	}
}

// 显式关闭保留窗口。
func TestStreamTailHoldDisabled(t *testing.T) {
	s := newSession(t, Config{Upstream: UpstreamProfile{
		NoiseFilters:  legacyNoise(),
		TailHoldBytes: -1,
	}})
	if s.bridge.holdBytes != 0 {
		t.Errorf("负数应关闭窗口，实际 holdBytes=%d", s.bridge.holdBytes)
	}
}

// 零值取默认而非关闭：配了尾部规则却忘设窗口的接入方，
// 应当默认拿到正确行为，而不是默认漏噪声。
func TestStreamTailHoldDefault(t *testing.T) {
	s := newSession(t, Config{Upstream: UpstreamProfile{NoiseFilters: legacyNoise()}})
	if s.bridge.holdBytes != DefaultTailHoldBytes {
		t.Errorf("默认窗口为 %d，期望 %d", s.bridge.holdBytes, DefaultTailHoldBytes)
	}
}

// 顽固身份认知的完整场景：上游每次回复都套着固定的开场与结尾。
// 这是用户提出这个需求的原始动机。
func TestStubbornPersonaUpstream(t *testing.T) {
	// 模拟一个身份认知很强的上游：开场自报家门，结尾推销自己。
	noise := []*regexp.Regexp{
		regexp.MustCompile(`(?m)^\s*我是[^\n]{0,30}助手[^\n]*\n+`),
		regexp.MustCompile(`(?s)\n+还有什么.{0,50}?(?:可以帮|需要).{0,50}?$`),
		regexp.MustCompile(`(?s)\n+—+\s*由.{0,30}提供支持\s*$`),
	}

	s := newSession(t, Config{Upstream: UpstreamProfile{NoiseFilters: noise}})
	compiled, err := s.Compile([]Tool{weatherTool()}, CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// 上游的回复：噪声包着正文，中间还夹着工具调用。
	output := "我是某某平台的智能助手\n" +
		"好的，我来查一下天气。\n" +
		compiled.Signal + "\n" +
		`<tool_call_envelope version="1"><call id="c1"><tool>get_weather</tool>` +
		`<arguments_json>{"city":"SF"}</arguments_json></call></tool_call_envelope>`

	sp, err := s.NewStreamParser()
	if err != nil {
		t.Fatal(err)
	}
	forwarded := feedStream(t, sp, output, 5)
	res := sp.Close()

	// 工具调用要正常解析出来——噪声过滤不能干扰协议识别。
	if res.Outcome != OutcomeCallsParsed {
		t.Fatalf("结局为 %q，噪声过滤干扰了协议识别", res.Outcome)
	}
	if len(res.Calls) != 1 || res.Calls[0].Name != "get_weather" {
		t.Fatalf("调用解析有误：%+v", res.Calls)
	}
	// 身份自述不能出现在给客户端的正文里。
	if strings.Contains(forwarded, "智能助手") {
		t.Errorf("身份自述泄漏：%q", forwarded)
	}
	if !strings.Contains(forwarded, "我来查一下天气") {
		t.Errorf("正文丢失：%q", forwarded)
	}
	// 协议内容同样不能漏。
	if strings.Contains(forwarded, compiled.Signal) || strings.Contains(forwarded, "envelope") {
		t.Errorf("协议内容泄漏：%q", forwarded)
	}
}

// 没有噪声配置时不该有任何缓冲开销。
func TestStreamZeroOverheadWithoutFilters(t *testing.T) {
	s := newSession(t, Config{})
	if _, err := s.Compile([]Tool{weatherTool()}, CompileOptions{}); err != nil {
		t.Fatal(err)
	}
	sp, err := s.NewStreamParser()
	if err != nil {
		t.Fatal(err)
	}

	out, err := sp.Write([]byte("旧金山"))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "旧金山" {
		t.Errorf("没有过滤规则时应立即透传，实际提交 %q", out)
	}
	if len(sp.Flush()) != 0 {
		t.Error("没有过滤规则时 Flush 应为空")
	}
}

// 纯正文（不含任何噪声）必须完整送达，一个字都不能少。
func TestStreamCleanTextPassesThrough(t *testing.T) {
	s := newSession(t, Config{Upstream: UpstreamProfile{NoiseFilters: legacyNoise()}})
	if _, err := s.Compile([]Tool{weatherTool()}, CompileOptions{}); err != nil {
		t.Fatal(err)
	}

	clean := "第一行。\n第二行。\n第三行没有换行结尾。"
	sp, err := s.NewStreamParser()
	if err != nil {
		t.Fatal(err)
	}
	forwarded := feedStream(t, sp, clean, 3)

	if forwarded != clean {
		t.Errorf("干净文本被改动了\n实际：%q\n期望：%q", forwarded, clean)
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
