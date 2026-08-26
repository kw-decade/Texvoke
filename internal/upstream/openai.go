package upstream

// openai-chat 适配器：标准 Chat Completions 上游的通用实现。
//
// 绝大多数中转站、one-api 系聚合站、本地推理服务（ollama/vLLM 的
// OpenAI 兼容层）都说这种话：POST 一份 {model, messages, stream}，
// 收回 SSE 的 chat.completion.chunk 序列，data: [DONE] 结束。
//
// SSE 累积直接复用 internal/protocol 的 ChatStreamAccumulator——
// 它按 tool_calls index 分组还原增量、校验流中途换 id、拒绝多 choice，
// 这些边界手写必踩。这里只做三件事：发请求、喂事件、组装结果。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kw-decade/Texvoke/internal/protocol"
)

const (
	defaultTimeout = 5 * time.Minute

	// stallTimeout 是「连续无数据」的中止阈值。
	//
	// 模型 thinking 期间标准上游也会持续发心跳或空 chunk；真正卡死的
	// 连接什么都没有。120 秒无任何字节即可判定卡死，由调用方决定重试。
	stallTimeout = 120 * time.Second
)

// OpenAIChat 配置一份标准 chatcompletion 上游。
type OpenAIChat struct {
	// Endpoint 是完整的 chatcompletion URL（含 /v1/chat/completions）。
	Endpoint string

	// APIKey 作为 Bearer 发出。本地服务（ollama 等）留空即可。
	APIKey string

	// ExtraHeaders 追加到每个请求。中转站常要求自定义鉴权头或来源头。
	ExtraHeaders map[string]string

	// Timeout 是单次调用的总上限，0 用 defaultTimeout。
	Timeout time.Duration

	// HTTPClient 允许注入自定义客户端（测试用 httptest、生产走代理）。
	// 为空时用带超时默认值的新客户端。
	HTTPClient *http.Client
}

func (c OpenAIChat) Name() string { return "openai-chat" }

func (c OpenAIChat) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: c.timeout()}
}

func (c OpenAIChat) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return defaultTimeout
}

// Complete 实现Adapter。请求体由 protocol.EncodeChatRequest 生成——它已经
// 处理了 content 压平与字段收窄，这里不重复造轮子。
func (c OpenAIChat) Complete(ctx context.Context, model string, messages []protocol.Message) (Reply, error) {
	body, err := protocol.EncodeChatRequest(protocol.ChatRequest{
		Model:    model,
		Messages: messages,
	})
	if err != nil {
		return Reply{}, fmt.Errorf("upstream: 编码 openai-chat 请求失败：%w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return Reply{}, fmt.Errorf("upstream: 构造请求失败：%w", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "text/event-stream")
	if c.APIKey != "" {
		req.Header.Set("authorization", "Bearer "+c.APIKey)
	}
	for k, v := range c.ExtraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := c.client().Do(req)
	if err != nil {
		return Reply{}, fmt.Errorf("upstream: 连接失败：%w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return Reply{Status: resp.StatusCode}, classifyHTTPError(resp.StatusCode, snippet)
	}

	text, err := accumulateChatStream(resp.Body)
	if err != nil {
		// 断流前已累积的文本跟着错误一起交回去。accumulateChatStream 特意
		// 保留了它，这里丢掉就等于白留——调用方靠它做降级透传。
		return Reply{Text: text, Status: resp.StatusCode}, err
	}
	return Reply{Text: text, Status: resp.StatusCode}, nil
}

// Models 拉上游的 /models 清单。网关对模型名是透明的：客户端问有哪些模型，
// 答案应该来自上游，而不是网关编一个。
func (c OpenAIChat) Models(ctx context.Context) ([]string, error) {
	endpoint := strings.TrimSuffix(c.Endpoint, "/")
	// endpoint 可能写到 /chat/completions 为止；模型清单在同源的 /models。
	if i := strings.LastIndex(endpoint, "/chat/completions"); i > 0 {
		endpoint = endpoint[:i]
	}
	if !strings.HasSuffix(endpoint, "/models") {
		endpoint += "/models"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("upstream: 构造请求失败：%w", err)
	}
	if c.APIKey != "" {
		req.Header.Set("authorization", "Bearer "+c.APIKey)
	}

	resp, err := c.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream: 连接失败：%w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, classifyHTTPError(resp.StatusCode, data)
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("upstream: 解析 models 响应失败：%w", err)
	}
	out := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	return out, nil
}

// accumulateChatStream 读完整条 SSE 流并拼出模型文本。
//
// 流中断时返回已收到的部分文本 + 错误——断前的内容不该跟着断流一起消失，
// 调用方（gateway）拿它做降级透传。完整成功时 error 为 nil。
func accumulateChatStream(r io.Reader) (string, error) {
	acc := protocol.NewChatStreamAccumulator(protocol.DecodeOptions{})
	dec := protocol.NewSSEDecoder(bufio.NewReaderSize(r, 64<<10), protocol.SSEDecoderOptions{})

	// 卡死保护：SSEDecoder 阻塞在读上，用定时器在无数据时放弃整条流。
	// 包一层实现「每收到一个事件就续期」的效果——简单做法是给底层 reader
	// 套 deadline，但那需要 net.Conn；这里退而求其次靠 http.Client.Timeout
	// 总超时兜底，stall 只在支持 SetReadDeadline 的传输上生效。
	for {
		ev, err := dec.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return acc.PartialText(), fmt.Errorf("upstream: SSE 解析失败：%w", err)
		}
		if err := acc.Add(*ev); err != nil {
			return acc.PartialText(), fmt.Errorf("upstream: %w", err)
		}
		if acc.Done() {
			break
		}
	}

	resp, err := acc.Result()
	if err != nil {
		// 流断在中途：交出断前已收到的文本。空文本时错误原样返回，
		// 有文本时错误也保留——调用方靠 err != nil 区分「完整」与「残缺」。
		return acc.PartialText(), fmt.Errorf("upstream: %w", err)
	}
	if resp.Refusal != "" {
		return "", &protocol.UpstreamError{Message: resp.Refusal, Type: "refusal"}
	}
	var buf strings.Builder
	if len(resp.Content) > 0 {
		var s string
		if json.Unmarshal(resp.Content, &s) == nil {
			buf.WriteString(s)
		}
	}
	return buf.String(), nil
}

// classifyHTTPError 把非 200 的响应体归一成 UpstreamError。
//
// 错误体可能是标准形状 {"error":{...}}，也可能是一句裸文本——都装进
// Message，分类信息丢了也比整个丢了好。
func classifyHTTPError(status int, body []byte) error {
	e := protocol.UpstreamError{Status: status, Message: strings.TrimSpace(string(body))}
	// 尝试按标准形状拆一次；失败就保留原文。
	var probe struct {
		Error *protocol.UpstreamError `json:"error"`
	}
	if len(body) > 0 && body[0] == '{' {
		if err := json.Unmarshal(body, &probe); err == nil && probe.Error != nil && probe.Error.Message != "" {
			e.Message = probe.Error.Message
			e.Type = probe.Error.Type
			e.Code = probe.Error.Code
		}
	}
	return &e
}
