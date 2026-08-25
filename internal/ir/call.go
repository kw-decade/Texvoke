package ir

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// CallSource 记录一个调用提案是怎么产生的。
//
// 这个字段只用于观测和降级决策，不参与授权：无论来自原生 tool calling 还是
// 虚拟协议解析，提案都要走同一条校验路径。
type CallSource string

const (
	SourceNative  CallSource = "native"  // 上游原生返回的结构化 tool call
	SourceVirtual CallSource = "virtual" // 从文本协议 envelope 解析得到
)

// Valid 报告 s 是否是已定义的来源。空值永远无效。
func (s CallSource) Valid() bool {
	switch s {
	case SourceNative, SourceVirtual:
		return true
	default:
		return false
	}
}

// 调用生命周期状态机（proposed → validating → approved → executing → settled）
// 随 4303ee5 的执行层一起失去意义：本项目现在只把调用提案交回客户端，
// 由客户端在自己的权限边界内决定执不执行。三个终态之后的状态在这里永远
// 不会发生，留着它们等于宣称一道本进程并不设防的门。
//
// 客户端侧要复现那道门，需要的是 ToolCallProposal 的内容加自己的策略，
// 不是这个包的枚举。

// ToolCallProposal 是模型提出的一次工具调用建议。
//
// 名字用 Proposal 而不是 Call 是刻意的：模型产出的永远只是候选程序，
// 执行与否由客户端决定。这个命名要在整个代码库里保持一致。
type ToolCallProposal struct {
	SessionID string `json:"session_id"`
	RequestID string `json:"request_id"`

	// CallID 保存上游协议给出的原始 ID（OpenAI 的 call_xxx、Anthropic 的
	// toolu_xxx 等）原样不动。规格三章列为必须纠正的问题：ID 一旦被重新生成，
	// 客户端就无法把结果关联回自己发出的调用。
	CallID string `json:"call_id"`

	// ProtocolItemID 是承载这次调用的协议 item 标识，目前只有 OpenAI
	// Responses 用得上（形如 fc_xxx）。为空表示该协议没有这个概念。
	//
	// 它与 CallID 是两个不同的东西，必须分开保存：CallID 把调用与结果
	// 关联起来，ProtocolItemID 标识流式事件里的那个 output item。规格三章
	// 把「每次渲染重新生成 ID」列为必须纠正的问题——流式事件里的 item_id
	// 与最终 response.output 里的 id 必须相同，客户端才能把增量拼回同一个
	// item。用一个字段硬扛两种 ID，重新渲染时必然丢一个。
	ProtocolItemID string `json:"protocol_item_id,omitempty"`

	Tool ToolID `json:"tool"`

	// Arguments 保留模型给出的原始 JSON 字节，不提前解析成 map。
	// 提前解析会丢失大整数精度和键序，而这两者都可能影响后续的幂等键计算。
	Arguments json.RawMessage `json:"arguments"`

	// ArgumentForm 说明 Arguments 该怎么读。零值是 InputFormObject。
	//
	// 形态为 text 时，Arguments 里存的是一个 JSON 字符串标量，真正的内容要用
	// ArgumentsText 取出来。之所以不直接存裸文本：Arguments 会被原样序列化进
	// HTTP 响应，装裸文本会让整个响应变成非法 JSON。
	ArgumentForm InputForm `json:"argument_form,omitempty"`

	Source CallSource `json:"source"`

	// RawCandidateDigest 是模型原始输出片段的 SHA-256 十六进制摘要。
	// 存摘要而不存原文，是为了让审计能证明“模型当时输出的就是这一段”，
	// 同时不把可能含敏感内容的原文写进日志。
	//
	// 语义边界：它记录的是**本次解码所见的那份原文**的指纹，不保证跨转发
	// 稳定。同一个调用经过一轮编解码后排版会被归一化，摘要随之改变——这是
	// 正确的，因为它证明的正是“我这一跳看到的字节长什么样”。真正有审计
	// 价值的是 Runtime 直接从上游响应解析出的那一次；历史消息里的调用可能
	// 已经过多轮序列化，其摘要只说明本跳所见。
	RawCandidateDigest string `json:"raw_candidate_digest,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

// Validate 检查提案自身的结构完整性。
//
// 这里只管“这条提案本身是否成形”，不查工具是否存在、参数是否符合 Schema、
// 策略是否允许——那三件事分别属于注册表、schema 包和 policy 包。
func (p ToolCallProposal) Validate() error {
	if p.SessionID == "" {
		return fmt.Errorf("ir: 调用提案缺少 session_id")
	}
	if p.RequestID == "" {
		return fmt.Errorf("ir: 调用提案缺少 request_id")
	}
	if p.CallID == "" {
		return fmt.Errorf("ir: 调用提案缺少 call_id")
	}
	if !p.Tool.Valid() {
		return fmt.Errorf("ir: 调用提案的工具标识非法：%+v", p.Tool)
	}
	if !p.Source.Valid() {
		return fmt.Errorf("ir: 调用提案的 source 非法：%q", p.Source)
	}
	if !p.ArgumentForm.Valid() {
		return fmt.Errorf("ir: 调用提案 %s 的 argument_form 非法：%q", p.CallID, p.ArgumentForm)
	}
	// 参数必须成形。规格三章禁止把解析失败的内容塞进兜底字段后继续执行，
	// 所以这里宁可报错也不接受空值。
	if len(p.Arguments) == 0 {
		if p.ArgumentForm.Text() {
			return fmt.Errorf("ir: 调用提案 %s 缺少 arguments", p.CallID)
		}
		return fmt.Errorf("ir: 调用提案 %s 缺少 arguments，空参数应显式写成 {}", p.CallID)
	}
	if p.ArgumentForm.Text() {
		// 裸文本形态存的是 JSON 字符串标量，只验它确实是个字符串。
		var s string
		if err := json.Unmarshal(p.Arguments, &s); err != nil {
			return fmt.Errorf("ir: 调用提案 %s 的 arguments 不是 JSON 字符串：%w", p.CallID, err)
		}
	} else {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(p.Arguments, &probe); err != nil {
			return fmt.Errorf("ir: 调用提案 %s 的 arguments 不是 JSON 对象：%w", p.CallID, err)
		}
	}
	if p.CreatedAt.IsZero() {
		return fmt.Errorf("ir: 调用提案 %s 缺少 created_at", p.CallID)
	}
	return nil
}

// ArgumentsText 取出裸文本形态的参数原文。
//
// 只对 ArgumentForm 为 text 的提案有意义；形态是对象时返回错误，而不是
// 把 JSON 对象的字节当成文本交出去——那会让调用方拿到一段看着像内容的垃圾。
func (p ToolCallProposal) ArgumentsText() (string, error) {
	if !p.ArgumentForm.Text() {
		return "", fmt.Errorf("ir: 调用提案 %s 的参数是 JSON 对象，不是裸文本", p.CallID)
	}
	var s string
	if err := json.Unmarshal(p.Arguments, &s); err != nil {
		return "", fmt.Errorf("ir: 调用提案 %s 的 arguments 不是 JSON 字符串：%w", p.CallID, err)
	}
	return s, nil
}

// IdempotencyKey 计算这次调用的幂等键，用于执行账本去重。
//
// 键覆盖会话、工具标识和参数三者：换了会话、换了工具版本或改了任何一个参数，
// 都算另一次调用，必须重新执行。同一次调用的重试则得到相同的键，从而被账本
// 拦住，不会重复产生副作用。
//
// ponytail: 参数直接按原始字节哈希，未做 canonical JSON 规范化。这意味着语义
// 相同但字节不同的两份参数（键序不同、空白不同）会得到不同的键。重试路径复用
// 的是同一份 ToolCallProposal，字节必然相同，所以当前够用；等到需要跨请求识别
// “同一次调用”时，再引入 RFC 8785 JCS 规范化。
func (p ToolCallProposal) IdempotencyKey() string {
	h := sha256.New()
	// 每段之间写入 0x00 分隔，避免 "a"+"bc" 与 "ab"+"c" 哈希到同一个值。
	for _, part := range []string{p.SessionID, p.Tool.String()} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	h.Write(p.Arguments)
	return hex.EncodeToString(h.Sum(nil))
}

// DigestRawCandidate 计算模型原始输出片段的摘要，供 RawCandidateDigest 使用。
func DigestRawCandidate(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
