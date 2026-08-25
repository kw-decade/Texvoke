// Command utr-redact 把真实抓包脱敏成可入库的评测 fixture。
//
// 为什么需要它：评测集必须用真实客户端的请求，而真实请求里有用户名、
// 绝对路径和用户自己写的指令文件——那些不能进 git。但直接把它们换成
// 一句占位又会毁掉 fixture 的价值：评测要测的正是「协议指令被几十 KB
// 业务指令稀释」这件事，长度是关键自变量，把 26 KB 的沙箱说明换成
// 「[已脱敏]」就什么都测不出来了。
//
// 所以这个工具的核心约束是**保字节数**：
//
//	12345           → USER0            （都是 5 字节）
//	C:\Users\12345  → C:\Users\USER0
//	用户的 AGENTS.md → 等长的中性中文文本
//
// 客户端自己的 prompt（Codex 的系统说明、沙箱规则、工具描述）是公开内容，
// 原样保留——那部分正是我们要测的稀释源。
//
// 脚本本身入库，这样 fixture 可复现、可审计：任何人都能拿自己的抓包
// 重新生成一份，也能核对入库的 fixture 到底改了什么。
//
// 用法：
//
//	utr-redact -in codex-real-request.json -out tests/fixtures/eval/codex-multi-turn.json
//	utr-redact -in x.json -out y.json -user 12345   # 指定要替换的用户名
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"
)

func main() {
	var (
		in       = flag.String("in", "", "输入的原始抓包 JSON（必填）")
		out      = flag.String("out", "", "输出的脱敏 fixture（必填）")
		userName = flag.String("user", "", "要替换的用户名，为空时从 C:\\Users\\<name> 自动识别")
		verbose  = flag.Bool("v", false, "打印每一类替换的次数")
	)
	flag.Parse()

	if *in == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "用法：utr-redact -in <原始抓包> -out <脱敏 fixture>")
		flag.PrintDefaults()
		os.Exit(2)
	}

	raw, err := os.ReadFile(*in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取失败：%v\n", err)
		os.Exit(1)
	}

	r := &redactor{verbose: *verbose}
	cleaned, err := r.run(raw, *userName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "脱敏失败：%v\n", err)
		os.Exit(1)
	}

	if err := os.WriteFile(*out, cleaned, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "写入失败：%v\n", err)
		os.Exit(1)
	}

	r.report(len(raw), len(cleaned))
}

// redactor 持有一次脱敏的计数，用于事后核对。
type redactor struct {
	verbose bool

	userName    string
	userHits    int
	pathHits    int
	droppedKeys []string

	// replaced 记录每一处被整体替换的内容，用于事后核对。
	//
	// 判据再严也可能误判，而误判的后果是 fixture 失去真实性——把客户端
	// 自己的 prompt 换成占位，评测就测不出真实的稀释程度了。把原文开头
	// 打出来，让人扫一眼就能发现「这条不该被替换」。
	replaced []replacedDoc
}

type replacedDoc struct {
	index int
	role  string
	bytes int
	head  string
}

// sensitiveTopLevel 是要整个清掉的顶层字段。
//
// 它们与协议行为无关，但可能带客户端标识或缓存键——留着没有测试价值，
// 只有泄漏风险。
var sensitiveTopLevel = []string{"client_metadata", "prompt_cache_key"}

// userDirPattern 从抓包里识别用户名。用 Windows 的用户目录形态，
// 因为那是抓包里出现最多、也最确定的一处。
var userDirPattern = regexp.MustCompile(`C:\\+Users\\+([A-Za-z0-9_.-]{1,32})`)

func (r *redactor) run(raw []byte, userName string) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("输入不是合法 JSON 对象：%w", err)
	}

	// 先定用户名。没指定就从路径里认——认不出来说明这份抓包不含
	// Windows 用户目录，那也不必替换。
	r.userName = userName
	if r.userName == "" {
		if m := userDirPattern.FindSubmatch(raw); m != nil {
			r.userName = string(m[1])
		}
	}

	for _, k := range sensitiveTopLevel {
		if _, ok := top[k]; ok {
			delete(top, k)
			r.droppedKeys = append(r.droppedKeys, k)
		}
	}

	if rawInput, ok := top["input"]; ok {
		redacted, err := r.redactInput(rawInput)
		if err != nil {
			return nil, err
		}
		top["input"] = redacted
	}

	// 剩下的字段做整体的字符串替换。工具描述在 input 里已经处理过，
	// 这一步兜住 instructions 之类还可能带路径的地方。
	for k, v := range top {
		if k == "input" {
			continue
		}
		top[k] = json.RawMessage(r.scrubBytes(v))
	}

	buf, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(buf, '\n'), nil
}

// redactInput 逐个 item 处理。
//
// 只有一类内容会被整体替换：用户自己写的指令文件（AGENTS.md / CLAUDE.md
// 之类）。判据是内容里出现那个文件名——它是用户的私人内容，而且往往
// 包含「关于我」这种直接的个人信息。其余 item 只做用户名与路径替换。
func (r *redactor) redactInput(rawInput json.RawMessage) (json.RawMessage, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(rawInput, &items); err != nil {
		// input 也可以是一段纯文本，那就整体替换。
		return json.RawMessage(r.scrubBytes(rawInput)), nil
	}

	for i, item := range items {
		var probe struct {
			Type    string          `json:"type"`
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(item, &probe); err != nil {
			return nil, fmt.Errorf("第 %d 个 item 不是合法对象：%w", i, err)
		}

		if isUserInstructionDoc(probe.Content) {
			replaced, err := r.replaceDocContent(item, probe.Content)
			if err != nil {
				return nil, fmt.Errorf("第 %d 个 item 替换失败：%w", i, err)
			}
			items[i] = replaced
			r.replaced = append(r.replaced, replacedDoc{
				index: i, role: probe.Role,
				bytes: len(probe.Content), head: head(probe.Content),
			})
			continue
		}
		items[i] = json.RawMessage(r.scrubBytes(item))
	}

	return json.Marshal(items)
}

// userDocMarkers 是「这段内容是用户自己写的指令文件」的判据。
//
// 用**确定的格式标记**而不是关键词。第一版写的是「含 AGENTS.md 就算」，
// 结果把客户端自己的一条 prompt 也替换了：
//
//	<multi_agent_mode>Do not spawn sub-agents unless the user or
//	applicable AGENTS.md/skill instructions explicitly ask for ...
//
// 那是 Codex 的公开说明，替换掉就毁了 fixture 的真实性——而 fixture 存在
// 的全部意义就是真实。所以判据要认「用户文档被注入时的固定包装」，
// 不是认它提到了什么。
//
// 漏替换的代价（私人内容进 git）比过度替换更严重，所以脱敏后会把每一处
// 替换的原文开头打出来，让人能当场核对。宁可靠人眼兜底，不靠放宽判据。
var userDocMarkers = []string{
	"# AGENTS.md instructions for", // Codex 注入用户 AGENTS.md 时的固定标题
	"# CLAUDE.md instructions for",
	"<INSTRUCTIONS>", // 用户文档的包装标记
}

func isUserInstructionDoc(content json.RawMessage) bool {
	if len(content) == 0 {
		return false
	}
	s := string(content)
	for _, m := range userDocMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// replaceDocContent 把一个 item 的正文换成等长的中性文本。
//
// 等长是关键：这份内容的长度决定了协议指令在整个 prompt 里的占比，
// 而那正是评测要测的东西。换短了，fixture 就测不出真实的稀释程度。
func (r *redactor) replaceDocContent(item, content json.RawMessage) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(item, &obj); err != nil {
		return nil, err
	}

	// content 有两种形态：字符串，或 content part 数组。两种都要按
	// 各自的形状还原，否则解码器会看到一个它不认识的形状。
	var asString string
	if json.Unmarshal(content, &asString) == nil {
		filler := neutralText(len(asString))
		encoded, err := json.Marshal(filler)
		if err != nil {
			return nil, err
		}
		obj["content"] = encoded
		return json.Marshal(obj)
	}

	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(content, &parts); err != nil {
		return nil, fmt.Errorf("content 既不是字符串也不是数组：%w", err)
	}
	for i, p := range parts {
		rawText, ok := p["text"]
		if !ok {
			continue
		}
		var text string
		if err := json.Unmarshal(rawText, &text); err != nil {
			continue
		}
		encoded, err := json.Marshal(neutralText(len(text)))
		if err != nil {
			return nil, err
		}
		parts[i]["text"] = encoded
	}
	encoded, err := json.Marshal(parts)
	if err != nil {
		return nil, err
	}
	obj["content"] = encoded
	return json.Marshal(obj)
}

// neutralSentence 是填充用的中性句子。
//
// 内容刻意写成「一份普通的项目约定」的样子：它要在语义上像真实的
// 用户指令文件（否则模型对 prompt 的反应会与真实场景不同），但不含
// 任何个人信息。
const neutralSentence = "这是一份用于测试的项目约定占位文本。它的长度与真实文件一致，" +
	"内容不含任何个人信息。约定包括：代码用英文命名，注释说明为什么这样写，" +
	"改完主动跑验证，不为了让测试通过而放宽校验。"

// neutralText 生成恰好 n 字节的中性文本。
//
// 精确到字节而不是「差不多长」：fixture 的字节数会被单测断言，也会
// 决定评测里协议指令的占比。多一个字节少一个字节都会让两次评测不可比。
func neutralText(n int) string {
	if n <= 0 {
		return ""
	}
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(neutralSentence)
	}
	s := b.String()

	// 按 UTF-8 边界截到不超过 n，再用空格补足。空格是单字节，
	// 所以这一步一定能精确命中 n。
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	s = s[:cut]
	if pad := n - len(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return s
}

// scrubBytes 做用户名与路径的等长替换。
//
// 在原始 JSON 字节上做而不是解码后逐字段做：路径会出现在任何地方
// （工具描述的示例、沙箱说明、错误信息里），逐字段处理必定漏。
// 替换串与原串等长，所以这样做不会破坏 JSON 结构。
func (r *redactor) scrubBytes(b json.RawMessage) []byte {
	if r.userName == "" {
		return b
	}
	s := string(b)

	before := strings.Count(s, r.userName)
	s = strings.ReplaceAll(s, r.userName, sameLenAlias(r.userName))
	r.userHits += before

	r.pathHits += len(userDirPattern.FindAllString(s, -1))
	return []byte(s)
}

// sameLenAlias 造一个与原名等长的替代名。
//
// 等长是为了让所有含路径的字符串保持原来的字节数——沙箱说明里的路径
// 出现一百多次，长度一变，整段的字节数就跟着变，稀释比例也就不真实了。
func sameLenAlias(name string) string {
	const base = "USER"
	if len(name) <= len(base) {
		return base[:len(name)]
	}
	// 比 USER 长的部分用 0 填，例如 12345（5 字节）→ USER0。
	return base + strings.Repeat("0", len(name)-len(base))
}

// head 取一段内容的开头，供人工核对。
//
// 只取开头而不是全文：报告要能一眼扫完，而一段用户文档可能有几千字节。
// 开头足以判断「这是用户写的还是客户端写的」。
func head(content json.RawMessage) string {
	s := string(content)
	const max = 70
	if len(s) > max {
		// 按 UTF-8 边界截，免得报告里出现半个字符。
		cut := max
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return strings.ReplaceAll(s, "\n", `\n`)
}

func (r *redactor) report(inBytes, outBytes int) {
	fmt.Printf("脱敏完成：%d → %d 字节\n", inBytes, outBytes)
	if r.userName != "" {
		fmt.Printf("  用户名 %q → %q，替换 %d 处\n",
			r.userName, sameLenAlias(r.userName), r.userHits)
	}
	fmt.Printf("  含用户目录的路径 %d 处（已随用户名一并替换）\n", r.pathHits)
	fmt.Printf("  用户指令文件 %d 处，换成等长中性文本：\n", len(r.replaced))
	for _, d := range r.replaced {
		fmt.Printf("    [%d] %s %d 字节：%s\n", d.index, d.role, d.bytes, d.head)
	}
	if len(r.droppedKeys) > 0 {
		fmt.Printf("  清掉的顶层字段：%s\n", strings.Join(r.droppedKeys, ", "))
	}
	if inBytes != outBytes && len(r.replaced) == 0 && len(r.droppedKeys) == 0 {
		// 只做了等长替换却改变了总长度，说明有替换没保住字节数。
		fmt.Fprintf(os.Stderr,
			"警告：只做了等长替换但长度变了（%d → %d），请检查\n", inBytes, outBytes)
	}
}
