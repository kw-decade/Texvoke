package protocol

import (
	"encoding/json"
	"fmt"
)

// UpstreamError 是上游以结构化形式返回的错误。
//
// 单独建模而不是压成一句字符串，是因为拒绝分类要靠它：规格七章要求区分
// upstream_policy_refusal（供应商明令禁止，不得绕过）与 transport_failure
// （暂态故障，可以重试）。把 type 与 code 保留下来，capability 包才有
// 判据可依；只留一句人话消息就只能靠关键词猜，而关键词猜测正是规格
// 明确反对的做法。
type UpstreamError struct {
	// Message 是面向人的描述。它可能含上游的内部信息，
	// 不应原样转发给客户端，更不应喂回给模型。
	Message string `json:"message"`

	// Type 与 Code 是上游给出的机器可读分类，两者都可能为空。
	Type  string `json:"type,omitempty"`
	Code  string `json:"code,omitempty"`
	Param string `json:"param,omitempty"`

	// Status 是 HTTP 状态码，由发起请求的一方填入——响应体里没有它，
	// 但区分 429（限流，可退避重试）与 403（策略拒绝，不可重试）全靠它。
	Status int `json:"status,omitempty"`

	// RequestID 是上游的请求标识，用于向供应商追查。
	RequestID string `json:"request_id,omitempty"`
}

func (e *UpstreamError) Error() string {
	switch {
	case e.Type != "" && e.Code != "":
		return fmt.Sprintf("上游错误 [%s/%s]: %s", e.Type, e.Code, e.Message)
	case e.Type != "":
		return fmt.Sprintf("上游错误 [%s]: %s", e.Type, e.Message)
	default:
		return fmt.Sprintf("上游错误: %s", e.Message)
	}
}

// decodeUpstreamError 尝试把响应体解析成上游错误。
//
// 返回 nil 表示这不是一个错误响应。两种协议的错误形状一致，都是顶层
// error 对象，所以共用一份实现。
func decodeUpstreamError(top map[string]json.RawMessage) *UpstreamError {
	raw, ok := top["error"]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var e UpstreamError
	if err := json.Unmarshal(raw, &e); err != nil {
		// error 字段存在但形状不认识：仍然当成错误响应，
		// 把原文放进 Message，总好过把它当成一次正常回复继续处理。
		return &UpstreamError{Message: string(raw)}
	}
	if e.Message == "" {
		e.Message = string(raw)
	}
	return &e
}
