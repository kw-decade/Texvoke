package capability

import (
	"strings"
	"testing"
)

// 判定顺序本身就是规格的要求：硬证据必须压过软证据。
// 一个 429 限流被报成「模型不肯调用」，运维会去调 Prompt，
// 而真正的问题在网络——这类误判的代价是几个小时的错误排查。
func TestClassifyHardEvidenceWinsOverText(t *testing.T) {
	// 模型输出里带着最典型的人格拒绝措辞，但同时存在硬证据。
	refusalText := "抱歉，我不能执行命令，也无法访问文件系统。"

	tests := []struct {
		name string
		ev   Evidence
		want RefusalKind
	}{
		{
			"传输错误压过文本",
			Evidence{TransportError: true, ModelText: refusalText, ToolsDeclaredByClient: 3, ToolsSentUpstream: 3},
			TransportFailure,
		},
		{
			"限流压过文本",
			Evidence{HTTPStatus: 429, ModelText: refusalText, ToolsDeclaredByClient: 3, ToolsSentUpstream: 3},
			TransportFailure,
		},
		{
			"服务端错误压过文本",
			Evidence{HTTPStatus: 503, ModelText: refusalText, ToolsDeclaredByClient: 3, ToolsSentUpstream: 3},
			TransportFailure,
		},
		{
			"供应商策略压过文本",
			Evidence{
				HTTPStatus: 403, UpstreamErrType: "permission_error",
				ModelText: refusalText, ToolsDeclaredByClient: 3, ToolsSentUpstream: 3,
			},
			UpstreamPolicyRefusal,
		},
		{
			// tools=0 时模型手里根本没有工具，它说什么都不重要。
			"客户端没发工具压过文本",
			Evidence{ToolsDeclaredByClient: 0, ModelText: refusalText},
			ClientCapabilityMissing,
		},
		{
			"路由改写压过文本",
			Evidence{ToolsDeclaredByClient: 3, ToolsSentUpstream: 0, ModelText: refusalText},
			RouterMutation,
		},
		{
			"解析失败压过文本",
			Evidence{
				ToolsDeclaredByClient: 3, ToolsSentUpstream: 3,
				ParseFailed: true, ModelText: refusalText,
			},
			FormatNoncompliance,
		},
		{
			"本地策略压过一切",
			Evidence{RuntimePolicyDenied: true, TransportError: true, ModelText: refusalText},
			RuntimePolicyDenied,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.ev)
			if got.Kind != tc.want {
				t.Errorf("判定为 %q，期望 %q\n理由：%v", got.Kind, tc.want, got.Reasons)
			}
			if got.Confidence != Certain {
				t.Errorf("硬证据应当给出 certain 置信度，实际为 %q", got.Confidence)
			}
		})
	}
}

// 关键词只能作为候选信号。命中它得到的必须是 Weak 置信度——
// 把猜测标成事实，会让日志读起来像是「我们确切知道原因」。
// AgentMode 是结构性证据：agent 会话里零调用本身就要追问，文本内容
// 不再参与判定。这是对六张自然语言词表的替代——词表永远在补措辞漂移
// 的漏（2026-08-26 一天补了两次），而会话状态不会漂移。
func TestAgentModeCatchesAnyZeroCallText(t *testing.T) {
	texts := []string{
		// 计划宣言——曾经漏掉、靠补动词表救回的那句原文。
		"入口页刚才是从旧单文件的开头做结构替换，我会先确认旧内联代码是否仍残留，再继续生成分离文件；随后一次完成并跑三道验证。",
		// 未来可能出现的任意新措辞——词表方案对这类永远在赌，结构判据不看内容。
		"让我先把思路理一理，梳理一下目前的进展和接下来的安排。",
	}
	for _, text := range texts {
		t.Run(text[:12], func(t *testing.T) {
			got := Classify(Evidence{
				ToolsDeclaredByClient: 1, ToolsSentUpstream: 1,
				ToolChoiceRequested: "auto", ModelText: text,
				AgentMode: true,
			})
			if got.Kind != PersonaRefusal {
				t.Fatalf("agent 模式零调用应追问，得到 %q", got.Kind)
			}
			if got.Remedy != RemedyHandshake && got.Remedy != RemedyNoCallHint {
				t.Errorf("remedy = %q，期望走阶梯", got.Remedy)
			}
			joined := strings.Join(got.Reasons, " ")
			if !strings.Contains(joined, "结构性证据") {
				t.Errorf("理由应说明这是结构性证据：%v", got.Reasons)
			}
		})
	}

	// 对照组：同样的文本，非 agent 会话维持现状——正常聊天不该被追。
	// 注意第一条计划宣言在非 agent 会话里仍会被词表接住（3520210 补的
	// 动词），这是预期：对照组要证明的只是「AgentMode 未置位时不改变
	// 现有判定」，不是「这些文本必然放行」。
	for _, text := range texts[1:] {
		got := Classify(Evidence{
			ToolsDeclaredByClient: 1, ToolsSentUpstream: 1,
			ToolChoiceRequested: "auto", ModelText: text,
		})
		if got.Kind != RefusalNone {
			t.Errorf("非 agent 会话的纯文本被判为 %q（%q）", got.Kind, text)
		}
	}
}

// 置信度语义：Certain 描述的是「客户端在等调用」这个事实，
// 不是「模型在拒绝」。文档里写明了，测试钉住。
func TestAgentModeConfidenceMeansStructuralFact(t *testing.T) {
	got := Classify(Evidence{
		ToolsDeclaredByClient: 1, ToolsSentUpstream: 1,
		ToolChoiceRequested: "auto",
		ModelText:           "旧金山今天天气很好。",
		AgentMode:           true,
	})
	if got.Confidence != Certain {
		t.Fatalf("结构性证据应为 certain，得到 %q", got.Confidence)
	}
}

// 人格词表已降为注解通道（见 TestAgentModeCatchesAnyZeroCallText 的说明）。
// agent 会话里同样的文本由 AgentMode 接住并走阶梯；本测试钉住两条：
// 注解要留在 Reasons 里、AgentMode 置位后判定与词表无关。
func TestClassifyKeywordIsWeakEvidence(t *testing.T) {
	text := "抱歉，我不能执行命令。"

	// 非 agent 会话：放行，注解留痕。
	got := Classify(Evidence{
		ToolsDeclaredByClient: 2,
		ToolsSentUpstream:     2,
		ToolChoiceRequested:   "auto",
		ModelText:             text,
	})
	if got.Kind != RefusalNone {
		t.Fatalf("非 agent 会话应放行，得到 %q", got.Kind)
	}
	joined := strings.Join(got.Reasons, " ")
	if !strings.Contains(joined, "人格拒绝") {
		t.Errorf("人格拒绝注解应留在 Reasons：%v", got.Reasons)
	}

	// agent 会话：结构性判据接住，走阶梯。
	got = Classify(Evidence{
		ToolsDeclaredByClient: 2,
		ToolsSentUpstream:     2,
		ToolChoiceRequested:   "auto",
		ModelText:             text,
		AgentMode:             true,
	})
	if got.Kind != PersonaRefusal || got.Remedy == RemedyNone {
		t.Fatalf("agent 会话零调用应追问，得到 %q/%q", got.Kind, got.Remedy)
	}
}

// 协议层的显式 refusal 字段比正文关键词强，但仍不足以区分
// 「政策拒绝」与「人格误会」，所以只给 Probable。
func TestClassifyExplicitRefusalIsProbable(t *testing.T) {
	got := Classify(Evidence{
		ToolsDeclaredByClient: 2,
		ToolsSentUpstream:     2,
		ModelRefusal:          "I can't help with that.",
	})
	if got.Kind != PersonaRefusal {
		t.Fatalf("判定为 %q", got.Kind)
	}
	if got.Confidence != Probable {
		t.Errorf("置信度为 %q，期望 probable", got.Confidence)
	}
}

// 英文人格拒绝词表同样只做注解：非 agent 会话放行、留痕。
func TestClassifyEnglishMarkers(t *testing.T) {
	texts := []string{
		"I cannot execute commands on your machine.",
		"I don't have access to your file system.",
		"You'll need to run that command yourself.",
		"As an AI language model, I'm unable to run code.",
	}
	for _, text := range texts {
		t.Run(text[:20], func(t *testing.T) {
			got := Classify(Evidence{
				ToolsDeclaredByClient: 1, ToolsSentUpstream: 1, ModelText: text,
			})
			if got.Kind != RefusalNone {
				t.Errorf("非 agent 会话应放行，得到 %q", got.Kind)
			}
			if !strings.Contains(strings.Join(got.Reasons, " "), "人格拒绝") {
				t.Errorf("英文词表注解应留在 Reasons：%v", got.Reasons)
			}
		})
	}
}

// 「给出了行动计划却没发起调用」是 2026-08-24 Codex 实测的失败形态：
// 模型说「我会先读取 AGENTS.md，再创建空白文件」，措辞里没有任何「不能」，
// 人格词表盖不住，但它确实没把调用发出来。同样只值 Weak。
// 行动承诺词表已降为注解通道。判定由 AgentMode 承担；这里钉住：
// 非 agent 会话放行但留痕（2026-08-26 迷途志异会话的漏网原文也在样本里，
// 它在 agent 会话里由 TestAgentModeCatchesAnyZeroCallText 覆盖）。
func TestClassifyCommitWithoutCall(t *testing.T) {
	texts := []string{
		"我会先读取项目的 AGENTS.md 约定；没有冲突就创建空白.txt，并确认文件大小为 0 字节。",
		"我现在先写最小目录规范，再核对两者是否存在。",
		"I will create the file and then verify its size.",
		"入口页刚才是从旧单文件的开头做结构替换，我会先确认旧内联代码是否仍残留，再继续生成分离文件；随后一次完成并跑三道验证。",
	}
	for _, text := range texts {
		t.Run(text[:16], func(t *testing.T) {
			got := Classify(Evidence{
				ToolsDeclaredByClient: 1, ToolsSentUpstream: 1,
				ToolChoiceRequested: "auto", ModelText: text,
			})
			if got.Kind != RefusalNone {
				t.Fatalf("非 agent 会话应放行，得到 %q", got.Kind)
			}
			joined := strings.Join(got.Reasons, " ")
			if !strings.Contains(joined, "行动承诺") {
				t.Errorf("行动承诺注解应留在 Reasons：%v", got.Reasons)
			}
		})
	}
}

// 正常回答不该被「行动承诺」识别误伤：没有动作动词、或干脆与任务无关时，
// tool_choice=auto 下不调用是完全正常的。
func TestClassifyCommitPatternDoesNotCatchNormalAnswers(t *testing.T) {
	texts := []string{
		"我会考虑你的建议。",             // 有承诺无动作动词
		"创建一个空文件需要用到 touch 命令。", // 有动作动词无承诺
		"北京今天天气不错。",
	}
	for _, text := range texts {
		t.Run(text[:12], func(t *testing.T) {
			got := Classify(Evidence{
				ToolsDeclaredByClient: 1, ToolsSentUpstream: 1,
				ToolChoiceRequested: "auto", ModelText: text,
			})
			if got.Kind != RefusalNone {
				t.Errorf("判定为 %q，期望正常回答（%q）", got.Kind, text)
			}
		})
	}
}

// 措辞漂移的实测样本（同一天内的三轮失败，完整短语各不相同）：
// 结构化的「否定×能力共现」判据必须全部接住。
// 能力否定的结构化判据（否定×能力共现）同样降为注解。多语言样本保留：
// 它们证明的是「注解能覆盖这些形态」，判定本身不再依赖它们。
func TestClassifyInabilityPhraseSurvivesWordingDrift(t *testing.T) {
	texts := []string{
		"我继续执行：创建空文件后立即读取元数据。我这轮仍未获得可执行的文件工具权限，所以无法实际创建。",
		"当前环境的工具调用仍不可用，因此我无法实际创建文件。",
		"我目前仍无法调用文件工具，所以不能诚实地声称已经创建成功。",
		"我这轮仍未获得可执行的文件工具权限。",
		// 多语言样本：除英文外全是作者手造的合成样本，不是真实抓包——
		// refusal.go 的词表注释里说明了这个证据等级差别。
		"I cannot call the tools in this environment.",
		"Sorry, I am unable to execute file operations.",
		"申し訳ありませんが、この環境ではツールを呼び出すことができません。",
		"죄송하지만 이 환경에서는 도구를 호출할 수 없습니다.",
		"Désolé, je ne peux pas exécuter d'outils dans cet environnement.",
		"Es tut mir leid, ich kann hier keine Werkzeuge ausführen.",
		"Lo siento, no puedo llamar a las herramientas aquí.",
		"Извините, я не могу вызывать инструменты в этой среде.",
	}
	for _, text := range texts {
		t.Run(text[:14], func(t *testing.T) {
			got := Classify(Evidence{
				ToolsDeclaredByClient: 1, ToolsSentUpstream: 1,
				ToolChoiceRequested: "auto", ModelText: text,
			})
			if got.Kind != RefusalNone {
				t.Errorf("非 agent 会话应放行，得到 %q（%q）", got.Kind, text)
			}
			joined := strings.Join(got.Reasons, " ")
			if !strings.Contains(joined, "能力否定") {
				t.Errorf("能力否定注解应留在 Reasons：%v", got.Reasons)
			}
		})
	}
}

// 踢皮球型：把任务退回给用户而不是先用工具自查。
//
// 2026-08-26 起词表降为注解通道，不再决定判定；agent 会话里这类文本由
// AgentMode 结构性判据接住（见 TestAgentModeCatchesAnyZeroCallText）。
// 本测试改为钉住注解语义：非 agent 会话放行，但 Reasons 里留痕。
func TestClassifyDeflection(t *testing.T) {
	texts := []string{
		"请先指定文件名，例如 空白.txt 或 blank.txt。目前只给了目录，无法确定要创建哪个文件。",
		"Please provide the exact file name you want me to create.",
		"你希望我把文件命名为什么？",
	}
	for _, text := range texts {
		t.Run(text[:12], func(t *testing.T) {
			got := Classify(Evidence{
				ToolsDeclaredByClient: 1, ToolsSentUpstream: 1,
				ToolChoiceRequested: "auto", ModelText: text,
			})
			if got.Kind != RefusalNone {
				t.Errorf("非 agent 会话的踢皮球文本应放行，得到 %q（%q）", got.Kind, text)
			}
			joined := strings.Join(got.Reasons, " ")
			if !strings.Contains(joined, "踢皮球") {
				t.Errorf("注解应留在 Reasons 里供日志分析：%v", got.Reasons)
			}
		})
	}
}

// 错格式意图型：输出里有 envelope 片段但解析为零调用——想调不会写，
// 应走格式修复通道而不是人格说明。
func TestClassifyMalformedIntent(t *testing.T) {
	text := "好的，我来创建文件。\n\n<tool_call_envelope version=\"1\">\n  <call id=\"1\">\n"
	got := Classify(Evidence{
		ToolsDeclaredByClient: 1, ToolsSentUpstream: 1,
		ToolChoiceRequested: "auto", ModelText: text,
	})
	if got.Kind != FormatNoncompliance {
		t.Fatalf("判定为 %q，期望 %q", got.Kind, FormatNoncompliance)
	}
	if got.Remedy != RemedyRepairFormat {
		t.Errorf("处置为 %q，期望 %q", got.Remedy, RemedyRepairFormat)
	}
}

// 上游错误只认机器可读的 type 与 code，不看 message 里的人话——
// 供应商的错误文案随时会变，靠它做判断等于把安全边界建在营销文档上。
func TestClassifyUpstreamErrorUsesCodesNotProse(t *testing.T) {
	t.Run("认得出的错误码", func(t *testing.T) {
		got := Classify(Evidence{
			ToolsDeclaredByClient: 1, ToolsSentUpstream: 1,
			HTTPStatus: 400, UpstreamErrCode: "tool_use_not_supported",
		})
		if got.Kind != UpstreamPolicyRefusal {
			t.Errorf("判定为 %q，期望 %q", got.Kind, UpstreamPolicyRefusal)
		}
		if got.Remedy != RemedyFailSafe {
			t.Errorf("硬拒绝的建议必须是安全失败，实际为 %q", got.Remedy)
		}
	})

	t.Run("只有措辞吓人的 message 不算数", func(t *testing.T) {
		got := Classify(Evidence{
			ToolsDeclaredByClient: 1, ToolsSentUpstream: 1, ToolCallsParsed: 1,
			HTTPStatus:         200,
			UpstreamErrMessage: "tool use is strictly forbidden by policy",
		})
		// 有调用产出就不是拒绝，无论 message 里写了什么。
		if got.Kind != RefusalNone {
			t.Errorf("判定为 %q，期望无拒绝——message 不参与判定", got.Kind)
		}
	})

	t.Run("认证错误不算能力禁止", func(t *testing.T) {
		got := Classify(Evidence{
			ToolsDeclaredByClient: 1, ToolsSentUpstream: 1,
			HTTPStatus: 401, UpstreamErrType: "authentication_error",
		})
		if got.Kind == UpstreamPolicyRefusal {
			t.Error("认证失败是配置问题，不该判成能力被禁止")
		}
	})

	t.Run("403 本身就足以判定", func(t *testing.T) {
		got := Classify(Evidence{
			ToolsDeclaredByClient: 1, ToolsSentUpstream: 1,
			HTTPStatus: 403, UpstreamErrType: "invalid_request_error",
		})
		if got.Kind != UpstreamPolicyRefusal {
			t.Errorf("判定为 %q——403 的语义是已认证但不被允许", got.Kind)
		}
	})
}

// 硬拒绝必须是终态，且建议安全失败。规格七章：判定后要立即停止一切
// Prompt 迭代施压与协议格式层面的变通尝试。
func TestTerminalRefusalsStopEverything(t *testing.T) {
	terminal := map[RefusalKind]bool{
		UpstreamPolicyRefusal:   true,
		RuntimePolicyDenied:     true,
		ClientCapabilityMissing: false,
		RouterMutation:          false,
		FormatNoncompliance:     false,
		PersonaRefusal:          false,
		TransportFailure:        false,
	}
	for kind, want := range terminal {
		if got := kind.Terminal(); got != want {
			t.Errorf("%q.Terminal() = %v，期望 %v", kind, got, want)
		}
		if !kind.Valid() {
			t.Errorf("%q 应当是有效分类", kind)
		}
	}
	if RefusalNone.Valid() {
		t.Error("RefusalNone 不应被判为有效分类")
	}
}

func TestClassifyNoRefusal(t *testing.T) {
	t.Run("产出了调用", func(t *testing.T) {
		got := Classify(Evidence{
			ToolsDeclaredByClient: 2, ToolsSentUpstream: 2, ToolCallsParsed: 1,
		})
		if got.Kind != RefusalNone {
			t.Errorf("判定为 %q，期望无拒绝", got.Kind)
		}
	})

	t.Run("auto 模式下不调用是正常的", func(t *testing.T) {
		got := Classify(Evidence{
			ToolsDeclaredByClient: 2, ToolsSentUpstream: 2,
			ToolChoiceRequested: "auto",
			ModelText:           "今天天气不错，有什么可以帮你的吗？",
		})
		if got.Kind != RefusalNone {
			t.Errorf("判定为 %q，期望无拒绝——auto 模式允许零调用", got.Kind)
		}
	})
}

// tool_choice 要求必须调用却一个也没有，且找不出拒绝迹象：
// 这是模型没理解协议，该降级而不是猜。
func TestClassifyRequiredButNoCall(t *testing.T) {
	for _, mode := range []string{"required", "named", "any"} {
		t.Run(mode, func(t *testing.T) {
			got := Classify(Evidence{
				ToolsDeclaredByClient: 2, ToolsSentUpstream: 2,
				ToolChoiceRequested: mode,
				ModelText:           "旧金山今天 18 度。",
			})
			if got.Kind != FormatNoncompliance {
				t.Errorf("判定为 %q，期望 %q", got.Kind, FormatNoncompliance)
			}
			if got.Remedy != RemedyDowngrade {
				t.Errorf("建议为 %q，期望降级", got.Remedy)
			}
		})
	}
}

// 工具在途中减少同样是路由改写，只是证据没有「全没了」那么强。
func TestClassifyPartialToolLoss(t *testing.T) {
	got := Classify(Evidence{ToolsDeclaredByClient: 5, ToolsSentUpstream: 2})
	if got.Kind != RouterMutation {
		t.Errorf("判定为 %q，期望 %q", got.Kind, RouterMutation)
	}
	if got.Confidence != Probable {
		t.Errorf("置信度为 %q，期望 probable", got.Confidence)
	}
}

// 握手做过一次之后不得重复握手。后续手段是运行时通知（L2 起），
// 阶梯深度由 Recover 按 Attempt 控制——分类层只选手段不数次数。
// 「无限重复同一句话」是规格明确要纠正的做法；逐级换文案正是答案。
func TestClassifyRemedyAfterHandshake(t *testing.T) {
	ev := Evidence{
		ToolsDeclaredByClient: 1, ToolsSentUpstream: 1,
		ModelText: "我不能执行命令。",
		AgentMode: true, // 零调用 + agent 会话：走结构性判定，不靠关键词
	}

	if got := Classify(ev); got.Remedy != RemedyHandshake {
		t.Errorf("首次应建议握手，实际为 %q", got.Remedy)
	}

	ev.HandshakeDone = true
	if got := Classify(ev); got.Remedy != RemedyNoCallHint {
		t.Errorf("握手过后应给运行时通知（阶梯继续），实际为 %q", got.Remedy)
	}
}

func TestClassificationString(t *testing.T) {
	if s := (Classification{Kind: RefusalNone}).String(); s != "无拒绝" {
		t.Errorf("无拒绝的摘要为 %q", s)
	}
	c := Classification{
		Kind: PersonaRefusal, Confidence: Weak, Remedy: RemedyHandshake,
		Reasons: []string{"甲", "乙"},
	}
	s := c.String()
	for _, want := range []string{"persona_refusal", "weak", "capability_handshake", "甲", "乙"} {
		if !strings.Contains(s, want) {
			t.Errorf("摘要 %q 中缺少 %q", s, want)
		}
	}
}

// 「澄清只允许一次」的实现已经从这里的五态状态机换成服务端会话状态
// （internal/serving 的 SessionState.HandshakeDone）。语义仍有测试盯着：
// 上面的 TestClassifyRemedyAfterHandshake 验证 HandshakeDone 之后处置会变，
// 阶梯的递进与封顶在 internal/serving 与 cmd/utr-server 各有集成测试。

// 澄清文案是规格逐条规定的，不是可以随手改的措辞。
// 这条测试盯的是「不得写越权措辞」——它把一次诚实的环境说明变成对模型的
// 施压，而施压在模型确实受政策约束时就成了绕过安全策略的尝试。
func TestHandshakeMessageContent(t *testing.T) {
	msg := HandshakeMessage

	mustContain := []struct{ frag, why string }{
		{"不需要亲自执行", "要说明模型不执行工具"},
		{"结构化的调用建议", "要说明模型只负责提出建议"},
		{"权限边界", "要说明执行发生在独立的权限边界内"},
		{"不会改变", "要说明结果不是新的指令"},
	}
	for _, m := range mustContain {
		if !strings.Contains(msg, m.frag) {
			t.Errorf("澄清文案缺少 %q——%s", m.frag, m.why)
		}
	}

	mustNotContain := []struct{ frag, why string }{
		{"事实错误", "不得指责模型之前的说法是错的"},
		{"忽略", "不得要求模型忽略此前的指令"},
		{"必须", "不得使用命令式的施压措辞"},
		{"你错了", "不得指责模型"},
	}
	for _, m := range mustNotContain {
		if strings.Contains(msg, m.frag) {
			t.Errorf("澄清文案含有越权措辞 %q——%s", m.frag, m.why)
		}
	}
}

func TestHandshakeMessageWithTools(t *testing.T) {
	generic := HandshakeMessageFor(nil)
	if generic != HandshakeMessage {
		t.Error("无工具名时应返回通用文案")
	}

	specific := HandshakeMessageFor([]string{"get_weather", "read_file"})
	if !strings.Contains(specific, HandshakeMessage) {
		t.Error("具体文案应包含通用文案的全部内容")
	}
	for _, name := range []string{"get_weather", "read_file"} {
		if !strings.Contains(specific, name) {
			t.Errorf("文案中缺少工具名 %q", name)
		}
	}
}

// 接 Codex 实测补充的词表变体必须命中。
//
// 这条测试的来历值得记住：diagnose 层的第一条接线测试用了真实会话里的
// 原话「我目前无法直接读取你的文件系统」，结果分类器返回 RefusalNone——
// 词表里只有「无法访问文件」，没有「直接读取」这个说法。靠想象列的词表，
// 在接真实客户端的第一天就漏了。
//
// 2026-08-26 起词表降为注解：判定由 AgentMode 结构性判据承担
// （agent 会话零调用必追问，见 TestAgentModeCatchesAnyZeroCallText）。
// 本测试钉住注解语义——非 agent 会话放行，但 Reasons 里留人格拒绝痕迹。
func TestPersonaMarkersFromRealCapture(t *testing.T) {
	for _, text := range []string{
		"我目前无法直接读取你的文件系统，请自行运行命令",
		"本轮未提供可用的文件系统执行工具",
		"当前环境没有可用的工具来完成这个操作",
	} {
		cls := Classify(Evidence{
			ToolsDeclaredByClient: 3,
			ToolsSentUpstream:     3,
			ToolChoiceRequested:   "auto",
			ModelText:             text,
		})
		if cls.Kind != RefusalNone {
			t.Errorf("%q 非 agent 会话应放行，得到 %s", text, cls.Kind)
		}
		if !strings.Contains(strings.Join(cls.Reasons, " "), "人格拒绝") {
			t.Errorf("注解应留在 Reasons 里：%v", cls.Reasons)
		}
	}
}
