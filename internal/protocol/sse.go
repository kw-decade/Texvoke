package protocol

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// SSE 帧层：Server-Sent Events 协议本身的增量解析与渲染，三种上层协议共用。
//
// 规格九章要求这里是一个真正的增量状态机，而不是「把完整响应拼接起来再用
// 正则处理」。实践中上游的 delta 可能以任意边界切开——前身实测约 8-9
// 个字符一块——所以下面每一处都不能假设「一个读取单位对应一个语义单元」。

// DefaultMaxSSELineBytes 是单行的默认上限。
//
// 一条没有换行的超长行会让解析器无限累积。SSE 的 data 行在实践中不会太大，
// 真需要传大对象应该用 artifact 引用而不是塞进事件流。
const DefaultMaxSSELineBytes = 1 << 20 // 1 MiB

// DefaultMaxSSEEventBytes 是单个事件（多行 data 拼接后）的默认上限。
const DefaultMaxSSEEventBytes = 8 << 20 // 8 MiB

// Event 是一个 SSE 事件。
type Event struct {
	// Type 来自 event: 行。规范规定缺省为 "message"，这里保持空串而不是
	// 补上默认值——上层需要区分「上游显式写了 message」和「上游根本没写
	// event 行」，前者是有意为之，后者可能意味着代理剥掉了字段。
	Type string

	// Data 是所有 data: 行用 \n 连接后的结果。
	Data []byte

	// ID 来自 id: 行，Retry 来自 retry: 行（毫秒）。
	ID    string
	Retry int

	// Comment 保存以 : 开头的注释行内容。Anthropic 用注释行做心跳（ping），
	// 丢掉它会让「连接还活着」与「上游卡住了」变得无法区分。
	Comment string
}

// IsComment 报告这是否是一个纯注释事件（心跳），不含任何字段。
func (e Event) IsComment() bool {
	return e.Comment != "" && e.Type == "" && len(e.Data) == 0 && e.ID == ""
}

// SSEDecoderOptions 配置解析器的资源上限。
type SSEDecoderOptions struct {
	// MaxLineBytes 为 0 时取 DefaultMaxSSELineBytes。
	MaxLineBytes int
	// MaxEventBytes 为 0 时取 DefaultMaxSSEEventBytes。
	MaxEventBytes int
}

// SSEDecoder 从字节流里增量解析 SSE 事件。
type SSEDecoder struct {
	scanner       *bufio.Scanner
	maxEventBytes int

	// 累积中的事件字段。
	eventType  string
	data       []byte
	id         string
	retry      int
	comment    strings.Builder
	hasField   bool
	dataWasSet bool
}

// NewSSEDecoder 创建一个增量解析器。
//
// 它不假设 r 每次返回完整的一行或一个事件：底层 bufio.Scanner 会持续读取
// 直到攒够一个完整的行。因此上游按任意字节边界切分（哪怕一次一个字节，
// 哪怕把一个 UTF-8 字符劈成两半）都能正确还原——多字节字符只是行内容的
// 一部分，行边界只看 \r 与 \n 这两个 ASCII 字节。
func NewSSEDecoder(r io.Reader, opts SSEDecoderOptions) *SSEDecoder {
	maxLine := opts.MaxLineBytes
	if maxLine <= 0 {
		maxLine = DefaultMaxSSELineBytes
	}
	maxEvent := opts.MaxEventBytes
	if maxEvent <= 0 {
		maxEvent = DefaultMaxSSEEventBytes
	}

	sc := bufio.NewScanner(r)
	// 初始缓冲区不能大于上限：Scanner 只在「缓冲区已满且其长度 >= 上限」时
	// 才报 ErrTooLong，初始就给 4096 而上限是 100 的话，一条 1000 字节的行
	// 会被安然读进来，上限等于没设。
	initial := 4096
	if maxLine < initial {
		initial = maxLine
	}
	sc.Buffer(make([]byte, 0, initial), maxLine)
	sc.Split(splitSSELines)

	return &SSEDecoder{scanner: sc, maxEventBytes: maxEvent}
}

// splitSSELines 按 SSE 规范切分行：CRLF、LF、CR 三种都算行结束符。
//
// 这不是 bufio.ScanLines 能替代的——后者只认 \n，并且把结尾的 \r 当成
// 内容的一部分保留。上游若用裸 \r 分行（少见但规范允许），ScanLines 会把
// 整个流当成一行，最终撞上行长上限。
func splitSSELines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}

	for i := 0; i < len(data); i++ {
		switch data[i] {
		case '\n':
			return i + 1, data[:i], nil
		case '\r':
			// \r 后面可能跟 \n，也可能是行尾本身。若 \r 恰好是当前缓冲区的
			// 最后一个字节，必须等下一次读取才能判断，否则会把 CRLF 误当成
			// 两个空行——而空行在 SSE 里是事件边界，多切一个就多出一个空事件。
			if i+1 < len(data) {
				if data[i+1] == '\n' {
					return i + 2, data[:i], nil
				}
				return i + 1, data[:i], nil
			}
			if atEOF {
				return i + 1, data[:i], nil
			}
			return 0, nil, nil // 请求更多数据
		}
	}

	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// Next 返回下一个完整事件。流正常结束时返回 io.EOF。
func (d *SSEDecoder) Next() (*Event, error) {
	for d.scanner.Scan() {
		line := d.scanner.Bytes()

		// 空行是事件边界。
		if len(line) == 0 {
			if !d.hasField {
				// 连续空行或流开头的空行：不产出空事件。
				continue
			}
			ev := d.flush()
			return ev, nil
		}

		// 以 : 开头是注释行。Anthropic 的 ping 心跳走这条路，不能丢。
		if line[0] == ':' {
			if d.comment.Len() > 0 {
				d.comment.WriteByte('\n')
			}
			d.comment.Write(bytes.TrimPrefix(line[1:], []byte(" ")))
			d.hasField = true
			continue
		}

		field, value := splitSSEField(line)
		switch field {
		case "event":
			d.eventType = string(value)
		case "data":
			// 多行 data 用 \n 连接，这是规范定义的行为。
			if d.dataWasSet {
				d.data = append(d.data, '\n')
			} else if d.data == nil {
				// 置成非 nil 的空切片：上层用 Data == nil 区分「上游根本没写
				// data 字段」与「写了但值为空」。前者该跳过，后者是一个内容
				// 为空的合法事件。
				d.data = []byte{}
			}
			d.data = append(d.data, value...)
			d.dataWasSet = true
			if len(d.data) > d.maxEventBytes {
				return nil, fmt.Errorf("protocol: SSE 事件数据 %d 字节超过上限 %d", len(d.data), d.maxEventBytes)
			}
		case "id":
			// 规范要求含 NUL 的 id 被忽略，而不是截断后使用。
			if !bytes.ContainsRune(value, 0) {
				d.id = string(value)
			}
		case "retry":
			if n, err := strconv.Atoi(string(value)); err == nil && n >= 0 {
				d.retry = n
			}
			// 非法的 retry 按规范忽略，不报错——它只是重连建议，
			// 不值得为此中断一条本来正常的流。
		default:
			// 未知字段按规范忽略。这里不报错是有意的：SSE 允许扩展，
			// 遇到没见过的字段就中断会让 Bridge 在上游升级后突然罢工。
		}
		d.hasField = true
	}

	if err := d.scanner.Err(); err != nil {
		if err == bufio.ErrTooLong {
			return nil, fmt.Errorf("protocol: SSE 单行超过长度上限")
		}
		return nil, fmt.Errorf("protocol: 读取 SSE 流失败：%w", err)
	}

	// 流结束时若还有半个事件，说明上游断在了事件中间。这与「正常结束」
	// 是两回事，必须区分——规格九章要求流结束时能分辨「普通文本完成」
	// 与「信号后结构截断」。
	if d.hasField {
		return nil, fmt.Errorf("protocol: SSE 流在事件中途结束，最后一个事件不完整")
	}
	return nil, io.EOF
}

// flush 产出累积好的事件并重置状态。
func (d *SSEDecoder) flush() *Event {
	ev := &Event{
		Type:    d.eventType,
		ID:      d.id,
		Retry:   d.retry,
		Comment: d.comment.String(),
	}
	if d.dataWasSet {
		ev.Data = d.data
	}

	d.eventType = ""
	d.data = nil
	d.id = ""
	d.retry = 0
	d.comment.Reset()
	d.hasField = false
	d.dataWasSet = false
	return ev
}

// splitSSEField 把一行拆成字段名与值。
//
// 规范：第一个冒号之前是字段名，之后是值；值前若有一个空格要去掉。
// 没有冒号的行整行都是字段名，值为空串。
func splitSSEField(line []byte) (field string, value []byte) {
	i := bytes.IndexByte(line, ':')
	if i < 0 {
		return string(line), nil
	}
	field = string(line[:i])
	value = line[i+1:]
	// 只去掉一个前导空格，不是 TrimSpace——data 的值可能有意以空格开头。
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	return field, value
}

// SSEEncoder 把事件写成 SSE 线上格式。
type SSEEncoder struct {
	w io.Writer
}

// NewSSEEncoder 创建一个渲染器。
func NewSSEEncoder(w io.Writer) *SSEEncoder {
	return &SSEEncoder{w: w}
}

// Write 渲染一个事件。
//
// data 里的换行会被拆成多个 data: 行，这是规范要求的——直接写进去会让
// 换行后的内容被解析成一个新字段，客户端拿到的 JSON 就断成了两截。
func (e *SSEEncoder) Write(ev Event) error {
	var buf bytes.Buffer

	if ev.Comment != "" {
		for _, line := range strings.Split(ev.Comment, "\n") {
			buf.WriteString(": ")
			buf.WriteString(line)
			buf.WriteByte('\n')
		}
	}
	if ev.Type != "" {
		if strings.ContainsAny(ev.Type, "\r\n") {
			return fmt.Errorf("protocol: SSE 事件类型不得含换行：%q", ev.Type)
		}
		buf.WriteString("event: ")
		buf.WriteString(ev.Type)
		buf.WriteByte('\n')
	}
	if ev.ID != "" {
		// NUL 与换行一样必须拒绝，但理由不同：换行会伪造出额外的字段行，
		// 而 SSE 规范要求接收端**忽略**含 NUL 的 id。照写不误的话，
		// 对端会静默丢掉这个 id，我们却以为自己发出去了。
		if strings.ContainsAny(ev.ID, "\r\n\x00") {
			return fmt.Errorf("protocol: SSE 事件 id 不得含换行或 NUL：%q", ev.ID)
		}
		buf.WriteString("id: ")
		buf.WriteString(ev.ID)
		buf.WriteByte('\n')
	}
	if ev.Retry > 0 {
		buf.WriteString("retry: ")
		buf.WriteString(strconv.Itoa(ev.Retry))
		buf.WriteByte('\n')
	}
	if ev.Data != nil {
		// 按 \n 拆分。注意 \r 也要处理：留在 data 里会被接收端当成行尾，
		// 后半截内容变成一个未知字段而被丢弃。
		normalized := bytes.ReplaceAll(ev.Data, []byte("\r\n"), []byte("\n"))
		normalized = bytes.ReplaceAll(normalized, []byte("\r"), []byte("\n"))
		for _, line := range bytes.Split(normalized, []byte("\n")) {
			buf.WriteString("data: ")
			buf.Write(line)
			buf.WriteByte('\n')
		}
	}

	// 空事件不写：它在接收端会被当成一个事件边界，凭空多出一个空事件。
	if buf.Len() == 0 {
		return nil
	}
	buf.WriteByte('\n')

	_, err := e.w.Write(buf.Bytes())
	return err
}

// Flush 把缓冲的数据推给客户端。
//
// 流式响应的意义在于「边生成边送达」，不 Flush 的话数据会积在
// http.ResponseWriter 的缓冲里，客户端要等整个响应结束才一次性收到——
// 那就退化成了非流式，还多花了协议开销。
func (e *SSEEncoder) Flush() error {
	f, ok := e.w.(interface{ Flush() error })
	if ok {
		return f.Flush()
	}
	// net/http 的 ResponseWriter 用的是不返回错误的 Flush。
	if f, ok := e.w.(interface{ Flush() }); ok {
		f.Flush()
	}
	return nil
}
