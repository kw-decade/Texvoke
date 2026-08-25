package toolbridge

import (
	"errors"
	"fmt"
	"strings"
)

// ErrorKind 是错误的分类。
//
// 接入方需要区分「重试一次可能就好了」和「这条路走不通」，
// 而一句字符串给不出这个判断。用 errors.As 取出 *Error 再看 Kind。
type ErrorKind string

const (
	// ErrConfig：配置或用法有问题，重试没有意义。
	ErrConfig ErrorKind = "config"

	// ErrNoTools：没有工具可编译。
	//
	// 这不是框架的问题，而是一个需要向上报告的信号：客户端没有声明工具时，
	// 再强的 Prompt 也变不出工具来。该去查 SDK 用法、模型目录与路由配置。
	ErrNoTools ErrorKind = "no_tools"

	// ErrInvalidTool：工具声明本身不合规。
	ErrInvalidTool ErrorKind = "invalid_tool"

	// ErrParseFailed：模型输出了信号，但后面的结构解析不出来。
	//
	// 这一类值得重试：把具体的格式错误反馈给模型，它多半能改对。
	// 但重试次数要有上限，且只能在尚未向客户端提交不可撤回的字节时进行。
	ErrParseFailed ErrorKind = "parse_failed"

	// ErrTruncated：模型输出了信号，但结构在闭合前断掉。
	//
	// 与 ErrParseFailed 分开是有用的：截断通常是上游断流或达到 token 上限，
	// 而不是模型不会写格式。前者该查链路，后者该调 Prompt。
	ErrTruncated ErrorKind = "truncated"

	// ErrLimitExceeded：超过了配置的资源上限。
	ErrLimitExceeded ErrorKind = "limit_exceeded"
)

// Error 是本包返回的错误。
type Error struct {
	Kind ErrorKind
	err  error
}

func (e *Error) Error() string {
	return fmt.Sprintf("toolbridge[%s]: %v", e.Kind, e.err)
}

func (e *Error) Unwrap() error { return e.err }

// Retryable 报告这类错误是否值得把具体错误反馈给模型后重试一次。
//
// 它只回答「值不值得」，不回答「该重试几次」——次数、退避与截止时间
// 由接入方按自己的预算决定。
func (e *Error) Retryable() bool {
	return e.Kind == ErrParseFailed
}

func wrap(kind ErrorKind, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Kind: kind, err: err}
}

// classify 把内部错误映射成分类错误。
//
// 依据错误文本判断分类是不得已：内部包用的是 fmt.Errorf 而非哨兵错误。
// 这层映射是门面的职责——让内部实现保持简单，把分类的复杂度收在这里。
// 内部包将来引入哨兵错误时，这个函数应当改用 errors.Is。
func classify(err error) error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return err
	}

	msg := err.Error()
	switch {
	case strings.Contains(msg, "超过上限"), strings.Contains(msg, "上限"):
		return wrap(ErrLimitExceeded, err)
	case strings.Contains(msg, "闭合前中断"), strings.Contains(msg, "截断"):
		return wrap(ErrTruncated, err)
	default:
		return wrap(ErrParseFailed, err)
	}
}

// WrapKind 按分类包装一段错误文本。
//
// 给 HTTP 门面用：/v1/parse 把错误序列化成 error + error_kind 两个字段，
// 调用方在 /v1/recover 里带回来时要能还原成同一种错误。没有它，还原侧
// 只能拿字符串重新猜一遍分类——那正是 classify 注释里说的不得已。
func WrapKind(kind ErrorKind, msg string) error {
	return wrap(kind, errors.New(msg))
}

// KindOf 从一个错误里取出分类，取不到时返回空串。
//
// 这是给接入方用的便捷函数，省去自己写 errors.As。
func KindOf(err error) ErrorKind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return ""
}
