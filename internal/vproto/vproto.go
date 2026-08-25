// Package vproto 定义虚拟工具协议：把结构化的工具调用编译进纯文本，
// 再从模型的文本输出里解析回来。
//
// 单独成包是因为生成侧（prompt）与解析侧（parser）在依赖树上平级，
// 把这份契约放进任何一边都会造成反向依赖。更要紧的是：Prompt 里教给模型
// 的格式和解析器认的格式必须逐字一致，分放两处迟早会悄悄分叉。
//
// 本包只依赖标准库。
package vproto

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// Version 是协议版本。改动格式必须同时升版本——否则一个会话中途换了
// Runtime 版本，模型按旧格式输出而新解析器不认，症状会表现成
// 「模型忽然不会调用工具了」，排查方向完全错误。
//
// 加 arguments_text 标签**没有**升版本，这是想清楚了的：版本不匹配会让
// 解析器**主动拒掉**整个 envelope。而新增一个可选标签是向后兼容的——
// 旧格式的输出新解析器照样认。升了版本，反而是会话中途升级 Runtime 时
// 把还在按 version="1" 输出的模型全部打成不合规。
const Version = "1"

// 信号的固定前后缀。选 ASCII 方括号而不是尖括号或特殊 Unicode：
// 尖括号在模型输出的 HTML/XML 片段里太常见，特殊字符则可能在某些
// 上游的编码往返中被改写。
const (
	signalPrefix = "[[UTR-CALL:"
	signalSuffix = "]]"

	// nonceBytes 是随机部分的字节数。16 字节（32 个十六进制字符）
	// 足以让普通文本误撞的概率可以忽略，同时不至于长到干扰模型输出。
	nonceBytes = 16
)

// Envelope 的标签。这些是逐字匹配的，不做大小写折叠也不容错——
// 模糊匹配会让模型输出的近似标签意外命中，而那正是解析歧义的来源。
const (
	TagEnvelopeOpen  = "<tool_call_envelope"
	TagEnvelopeClose = "</tool_call_envelope>"
	TagCallOpen      = "<call"
	TagCallClose     = "</call>"
	TagTool          = "tool"
	TagArguments     = "arguments_json"

	// TagArgumentsText 承载「参数不是 JSON 对象」的工具的输入原文。
	//
	// 与 TagArguments 二选一。单独开一个标签而不是复用 arguments_json 装
	// 一个 JSON 字符串标量：让能力较弱的纯文本模型把多行代码做 JSON 转义
	// （引号、换行、反斜杠）极易出错，而这恰恰是 Codex 的主力路径。
	// CDATA 裸文本对模型友好得多。
	TagArgumentsText = "arguments_text"

	CDATAOpen  = "<![CDATA["
	CDATAClose = "]]>"
)

// Nonce 是一次会话的协议触发信号。
//
// 规格三章把「进程级固定触发符」列为必须纠正的做法：固定符号会被模型
// 从训练语料或前几轮对话里学去，在不该调用工具时也照着输出。会话级 nonce
// 让误触发只能发生在本次会话内。
//
// **它不是身份凭证。** 模型输出了正确的 nonce 只说明它读到了本轮 Prompt，
// 不证明任何权限。所有授权判断都在 policy 层，与 nonce 无关。
type Nonce struct {
	// value 是随机部分，不含前后缀。
	value string

	// 绑定信息。解析时核对这三项，防止把上一轮或另一个会话的输出
	// 当成本轮的调用——那在多轮对话里是真实会发生的：上游可能把
	// 历史消息原样回显。
	sessionID string
	requestID string
	version   string
}

// NewNonce 为一次请求生成 nonce。
//
// 用 crypto/rand 而不是 math/rand：可预测的信号等于没有信号，
// 攻击者只要猜中就能让 Runtime 把一段普通文本解析成工具调用。
func NewNonce(sessionID, requestID string) (Nonce, error) {
	if sessionID == "" || requestID == "" {
		return Nonce{}, fmt.Errorf("vproto: nonce 必须绑定非空的 session 与 request")
	}
	buf := make([]byte, nonceBytes)
	if _, err := rand.Read(buf); err != nil {
		return Nonce{}, fmt.Errorf("vproto: 生成随机 nonce 失败：%w", err)
	}
	return Nonce{
		value:     hex.EncodeToString(buf),
		sessionID: sessionID,
		requestID: requestID,
		version:   Version,
	}, nil
}

// NonceFromValue 用一个已知的随机值重建 Nonce，供解析侧与测试使用。
func NonceFromValue(value, sessionID, requestID string) (Nonce, error) {
	if !validNonceValue(value) {
		return Nonce{}, fmt.Errorf("vproto: nonce 值 %q 非法，应为 %d 个十六进制字符", value, nonceBytes*2)
	}
	if sessionID == "" || requestID == "" {
		return Nonce{}, fmt.Errorf("vproto: nonce 必须绑定非空的 session 与 request")
	}
	return Nonce{value: value, sessionID: sessionID, requestID: requestID, version: Version}, nil
}

func validNonceValue(v string) bool {
	if len(v) != nonceBytes*2 {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Value 返回随机部分。
func (n Nonce) Value() string { return n.value }

// SessionID 与 RequestID 返回绑定信息。
func (n Nonce) SessionID() string { return n.sessionID }
func (n Nonce) RequestID() string { return n.requestID }

// Version 返回协议版本。
func (n Nonce) Version() string { return n.version }

// Zero 报告这是否是一个未初始化的 nonce。
func (n Nonce) Zero() bool { return n.value == "" }

// Signal 返回完整的触发信号字符串。
//
// 协议要求它独占一行：夹在正文中间的信号无法与模型引用协议说明的情形
// 区分开——模型完全可能在解释「我该怎么调用工具」时把信号原样写出来。
func (n Nonce) Signal() string {
	return signalPrefix + n.value + signalSuffix
}

// SignalLength 返回信号的字节长度，供流式解析器计算安全窗口。
func (n Nonce) SignalLength() int { return len(n.Signal()) }

// MaxSignalLength 是任何 nonce 的信号长度上限。
//
// 流式解析器在还没拿到 nonce 时也要预留安全窗口，所以需要一个不依赖
// 具体实例的上限。
const MaxSignalLength = len(signalPrefix) + nonceBytes*2 + len(signalSuffix)

// Matches 报告一行文本是否正是本 nonce 的信号行。
//
// 严格逐字匹配整行（允许两侧空白）：子串匹配会让模型在正文里提到信号时
// 被误判成发起调用。
func (n Nonce) Matches(line string) bool {
	if n.Zero() {
		return false
	}
	return strings.TrimSpace(line) == n.Signal()
}

// LooksLikeSignal 报告一行文本是否长得像任意 nonce 的信号，
// 但未必是本次会话的那一个。
//
// 用途是诊断而非解析：命中它却没通过 Matches，说明模型回放了历史轮次的
// 信号，或者有人在尝试注入。这两种都值得记一笔，但都不该触发调用。
func LooksLikeSignal(line string) bool {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, signalPrefix) || !strings.HasSuffix(t, signalSuffix) {
		return false
	}
	inner := t[len(signalPrefix) : len(t)-len(signalSuffix)]
	return validNonceValue(inner)
}
