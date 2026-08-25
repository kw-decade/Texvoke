// Command utr-eval 用真实上游测量工具调用的成功率与失败构成。
//
// 为什么需要它：改 Prompt 措辞、调工具描述上限、开关指令去重——这些改动
// 的效果只能在真实模型上观察，而「手动打六次数一数」既慢又不可比。更糟的
// 是它只给一个笼统的成功率，看不出失败是哪一类：模型没意识到自己有工具、
// 格式写坏了、上游政策禁止，这三种的处置方式完全相反，混成一个数字等于
// 什么都没测。
//
// 所以这个工具的输出不是成功率，而是**按根因分类的分布**：
//
//	codex-first-turn  n=6
//	  calls_parsed          4   67%
//	  persona_refusal       2   33%   ← 模型说「我不能直接读取文件系统」
//	  format_noncompliance  0
//
// 分类用的是 internal/capability 的那套判据（从硬证据到软证据），与将来
// 闭环恢复要用的是同一套——所以评测本身也顺带验证了那套判据准不准。
//
// 它打的是**反代**而不是上游，所以一行凭据都不碰：key 由反代管。
//
// 刻意不做成 go test：日常验证必须保持快且零外部依赖，那是项目杀毒白名单
// 依据的一部分。这是个测量工具，要手动跑。
//
// 用法：
//
//	utr-eval                                  # 跑全部 fixture，每份 6 次
//	utr-eval -n 12 -only codex-first-turn     # 只跑一份，跑 12 次
//	utr-eval -base http://127.0.0.1:18199     # 指定反代地址
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kw-decade/Texvoke/internal/capability"
)

const defaultFixtureDir = "tests/fixtures/eval"

// protocolPaths 是三协议在反代上的端点。
//
// 写死这三条是因为它们是 OpenAI 与 Anthropic 的既定路径，不是本项目的选择。
var protocolPaths = map[string]string{
	"chat":      "/v1/chat/completions",
	"anthropic": "/v1/messages",
	"responses": "/v1/responses",
}

func main() {
	var (
		base    = flag.String("base", "http://127.0.0.1:18199", "反代地址（不是上游——凭据由反代管）")
		dir     = flag.String("dir", defaultFixtureDir, "fixture 目录")
		n       = flag.Int("n", 6, "每份 fixture 跑几次")
		only    = flag.String("only", "", "只跑名字含这个子串的 fixture")
		timeout = flag.Duration("timeout", 3*time.Minute, "单次请求超时")
		verbose = flag.Bool("v", false, "打印每一次的模型输出摘要")
	)
	flag.Parse()

	m, err := loadManifest(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取 manifest 失败：%v\n", err)
		os.Exit(1)
	}

	runner := &runner{
		base:    strings.TrimRight(*base, "/"),
		dir:     *dir,
		times:   *n,
		verbose: *verbose,
		client:  &http.Client{Timeout: *timeout},
	}

	var reports []report
	for _, f := range m.Fixtures {
		if *only != "" && !strings.Contains(f.File, *only) {
			continue
		}
		rep, err := runner.run(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s：%v\n", f.File, err)
			continue
		}
		reports = append(reports, rep)
		rep.print()
	}

	if len(reports) == 0 {
		fmt.Fprintln(os.Stderr, "一份 fixture 都没跑——检查 -dir 与 -only")
		os.Exit(1)
	}
	printSummary(reports)

	// 退出码只对硬拒绝非零。
	//
	// 它是测量工具而不是守门人：成功率低说明模型不给力或 Prompt 待调，
	// 那是要看的数字，不是构建失败。但上游政策禁止是另一回事——那意味着
	// 这条路根本走不通，继续调什么都是白费。
	for _, r := range reports {
		if r.byKind[string(capability.UpstreamPolicyRefusal)] > 0 {
			fmt.Fprintln(os.Stderr, "\n有 fixture 撞上上游政策拒绝——这条路走不通，先查上游能力")
			os.Exit(1)
		}
	}
}

/* ---------- manifest ---------- */

type manifest struct {
	Fixtures []fixture `json:"fixtures"`
}

type fixture struct {
	File       string `json:"file"`
	Protocol   string `json:"protocol"`
	Why        string `json:"why"`
	Expect     string `json:"expect"`
	ExpectTool string `json:"expect_tool"`
}

func loadManifest(dir string) (*manifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if len(m.Fixtures) == 0 {
		return nil, fmt.Errorf("manifest 里没有 fixture")
	}
	return &m, nil
}

/* ---------- 跑一份 fixture ---------- */

type runner struct {
	base    string
	dir     string
	times   int
	verbose bool
	client  *http.Client
}

type report struct {
	fixture fixture
	runs    int
	byKind  map[string]int
	// wrongTool 记录调用了工具但不是期望的那个——它不算失败，
	// 但值得单独看：那是任务语义层的问题，协议层解决不了。
	wrongTool int
	// samples 存每一类失败的一个样例文本，用于人工判断分类准不准。
	samples map[string]string
	elapsed time.Duration
}

func (r *runner) run(f fixture) (report, error) {
	path, ok := protocolPaths[f.Protocol]
	if !ok {
		return report{}, fmt.Errorf("未知协议 %q", f.Protocol)
	}
	body, err := os.ReadFile(filepath.Join(r.dir, f.File))
	if err != nil {
		return report{}, err
	}
	// fixture 里可能带 stream:true（真实抓包就是），评测统一走非流式：
	// 我们要的是最终结构，而流式只是同一个结果的另一种传输方式。
	body, err = forceNonStream(body)
	if err != nil {
		return report{}, err
	}
	declared := countTools(body)

	rep := report{
		fixture: f,
		byKind:  map[string]int{},
		samples: map[string]string{},
	}
	start := time.Now()

	for i := 0; i < r.times; i++ {
		// runs 是尝试次数，所有分支都要算——它是所有百分比的分母。
		// 只在成功时递增会让失败率算出 800% 这种数。
		rep.runs++

		out, err := r.once(r.base+path, body)
		if err != nil {
			rep.byKind[string(capability.TransportFailure)]++
			rep.keepSample(string(capability.TransportFailure), err.Error())
			continue
		}

		// 用与闭环恢复同一套判据分类，而不是自己写一套 if-else。
		cls := capability.Classify(capability.Evidence{
			ToolsDeclaredByClient: declared,
			ToolsSentUpstream:     declared, // 反代不删工具，这一项恒等
			ToolChoiceRequested:   "auto",
			HTTPStatus:            out.status,
			ToolCallsParsed:       len(out.calls),
			ModelText:             out.text,
		})

		kind := string(cls.Kind)
		if cls.Kind == capability.RefusalNone {
			kind = "calls_parsed"
			if len(out.calls) == 0 {
				// 没有调用也不算拒绝：tool_choice=auto 时模型正常回答是合法的。
				kind = "plain_text_ok"
			} else if f.ExpectTool != "" && !hasTool(out.calls, f.ExpectTool) {
				rep.wrongTool++
			}
		}
		rep.byKind[kind]++
		rep.keepSample(kind, out.text)

		if r.verbose {
			fmt.Printf("  [%d/%d] %-22s %s\n", i+1, r.times, kind, brief(out, 60))
		}
	}
	rep.elapsed = time.Since(start)
	return rep, nil
}

func (r *report) keepSample(kind, text string) {
	if _, ok := r.samples[kind]; !ok && strings.TrimSpace(text) != "" {
		r.samples[kind] = text
	}
}

/* ---------- 打一次请求 ---------- */

type outcome struct {
	status int
	text   string
	calls  []string
}

func (r *runner) once(url string, body []byte) (outcome, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return outcome{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return outcome{}, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return outcome{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return outcome{status: resp.StatusCode},
			fmt.Errorf("HTTP %d：%s", resp.StatusCode, trim(string(raw), 200))
	}
	return parseAny(raw, resp.StatusCode)
}

// parseAny 从三种协议的响应里取出文本与工具调用名。
//
// 只认这两样东西，不做完整解码：评测关心的是「有没有调用、模型说了什么」，
// 而完整解码是 internal/protocol 的职责，在这里重做一遍只会多一处分叉。
func parseAny(raw []byte, status int) (outcome, error) {
	var r struct {
		// Chat
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Function struct{ Name string } `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		// Anthropic
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Name string `json:"name"`
		} `json:"content"`
		// Responses
		Output []struct {
			Type    string `json:"type"`
			Name    string `json:"name"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return outcome{status: status}, fmt.Errorf("响应不是合法 JSON：%w", err)
	}

	out := outcome{status: status}
	for _, c := range r.Choices {
		out.text += c.Message.Content
		for _, tc := range c.Message.ToolCalls {
			out.calls = append(out.calls, tc.Function.Name)
		}
	}
	for _, b := range r.Content {
		switch b.Type {
		case "text":
			out.text += b.Text
		case "tool_use":
			out.calls = append(out.calls, b.Name)
		}
	}
	for _, it := range r.Output {
		switch {
		case strings.Contains(it.Type, "tool_call"):
			out.calls = append(out.calls, it.Name)
		case it.Type == "message":
			for _, c := range it.Content {
				out.text += c.Text
			}
		}
	}
	return out, nil
}

/* ---------- 输出 ---------- */

func (r report) print() {
	fmt.Printf("\n%s  n=%d  (%s)\n", r.fixture.File, r.runs, r.elapsed.Round(time.Second))
	if r.fixture.Expect != "" {
		fmt.Printf("  期望：%s\n", r.fixture.Expect)
	}

	kinds := make([]string, 0, len(r.byKind))
	for k := range r.byKind {
		kinds = append(kinds, k)
	}
	// 成功排最前，其余按次数降序——一眼看到主要失败模式。
	sort.Slice(kinds, func(i, j int) bool {
		if (kinds[i] == "calls_parsed") != (kinds[j] == "calls_parsed") {
			return kinds[i] == "calls_parsed"
		}
		if r.byKind[kinds[i]] != r.byKind[kinds[j]] {
			return r.byKind[kinds[i]] > r.byKind[kinds[j]]
		}
		return kinds[i] < kinds[j]
	})

	total := r.runs
	if total == 0 {
		total = 1
	}
	for _, k := range kinds {
		c := r.byKind[k]
		fmt.Printf("    %-24s %2d  %3.0f%%", k, c, float64(c)/float64(total)*100)
		if s, ok := r.samples[k]; ok && k != "calls_parsed" && c > 0 {
			fmt.Printf("   ← %s", trim(oneLine(s), 56))
		}
		fmt.Println()
	}
	if r.wrongTool > 0 {
		fmt.Printf("    %-24s %2d       调用了工具但不是期望的那个（任务语义层，协议层管不了）\n",
			"(wrong_tool)", r.wrongTool)
	}
}

func printSummary(reports []report) {
	fmt.Printf("\n%s\n", strings.Repeat("─", 64))
	var runs, ok int
	for _, r := range reports {
		runs += r.runs
		ok += r.byKind["calls_parsed"]
	}
	fmt.Printf("合计 %d 份 fixture，%d 次调用，%d 次产出工具调用（%.0f%%）\n",
		len(reports), runs, ok, pct(ok, runs))
	fmt.Println("\n注意这个百分比只反映协议可靠性。模型选对工具、填对参数属于")
	fmt.Println("任务语义可靠性，不在协议层的能力范围内——两者不要混着看。")
}

/* ---------- 小工具 ---------- */

// forceNonStream 把 stream 字段改成 false。
func forceNonStream(body []byte) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, fmt.Errorf("fixture 不是合法 JSON 对象：%w", err)
	}
	top["stream"] = json.RawMessage("false")
	return json.Marshal(top)
}

// countTools 数客户端声明了多少工具。
//
// 三协议三个位置，还要算上 Responses 的 additional_tools item——Codex 把
// 全部工具放在那里，只看顶层 tools 会数出 0，然后分类器就会把一切都判成
// client_capability_missing。
func countTools(body []byte) int {
	var top struct {
		Tools []json.RawMessage `json:"tools"`
		Input []struct {
			Type  string            `json:"type"`
			Tools []json.RawMessage `json:"tools"`
		} `json:"input"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		return 0
	}
	n := len(top.Tools)
	for _, it := range top.Input {
		if it.Type == "additional_tools" {
			n += len(it.Tools)
		}
	}
	return n
}

func hasTool(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}

func brief(o outcome, max int) string {
	if len(o.calls) > 0 {
		return "调用 " + strings.Join(o.calls, ",")
	}
	return trim(oneLine(o.text), max)
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func trim(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// 按 UTF-8 边界截，免得输出里出现半个汉字。
	cut := max
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b) * 100
}
