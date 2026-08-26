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
	"sync"
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
	// 非流式路径不需要增量交付：客户端等的是一个完整 JSON 响应体。
	res := g.loop(ctx, sess, decoded, sk, compiled.ToolsIncluded, nil)

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
//
// onChunk 非 nil 时走真流式：每一轮上游调用的原始增量都交给它（由调用方
// 经 StreamParser 过滤后转发给客户端）。阶梯追问轮同样交付——客户端看到的
// 是各轮叙述的自然拼接，与「失败回应回灌进历史」同构。
//
// newHandler 在每轮开始时被调用一次，返回该轮的增量接收器（可以是 nil，
// 表示这一轮不做增量交付）。传 nil 表示整条循环都不做流式。
func (g *Gateway) loop(ctx context.Context, sess *toolbridge.Session, decoded *toolbridge.Request,
	sk string, toolsIncluded []string, newHandler func() upstream.StreamHandler) *toolbridge.Result {

	// last 保留上一轮已经拿到的结果。上游中途失败或墙钟预算耗尽时把它交出去，
	// 而不是丢掉重来——阶梯第三轮挂掉不该让前两轮的文本一起消失。
	var last *toolbridge.Result

	for round := 0; round < maxRecoverRounds; round++ {
		var onChunk upstream.StreamHandler
		if newHandler != nil {
			onChunk = newHandler()
		}
		text, uerr := g.completeWithRetry(ctx, decoded.Model(), decoded.Messages(), onChunk)
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
		// 模型这轮的纯文本回应先回灌进历史，再追加追问。
		//
		// 不回灌的后果：下一轮请求里模型看不到自己刚说过什么——它的叙述
		// 凭空消失，取而代之的是一条追问。弱模型于是把每一轮都当成「第一次
		// 面对这个问题」，重复同样的话；L1-L4 五连全部无效的实测里，
		// 每轮 49-90 秒的生成都是在认真重写同一段汇报。回灌之后它至少
		// 看得到「我已经解释过了」，才有可能切换到行动。
		if t := res.Text; strings.TrimSpace(t) != "" {
			decoded.AppendMessages([]toolbridge.RecoveryMessage{{Role: "assistant", Text: t}})
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
			// agent 模式是结构性证据：会话历史里有成功调用、客户端要求必须
			// 调用、或客户端声明了工具（声明工具 = 客户端具备执行能力，
			// 它就在 agent 场景里）——三者任一成立时，零调用本身就是追问的
			// 充分条件，文本内容不参与判定。词表只留给日志注解。
			ev.AgentMode = snap.HasCalls || decoded.RequireCall() || len(toolsIncluded) > 0
		}
	} else {
		// 无会话键（理论上 gateway 总有）：退化为单请求内 L1。
		ev.Attempt = 1
		ev.AgentMode = decoded.RequireCall()
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
//
// onChunk 非 nil 时走真流式：适配器实现了 StreamWriter 就边收边交付，
// 没实现就回落到 Complete 后一次性交付（客户端仍是流式事件，只是没有
// 逐字节奏）。
//
// **已经交付过字节之后不再重试**：那些字节已经在客户端屏幕上，重试会让
// 同一段内容出现两遍。这不是保守，是流式的物理约束。
func (g *Gateway) completeWithRetry(ctx context.Context, model string, messages []protocol.Message,
	onChunk upstream.StreamHandler) (string, error) {

	var lastErr error
	var lastPartial string
	emitted := false
	wrapped := onChunk
	if onChunk != nil {
		wrapped = func(chunk []byte) error {
			if len(chunk) > 0 {
				emitted = true
			}
			return onChunk(chunk)
		}
	}

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
		reply, err := g.complete(ctx, model, messages, wrapped)
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
		if emitted {
			// 已经发给客户端的内容收不回来，重试等于让它看到两遍。
			g.log.Warn("已交付流式字节，放弃重试", "error", lastErr.Error())
			return lastPartial, lastErr
		}
		if attempt < maxUpstreamRetries {
			g.log.Warn("上游瞬态故障，重试", "attempt", attempt+1, "error", lastErr.Error())
		}
	}
	return lastPartial, lastErr
}

// complete 按 onChunk 是否为 nil 与适配器能力选调用方式。
func (g *Gateway) complete(ctx context.Context, model string, messages []protocol.Message,
	onChunk upstream.StreamHandler) (upstream.Reply, error) {

	if onChunk != nil {
		if sw, ok := g.cfg.Adapter.(upstream.StreamWriter); ok {
			return sw.CompleteStream(ctx, model, messages, onChunk)
		}
		// 适配器不支持流式：拿到全文后一次性交付，客户端事件形状不变。
		reply, err := g.cfg.Adapter.Complete(ctx, model, messages)
		if reply.Text != "" {
			if cerr := onChunk([]byte(reply.Text)); cerr != nil && err == nil {
				err = cerr
			}
		}
		return reply, err
	}
	return g.cfg.Adapter.Complete(ctx, model, messages)
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
// 停止是同步且幂等的：Stop 返回后保证不会再有心跳写入 w，重复调用无副作用。
// 所以调用方不需要为「心跳与真实事件同时写」加锁——真流式下在第一个增量
// 到达时停一次、循环结束再兜一次，两次都安全。少一把锁少一类交错 bug。
type heartbeat struct {
	stop chan struct{}
	done chan struct{}
	once sync.Once
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

// Stop 停掉心跳并等它真的停下。幂等：重复调用直接返回。
func (h *heartbeat) Stop() {
	h.once.Do(func() { close(h.stop) })
	<-h.done
}

/* ---------- 渲染 ---------- */

func (g *Gateway) render(sess *toolbridge.Session, decoded *toolbridge.Request, res *toolbridge.Result, clientModel string) ([]byte, error) {
	return sess.RenderResponse(decoded, res, toolbridge.RenderOptions{Model: clientModel})
}

// HandleSSE 是 Handle 的流式版本：把结果渲染成客户端协议的 SSE 事件序列
// 写进 w。
//
// **真流式**：上游吐出的字节经 StreamParser 判定「确定不是协议信号」之后
// 立刻转成客户端协议的增量事件发出。客户端看到的是逐段到达的文本，而不是
// 等整轮说完的一次性投递。
//
// 与闭环恢复的关系（这曾被判为互斥，实际不是）：协议说明教模型「先输出
// 信号再写 envelope」，所以打算调用时信号出现在最前面，StreamParser 从
// 信号起扣住全部字节——envelope 的任何片段都不会漏给客户端。模型在叙述时
// 全文都是安全文本，实时透传。阶梯追问轮同样边收边发，客户端看到的是
// 各轮叙述的自然拼接，与「失败回应回灌进历史」同构。
//
// 已交付的字节收不回来，所以两条纪律必须守住：
//   - 交付过字节之后不再做瞬态重试（见 completeWithRetry）
//   - 渲染器不重发文本，Finish 只补工具调用与终止事件
//
// 等待期（连接建立到第一个增量之间，上游排队时仍可能有几秒）由心跳兜底。
//
// 与 Handle 共用同一条编排循环；差异只在交付方式与渲染出口。
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

	sr, err := sess.NewStreamRenderer(decoded, w, toolbridge.RenderOptions{Model: clientModel})
	if err != nil {
		return fmt.Errorf("gateway: 建立流式渲染失败：%w", err)
	}

	// 心跳只覆盖「连接建立到第一个增量」的空窗。第一个增量到达前必须停掉：
	// 之后 w 归渲染器独占，两个 goroutine 同时写同一个 writer 是数据竞争。
	// Stop 是同步且幂等的，所以这里停一次、loop 之后再兜一次都安全。
	hb := startHeartbeat(w, g.cfg.HeartbeatInterval)
	defer hb.Stop()

	// streamed 记下客户端实际收到的全部文本——收尾时用它作为 Result.Text
	// 的权威值（不变量 8：增量与终态必须一致）。
	var streamed strings.Builder
	var writeErr error

	// 每一轮上游调用配一个新的 StreamParser：跨轮复用会把上一轮的半截
	// 状态带进来。newHandler 在每轮开始时被 loop 调用。
	newHandler := func() upstream.StreamHandler {
		sp, err := sess.NewStreamParser()
		if err != nil {
			// 建不出解析器就本轮不做增量交付：文本仍会在收尾时一次性发出，
			// 不损失内容。
			g.log.Warn("流式解析器创建失败，本轮回落一次性渲染", "error", err.Error())
			return nil
		}
		return func(chunk []byte) error {
			if writeErr != nil {
				return writeErr
			}
			safe, _ := sp.Write(chunk)
			// 解析错误不在这里处理：整轮结束后由 loop 的 Parse 统一判定，
			// 那条路径有降级纪律（不变量 16）。这里只管交付安全字节。
			if len(safe) == 0 {
				return nil
			}
			hb.Stop() // 渲染器要动笔了，心跳让位
			streamed.Write(safe)
			if err := sr.WriteText(safe); err != nil {
				// 写失败说明客户端断开：记下来让适配器尽快停手。
				writeErr = err
				return err
			}
			return nil
		}
	}

	res := g.loop(ctx, sess, decoded, sk, compiled.ToolsIncluded, newHandler)
	hb.Stop()

	final := *res
	if streamed.Len() > 0 {
		// 客户端已经收到这些字节，终态必须与它们一致。
		final.Text = streamed.String()
	}
	if err := sr.Finish(final); err != nil {
		return fmt.Errorf("gateway: 渲染 SSE 失败：%w", err)
	}
	return nil
}
