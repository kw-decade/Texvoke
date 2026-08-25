// Package toolbridge 是 Texvoke 的公开门面：把「让纯文本模型支持工具
// 调用」这件事包装成三个方法，供任意反代接入。
//
// 典型用法：
//
//	b, _ := toolbridge.New(toolbridge.Config{})
//	sess, _ := b.NewSession("sess-1", "req-1")
//
//	// 请求进来时：把工具定义编译进 system prompt
//	prompt, _ := sess.Compile(tools, toolbridge.CompileOptions{})
//
//	// 模型回复后：解析出工具调用
//	res, _ := sess.Parse(modelOutput)
//	for _, c := range res.Calls { ... }
//
// 会话是一等公民：协议信号（nonce）绑定在它上面，整个会话内不变。
// 这不是为了省事——提示词注入与历史回灌必须用同一个信号，否则模型看到的
// 历史示例与当前规范自相矛盾，工具调用会永久解析失败。这是从前身项目
// 实测出来的教训：教模型的示例格式与解析器认的格式必须同源渲染。
//
// 本包是稳定 API。internal/ 下的包不是，也不应被外部引用——
// 那道边界由 Go 编译器强制。
package toolbridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/kw-decade/Texvoke/internal/ir"
	"github.com/kw-decade/Texvoke/internal/parser"
	"github.com/kw-decade/Texvoke/internal/prompt"
	"github.com/kw-decade/Texvoke/internal/vproto"
)

// Tool 是一个工具声明。
//
// 三种客户端协议的工具定义形状各不相同（Chat 有 function 包装、
// Anthropic 用 input_schema、Responses 是扁平的），归一化成这个形状是
// 接入方的事——它比框架更清楚自己面对的是哪种客户端。
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`

	// Freeform 标明这个工具收的是一段裸文本而不是 JSON 对象
	// （Responses 协议的 custom 类型，Codex 的 exec 就是这一类）。
	//
	// 为 true 时 InputSchema 必须为空：给裸文本工具补一份空对象 schema，
	// Prompt 会照着教模型写 JSON，模型输出 {} 而不是内容——不报错，
	// 只是永远调不动。
	Freeform bool `json:"freeform,omitempty"`
}

// Call 是解析出的一次工具调用。
type Call struct {
	// ID 是模型在本次 envelope 内给出的本地标识，用于把结果关联回来。
	ID string `json:"id"`
	// Name 是工具名，与声明时逐字一致。
	Name string `json:"name"`
	// Arguments 是参数。常规工具是一个 JSON 对象；Freeform 为 true 时，
	// 它是一个 JSON **字符串标量**，真正的内容在字符串里。
	//
	// 之所以裸文本也包成 JSON：这个字段会被直接序列化进 HTTP 响应
	// （/v1/parse），装裸文本会让整个响应变成非法 JSON，接入方在完全
	// 不相干的地方炸掉。
	//
	// 它已经通过形态校验，但**没有**经过 Schema 校验——那需要工具的
	// schema，由接入方决定何时做。
	Arguments json.RawMessage `json:"arguments"`

	// Freeform 标明这次调用的参数是裸文本。
	Freeform bool `json:"freeform,omitempty"`
}

// Outcome 是一次解析的结局。
type Outcome string

const (
	// OutcomePlainText：模型没有发起调用，只是正常回答。
	OutcomePlainText Outcome = "plain_text"
	// OutcomeCallsParsed：解析出了工具调用。
	OutcomeCallsParsed Outcome = "calls_parsed"
	// OutcomeTruncated：模型发出了信号，但结构在闭合前断掉。
	OutcomeTruncated Outcome = "truncated"
	// OutcomeMalformed：模型发出了信号，但结构非法。
	OutcomeMalformed Outcome = "malformed"
)

// Result 是一次解析的结果。
type Result struct {
	// Text 是可以原样转发给客户端的普通文本。
	// 协议标记与 envelope 结构不会出现在这里。
	Text string `json:"text"`

	// Calls 只在 Outcome 为 OutcomeCallsParsed 时非空。
	Calls []Call `json:"calls,omitempty"`

	Outcome Outcome `json:"outcome"`

	// Trailing 是模型在 envelope 闭合之后多说的话。
	//
	// 协议规则要求闭合后不再输出，但纯文本模型经常收个尾「我已经调用了
	// 工具」，或者把整段 envelope 包在 markdown 代码块里于是末尾多一行
	// 围栏。为这个拒掉一个已经完整解析出来的调用，代价远大于收益。
	//
	// 所以它被容忍了，但不静默：内容记在这，也**不会**并进 Text——
	// 那些多半是协议相关的自言自语，不该转发给客户端。接入方应该把它
	// 记进日志，模型总在这里说话说明 Prompt 的约束没生效。
	Trailing string `json:"trailing,omitempty"`

	// UnknownTools 是模型调用了、但本次会话没有声明过的工具名。
	//
	// 调用仍然会出现在 Calls 里（原样，不猜测），但接入方应当据此拒绝
	// 执行并把情况告诉模型，而不是把一个不存在的工具转给客户端——
	// 那样症状会出现在链路的另一端，排查方向完全错误。
	//
	// 会话用 RestoreSession 重建时这里恒为空：手上没有工具名单，
	// 就不该假装能判断。
	UnknownTools []string `json:"unknown_tools,omitempty"`
}

// UpstreamProfile 描述上游的脾气。
//
// 这些都是上游特定的，所以由接入方填而不是框架内置。把某一家的噪声正则
// 硬编码进核心，等于宣告这个框架只服务那一家。
type UpstreamProfile struct {
	// NoiseFilters 会从模型输出里剥掉。
	//
	// 用途是过滤上游的品牌噪声——有些被调教过的模型每次回复都带
	// 「你好我是 XX 助手」开头和「还想了解更多 XX 吗」结尾。
	// 这些文本会混进要转发给客户端的正文里。
	//
	// 流式路径上首尾都能过滤，代价是输出滞后：见 TailHoldBytes。
	NoiseFilters []*regexp.Regexp

	// TailHoldBytes 是流式时在输出末尾扣住不发的字节数。
	//
	// 为什么需要它：尾部规则（`...$`）锚定输出结尾，而流式时**每一刻的
	// 当前结尾都可能不是真正的结尾**。做法是在累积文本上反复跑正则——
	// `$` 匹配当前结尾，一旦匹配就说明从匹配起点起全是噪声，扣住不发。
	//
	// 但有「回溯」问题：`\n如果你.{0,40}?p5\.?js.*$` 这类规则，前半句
	// 到达时还不匹配，等 `p5.js` 到了才匹配，而前半句可能已经发出去了。
	// 保留窗口就是覆盖这段回溯距离——窗口比规则的最大匹配长度大，
	// 尾部噪声就一个字都不会外泄。
	//
	// 配了 NoiseFilters 但没设这个值时取默认 1024 字节。按你的噪声实际
	// 长度设：一两句话的品牌横幅 512 够用，多段落的免责声明要更大。
	// 设成负数显式关闭（尾部噪声会先转发出去，只在最终文本里消失）。
	//
	// 代价是最后这些字节延迟到流结束才发出。对「模型说完一段话」这个
	// 尺度来说，1 KB 的延迟通常察觉不到。
	TailHoldBytes int

	// MaxToolsInPrompt 是进入 Prompt 的工具数上限，0 取默认值 24。
	//
	// 默认值来自实测而非拍脑袋：真实的 Claude Code 会带 80 多个工具，
	// 全量描述几十 KB，足以把协议指令稀释到模型不再遵守。
	MaxToolsInPrompt int

	// MaxToolDescBytes 是单个工具描述的字节上限，0 取默认值 2000。
	// 只截断描述文字，schema 原样保留——类型、必填、枚举是校验的依据。
	//
	// 默认值从 200 提到 2000 是实测改的：现代 agent 的工具描述普遍几 KB，
	// 真实 Codex 的 exec 有 11 KB，而真正说明它能跑 shell 的部分在第 4.7 KB
	// 处——截到 200 字节等于把说明书撕掉只留封面，模型会说「我没有这个工具」。
	//
	// 截断存在的理由仍然成立（协议指令会被工具描述稀释），但那个问题现在
	// 由末尾的格式提醒解决，不必再靠压缩描述来换。接 Codex 这类描述特别长的
	// 客户端时，把它调到 16000 并配合提醒，实测 4/4 正确调用。
	MaxToolDescBytes int
}

// Limits 是资源上限。零值各项都取保守的默认值。
type Limits struct {
	MaxOutputBytes   int
	MaxLineBytes     int
	MaxEnvelopeBytes int
	MaxCalls         int
	MaxArgumentBytes int
	MaxJSONDepth     int
}

// Config 是框架配置。零值可用。
type Config struct {
	Upstream UpstreamProfile
	Limits   Limits
}

// DefaultTailHoldBytes 是尾部保留窗口的默认值。
//
// 1 KB 足以覆盖绝大多数品牌横幅与免责声明。真实的尾部噪声长这样：
// 「\n\n如果你想了解更多 p5.js 的用法，随时问我。」——不到 100 字节。
const DefaultTailHoldBytes = 1024

// Bridge 是框架入口，可安全地被多个 goroutine 共享。
//
// 它本身不持有可变状态——每次请求的状态都在 Session 里。
type Bridge struct {
	profile   UpstreamProfile
	limits    parser.Limits
	holdBytes int
}

// New 创建一个 Bridge。
func New(cfg Config) (*Bridge, error) {
	p := cfg.Upstream
	if p.MaxToolsInPrompt <= 0 {
		p.MaxToolsInPrompt = 24
	}
	if p.MaxToolDescBytes <= 0 {
		p.MaxToolDescBytes = 2000
	}

	// 零值取默认而非关闭：一个配了尾部规则却忘了设窗口的接入方，
	// 应当默认拿到正确行为，而不是默认漏噪声。要关闭得显式写负数。
	hold := p.TailHoldBytes
	switch {
	case hold < 0:
		hold = 0
	case hold == 0:
		hold = DefaultTailHoldBytes
	}

	return &Bridge{
		profile:   p,
		holdBytes: hold,
		limits: parser.Limits{
			MaxTotalBytes:    cfg.Limits.MaxOutputBytes,
			MaxLineBytes:     cfg.Limits.MaxLineBytes,
			MaxEnvelopeBytes: cfg.Limits.MaxEnvelopeBytes,
			MaxCalls:         cfg.Limits.MaxCalls,
			MaxArgumentBytes: cfg.Limits.MaxArgumentBytes,
			MaxDepth:         cfg.Limits.MaxJSONDepth,
		},
	}, nil
}

// Session 是一次会话。协议信号绑定在它上面，整个生命周期内不变。
//
// 一个 Session 对应客户端的一次请求（或一轮多次交互）。它不是并发安全的：
// 同一个会话的编译与解析由同一条执行路径串行完成。
type Session struct {
	bridge *Bridge
	nonce  vproto.Nonce

	// tools 记录本会话声明过的工具，供解析后核对工具名。
	tools map[string]Tool
}

// NewSession 开一个会话。
//
// sessionID 与 requestID 会绑进信号，用于诊断「这段输出是不是本轮的」——
// 多轮对话里上游可能把历史消息原样回显，回显里的旧信号不该触发新调用。
func (b *Bridge) NewSession(sessionID, requestID string) (*Session, error) {
	n, err := vproto.NewNonce(sessionID, requestID)
	if err != nil {
		return nil, wrap(ErrConfig, err)
	}
	return &Session{bridge: b, nonce: n, tools: make(map[string]Tool, 8)}, nil
}

// RestoreSession 用一个已知的信号值重建会话。
//
// 这是无状态接入的关键：HTTP sidecar 不保存任何会话，Compile 时把信号值
// 返回给调用方，Parse 时调用方带回来重建。服务端因此不需要清理会话、
// 不会内存泄漏，也可以随便重启或横向扩容。
//
// 把信号交给调用方保管是安全的：它本来就要写进 system prompt 让模型看到，
// 而且它**不是身份凭证**——篡改它只会让解析失败，不会绕过任何检查。
func (b *Bridge) RestoreSession(sessionID, requestID, nonceValue string) (*Session, error) {
	n, err := vproto.NonceFromValue(nonceValue, sessionID, requestID)
	if err != nil {
		return nil, wrap(ErrConfig, err)
	}
	return &Session{bridge: b, nonce: n, tools: make(map[string]Tool, 8)}, nil
}

// Signal 返回本会话的协议信号。
//
// 一般不需要用到——Compile 已经把它写进 Prompt，Parse 会自己识别。
// 暴露它是为了让接入方能在日志里核对两端用的是同一个信号。
// 注意它属于会话内部约定，**不是身份凭证**，不能用来做任何授权判断。
func (s *Session) Signal() string { return s.nonce.Signal() }

// NonceValue 返回信号的随机部分，供 RestoreSession 重建会话。
//
// 与 Signal 的区别：Signal 是写进 Prompt 的完整标记，NonceValue 是它的
// 内核。跨进程传递用后者——前者带着协议前后缀，混进别的文本里会被
// 解析器当成真的信号。
func (s *Session) NonceValue() string { return s.nonce.Value() }

// CompileOptions 调整单次编译。
type CompileOptions struct {
	// Query 用于工具候选排序，通常传用户最近一条消息。
	// 为空时保持工具的原始顺序。
	Query string

	// AlwaysInclude 列出无论相关性如何都必须进入 Prompt 的工具名。
	AlwaysInclude []string

	// RequireCall 对应 tool_choice=required/named。
	//
	// 它只会变成 Prompt 里的一句约束，**不是保证**。模型没调用时
	// 接入方必须返回明确状态，绝不能伪造一个调用来满足它。
	RequireCall bool
	// RequiredTool 非空时要求模型调用指定的工具。
	RequiredTool string

	// ExtraInstructions 追加在协议说明之后的补充指令。
	//
	// 存在这个口子是因为「怎么用工具」有一部分知识是客户端特定的，通用
	// 中间件不该内置。举一个真实的例子：Codex 的沙箱审批要通过工具参数
	// 发起（shell_command 带 sandbox_permissions: "require_escalated"），
	// 而弱一点的模型会退回到用自然语言问用户「请允许我访问该目录」——
	// 信息它都看到了，就是没把两件事连起来。
	//
	// 这类提示应该由接入方写：它知道自己面对的是哪个客户端。中间件只
	// 提供位置。
	//
	// 边界：这里写的是**怎么用已有机制**，不是**绕过机制**。教模型发起
	// 审批调用是对的；写一句「你可以写任何目录」既越界又没用——它只改变
	// 模型的认知，不改变客户端的执行，结果是从「礼貌地问你」变成「反复失败」。
	ExtraInstructions string

	// MaxToolDescBytes 覆盖本次编译的工具描述上限，0 用 Bridge 的配置。
	//
	// 需要按请求覆盖是因为这个值有一对矛盾的约束，而平衡点取决于客户端：
	// 调小了模型不知道工具能干什么（Codex 的 exec 描述有 11 KB，真正说明
	// 它能跑 shell 的部分在第 4 KB 处，截到 200 字节等于把说明书撕掉只留
	// 封面）；调大了协议指令会被工具描述稀释——实测 16 KB 的工具清单配
	// 1 KB 的协议说明，模型就不再按格式输出了。
	MaxToolDescBytes int

	// MaxTools 覆盖本次编译进入 Prompt 的工具数上限，0 用 Bridge 的配置。
	//
	// 默认上限 24 是按「几百个 MCP 工具、按查询挑相关子集」的场景定的。
	// 但 Claude Code 这类 agent 客户端声明的工具里混着大量长描述插件工具
	// （Workflow / Agent / mcp_*，单个描述几 KB），24 个塞进清单后协议
	// 说明会被淹没——实测模型看完几十 KB 的清单就忘了「这些工具我能调用」，
	// 回复「当前会话没有提供文件读取或终端工具」。收紧上限 + AlwaysInclude
	// 核心工具能把清单压回模型能消化的规模。
	MaxTools int
}

// CompileResult 是一次编译的产物。
type CompileResult struct {
	// SystemPrompt 是要拼进 system 消息的内容。
	SystemPrompt string `json:"system_prompt"`

	// Signal 是本会话的协议信号，仅供日志核对。
	Signal string `json:"signal"`

	// ToolsIncluded 是实际进入 Prompt 的工具名。
	ToolsIncluded []string `json:"tools_included"`

	// ToolsDropped 是被候选筛选丢掉的工具数。
	//
	// 不为 0 时值得记一笔：模型「没有用那个工具」的原因可能是它压根
	// 没看见。悄悄截断会让这个问题无从查起。
	ToolsDropped int `json:"tools_dropped"`

	// VirtualProtocol 报告这一轮是否注入了虚拟协议。
	//
	// 客户端没声明任何工具时它是 false：SystemPrompt 为空，请求原样转给
	// 上游，模型的输出就是最终答案。
	//
	// 这个字段是接真实 agent 时逼出来的。Codex 每跑一轮任务，除了主请求
	// 还会发一批不带工具的辅助请求（提取记忆、压缩上下文、起标题）。
	// 实测 24 次请求里 20 次是这种——把它们当错误打回，整个 agent 当场卡死。
	//
	// 调用方可以据此跳过解析。不跳过也是安全的：没有信号的输出会被判为
	// plain_text，原样透传。
	VirtualProtocol bool `json:"virtual_protocol"`

	// Reminder 是一句放在对话末尾的格式提醒。
	//
	// 协议说明在 system 里，而工具清单可能有十几 KB——真实 Codex 的一个
	// exec 描述就有 11 KB。实测 14 KB 清单配 1 KB 说明时，模型就不再按
	// 格式输出了：指令被稀释掉了。把提醒放在最后一条消息之后，它离模型
	// 开始生成的位置最近。
	//
	// PrepareForTextUpstream 已经替你加进消息序列了，这里单独给出一份
	// 只为方便调试与日志核对。
	Reminder string `json:"reminder,omitempty"`
}

// Compile 把工具定义编译成 system prompt。
//
// 返回的 SystemPrompt 应当拼进发给上游的 system 消息。同一个会话内
// 多次调用是允许的（比如候选集随对话变化），信号保持不变。
func (s *Session) Compile(tools []Tool, opts CompileOptions) (*CompileResult, error) {
	if len(tools) == 0 {
		// 没有工具就不该注入协议——注入了模型会以为自己有工具可用，
		// 然后编一个出来。规格把 tools=0 列为需要向上报告的信号。
		return nil, wrap(ErrNoTools, errors.New("toolbridge: 没有工具可编译"))
	}

	decls := make([]ir.ToolDeclaration, 0, len(tools))
	for i, t := range tools {
		d := ir.ToolDeclaration{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		}
		if t.Freeform {
			d.InputForm = ir.InputFormText
			d.InputSchema = nil
		} else if len(d.InputSchema) == 0 {
			d.InputSchema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		if err := d.Validate(); err != nil {
			return nil, wrap(ErrInvalidTool, fmt.Errorf("第 %d 个工具：%w", i, err))
		}
		decls = append(decls, d)
		s.tools[t.Name] = t
	}

	// 候选按查询相关度排序，再按 MaxTools 截断。AlwaysInclude 优先于任何
	// 词面得分，所以对固定工具集的 agent 客户端（Claude Code 声明几十个
	// 工具、中文查询对英文工具名又几乎零匹配），核心工具必须走 AlwaysInclude
	// 进来——否则它们按相关度排不进前 N，被 MaxTools 截掉，模型就说没有工具。
	maxTools := s.bridge.profile.MaxToolsInPrompt
	if opts.MaxTools > 0 {
		maxTools = opts.MaxTools
	}
	sel := prompt.SelectCandidates(decls, prompt.SelectOptions{
		MaxTools:      maxTools,
		Query:         opts.Query,
		AlwaysInclude: opts.AlwaysInclude,
	})

	maxDesc := opts.MaxToolDescBytes
	if maxDesc <= 0 {
		maxDesc = s.bridge.profile.MaxToolDescBytes
	}
	catalog, err := prompt.ToolCatalog(sel.Selected, maxDesc)
	if err != nil {
		return nil, wrap(ErrInvalidTool, err)
	}

	names := make([]string, 0, len(sel.Selected))
	briefs := make([]vproto.ToolBrief, 0, len(sel.Selected))
	for _, c := range sel.Selected {
		names = append(names, c.Tool.Name)
		briefs = append(briefs, vproto.ToolBrief{
			Name: c.Tool.Name, Freeform: c.Tool.InputForm.Text(),
		})
	}

	instructions, err := vproto.Instructions(s.nonce, briefs)
	if err != nil {
		return nil, wrap(ErrConfig, err)
	}
	reminder, err := vproto.Reminder(s.nonce, briefs)
	if err != nil {
		return nil, wrap(ErrConfig, err)
	}

	var b strings.Builder
	b.WriteString(catalog)
	b.WriteString("\n")
	b.WriteString(instructions)
	if c := constraintFor(opts); c != "" {
		b.WriteString("\n")
		b.WriteString(c)
	}
	// 补充指令放在最后：它是对前面规则的补充，越靠后约束越强。
	if opts.ExtraInstructions != "" {
		b.WriteString("\n")
		b.WriteString(opts.ExtraInstructions)
		if !strings.HasSuffix(opts.ExtraInstructions, "\n") {
			b.WriteString("\n")
		}
	}

	return &CompileResult{
		SystemPrompt:    b.String(),
		Signal:          s.nonce.Signal(),
		ToolsIncluded:   names,
		ToolsDropped:    sel.Dropped,
		VirtualProtocol: true,
		Reminder:        reminder,
	}, nil
}

// constraintFor 渲染 tool_choice 约束。
//
// 措辞是陈述而非命令：告诉模型这一轮的预期，而不是压它就范。
// 强硬措辞在模型确实受政策约束时会变成绕过安全策略的尝试。
func constraintFor(opts CompileOptions) string {
	switch {
	case opts.RequiredTool != "":
		return fmt.Sprintf("这一轮预期会用到 `%s` 这个工具。", opts.RequiredTool)
	case opts.RequireCall:
		return "这一轮预期至少会用到一个工具。"
	default:
		return ""
	}
}

// Parse 解析一段完整的模型输出。
//
// 上游是流式的话用 NewStreamParser——那条路径能在信号出现之前就把安全的
// 文本转发出去，而这个方法要等全部内容到齐。
func (s *Session) Parse(text string) (*Result, error) {
	sp, err := s.NewStreamParser()
	if err != nil {
		return nil, err
	}
	if _, err := sp.Write([]byte(text)); err != nil {
		res := sp.Close()
		return &res, err
	}
	res := sp.Close()
	if res.Outcome == OutcomeMalformed || res.Outcome == OutcomeTruncated {
		return &res, sp.err()
	}
	return &res, nil
}

// StreamParser 增量解析模型输出。
//
// 它维护「提交边界」：Write 返回的字节是确定可以转发给客户端的普通文本，
// 一旦返回就不可撤回。可能属于协议信号的字节会被扣住，直到能确定它不是。
type StreamParser struct {
	sess *Session
	p    *parser.Parser
	last error

	// accum 是解析器已提交的全部普通文本，emitted 是其中已经交给调用方的
	// 字节数。两者分开是尾部过滤的基础：正则要在**完整的累积文本**上跑，
	// 而 emitted 之前的内容已经发出去、收不回来了。
	//
	// 只在配置了噪声过滤时使用；没配时 Write 是零延迟透传。
	accum   []byte
	emitted int
}

// NewStreamParser 创建一个流式解析器。一个会话可以创建多个（每轮一个）。
func (s *Session) NewStreamParser() (*StreamParser, error) {
	p, err := parser.New(s.nonce, s.bridge.limits)
	if err != nil {
		return nil, wrap(ErrConfig, err)
	}
	return &StreamParser{sess: s, p: p}, nil
}

// Write 喂入一段模型输出，返回本次可以安全转发的普通文本。
//
// 返回的字节已经过噪声过滤，且**确定不会是尾部噪声的一部分**。
// 返回错误时流即告失败，后续 Write 无意义，应当直接 Flush 后 Close。
//
// 配置了噪声过滤时输出会滞后，滞后量见 UpstreamProfile.TailHoldBytes。
// 没配置时零延迟透传。
func (sp *StreamParser) Write(chunk []byte) ([]byte, error) {
	out, err := sp.p.Write(chunk)
	if err != nil {
		sp.last = err
		return sp.filterStream(out), classify(err)
	}
	return sp.filterStream(out), nil
}

// filterStream 计算当前可以安全发出的内容。
//
// 三条边界共同决定「安全位置」，取最靠前的那个：
//
//  1. **行完整性**——正则通常按行写（`^...` / `\n+...`），半行喂进去
//     匹配不上。保留到最后一个换行符**之前**，让换行符本身留在缓冲里，
//     这样下一轮 `\n+如果...` 这类规则的前导换行还能参与匹配。
//
//  2. **尾部保留窗口**——覆盖规则的回溯距离。见 TailHoldBytes 的说明。
//
//  3. **最早的匹配起点**——在累积文本上跑所有规则，任何一条匹配上，
//     从它的起点起就都是噪声，一个字节都不能发。
//
// 三条里第三条是关键：它让「已经确认是噪声」的内容立刻被扣住，
// 而不必等到流结束。第二条兜住第三条还判断不出来的那段。
func (sp *StreamParser) filterStream(b []byte) []byte {
	filters := sp.sess.bridge.profile.NoiseFilters
	if len(filters) == 0 {
		return b
	}
	sp.accum = append(sp.accum, b...)

	// 边界一：最后一个换行符的位置（不含它本身）。
	boundary := bytes.LastIndexByte(sp.accum, '\n')
	if boundary < 0 {
		boundary = 0
	}

	// 边界二：尾部保留窗口。
	if hold := sp.sess.bridge.holdBytes; hold > 0 {
		if h := len(sp.accum) - hold; h < boundary {
			boundary = h
		}
	}

	// 边界三：任何规则的最早匹配起点。
	for _, re := range filters {
		if loc := re.FindIndex(sp.accum); loc != nil && loc[0] < boundary {
			boundary = loc[0]
		}
	}

	if boundary <= sp.emitted {
		return nil
	}

	// 这一段已经确定不属于任何噪声匹配，可以发出。
	// 仍然过一遍规则：首部与中间的噪声要在这里剥掉。
	chunk := string(sp.accum[sp.emitted:boundary])
	for _, re := range filters {
		chunk = re.ReplaceAllString(chunk, "")
	}
	sp.emitted = boundary
	return []byte(chunk)
}

// Flush 取出缓冲里剩余的内容，必须在 Close 之前调用。
//
// 流已经结束，此刻的累积文本就是真正的完整输出，尾部规则可以正确判定。
// 被规则吃掉的部分就此永久丢弃——这正是尾部噪声该有的下场。
func (sp *StreamParser) Flush() []byte {
	if len(sp.sess.bridge.profile.NoiseFilters) == 0 || sp.emitted >= len(sp.accum) {
		return nil
	}
	rest := string(sp.accum[sp.emitted:])
	for _, re := range sp.sess.bridge.profile.NoiseFilters {
		rest = re.ReplaceAllString(rest, "")
	}
	sp.emitted = len(sp.accum)
	return []byte(rest)
}

// Close 结束这一轮并返回结果。
//
// 调用它之前应先取走 Flush 的返回值——缓冲里可能还扣着内容。
func (sp *StreamParser) Close() Result {
	r := sp.p.Close()
	if r.Err != nil {
		sp.last = r.Err
	}

	// 最终文本对全文应用过滤，尾部锚定的规则在这里才能正确判定。
	text := r.Text
	for _, re := range sp.sess.bridge.profile.NoiseFilters {
		text = re.ReplaceAllString(text, "")
	}

	out := Result{Text: text, Outcome: mapOutcome(r.Outcome), Trailing: r.Trailing}
	for _, c := range r.Calls {
		name, known := sp.sess.resolveTool(c.Tool)
		call := Call{ID: c.ID, Name: name}
		if !known {
			out.UnknownTools = append(out.UnknownTools, c.Tool)
		}
		if c.Freeform {
			call.Freeform = true
			call.Arguments = ir.TextArguments(c.ArgumentsText)
		} else {
			call.Arguments = json.RawMessage(c.ArgumentsJSON)
		}
		out.Calls = append(out.Calls, call)
	}
	return out
}

func (sp *StreamParser) err() error {
	if sp.last == nil {
		return nil
	}
	return classify(sp.last)
}

func mapOutcome(o parser.Outcome) Outcome {
	switch o {
	case parser.OutcomePlainText:
		return OutcomePlainText
	case parser.OutcomeCallsParsed:
		return OutcomeCallsParsed
	case parser.OutcomeTruncated:
		return OutcomeTruncated
	default:
		return OutcomeMalformed
	}
}

// DeclareTools 告诉会话本轮声明过哪些工具，供解析后核对工具名。
//
// Compile 会自动记下来，所以只有用 RestoreSession 重建会话时才需要它——
// 无状态 sidecar 的常态：编译发生在上一个 HTTP 请求里，解析时手上没有
// 名单。不给名单也能工作，只是 Result.UnknownTools 恒为空，模型写错
// 工具名不会有人发现。
func (s *Session) DeclareTools(names []string) {
	for _, n := range names {
		if n != "" {
			s.tools[n] = Tool{Name: n}
		}
	}
}

// resolveTool 把模型写的工具名核对回声明过的那一个。
//
// 为什么需要这一步：模型写错工具名是常见的，而错了之后**没有任何人会报错**。
// 调用原样转给客户端，客户端说「没有这个工具」，症状出现在链路另一端。
//
// 两种真实的错法：
//
//   - 带上协议前缀。Codex 的 developer message 里工具是按
//     `to=functions.collaboration.spawn_agent` 这种 recipient 形式引用的，
//     模型很容易照着写成 `functions.exec`。
//   - 大小写或空白不一致。
//
// 只做这两类**能确定意图**的还原，不做模糊匹配：近似匹配会让模型输出的
// 一个错名字意外命中另一个它无权调用的工具，那比调用失败危险得多。
//
// 返回的第二个值报告这个名字是否对得上。会话是用 RestoreSession 重建的
// （无状态 sidecar 的常态）时工具表是空的，此时一律当作「对得上」——
// 手上没有名单，就不该假装能判断。
func (s *Session) resolveTool(name string) (string, bool) {
	if len(s.tools) == 0 {
		return name, true
	}
	if _, ok := s.tools[name]; ok {
		return name, true
	}
	// 剥掉协议前缀后再试一次。
	if i := strings.LastIndex(name, "."); i >= 0 {
		if bare := name[i+1:]; bare != "" {
			if _, ok := s.tools[bare]; ok {
				return bare, true
			}
		}
	}
	// 大小写与空白不一致。
	trimmed := strings.TrimSpace(name)
	for declared := range s.tools {
		if strings.EqualFold(declared, trimmed) {
			return declared, true
		}
	}
	return name, false
}
