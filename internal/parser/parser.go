package parser

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/kw-decade/Texvoke/internal/vproto"
)

// State 是解析器的状态。
type State string

const (
	// StateText：正在输出普通文本。
	StateText State = "text"
	// StateThinking：在 think 区域内。此处不检测信号——模型在思考里
	// 提到协议格式是常见的，不该被当成发起调用。
	StateThinking State = "thinking"
	// StateEnvelope：已见到信号，正在累积 envelope。
	StateEnvelope State = "envelope"
	// StateDone：envelope 已闭合。
	StateDone State = "done"
	// StateFailed：解析失败，不再接受输入。
	StateFailed State = "failed"
)

// Limits 是解析器的资源上限。
//
// 规格九章要求每个会话都有这些上限：没有它们，一段构造的输入就能让
// 解析器无界累积直到进程内存耗尽。
type Limits struct {
	// MaxLineBytes 是单行上限。一行迟迟不换行会让待定窗口一直增长。
	MaxLineBytes int
	// MaxTotalBytes 是整个响应的上限。
	MaxTotalBytes int
	// MaxEnvelopeBytes 是单个 envelope 的上限。
	MaxEnvelopeBytes int
	// MaxCalls 是单个 envelope 内的调用数上限。
	MaxCalls int
	// MaxArgumentBytes 是单次调用参数的上限。
	MaxArgumentBytes int
	// MaxDepth 是 XML 嵌套深度上限。
	MaxDepth int
}

// DefaultLimits 返回一组保守的默认上限。
func DefaultLimits() Limits {
	return Limits{
		MaxLineBytes:     64 << 10,  // 64 KiB
		MaxTotalBytes:    16 << 20,  // 16 MiB
		MaxEnvelopeBytes: 1 << 20,   // 1 MiB
		MaxCalls:         32,        //
		MaxArgumentBytes: 256 << 10, // 256 KiB
		MaxDepth:         8,         //
	}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.MaxLineBytes <= 0 {
		l.MaxLineBytes = d.MaxLineBytes
	}
	if l.MaxTotalBytes <= 0 {
		l.MaxTotalBytes = d.MaxTotalBytes
	}
	if l.MaxEnvelopeBytes <= 0 {
		l.MaxEnvelopeBytes = d.MaxEnvelopeBytes
	}
	if l.MaxCalls <= 0 {
		l.MaxCalls = d.MaxCalls
	}
	if l.MaxArgumentBytes <= 0 {
		l.MaxArgumentBytes = d.MaxArgumentBytes
	}
	if l.MaxDepth <= 0 {
		l.MaxDepth = d.MaxDepth
	}
	return l
}

// think 标签。DeepSeek R1 之类的模型会在纯文本里输出这对标签，
// 而它们内部的内容常常包含对协议格式的复述。
const (
	thinkOpen  = "<think>"
	thinkClose = "</think>"
)

// Outcome 是流结束时的分类。
//
// 规格九章要求流结束时能区分这四种情形——它们对应完全不同的处置：
// 前两种是正常路径，后两种要进入格式修复或拒绝分类。
type Outcome string

const (
	// OutcomePlainText：没有信号，模型只是正常回答。
	OutcomePlainText Outcome = "plain_text"
	// OutcomeCallsParsed：信号出现且 envelope 完整解析。
	OutcomeCallsParsed Outcome = "calls_parsed"
	// OutcomeTruncated：信号出现但结构在闭合前断掉。
	OutcomeTruncated Outcome = "truncated_after_signal"
	// OutcomeMalformed：信号出现，结构完整但内容非法。
	OutcomeMalformed Outcome = "malformed_after_signal"
)

// Result 是一次解析的最终结果。
type Result struct {
	Outcome Outcome
	State   State

	// Text 是全部已提交的普通文本。
	Text string

	// Calls 只在 OutcomeCallsParsed 时非空。
	Calls []vproto.Call

	// Err 记录失败原因，供拒绝分类使用。
	Err error

	// Trailing 是 envelope 闭合之后模型多说的那部分内容。
	//
	// 协议规则要求闭合后不再输出，但纯文本模型经常做不到——收个尾
	// 「我已经调用了工具」，或者把整段 envelope 包在 markdown 代码块里
	// 于是末尾多一行 ```。实测这是最常见的违规形态。
	//
	// 为它拒掉一个**已经完整解析出来**的调用，代价远大于收益：agent 会
	// 卡在一句无害的废话上。所以这里容忍它，但不静默——内容记在这，
	// 接入方该把它记进日志。
	//
	// 它**不会**并进 Text 发给客户端：那些内容多半是协议相关的自言自语，
	// 甚至可能带着 markdown 围栏。
	Trailing string
}

// Parser 是虚拟协议的增量状态机解析器。
//
// 核心职责是维护「提交边界」：流式转发时既要边解析边把普通文本发给客户端，
// 又不能把可能是信号开头的字节发出去。一旦提交就不可撤回——已经发到客户端
// 屏幕上的字符收不回来，所以宁可晚提交，不可错提交。
//
// 它不是线程安全的：一条流由一个 goroutine 串行喂入。
type Parser struct {
	nonce  vproto.Nonce
	limits Limits

	state State

	// buf 是当前这一行尚未处理完的字节，行首起算。
	//
	// 注意它与「未提交」不是一回事：一行里靠前的部分可能已经作为普通文本
	// 提交出去了，但字节仍留在 buf 里——因为 think 标签之类的判断需要看到
	// 完整的一行。lineCommitted 记录已经提交了多少，避免重复发送。
	buf []byte

	// lineCommitted 是当前这一行已提交的字节数。
	lineCommitted int

	// text 累积已提交的普通文本。
	text bytes.Buffer

	// envelope 累积信号之后的结构内容。
	envelope bytes.Buffer

	// signalSeen 记录信号是否出现过。规格要求信号只能出现一次，
	// 第二次出现按歧义拒绝而不是取最后一个。
	signalSeen bool

	totalBytes int
	calls      []vproto.Call
	err        error

	// trailing 累积 envelope 闭合之后模型多说的内容，见 Result.Trailing。
	trailing strings.Builder

	// trailWindow 是尾部信号检测的滑动窗口，见 trailingHasSignal。
	trailWindow []byte
}

// New 创建一个解析器。
func New(nonce vproto.Nonce, limits Limits) (*Parser, error) {
	if nonce.Zero() {
		return nil, fmt.Errorf("parser: 需要一个已初始化的 nonce")
	}
	return &Parser{
		nonce:  nonce,
		limits: limits.withDefaults(),
		state:  StateText,
	}, nil
}

// State 返回当前状态。
func (p *Parser) State() State { return p.state }

// Write 喂入一段字节，返回本次可以安全提交为普通文本的部分。
//
// 返回的字节一旦交给调用方就视为已发出，不可撤回。因此这里的判断标准是
// 「确定不可能是协议内容」而不是「看起来像普通文本」。
func (p *Parser) Write(chunk []byte) ([]byte, error) {
	if p.state == StateFailed {
		return nil, p.err
	}
	if p.state == StateDone {
		// envelope 闭合之后不该再有内容，但纯文本模型经常收个尾。
		// 记下来，不失败——理由见 Result.Trailing 的注释。
		return nil, p.addTrailing(chunk)
	}

	p.totalBytes += len(chunk)
	if p.totalBytes > p.limits.MaxTotalBytes {
		return nil, p.fail(fmt.Errorf("parser: 响应累积 %d 字节超过上限 %d",
			p.totalBytes, p.limits.MaxTotalBytes))
	}

	p.buf = append(p.buf, chunk...)
	return p.drain()
}

// drain 尽可能多地处理 buf 里的内容。
func (p *Parser) drain() ([]byte, error) {
	var committed []byte

	for {
		if p.state == StateEnvelope {
			done, err := p.feedEnvelope()
			if err != nil {
				return committed, err
			}
			if !done {
				return committed, nil
			}
			continue
		}

		// 行边界只看 \n：\r 留在行内容里由 trim 处理。这样 CRLF 与 LF 都能
		// 正确切分，而多字节 UTF-8 字符不含这两个字节，被劈开也不影响判断。
		i := bytes.IndexByte(p.buf, '\n')

		// 信号检测不依赖行边界：模型经常把信号直接接在正文后面，不换行。
		// 实测约一半的调用长这样。要求信号独占一行会让这些调用被判成普通
		// 文本，于是整段协议 XML 原样吐给客户端——比误判严重得多。
		//
		// 判据改成「信号 + 其后必须是合法 envelope」：切进 StateEnvelope 后
		// 结构不合法就是 malformed，模型单纯复述协议说明时不会紧跟一个结构
		// 完整的信封。这比「独占一行」更贴近真实意图。
		if sigAt := p.findSignal(); sigAt >= 0 {
			out, err := p.enterEnvelope(sigAt)
			if err != nil {
				return committed, err
			}
			committed = append(committed, out...)
			continue
		}

		if i < 0 {
			out, err := p.tryCommitPartial()
			if err != nil {
				return committed, err
			}
			committed = append(committed, out...)
			return committed, nil
		}

		line := p.buf[:i+1] // 含换行符，且是从行首起算的完整一行
		alreadySent := p.lineCommitted
		p.buf = p.buf[i+1:]
		p.lineCommitted = 0

		out, err := p.handleLine(line, alreadySent)
		if err != nil {
			return committed, err
		}
		committed = append(committed, out...)
	}
}

// findSignal 在 buf 里定位本轮信号，返回它的起始下标；没有则返回 -1。
//
// think 区域内不检测：模型在思考里复述协议格式是常见的，把那当成发起调用
// 会凭空造出一个模型并未打算执行的工具调用。状态位只在行结束时更新，所以
// 这里还要额外看 buf 内有没有未闭合的 think 开标签——信号与它同处一行且
// 尚未换行时，光看状态位是不够的。
func (p *Parser) findSignal() int {
	if p.state != StateText || p.nonce.Zero() {
		return -1
	}
	idx := bytes.Index(p.buf, []byte(p.nonce.Signal()))
	if idx < 0 {
		return -1
	}
	if open := bytes.Index(p.buf[:idx], []byte(thinkOpen)); open >= 0 {
		if !bytes.Contains(p.buf[open:idx], []byte(thinkClose)) {
			return -1 // 信号落在未闭合的 think 里
		}
	}
	return idx
}

// enterEnvelope 在 buf 的 sigAt 处切进 envelope 状态。
//
// 信号之前的部分是正文，补发出去；信号本身一个字节都不发；信号之后的内容
// 留在 buf 里，由 feedEnvelope 接手。
func (p *Parser) enterEnvelope(sigAt int) ([]byte, error) {
	if p.lineCommitted > sigAt {
		// 信号的字节已经被提交出去了，收不回来。走到这里说明提交窗口
		// 算错了，属于程序缺陷而非输入问题。
		return nil, p.fail(fmt.Errorf(
			"parser: 内部错误——已提交 %d 字节，但信号起于第 %d 字节", p.lineCommitted, sigAt))
	}
	if p.signalSeen {
		// 规格：重复信号按策略拒绝，不取「最后一个」掩盖歧义。
		// 两个信号意味着模型给出了两套互相矛盾的调用，
		// 选任何一个都是替调用方做了它没授权的决定。
		return nil, p.fail(fmt.Errorf("parser: 信号出现了第二次，存在歧义"))
	}

	var out []byte
	if sigAt > p.lineCommitted {
		out = p.buf[p.lineCommitted:sigAt]
		p.text.Write(out)
	}

	p.signalSeen = true
	p.state = StateEnvelope
	p.buf = p.buf[sigAt+len(p.nonce.Signal()):]
	p.lineCommitted = 0
	return out, nil
}

// handleLine 处理一个完整的行（含结尾换行符）。
//
// 信号已经在 drain 里按字节位置处理过了，这里只管 think 标签与普通文本。
//
// alreadySent 是这一行中已经提前提交出去的字节数——只有确定不是信号的字节
// 才会走到那一步，所以这里只需要把剩下的部分补发出去，不能重发。
func (p *Parser) handleLine(line []byte, alreadySent int) ([]byte, error) {
	if len(line) > p.limits.MaxLineBytes {
		return nil, p.fail(fmt.Errorf("parser: 单行 %d 字节超过上限 %d",
			len(line), p.limits.MaxLineBytes))
	}

	s := string(line)

	// 剩余待发的部分。
	rest := line
	if alreadySent > 0 && alreadySent <= len(line) {
		rest = line[alreadySent:]
	}

	// think 区域内不检测信号。模型在思考里复述协议格式是常见的，
	// 把那当成发起调用会凭空造出一个模型并未打算执行的工具调用。
	if p.state == StateThinking {
		p.text.Write(rest)
		if strings.Contains(s, thinkClose) {
			p.state = StateText
		}
		return rest, nil
	}
	if strings.Contains(s, thinkOpen) && !strings.Contains(s, thinkClose) {
		p.state = StateThinking
		p.text.Write(rest)
		return rest, nil
	}

	// 形状像信号但不是本轮的：模型回放了历史轮次，或有人在尝试注入。
	// 当普通文本透传，但这一行值得被上层记一笔。
	p.text.Write(rest)
	return rest, nil
}

// tryCommitPartial 判断当前不完整行里还能提交多少。
//
// 只保留末尾那段「还可能长成信号开头」的字节，其余都可以安全提交。信号
// 可以出现在行内任意位置，所以判据是后缀-前缀匹配，而不是整行前缀。
func (p *Parser) tryCommitPartial() ([]byte, error) {
	if len(p.buf) == 0 {
		return nil, nil
	}
	if len(p.buf) > p.limits.MaxLineBytes {
		return nil, p.fail(fmt.Errorf("parser: 未换行的内容已达 %d 字节，超过行长上限 %d",
			len(p.buf), p.limits.MaxLineBytes))
	}

	end := len(p.buf)
	// think 区域内不需要留窗口——那里根本不检测信号。
	if p.state != StateThinking {
		// 末尾这几个字节可能是信号的开头，必须继续等。这就是待定窗口，
		// 它的长度天然不超过信号本身。
		end -= pendingSignalPrefix(p.buf, []byte(p.nonce.Signal()))
	}

	if end <= p.lineCommitted {
		return nil, nil
	}
	out := p.buf[p.lineCommitted:end]
	p.text.Write(out)
	p.lineCommitted = end
	return out, nil
}

// pendingSignalPrefix 返回 b 末尾有多少字节可能是 signal 的开头。
//
// 信号可以紧接在正文后面，所以任何一段后缀都可能是信号的前半截。取最长的
// 那个匹配：宁可多等几个字节，也不能把信号的头部当正文发出去——已经发到
// 客户端屏幕上的字符收不回来。
//
// 完整的信号本身也算（它是自己的前缀），于是信号刚读完、还没读到后续内容
// 时也会被 hold 住，等 drain 下一轮把它识别出来。
func pendingSignalPrefix(b, signal []byte) int {
	if len(signal) == 0 {
		return 0
	}
	n := len(signal)
	if n > len(b) {
		n = len(b)
	}
	for ; n > 0; n-- {
		if bytes.HasPrefix(signal, b[len(b)-n:]) {
			return n
		}
	}
	return 0
}

// feedEnvelope 把 buf 里的内容并入 envelope 缓冲，返回 envelope 是否已闭合。
func (p *Parser) feedEnvelope() (bool, error) {
	if len(p.buf) == 0 {
		return false, nil
	}

	p.envelope.Write(p.buf)
	p.buf = nil
	p.lineCommitted = 0

	if p.envelope.Len() > p.limits.MaxEnvelopeBytes {
		return false, p.fail(fmt.Errorf("parser: envelope 累积 %d 字节超过上限 %d",
			p.envelope.Len(), p.limits.MaxEnvelopeBytes))
	}

	idx := bytes.Index(p.envelope.Bytes(), []byte(vproto.TagEnvelopeClose))
	if idx < 0 {
		return false, nil
	}

	full := p.envelope.Bytes()[:idx+len(vproto.TagEnvelopeClose)]
	rest := bytes.TrimSpace(p.envelope.Bytes()[idx+len(vproto.TagEnvelopeClose):])

	calls, err := parseEnvelope(full, p.limits)
	if err != nil {
		return false, p.fail(err)
	}
	p.calls = calls
	p.state = StateDone

	if err := p.addTrailing(rest); err != nil {
		return false, err
	}
	return true, nil
}

// maxTrailingBytes 是尾部噪声的记录上限。
//
// 有上限是因为它来自模型，而模型可能一直说下去。超出的部分丢掉——
// 记这段内容是为了让违规可见，不是为了完整保存它。
const maxTrailingBytes = 4096

// addTrailing 收下 envelope 闭合之后的内容。
//
// 唯一仍然失败的情形是尾部又出现了信号：那不是收尾的废话，而是模型想
// 发起第二次调用。两个调用意图摆在一起，取哪个都是替模型做决定——
// 「信号只能出现一次」是硬规则，这里必须按歧义拒绝。
func (p *Parser) addTrailing(b []byte) error {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	if p.trailingHasSignal(b) {
		return p.fail(fmt.Errorf("parser: envelope 闭合后又出现了信号，存在歧义"))
	}
	if room := maxTrailingBytes - p.trailing.Len(); room > 0 {
		if len(b) > room {
			b = b[:room]
		}
		p.trailing.Write(b)
	}
	return nil
}

// trailingHasSignal 在尾部内容里找信号，跨块也要找得到。
//
// 用滑动窗口而不是在累积的 trailing 上找，有两个原因：流式喂入时信号会被
// chunk 切碎，逐块检查一个都发现不了；而 trailing 有字节上限，超限之后
// 出现的信号会因为没被保存而检测不到——那正好是「模型说了很多废话之后
// 又发起一次调用」，最该被拦住的情形。
func (p *Parser) trailingHasSignal(b []byte) bool {
	if p.nonce.Zero() {
		return false
	}
	sig := []byte(p.nonce.Signal())
	joined := append(p.trailWindow, b...)
	if bytes.Contains(joined, sig) {
		return true
	}
	// 只留可能构成信号前缀的那一小段，窗口本身不增长。
	if n := len(sig) - 1; len(joined) > n {
		joined = joined[len(joined)-n:]
	}
	p.trailWindow = append(p.trailWindow[:0], joined...)
	return false
}

func (p *Parser) fail(err error) error {
	p.state = StateFailed
	p.err = err
	return err
}

// Close 结束流并返回最终结果。
//
// 四种结局的区分是规格九章的明确要求：它们对应完全不同的处置，
// 混为一谈会让「模型正常回答」和「模型的调用被截断」得到同样的处理。
func (p *Parser) Close() Result {
	// 最后一行可能没有换行符就结束了，但它仍然可能是一个完整的信号行。
	//
	// 这是 FuzzParse 抓到的缺陷：原先无条件把残留内容冲进普通文本，
	// 于是一个「信号在末尾且没换行」的输出会把信号原样发给客户端——
	// 屏幕上出现一串本该被消化掉的协议标记。正确的判定是：模型给出了信号
	// 却没跟上 envelope，属于信号后结构截断。
	if p.state == StateText && p.lineCommitted == 0 && len(p.buf) > 0 &&
		p.nonce.Matches(string(p.buf)) {
		p.signalSeen = true
		p.buf = nil
	} else if p.state != StateEnvelope && p.state != StateFailed && p.lineCommitted < len(p.buf) {
		p.text.Write(p.buf[p.lineCommitted:])
		p.lineCommitted = len(p.buf)
	}

	r := Result{State: p.state, Text: p.text.String(), Calls: p.calls, Err: p.err,
		Trailing: p.trailing.String()}

	switch {
	case p.state == StateFailed:
		r.Outcome = OutcomeMalformed
	case p.state == StateDone:
		r.Outcome = OutcomeCallsParsed
	case p.signalSeen:
		// 见过信号但没闭合：流断在了结构中间。
		r.Outcome = OutcomeTruncated
		r.Err = fmt.Errorf("parser: 信号之后的结构在闭合前中断")
		p.state = StateFailed
		r.State = StateFailed
	default:
		r.Outcome = OutcomePlainText
	}
	return r
}

// CommittedText 返回目前已提交的全部普通文本。
func (p *Parser) CommittedText() string { return p.text.String() }
