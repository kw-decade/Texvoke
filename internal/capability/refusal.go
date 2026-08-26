package capability

import (
	"fmt"
	"strings"
)

// RefusalKind 是「上游没有产出工具调用」的根因分类。
//
// 规格七章要求把它做成可观察的状态而不是一句日志：不同根因的处置方式
// 截然相反——client_capability_missing 要去修客户端配置，persona_refusal
// 值得做一次能力说明，而 upstream_policy_refusal 必须立刻停手。分错一次，
// 后面所有动作都是白费，甚至是在试图绕过供应商的安全策略。
type RefusalKind string

const (
	// RefusalNone 表示没有发生拒绝。
	RefusalNone RefusalKind = ""

	// ClientCapabilityMissing：客户端根本没发工具（tools=0）。
	// 此时再强的 Prompt 也没有意义，要查 SDK、模型目录、CCS 配置和 /v1 路径。
	ClientCapabilityMissing RefusalKind = "client_capability_missing"

	// RouterMutation：客户端发了工具，但到达上游时没了——中间的代理或
	// 路由删改了 tools / tool_choice / 认证头。
	RouterMutation RefusalKind = "router_mutation"

	// FormatNoncompliance：模型愿意调用，但输出缺字段、截断、非法 JSON、
	// 未知工具或 Schema 不通过。
	FormatNoncompliance RefusalKind = "format_noncompliance"

	// PersonaRefusal：模型把自己误认为普通网页助手，拒绝的是「亲自执行」。
	// 这是事实性误会，不是供应商政策，值得做一次诚实的能力说明。
	PersonaRefusal RefusalKind = "persona_refusal"

	// UpstreamPolicyRefusal：供应商政策、模型安全策略或 API capability gate
	// 明确禁止。**这一类不得绕过**。
	UpstreamPolicyRefusal RefusalKind = "upstream_policy_refusal"

	// RuntimePolicyDenied：本地工具风险、租户权限、审批或沙箱策略禁止执行。
	RuntimePolicyDenied RefusalKind = "runtime_policy_denied"

	// TransportFailure：超时、断流、上游 4xx/5xx、地区或路由问题。
	// 绝不能误报成模型拒绝——那会让运维去调 Prompt，而真正的问题在网络。
	TransportFailure RefusalKind = "transport_failure"
)

// Valid 报告 k 是否是已定义的分类（不含 RefusalNone）。
func (k RefusalKind) Valid() bool {
	switch k {
	case ClientCapabilityMissing, RouterMutation, FormatNoncompliance,
		PersonaRefusal, UpstreamPolicyRefusal, RuntimePolicyDenied, TransportFailure:
		return true
	default:
		return false
	}
}

// Terminal 报告这类拒绝是否不可通过任何本地手段挽回。
//
// 规格七章：判定为硬拒绝后要立即停止一切 Prompt 迭代施压与协议格式层面的
// 变通尝试（更换措辞、改写工具名、调整封装格式均属此类）。
func (k RefusalKind) Terminal() bool {
	return k == UpstreamPolicyRefusal || k == RuntimePolicyDenied
}

// Confidence 是分类结论的把握程度。
//
// 保留这个维度是因为规格明确要求「关键词只能作为候选信号，不能仅靠关键词
// 决定根因」。把「HTTP 403 加供应商错误码」和「模型说了句我不能执行」标成
// 同一个确定性，等于把猜测伪装成事实。
type Confidence string

const (
	// Certain：有协议层或传输层的硬证据（错误码、字段计数、解析失败）。
	Certain Confidence = "certain"
	// Probable：多项间接证据一致指向同一结论。
	Probable Confidence = "probable"
	// Weak：主要依据文本特征，随时可能误判。
	Weak Confidence = "weak"
)

// Remedy 是针对某类拒绝建议采取的下一步。
//
// 它只给建议，不替调用方决定：是否真的执行由 agent 层结合预算、
// 重试次数和策略决定。
type Remedy string

const (
	RemedyNone Remedy = "none"
	// RemedyFixClient：需要人去改客户端或路由配置，程序无能为力。
	RemedyFixClient Remedy = "fix_client_configuration"
	// RemedyHandshake：做一次能力说明，仅限一次。
	RemedyHandshake Remedy = "capability_handshake"
	// RemedyRepairFormat：把具体的格式错误反馈给模型，做有限次修复。
	RemedyRepairFormat Remedy = "repair_format"
	// RemedyNoCallHint：模型没发信号也没显式拒绝（「承诺不调用」型）时，
	// 指出「上一次回复没有发起任何调用」这个事实并重述格式要点。与
	// RemedyRepairFormat 的分工：那个是发了信号但写错了，该讲哪里错；
	// 这个压根没发，讲格式错误没有对象。措辞同样不含施压。
	RemedyNoCallHint Remedy = "no_call_hint"
	// RemedyDowngrade：降到槽位填充或确定性控制器。
	RemedyDowngrade Remedy = "downgrade_ladder"
	// RemedyBackoff：暂态故障，指数退避后重试。
	RemedyBackoff Remedy = "backoff_retry"
	// RemedyFailSafe：安全失败，返回错误码，不再尝试任何变通。
	RemedyFailSafe Remedy = "fail_safe"
)

// Evidence 是分类所依据的可观测事实。
//
// 全部用基础类型而不引用协议层的结构，有两个原因：一是 capability 包在
// 依赖树上只依赖 ir，二是这样能强迫调用方把「观测到了什么」显式摊开，
// 而不是把一个协议对象丢进来让分类器自己猜。
type Evidence struct {
	// ToolsDeclaredByClient 是客户端在请求里声明的工具数。
	// 为 0 是第一类根因的直接证据。
	ToolsDeclaredByClient int

	// ToolsSentUpstream 是实际发给上游的工具数。它与上一项不等，
	// 说明中间有环节删改了请求。
	ToolsSentUpstream int

	// ToolChoiceRequested 是客户端要求的工具选择模式。
	ToolChoiceRequested string

	// HTTPStatus 是上游响应的 HTTP 状态码，0 表示没拿到响应。
	HTTPStatus int

	// UpstreamErrType / Code / Message 来自上游的结构化错误。
	UpstreamErrType    string
	UpstreamErrCode    string
	UpstreamErrMessage string

	// TransportError 表示连接层面出了问题（超时、断流、DNS 失败）。
	TransportError bool

	// ToolCallsParsed 是最终解析出的调用数。
	ToolCallsParsed int

	// ParseFailed 表示模型输出里有疑似调用但解析不出来。
	ParseFailed    bool
	ParseErrorKind string

	// ModelText 是模型输出的可读文本，ModelRefusal 是协议层的显式拒绝字段。
	// 前者只能作为弱证据，后者是模型自己声明的拒绝，强一些但仍不足以
	// 区分「政策拒绝」与「人格误会」。
	ModelText    string
	ModelRefusal string

	// HandshakeDone 表示已经做过一次能力说明。做过之后仍拒绝，
	// 就不该再做第二次。
	HandshakeDone bool

	// AgentMode 表示本会话正处于 agent 工作循环：历史里出现过成功的
	// 工具调用，或客户端显式要求必须调用。这是**结构性证据**，来自
	// 会话状态而不是文本内容——它回答的是「客户端此刻是否在等一个调用」，
	// 与模型把这轮没调用说成什么措辞无关。
	//
	// 它存在的理由是六张自然语言词表的教训（见 Classify）：措辞会漂移，
	// 词表永远在补。而 agent 会话里「零调用」本身就是要追问的充分条件——
	// 客户端的任务停在半路，不管模型的叙述读起来多像正常回答。置位时
	// 零调用分支跳过全部软证据直接走阶梯；未置位时不影响任何现有判定。
	AgentMode bool

	// RuntimePolicyDenied 表示本地策略拒绝了执行。
	RuntimePolicyDenied bool
}

// Classification 是一次分类的完整结论。
type Classification struct {
	Kind       RefusalKind
	Confidence Confidence
	Remedy     Remedy

	// Reasons 是判定依据的人类可读描述，按证据强度从高到低排列。
	// 它进审计日志，让「为什么判成这一类」事后可追。
	Reasons []string
}

// String 返回一行摘要，供日志使用。
func (c Classification) String() string {
	if c.Kind == RefusalNone {
		return "无拒绝"
	}
	return fmt.Sprintf("%s（%s，建议 %s）：%s",
		c.Kind, c.Confidence, c.Remedy, strings.Join(c.Reasons, "；"))
}

// upstreamPolicyErrorTypes 是各家供应商用来表示「能力被禁止」的错误分类。
//
// 这些是机器可读的错误码，不是从人话里猜出来的关键词——把它们与
// ModelText 的关键词检测分开，正是「不能仅靠关键词决定根因」的落点。
var upstreamPolicyErrorTypes = map[string]bool{
	"invalid_request_error": false, // 太笼统，单独出现不足以判定
	"permission_error":      true,
	"authentication_error":  false, // 认证问题属于配置，不是能力禁止
	"policy_violation":      true,
	"content_policy":        true,
	"unsupported_feature":   true,
	"capability_disabled":   true,
}

var upstreamPolicyErrorCodes = map[string]bool{
	"unsupported_feature":       true,
	"tool_use_not_supported":    true,
	"function_calling_disabled": true,
	"policy_violation":          true,
	"content_filter":            true,
	"permission_denied":         true,
}

// personaRefusalMarkers 是「模型误以为自己不能执行」的文本特征。
//
// 这些**只是候选信号**。规格七章明确要求不能仅靠关键词决定根因，所以命中
// 它们最多得到 Weak 置信度，而且必须在排除了所有硬证据之后才轮到它们。
// 中英文都列，是因为上游模型的回复语言取决于用户的输入语言。
var personaRefusalMarkers = []string{
	"我不能执行", "我无法执行", "无法访问文件", "不能访问文件系统",
	"我没有权限", "我不能运行", "无法调用工具", "不支持调用工具",
	"请你自行运行", "请手动执行", "我只是一个",
	// 接 Codex 实测补充的变体：模型的原话是「我目前无法直接读取你的
	// 文件系统」和「本轮未提供可用的文件系统执行工具」。上面那批没盖住
	// 「直接读取」这个说法——接真实客户端之前靠想象列词表，就是这个下场。
	"无法直接读取", "不能直接读取", "无法直接操作",
	"未提供可用", "没有可用的工具", "本轮没有工具",
	// 2026-08-24 Claude Code/Codex 实测再补：「当前回合的工具调用接口
	// 不可用」「我不能直接创建文件」。模型的措辞一直在换，每接一个客户端
	// 都要回来添几条——这批词表是活的，不是一次写完的。
	"无法直接创建", "不能直接创建", "无法直接写入", "不能直接写入",
	"接口不可用", "调用通道", "工具调用通道",
	"i cannot execute", "i can't execute", "i am unable to execute",
	"i don't have access to", "i do not have access to",
	"i cannot access the file", "i'm unable to run", "i cannot run",
	"you'll need to run", "you will need to run", "i'm just a",
	"as a text-based", "as an ai language model",
}

// commitVerbs 是「行动承诺」识别里的动作动词。
//
// 2026-08-24 Codex 实测的新失败形态：模型回复「我会先读取 AGENTS.md，
// 再创建空白文件」——它打算做这件事，却没有通过约定的格式把调用发出来。
// 措辞里没有任何「不能」，人格词表盖不住。但这仍是人格误会的一种：
// 它没把「提出结构化建议」与「亲自执行」连起来，一次能力说明对症。
//
// 2026-08-26 长程会话补测的新形态：「我会先确认旧内联代码是否仍残留，
// 再继续生成分离文件；随后一次完成并跑三道验证」——确认 / 生成 / 验证
// 都不在原表里，整段计划宣言逃过识别，被当成正常回答透传，客户端认为
// 本轮完成、任务停摆。这三个词与表里原有的词同属一类：它们描述的都是
// 「要靠工具执行的动作」，正常聊天极少以第一人称承诺去做它们。
var commitVerbs = []string{"创建", "写", "写入", "读取", "删除", "运行", "执行", "修改", "编辑",
	"确认", "生成", "验证",
	"create", "write", "read", "delete", "run", "execute", "edit", "modify",
	"verify", "generate"}

// 措辞漂移的教训（2026-08-24 一天内三次）：上午是「无法直接读取」，中午是
// 「工具调用接口不可用」，下午同一任务变成「仍未获得可执行的文件工具权
// 锁」「工具调用仍不可用」「仍无法调用文件工具」。完整短语匹配每次都要补，
// 补完就过期。结构化的判据抗漂移：**否定词 × 能力对象共现**——不管模型把
// 句子组织成什么样，「说自己够不着某个执行能力」的语义骨架不变。
//
// 否定词覆盖中英日韩法德西俄。**证据等级要分清**：中文与英文的形态来自
// tests/fixtures/eval/ 的真实抓包；日 / 韩 / 法 / 德 / 西 / 俄六种只有单测里
// 作者手造的合成样本，没有任何一条真实上游的回复作为依据。它们能工作，
// 但「八语种识别」这句话在真实分布上未经验证，写文档时不要说成实测覆盖。
//
// 对象表原先只有中英日法德西俄——韩语有否定词却没有能力对象，共现判据
// 永不触发，等于半个语言。2026-08-25 补齐。
//
// 同样的限制：弱证据，只在零调用的分支里生效，命中也只值 Weak。
var inabilityNegations = []string{
	// 中文
	"无法", "不能", "不可以", "没法", "未能", "未获得", "不可用", "没有获得",
	// 「没有工具」是 2026-08-25 实测最高频的形态：模型把「没有 X 可用」
	// 说成「我没有工具」，与「没有获得」不同，它后面直接跟能力对象。
	"没有工具", "没有可用的", "无可用",
	// 英文
	"cannot", "can't", "unable to", "not able to",
	// 日语
	"できない", "できません", "不可",
	// 韩语
	"할 수 없", "수 없습니다",
	// 法语
	"ne peux pas", "ne peut pas", "impossible de",
	// 德语（keine 是否定冠词：单看太宽，但共现判据要求同时命中能力对象，
	// 「keine Zeit」这类不含对象的句子不会误触发）
	"kann nicht", "nicht möglich", "kann ich nicht", "keine",
	// 西班牙语
	"no puedo", "no es posible",
	// 俄语
	"не могу", "не может",
}
var inabilityTargets = []string{
	// 中文
	"工具", "调用", "执行", "权限",
	// 英文（execut 前缀同时盖 execute/execution/executor）
	"tool", "call", "execut", "permission",
	// 日语
	"ツール", "呼び出し", "実行",
	// 韩语（2026-08-25 补：此前只有否定词没有能力对象，共现判据永不触发，
	// 等于「支持韩语」这个声称是空的）
	"도구", "호출", "실행", "권한",
	// 法语（é 是独立码点，execut 盖不住 exécuter）
	"exécut", "outils",
	// 德语
	"werkzeug", "ausführ",
	// 西班牙语
	"herramienta", "llamar",
	// 俄语
	"инструмент", "вызыв",
}

func looksLikeInabilityPhrase(text string) bool {
	lower := strings.ToLower(text)
	hasNegation, hasTarget := false, false
	for _, w := range inabilityNegations {
		if strings.Contains(lower, w) {
			hasNegation = true
			break
		}
	}
	if !hasNegation {
		return false
	}
	for _, w := range inabilityTargets {
		if strings.Contains(lower, w) {
			hasTarget = true
			break
		}
	}
	return hasTarget
}

// deflectStarters 是「踢皮球」特征：模型把任务退回给用户，让用户自己提供
// 信息或动手执行，而不是发起调用。2026-08-24 实测形态：「请先指定文件名」
// 「请告诉我文件名」。它不是拒绝也不是承诺，但同样该被追问一次——任务里
// 缺的信息模型应该先用工具查（读目录、看现有命名），而不是干等用户。
var deflectStarters = []string{
	"请告诉我", "请提供", "请指定", "请先指定", "你希望我", "请确认", "请先说明",
	"please provide", "please tell me", "please specify", "which would you like",
	"could you provide", "kindly provide",
}

func looksLikeDeflection(text string) bool {
	lower := strings.ToLower(text)
	for _, s := range deflectStarters {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}

// malformedIntentTags 是「想调但完全不会写格式」的特征：输出里出现了
// envelope 的标签片段，但 parser 判为 plain_text（零调用且无解析失败）。
// 说明模型有调用意图、连信号都写了，只是结构坏到认不出来。对它讲道理
// 没用，直接给完整示范（repair_format 通道会翻译成具体指引）。
var malformedIntentTags = []string{
	"<tool_call_envelope", "arguments_json", "arguments_text", "[[UTR-CALL",
}

func looksLikeMalformedIntent(text string) bool {
	for _, tag := range malformedIntentTags {
		if strings.Contains(text, tag) {
			return true
		}
	}
	return false
}

var commitStarters = []string{"我会", "我将", "我会先", "现在先", "我继续", "接下来我",
	"i will ", "i'll ", "let me "}

// looksLikeCommitWithoutCall 报告文本是否像「行动计划却没有调用」。
// 与人格词表同样的限制：弱证据，必须在一切硬证据之后才轮到。
func looksLikeCommitWithoutCall(text string) bool {
	lower := strings.ToLower(text)
	commit := false
	for _, s := range commitStarters {
		if strings.Contains(lower, s) {
			commit = true
			break
		}
	}
	if !commit {
		return false
	}
	for _, v := range commitVerbs {
		if strings.Contains(lower, v) {
			return true
		}
	}
	return false
}

// Classify 依据证据判定拒绝根因。
//
// 判定顺序是从硬证据到软证据，这个顺序本身就是规格的要求：一个 429 限流
// 必须报成 transport_failure 而不是「模型不肯调用」，否则运维会去调 Prompt，
// 而真正的问题在网络。同理，tools=0 时无论模型说了什么，根因都在客户端。
func Classify(e Evidence) Classification {
	// 本地策略拒绝是我们自己做的决定，最确定。
	if e.RuntimePolicyDenied {
		return Classification{
			Kind:       RuntimePolicyDenied,
			Confidence: Certain,
			Remedy:     RemedyFailSafe,
			Reasons:    []string{"本地策略拒绝了执行"},
		}
	}

	// 传输层故障优先于一切内容判断——连响应都没拿到，谈不上模型拒绝。
	if e.TransportError {
		return Classification{
			Kind:       TransportFailure,
			Confidence: Certain,
			Remedy:     RemedyBackoff,
			Reasons:    []string{"传输层错误，未能取得完整响应"},
		}
	}
	if e.HTTPStatus == 429 || (e.HTTPStatus >= 500 && e.HTTPStatus < 600) {
		return Classification{
			Kind:       TransportFailure,
			Confidence: Certain,
			Remedy:     RemedyBackoff,
			Reasons:    []string{fmt.Sprintf("上游返回 HTTP %d，属于可退避重试的暂态故障", e.HTTPStatus)},
		}
	}

	// 上游明确禁止：这是唯一不能做任何变通的一类。
	if kind, reasons := classifyUpstreamError(e); kind != RefusalNone {
		return Classification{
			Kind:       kind,
			Confidence: Certain,
			Remedy:     RemedyFailSafe,
			Reasons:    reasons,
		}
	}

	// 客户端压根没发工具。此时模型说什么都不重要——它手里没有工具可调。
	if e.ToolsDeclaredByClient == 0 {
		return Classification{
			Kind:       ClientCapabilityMissing,
			Confidence: Certain,
			Remedy:     RemedyFixClient,
			Reasons: []string{
				"客户端声明的工具数为 0",
				"检查 SDK 用法、模型目录、model_catalog_json 与 /v1 路径",
			},
		}
	}

	// 客户端发了但上游没收到：中间有环节把它删了。
	if e.ToolsSentUpstream == 0 {
		return Classification{
			Kind:       RouterMutation,
			Confidence: Certain,
			Remedy:     RemedyFixClient,
			Reasons: []string{
				fmt.Sprintf("客户端声明了 %d 个工具，实际发往上游 0 个", e.ToolsDeclaredByClient),
				"检查代理或 CCS 是否改写了请求字段",
			},
		}
	}
	if e.ToolsSentUpstream < e.ToolsDeclaredByClient {
		return Classification{
			Kind:       RouterMutation,
			Confidence: Probable,
			Remedy:     RemedyFixClient,
			Reasons: []string{
				fmt.Sprintf("工具数在途中减少：客户端 %d 个，上游收到 %d 个",
					e.ToolsDeclaredByClient, e.ToolsSentUpstream),
			},
		}
	}

	// 模型愿意调用但输出不合格式。这是可以通过反馈具体错误来修的。
	if e.ParseFailed {
		reasons := []string{"模型输出中有疑似调用，但解析失败"}
		if e.ParseErrorKind != "" {
			reasons = append(reasons, "解析错误类型："+e.ParseErrorKind)
		}
		return Classification{
			Kind:       FormatNoncompliance,
			Confidence: Certain,
			Remedy:     RemedyRepairFormat,
			Reasons:    reasons,
		}
	}

	// 到这里已经产出了调用，不构成拒绝。
	if e.ToolCallsParsed > 0 {
		return Classification{Kind: RefusalNone, Confidence: Certain, Remedy: RemedyNone}
	}

	// 零调用 + agent 模式：结构性证据直接定案。
	//
	// 客户端正在跑工具循环（历史里有成功调用，或显式要求必须调用），
	// 此刻零调用意味着任务停在半路——不管模型把这件事叙述成「计划宣言」
	// 「能力否定」还是「正常回答」，处置都是同一个：走阶梯追问。文本词表
	// 在这里没有增量价值，只有漏判风险（措辞永远在漂移，词表永远在补，
	// 2026-08-26 一天补了两次）。显式拒绝与硬通道在上面已经处理完了，
	// 走不到这里；能走到这里的纯文本，一律按「该调没调」追一次。
	//
	// 置信度标 Certain 是诚实的：确定的是「客户端在等调用」这个事实，
	// 不是「模型在拒绝」。Remedy 交给 remedyForPersona 选阶梯强度。
	if e.AgentMode {
		return Classification{
			Kind:       PersonaRefusal,
			Confidence: Certain,
			Remedy:     remedyForPersona(e),
			Reasons: []string{
				"会话处于 agent 循环（历史有成功调用或 tool_choice 要求调用），本轮零调用",
				"结构性证据：不依赖文本内容，无措辞漂移风险",
			},
		}
	}

	// 一个也没调用。先看模型有没有显式拒绝——这比从正文里找关键词强，
	// 但仍不足以区分「政策拒绝」与「人格误会」，所以只给 Probable。
	if e.ModelRefusal != "" {
		return Classification{
			Kind:       PersonaRefusal,
			Confidence: Probable,
			Remedy:     remedyForPersona(e),
			Reasons: []string{
				"模型在协议层给出了显式 refusal",
				"未见供应商政策错误码，倾向于人格误会而非政策拒绝",
			},
		}
	}

	// 以下文本判据（人格词表 / 行动承诺 / 能力否定 / 踢皮球）保留为
	// **注解通道**：命中只写进 Reasons 供日志与人工分析（见函数末尾的
	// notes 收集），不再决定 retry。
	//
	// 决策路径上它们已被 AgentMode 取代（见上面的结构性证据分支）。理由：
	// 措辞永远在漂移而词表永远在补——2026-08-24 一天三次补完整短语，
	// 改成结构化共现后降到词汇级漂移，2026-08-26 又补了三个动词。每补一次
	// 都是一次「修轮子」；漏掉一次就是任务静默停摆。会话状态不会漂移。
	// 词表真正的剩余价值是可解释性：事后看日志时，「这轮命中了人格拒绝
	// 特征」比「这轮是 agent 模式零调用」多一层归因线索。
	// 错格式意图：输出里带着 envelope 的标签片段却没解析出任何东西——
	// 模型想调、连信号都写了，只是结构坏到认不出来。这不是人格问题，
	// 讲道理没有对象，直接归格式修复通道（追问会带完整示范）。
	//
	// 这一个**留在决策路径**：它检测的是协议自己的词汇（envelope 标签），
	// 不是自然语言，没有漂移问题，且它的处置（repair_format）与阶梯不同。
	if looksLikeMalformedIntent(e.ModelText) {
		return Classification{
			Kind:       FormatNoncompliance,
			Confidence: Weak,
			Remedy:     RemedyRepairFormat,
			Reasons: []string{
				"模型输出含 envelope 标签片段但零调用——有调用意图，格式完全走样",
				"弱信号，未见硬证据",
			},
		}
	}

	// 行动承诺 / 能力否定 / 踢皮球 / 人格词表：降为注解。agent 会话走不到
	// 这里（上面已定案）；非 agent 会话里这些形态多数本来就是正常对话的
	// 一部分，词表命中只值得在日志里留一笔。
	var notes []string
	if _, ok := matchPersonaMarker(e.ModelText); ok {
		notes = append(notes, "人格拒绝")
	}
	if looksLikeCommitWithoutCall(e.ModelText) {
		notes = append(notes, "行动承诺")
	}
	if looksLikeInabilityPhrase(e.ModelText) {
		notes = append(notes, "能力否定")
	}
	if looksLikeDeflection(e.ModelText) {
		notes = append(notes, "踢皮球")
	}

	// 该调用却没调用，又找不出原因。不猜。
	if requiresCall(e.ToolChoiceRequested) {
		reasons := []string{
			fmt.Sprintf("tool_choice=%s 要求至少一个调用，实际为 0", e.ToolChoiceRequested),
			"未见拒绝迹象，可能是模型未理解协议",
		}
		if len(notes) > 0 {
			reasons = append(reasons, "文本注解："+strings.Join(notes, "、")+"（仅记录，不参与判定）")
		}
		return Classification{
			Kind:       FormatNoncompliance,
			Confidence: Weak,
			Remedy:     RemedyDowngrade,
			Reasons:    reasons,
		}
	}

	// tool_choice=auto 时不调用工具是完全正常的。词表注解（若有）跟着
	// 进日志，供人工分析——它们不改变判定，但解释了「这轮文本长什么样」。
	if len(notes) > 0 {
		return Classification{
			Kind: RefusalNone, Confidence: Certain, Remedy: RemedyNone,
			Reasons: []string{
				"tool_choice=auto 下正常回答，不追问",
				"文本注解：" + strings.Join(notes, "、") + "（仅记录）",
			},
		}
	}
	return Classification{Kind: RefusalNone, Confidence: Certain, Remedy: RemedyNone}
}

// classifyUpstreamError 从上游的结构化错误里识别硬拒绝。
//
// 只认机器可读的 type 与 code，不看 message 里的人话——供应商的错误文案
// 随时会变，靠它做判断等于把安全边界建在营销文档上。
func classifyUpstreamError(e Evidence) (RefusalKind, []string) {
	if e.UpstreamErrType == "" && e.UpstreamErrCode == "" {
		return RefusalNone, nil
	}

	var reasons []string
	policy := false

	if upstreamPolicyErrorTypes[e.UpstreamErrType] {
		policy = true
		reasons = append(reasons, fmt.Sprintf("上游错误类型 %q 表示能力被禁止", e.UpstreamErrType))
	}
	if upstreamPolicyErrorCodes[e.UpstreamErrCode] {
		policy = true
		reasons = append(reasons, fmt.Sprintf("上游错误码 %q 表示能力被禁止", e.UpstreamErrCode))
	}
	// 403 配合任意错误码就足以判定为策略拒绝：这个状态码的语义是
	// 「认证没问题，但你不被允许做这件事」。
	if e.HTTPStatus == 403 {
		policy = true
		reasons = append(reasons, "上游返回 HTTP 403，语义为已认证但不被允许")
	}

	if policy {
		if e.UpstreamErrMessage != "" {
			// 消息只作记录，不参与判定。它可能含上游的内部信息，
			// 不应转发给客户端，更不应喂回给模型。
			reasons = append(reasons, "上游原始说明已脱敏记录")
		}
		return UpstreamPolicyRefusal, reasons
	}

	// 400 系列的其余错误多半是请求本身有问题，属于格式或配置范畴。
	if e.HTTPStatus >= 400 && e.HTTPStatus < 500 {
		return RefusalNone, nil
	}
	return RefusalNone, nil
}

// remedyForPersona 决定人格拒绝的处置方式。
//
// 规格七章：澄清后仍拒绝时，只做一次受策略允许的格式修复或切换到 Level 4，
// 不得循环重试。所以握手做过一次之后就只能降级——例外是「承诺不调用」型：
// 模型没以为自己不能，它只是没把「提出建议」这个动作发出来。对它重述
// 「上一次回复没有发起任何调用」的事实（RemedyNoCallHint）正是那次被允许
// 的格式修复；次数仍由调用方的尝试上限兜底。
func remedyForPersona(e Evidence) Remedy {
	if e.HandshakeDone {
		// 握手已做，但阶梯还没到顶：能力否定继续走运行时通知（L2 起），
		// 不在这里提前放弃。提前 downgrade 会让「我没有工具可用」这类
		// 最高频的否定形态在 L1 之后直接停手——2026-08-25 gateway 单测
		// 暴露的缺口；阶梯深度由 Recover 按 Attempt 控制，这里只选手段。
		return RemedyNoCallHint
	}
	return RemedyHandshake
}

func matchPersonaMarker(text string) (string, bool) {
	if text == "" {
		return "", false
	}
	lower := strings.ToLower(text)
	for _, m := range personaRefusalMarkers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return m, true
		}
	}
	return "", false
}

func requiresCall(toolChoice string) bool {
	return toolChoice == "required" || toolChoice == "named" || toolChoice == "any"
}
