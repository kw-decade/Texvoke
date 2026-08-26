// Package gateway 是 Texvoke 的一体化编排层：客户端请求进、上游调用出、
// 救援循环与响应渲染全部在这一个包里闭环。
//
// 它取代前身实现的全部职责。前身实现时代的结构性问题是能力分散：
// Go 核心有测试网保护零事故，而 JS 胶水（编排循环、会话状态、恢复调度）
// 无测试却承担了最复杂的逻辑，三类事故全部发生在那一层。这个包把编排
// 收回测试网保护的这一侧。
//
// 编排流程（Handle 内）：
//
//	decode+compile → adapter.Complete → parse → 失败则 recover 循环 → render
//
// 三条从实战事故固化的纪律在这里生效：
//   - 异常路径永不杀死请求：parse/recover 异常降级为纯文本透传
//   - 会话阶梯由服务端状态驱动：递进、封顶停手、成功归位全自动
//   - 上游瞬态故障静默重试（仅限尚未产出任何结果时）
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/kw-decade/Texvoke/internal/observability"
	"github.com/kw-decade/Texvoke/internal/protocol"
	"github.com/kw-decade/Texvoke/internal/serving"
	"github.com/kw-decade/Texvoke/internal/upstream"
	"github.com/kw-decade/Texvoke/pkg/toolbridge"
)

// maxRecoverRounds 是防御性兜底上限。正常情况下阶梯封顶就会停手，
// 这个数只防「状态异常导致无限循环」这一类意外。前身实现同款值。
const maxRecoverRounds = 10

// maxUpstreamRetries 是上游瞬态故障的静默重试次数。
// 前身实现的健壮重试逻辑实测：偶发空响应/连接重置一次重试就能
// 覆盖绝大多数，3 次是余量。永久性错误不浪费重试。
const maxUpstreamRetries = 3

// defaultRequestBudget 是一次客户端请求的墙钟总预算。
//
// 为什么必须有这个数：救援循环最多 maxRecoverRounds 轮，每轮的上游调用又
// 各自带 5 分钟超时和 3 次瞬态重试——相乘的最坏情况是几十分钟，而客户端
// 早就放弃了，服务端还在替一个没人等的请求打上游。10 分钟足够跑完
// 「首轮 + L1-L4 四级追问」的正常路径，又不会让单个请求变成资源黑洞。
//
// 预算耗尽不是错误：把已经拿到的文本如实渲染回去，与上游挂掉同一处置。
const defaultRequestBudget = 10 * time.Minute

// defaultHeartbeatInterval 是伪流式等待期的保活间隔。
//
// 为什么必须有：HandleSSE 的第一个数据事件要等上游说完，而一轮上游调用加上
// 阶梯追问可能要几十秒到几分钟。客户端在这段静默里会判定连接已死并断开——
// 服务端还在正常干活，客户端已经走了，用户看到的是「请求失败」。
// SSE 注释帧（`: ...` 开头）不携带内容且规范要求所有客户端忽略它，是保持
// 连接最便宜的方式。15 秒给最激进的 30 秒空闲阈值留了一倍余量。
const defaultHeartbeatInterval = 15 * time.Second

// keepaliveFrame 是心跳写出的字节。SSE 注释帧，一个字节的语义都没有。
const keepaliveFrame = ": texvoke keepalive\n\n"

// Config 配置一个网关实例。
type Config struct {
	Bridge *toolbridge.Bridge

	// Adapter 是纯文本上游。openai-chat 覆盖大多数场景；自定义适配器见 upstream 包的 Adapter 接口。
	Adapter upstream.Adapter

	// UpstreamModel 覆盖发给上游的模型名。为空时保留客户端写的那个。
	UpstreamModel string

	// AlwaysInclude / MaxTools / MaxToolDescBytes / ExtraInstructions
	// 语义与 sidecar /v1/adapt 的同名参数一致。
	AlwaysInclude     []string
	MaxTools          int
	MaxToolDescBytes  int
	ExtraInstructions string

	// RequestBudget 是单次请求的墙钟总上限，0 用 defaultRequestBudget。
	// 负值表示不设预算（只受客户端断开约束）——只在调试时用。
	RequestBudget time.Duration

	// HeartbeatInterval 覆盖流式等待期的保活间隔，0 用
	// defaultHeartbeatInterval。负值关闭心跳。
	HeartbeatInterval time.Duration

	Log *slog.Logger
}

// Gateway 是配置完成的网关。Handle 并发安全。
type Gateway struct {
	cfg  Config
	log  *slog.Logger
	sess *serving.SessionStore
}

// New 创建网关。
func New(cfg Config) (*Gateway, error) {
	if cfg.Bridge == nil {
		return nil, fmt.Errorf("gateway: 需要 Bridge")
	}
	if cfg.Adapter == nil {
		return nil, fmt.Errorf("gateway: 需要 Adapter")
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Gateway{cfg: cfg, log: log, sess: serving.NewSessionStore()}, nil
}

// withBudget 给一次请求套上墙钟预算。返回的 cancel 必须被调用。
//
// 客户端自己的 ctx 仍然有效——谁先到期谁生效，所以客户端断开依然是
// 立刻停手。负值表示放弃预算保护，只在本地调试时用。
func (g *Gateway) withBudget(ctx context.Context) (context.Context, context.CancelFunc) {
	d := g.cfg.RequestBudget
	switch {
	case d == 0:
		d = defaultRequestBudget
	case d < 0:
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}

/* ---------- 会话键 ---------- */

// sessionKeyOf 计算会话键：Codex 带 prompt_cache_key 用它；
// 其余客户端用首条消息的前 200 字节摘要兜底。与前身实现同一算法，
// 保证迁移前后同一个对话落在同一个键上。
func sessionKeyOf(body []byte) string {
	var probe struct {
		PromptCacheKey string          `json:"prompt_cache_key"`
		Input          json.RawMessage `json:"input"`
		Messages       json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	if probe.PromptCacheKey != "" {
		return "pck:" + probe.PromptCacheKey
	}
	first := firstItem(probe.Input)
	if len(first) == 0 {
		first = firstItem(probe.Messages)
	}
	head := string(first)
	if len(head) > 200 {
		head = head[:200]
	}
	return "hash:" + observability.Digest(head)[:24]
}

// firstItem 取 JSON 数组的第一个元素原文。不是数组就返回 nil。
func firstItem(raw json.RawMessage) []byte {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil || len(arr) == 0 {
		return nil
	}
	return arr[0]
}

// randomID 生成请求/会话标识。只需要唯一性，hex 随机数足够。
func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("gateway: 系统随机源不可用：%v", err))
	}
	return hex.EncodeToString(b[:])
}

/* ---------- 请求处理 ---------- */

// Handle 处理一次完整的客户端请求。
//
// protocolName 是客户端说的话（chat / anthropic / responses），body 是原始
// 请求体。返回可直接写回客户端的 JSON 响应体。HTTP 细节由调用方处理，
// httptest 单测与真实监听共用这一条路径。流式见 HandleSSE。
func (g *Gateway) Handle(ctx context.Context, protocolName string, body []byte) ([]byte, error) {
	proto := toolbridge.Protocol(protocolName)
	if !proto.Valid() {
		return nil, fmt.Errorf("gateway: 未知协议 %q", proto)
	}
	ctx, cancel := g.withBudget(ctx)
	defer cancel()
	sessionID := randomID()
	requestID := randomID()
	sk := sessionKeyOf(body)

	// ---- adapt：解码 + 编译协议说明 + 记录会话事实 ----
	decoded, sess, compiled, clientModel, err := g.adapt(proto, sessionID, requestID, body, sk)
	if err != nil {
		return nil, err
	}
	g.log.Debug("adapt",
		"session", observability.Digest(sessionID)[:8],
		"protocol", proto,
		"virtual_protocol", compiled.VirtualProtocol,
		"tools_included", len(compiled.ToolsIncluded))

	// ---- 上游调用 → 解析 → 救援循环 ----
	res := g.loop(ctx, sess, decoded, sk, compiled.ToolsIncluded)

	// ---- render：无论救援走到哪一步，都要渲染回客户端协议 ----
	out, err := g.render(sess, decoded, res, clientModel)
	if err != nil {
		return nil, fmt.Errorf("gateway: 渲染 %s 响应失败（outcome=%s）：%w",
			proto, res.Outcome, err)
	}
	return out, nil
}

// adapt 完成 decode→compile 两步，返回渲染阶段要用的全部句柄。
func (g *Gateway) adapt(proto toolbridge.Protocol, sessionID, requestID string, body []byte, sk string,
) (*toolbridge.Request, *toolbridge.Session, *toolbridge.CompileResult, string, error) {

	decoded, err := toolbridge.DecodeRequest(proto, body, toolbridge.DecodeOptions{
		SessionID: sessionID,
		RequestID: requestID,
	})
	if err != nil {
		return nil, nil, nil, "", fmt.Errorf("gateway: 解码 %s 请求失败：%w", proto, err)
	}

	clientModel := decoded.Model()
	if g.cfg.UpstreamModel != "" {
		decoded.SetModel(g.cfg.UpstreamModel)
	}

	// 会话键存在时登记「历史里已有成功调用」，供 L2 的运行时通知使用。
	if sk != "" && serving.HasToolCallInBody(body) {
		g.sess.MarkCalls(sk)
	}

	sess, err := g.cfg.Bridge.NewSession(sessionID, requestID)
	if err != nil {
		return nil, nil, nil, "", fmt.Errorf("gateway: 新建会话失败：%w", err)
	}

	compiled, err := sess.CompileRequest(decoded, toolbridge.CompileOptions{
		AlwaysInclude:     g.cfg.AlwaysInclude,
		MaxTools:          g.cfg.MaxTools,
		MaxToolDescBytes:  g.cfg.MaxToolDescBytes,
		ExtraInstructions: g.cfg.ExtraInstructions,
	})
	if err != nil {
		return nil, nil, nil, "", fmt.Errorf("gateway: 编译失败：%w", err)
	}
	return decoded, sess, compiled, clientModel, nil
}

/* ---------- 编排循环 ---------- */

// loop 执行「调上游 → 解析 → 没拿到就按阶梯追问」的完整循环。
//
// 返回的 Result 永远非 nil：任何异常路径都降级成纯文本透传，
// 绝不让调用方拿到空指针——这是前身实现三类事故里最痛的一类。
func (g *Gateway) loop(ctx context.Context, sess *toolbridge.Session, decoded *toolbridge.Request,
	sk string, toolsIncluded []string) *toolbridge.Result {

	// last 保留上一轮已经拿到的结果。上游中途失败或墙钟预算耗尽时把它交出去，
	// 而不是丢掉重来——阶梯第三轮挂掉不该让前两轮的文本一起消失。
	var last *toolbridge.Result

	for round := 0; round < maxRecoverRounds; round++ {
		text, uerr := g.completeWithRetry(ctx, decoded.Model(), decoded.Messages())
		if uerr != nil {
			// 上游彻底不可用（含永久性错误与预算耗尽）。降级顺序：
			// 断流前的部分文本 → 上一轮的完整结果 → 空文本的 plain_text。
			// 半截回答也比让用户等完一整轮重生成强，比空响应更强。
			g.log.Warn("上游调用最终失败", "round", round, "error", uerr.Error())
			if strings.TrimSpace(text) != "" {
				g.log.Info("透传断流前的部分文本", "bytes", len(text))
				return &toolbridge.Result{Outcome: toolbridge.OutcomePlainText, Text: text}
			}
			if last != nil {
				return last
			}
			return &toolbridge.Result{Outcome: toolbridge.OutcomePlainText}
		}

		res, perr := sess.Parse(text)
		if res == nil {
			// Parse 连结果对象都没给出（理论上不发生）：防御性兜底。
			res = &toolbridge.Result{Outcome: toolbridge.OutcomePlainText, Text: text}
		}
		if sk != "" && res.Outcome == toolbridge.OutcomeCallsParsed {
			// 解析出调用 = 救援成功：该会话阶梯归位。
			g.sess.Succeed(sk)
			return res
		}
		if perr != nil && res.Outcome != toolbridge.OutcomeMalformed && res.Outcome != toolbridge.OutcomeTruncated {
			// 解析器自身异常（不是模型的格式问题）：降级纯文本透传。
			// 前身实现教训：这里抛错会让整个请求死掉。
			g.log.Warn("解析异常，按纯文本透传", "error", perr.Error())
			return &toolbridge.Result{Outcome: toolbridge.OutcomePlainText, Text: text}
		}

		// 没拿到可用调用：诊断并决定要不要追问。
		last = res
		shouldRetry, msgs, reason := g.recover(sess, decoded, res, perr, sk, toolsIncluded)
		if !shouldRetry || len(msgs) == 0 {
			g.log.Info("停止追问，按当前结果渲染", "reason", reason)
			return res
		}
		// 针对性追加，不是重发同一个 prompt。
		decoded.AppendMessages(msgs)
	}
	// 防御上限兜底：理论上到不了（阶梯封顶先停手）。
	if last != nil {
		return last
	}
	return &toolbridge.Result{Outcome: toolbridge.OutcomePlainText}
}

// recover 包装 Diagnose+Recover，返回「要不要追问、追加什么消息」。
// 带会话键时阶梯由服务端状态驱动；封顶后如实停手。
func (g *Gateway) recover(sess *toolbridge.Session, decoded *toolbridge.Request, res *toolbridge.Result, perr error,
	sk string, toolsIncluded []string) (bool, []toolbridge.RecoveryMessage, string) {

	ev := toolbridge.Evidence{
		ToolsDeclared: len(toolsIncluded),
		ToolsSent:     len(toolsIncluded),
		ModelText:     res.Text,
	}
	// tool_choice 语义进证据：required/named 却零调用是格式不合规，
	// 与 auto 下「正常聊天」的处置完全不同。漏了它，required 场景会被
	// 误判成「无拒绝」直接透传——客户端在等一个不会来的调用。
	if decoded.RequireCall() {
		ev.ToolChoice = "required"
	}

	ladderExhausted := false
	if sk != "" {
		level, snap := g.sess.Advance(sk)
		if level > serving.MaxLadderLevel {
			ladderExhausted = true
		} else {
			ev.Attempt = level
			ev.HandshakeDone = snap.HandshakeDone
			ev.HasSuccessfulHistory = snap.HasCalls
		}
	} else {
		// 无会话键（理论上 gateway 总有）：退化为单请求内 L1。
		ev.Attempt = 1
	}
	if ladderExhausted {
		return false, nil, "突破阶梯已用尽：连续多轮拒绝调用，如实上报而不是继续追问"
	}

	d := sess.Diagnose(res, perr, ev)

	// 明确不重试的诊断（fix_client 等）直接停手，与 sidecar 行为一致。
	rc, err := sess.Recover(d, res, perr, toolsIncluded, ev)
	if err != nil {
		// recover 出错不该杀死请求：放弃追问，当前结果照常渲染。
		g.log.Warn("recover 异常，放弃追问", "error", err.Error())
		return false, nil, "recover error"
	}
	if rc.HandshakeDone && sk != "" {
		g.sess.MarkHandshake(sk)
	}
	g.log.Info("recover",
		"kind", d.Kind,
		"remedy", d.Remedy,
		"retry", rc.ShouldRetry,
		"reason", rc.Reason)
	return rc.ShouldRetry, rc.Messages, rc.Reason
}

// completeWithRetry 带静默重试的上游调用。
//
// 重试只针对瞬态故障（连接断、超时、上游内嵌的 country/403 类抖动）；
// 内容审查等永久性错误第一次就返回。判定复用 capability 的传输层经验：
// 错误消息匹配瞬态特征才重试，其余直接失败。
//
// 返回值约定：错误时 string 尽量非空——上游断流前已经吐出的部分文本
// 跟着错误一起交回来，调用方拿它降级透传，而不是让用户等完一整轮
// 重生成再从零开始。
func (g *Gateway) completeWithRetry(ctx context.Context, model string, messages []protocol.Message) (string, error) {
	var lastErr error
	var lastPartial string
	for attempt := 0; attempt <= maxUpstreamRetries; attempt++ {
		// 预算耗尽或客户端断开时立刻停手。这一步不能省：http.Client 的超时
		// 错误文本里带 timeout，会被 isTransient 判成瞬态，于是一个已经没人
		// 等的请求还要白等三轮。
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return lastPartial, lastErr
			}
			return "", err
		}
		reply, err := g.cfg.Adapter.Complete(ctx, model, messages)
		if err == nil {
			if strings.TrimSpace(reply.Text) != "" {
				return reply.Text, nil
			}
			// 空响应按瞬态处理：这类上游实测偶发空回复，重试即恢复。
			lastErr = errors.New("上游返回空响应")
		} else {
			lastErr = err
			// 断流前已收到的部分文本记下来。重试成功就用完整的，
			// 全部失败时把它交出去——半截回答好过一无所有。
			if strings.TrimSpace(reply.Text) != "" {
				lastPartial = reply.Text
			}
			if !isTransient(err) {
				return reply.Text, err
			}
		}
		if attempt < maxUpstreamRetries {
			g.log.Warn("上游瞬态故障，重试", "attempt", attempt+1, "error", lastErr.Error())
		}
	}
	return lastPartial, lastErr
}

// isTransient 判断上游错误是否值得重试。与前身实现的正则同源：
// /country|403|econnreset|socket|timeout|proxy|fetch/i，另加 refused /
// eof / broken pipe——本地推理服务重启窗口、负载均衡摘除节点都表现为
// connection refused，与 reset 同属「等一下就好」类。
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, kw := range []string{"country", "403", "econnreset", "connection reset", "connection refused", "socket", "timeout", "proxy", "fetch", "eof", "broken pipe"} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}

/* ---------- 流式保活 ---------- */

// heartbeat 在伪流式的等待期里定期写 SSE 注释帧保活。
//
// 停止是同步的：Stop 返回后保证不会再有心跳写入 w。所以调用方不需要为
// 「心跳与真实事件同时写」加锁——把 Stop 放在渲染器动笔之前就够了。
// 少一把锁少一类交错 bug。
type heartbeat struct {
	stop chan struct{}
	done chan struct{}
}

// startHeartbeat 起一个心跳协程。every 为 0 取默认间隔，负值直接关掉。
func startHeartbeat(w io.Writer, every time.Duration) *heartbeat {
	h := &heartbeat{stop: make(chan struct{}), done: make(chan struct{})}
	if every < 0 {
		close(h.done)
		return h
	}
	if every == 0 {
		every = defaultHeartbeatInterval
	}
	go func() {
		defer close(h.done)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-h.stop:
				return
			case <-t.C:
				if _, err := io.WriteString(w, keepaliveFrame); err != nil {
					// 写失败说明客户端已经走了。停手即可：真正的收尾由
					// ctx 取消驱动，心跳不该自己判断请求该不该继续。
					return
				}
				// 不 Flush 等于没有心跳：字节会停在 net/http 的 4KB 缓冲里，
				// 客户端仍然什么都收不到。
				if f, ok := w.(interface{ Flush() }); ok {
					f.Flush()
				}
			}
		}
	}()
	return h
}

// Stop 停掉心跳并等它真的停下。只能调用一次。
func (h *heartbeat) Stop() {
	close(h.stop)
	<-h.done
}

/* ---------- 渲染 ---------- */

func (g *Gateway) render(sess *toolbridge.Session, decoded *toolbridge.Request, res *toolbridge.Result, clientModel string) ([]byte, error) {
	return sess.RenderResponse(decoded, res, toolbridge.RenderOptions{Model: clientModel})
}

// HandleSSE 是 Handle 的流式版本：把结果渲染成客户端协议的 SSE 事件序列
// 写进 w。事件形状完全正确（该有的 message_stop / [DONE] 一个不少），
// 但第一个字要等上游说完——伪流式与闭环恢复互斥，这是当前架构的自觉取舍。
//
// 等待期不是静默的：心跳写 SSE 注释帧保住连接，否则客户端会在服务端仍在
// 正常工作时先超时断开。
//
// 与 Handle 共用同一条编排循环；差异只在最后的渲染出口。
func (g *Gateway) HandleSSE(ctx context.Context, protocolName string, body []byte, w io.Writer) error {
	proto := toolbridge.Protocol(protocolName)
	if !proto.Valid() {
		return fmt.Errorf("gateway: 未知协议 %q", proto)
	}
	ctx, cancel := g.withBudget(ctx)
	defer cancel()
	sessionID := randomID()
	requestID := randomID()
	sk := sessionKeyOf(body)

	decoded, sess, compiled, clientModel, err := g.adapt(proto, sessionID, requestID, body, sk)
	if err != nil {
		return err
	}

	// 从这里开始要等上游。心跳上岗，渲染器动笔之前必须停掉——Stop 是同步的，
	// 返回后 w 又归渲染器独占，不需要锁。
	hb := startHeartbeat(w, g.cfg.HeartbeatInterval)
	res := g.loop(ctx, sess, decoded, sk, compiled.ToolsIncluded)
	hb.Stop()

	sr, err := sess.NewStreamRenderer(decoded, w, toolbridge.RenderOptions{Model: clientModel})
	if err != nil {
		return fmt.Errorf("gateway: 建立流式渲染失败：%w", err)
	}
	if err := sr.Finish(*res); err != nil {
		return fmt.Errorf("gateway: 渲染 SSE 失败：%w", err)
	}
	return nil
}
