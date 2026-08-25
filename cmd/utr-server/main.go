// Command utr-server 是 Texvoke 的 HTTP sidecar。
//
// 它让任何语言写的反代都能接入工具调用能力：把工具定义 POST 过来换一段
// system prompt，把模型输出 POST 过来换回结构化的工具调用。
//
// 设计成**完全无状态**：会话信号（nonce）在 /compile 时生成并返回给调用方，
// 调用方在 /parse 时带回来。服务端不保存任何会话，因此不需要清理、
// 不会内存泄漏，也可以随便重启或横向扩容。
//
// nonce 交给调用方保管是安全的——它本来就要写进 system prompt 让模型看到，
// 而且它不是身份凭证，篡改它只会导致解析失败，不会绕过任何检查。
//
// 运行：
//
//	utr-server -addr 127.0.0.1:8757
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/kw-decade/Texvoke/internal/observability"
	"github.com/kw-decade/Texvoke/internal/serving"
	"github.com/kw-decade/Texvoke/pkg/toolbridge"
)

const (
	defaultAddr = "127.0.0.1:8757"

	// maxBodyBytes 是请求体上限。规格三章把「readBody 无上限拼接」
	// 列为必须纠正的问题：没有上限，一个构造的大请求就能吃光内存。
	maxBodyBytes = 32 << 20

	// maxStreamBytes 是流式解析单次会话的上限。
	maxStreamBytes = 16 << 20
)

func main() {
	var (
		addr        = flag.String("addr", defaultAddr, "监听地址")
		maxTools    = flag.Int("max-tools", 0, "进入 Prompt 的工具数上限，0 用默认值 24")
		maxDesc     = flag.Int("max-tool-desc", 0, "工具描述字节上限，0 用默认值 2000；接 Codex 建议 16000")
		noiseFile   = flag.String("noise-filters", "", "上游噪声正则文件，每行一条")
		allowRemote = flag.Bool("allow-remote", false, "允许监听非环回地址（需要显式开启）")
		verbose     = flag.Bool("v", false, "输出调试日志")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	// 默认只监听环回。规格十章：需要暴露局域网时必须显式配置——
	// 这个服务没有认证，绑到 0.0.0.0 等于把工具调用能力开放给整个网络。
	if !*allowRemote && !serving.LoopbackOnly(*addr) {
		logger.Error("拒绝监听非环回地址", "addr", *addr,
			"提示", "本服务没有认证机制，确需暴露请显式加 -allow-remote 并自行做好访问控制")
		os.Exit(1)
	}

	filters, err := loadNoiseFilters(*noiseFile)
	if err != nil {
		logger.Error("加载噪声过滤规则失败", "error", err)
		os.Exit(1)
	}

	bridge, err := toolbridge.New(toolbridge.Config{
		Upstream: toolbridge.UpstreamProfile{
			NoiseFilters:     filters,
			MaxToolsInPrompt: *maxTools,
			MaxToolDescBytes: *maxDesc,
		},
	})
	if err != nil {
		logger.Error("初始化失败", "error", err)
		os.Exit(1)
	}

	srv := &server{bridge: bridge, log: logger, sessions: serving.NewSessionStore()}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/compile", srv.handleCompile)
	mux.HandleFunc("POST /v1/parse", srv.handleParse)
	mux.HandleFunc("POST /v1/parse/stream", srv.handleParseStream)
	mux.HandleFunc("POST /v1/adapt", srv.handleAdapt)
	mux.HandleFunc("POST /v1/recover", srv.handleRecover)
	mux.HandleFunc("POST /v1/render", srv.handleRender)
	mux.HandleFunc("POST /v1/render/stream", srv.handleRenderStream)
	mux.HandleFunc("GET /healthz", srv.handleHealth)

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// 不设 WriteTimeout：流式解析可能持续很久，由客户端的
		// context 与 maxStreamBytes 兜底。
		IdleTimeout: 120 * time.Second,
	}

	go func() {
		logger.Info("utr-server 已启动", "addr", *addr,
			"endpoints", "/v1/compile /v1/parse /v1/parse/stream /v1/adapt /v1/recover /v1/render /v1/render/stream /healthz")
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("服务异常退出", "error", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	logger.Info("收到退出信号，正在关闭")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		logger.Error("关闭超时", "error", err)
	}
}

type server struct {
	bridge *toolbridge.Bridge
	log    *slog.Logger

	// sessions 是会话级突破阶梯状态。键由接入方提供（通常是
	// prompt_cache_key 或首条消息摘要），跨请求延续救援进度。
	// 实现在 internal/serving，与 texvoke 一体化网关共用同一份判据。
	sessions *serving.SessionStore
}

/* ---------- /v1/compile ---------- */

type compileRequest struct {
	SessionID string `json:"session_id"`
	RequestID string `json:"request_id"`

	Tools []toolbridge.Tool `json:"tools"`

	// Query 用于工具候选排序，通常传用户最近一条消息。
	Query string `json:"query,omitempty"`
	// AlwaysInclude 列出必须进入 Prompt 的工具名。
	AlwaysInclude []string `json:"always_include,omitempty"`

	// RequireCall 与 RequiredTool 对应 tool_choice。
	// 注意它们只会变成 Prompt 里的一句预期，不是保证。
	RequireCall  bool   `json:"require_call,omitempty"`
	RequiredTool string `json:"required_tool,omitempty"`
}

type compileResponse struct {
	SystemPrompt string `json:"system_prompt"`

	// Nonce 是本会话的协议信号值，调用 /parse 时必须带回来。
	Nonce string `json:"nonce"`
	// Signal 是完整的信号行，仅供日志核对。
	Signal string `json:"signal"`

	ToolsIncluded []string `json:"tools_included"`

	// ToolsDropped 不为 0 时值得记一笔：模型「没有用那个工具」的原因
	// 可能是它压根没看见。
	ToolsDropped int `json:"tools_dropped"`
}

func (s *server) handleCompile(w http.ResponseWriter, r *http.Request) {
	var req compileRequest
	if !s.decode(w, r, &req) {
		return
	}
	if req.SessionID == "" || req.RequestID == "" {
		s.fail(w, http.StatusBadRequest, "bad_request", "session_id 与 request_id 必填")
		return
	}

	sess, err := s.bridge.NewSession(req.SessionID, req.RequestID)
	if err != nil {
		s.failErr(w, err)
		return
	}

	res, err := sess.Compile(req.Tools, toolbridge.CompileOptions{
		Query:         req.Query,
		AlwaysInclude: req.AlwaysInclude,
		RequireCall:   req.RequireCall,
		RequiredTool:  req.RequiredTool,
	})
	if err != nil {
		s.failErr(w, err)
		return
	}

	s.log.Debug("compile",
		"session", observability.Digest(req.SessionID)[:8],
		"tools_in", len(req.Tools),
		"tools_included", len(res.ToolsIncluded),
		"tools_dropped", res.ToolsDropped)

	s.json(w, http.StatusOK, compileResponse{
		SystemPrompt:  res.SystemPrompt,
		Nonce:         sess.NonceValue(),
		Signal:        res.Signal,
		ToolsIncluded: res.ToolsIncluded,
		ToolsDropped:  res.ToolsDropped,
	})
}

/* ---------- /v1/parse ---------- */

type parseRequest struct {
	SessionID string `json:"session_id"`
	RequestID string `json:"request_id"`
	// Nonce 来自 /compile 的响应。
	Nonce string `json:"nonce"`
	// Text 是模型的完整输出。
	Text string `json:"text"`

	// Tools 是本轮声明过的工具名，用于核对模型写的名字。
	//
	// 可以不给，那样 unknown_tools 恒为空。给了才能发现两类真实错误：
	// 模型把工具名写成 functions.exec 这种带协议前缀的形式，或者干脆
	// 编了一个不存在的工具——两种都会让调用在客户端那边静默失败。
	//
	// 只要名字，不需要完整定义：/v1/adapt 返回的 tools_included 原样传回来即可。
	Tools []string `json:"tools,omitempty"`

	// SessionKey 是会话级阶梯状态的键（与 /v1/adapt、/v1/recover 用的同一个）。
	// 带了它且解析出调用时，该会话的突破阶梯自动归位——下次卡住从 L1 重新开始。
	SessionKey string `json:"session_key,omitempty"`
}

type parseResponse struct {
	// Text 是可以原样转发给客户端的普通文本。
	Text string `json:"text"`
	// Calls 是解析出的工具调用。
	Calls []toolbridge.Call `json:"calls"`
	// Outcome 是四种结局之一：plain_text / calls_parsed / truncated / malformed。
	Outcome string `json:"outcome"`

	// Trailing 是模型在 envelope 闭合之后多说的话。协议规则不允许，
	// 但纯文本模型经常收个尾。它被容忍了，也不会并进 Text——记在这里
	// 是为了让违规可见。总是非空说明 Prompt 的约束没生效。
	Trailing string `json:"trailing,omitempty"`

	// UnknownTools 是模型调用了、但本次请求没有声明过的工具名。
	//
	// 注意无状态调用（会话用 nonce 重建）时它恒为空：服务端手上没有
	// 工具名单，就不该假装能判断。要用上这个字段，得在 /v1/parse 里
	// 一并带上 tools。
	UnknownTools []string `json:"unknown_tools,omitempty"`

	// Error 与 ErrorKind 只在解析失败时有值。
	// 注意 Text 仍然可能非空——信号之前的正文是有效的。
	Error     string `json:"error,omitempty"`
	ErrorKind string `json:"error_kind,omitempty"`
	Retryable bool   `json:"retryable,omitempty"`
}

func (s *server) handleParse(w http.ResponseWriter, r *http.Request) {
	var req parseRequest
	if !s.decode(w, r, &req) {
		return
	}
	sess, ok := s.restore(w, req.SessionID, req.RequestID, req.Nonce)
	if !ok {
		return
	}
	sess.DeclareTools(req.Tools)

	res, err := sess.Parse(req.Text)
	// 解析出调用 = 救援成功：该会话的阶梯归位，下次从 L1 重新开始。
	if req.SessionKey != "" && res.Outcome == toolbridge.OutcomeCallsParsed {
		s.sessions.Succeed(req.SessionKey)
	}
	out := parseResponse{Outcome: string(res.Outcome), Calls: res.Calls, Text: res.Text,
		Trailing: res.Trailing, UnknownTools: res.UnknownTools}
	if len(res.UnknownTools) > 0 {
		s.log.Warn("模型调用了没有声明过的工具",
			"session", observability.Digest(req.SessionID)[:8],
			"tools", res.UnknownTools)
	}
	if res.Trailing != "" {
		s.log.Warn("模型在 envelope 闭合后仍有输出",
			"session", observability.Digest(req.SessionID)[:8],
			"bytes", len(res.Trailing))
	}
	if out.Calls == nil {
		out.Calls = []toolbridge.Call{}
	}
	if err != nil {
		out.Error = err.Error()
		out.ErrorKind = string(toolbridge.KindOf(err))
		out.Retryable = toolbridge.KindOf(err) == toolbridge.ErrParseFailed
	}

	// 解析失败返回 200 而不是 4xx/5xx：这不是 HTTP 层的错误，
	// 而是一次成功的分析，结论是「模型的输出不合格式」。用状态码表达它
	// 会让调用方分不清「服务挂了」和「模型写错了」。
	s.json(w, http.StatusOK, out)
}

/* ---------- /v1/parse/stream ---------- */

// 流式解析：请求体是模型输出的字节流，响应体是 NDJSON 事件流。
//
// 用 chunked HTTP 而非 WebSocket：任何语言的 HTTP 客户端都能流式发送与
// 接收，不需要额外依赖。Python 用 httpx.stream，Node 用 fetch 的 body 流。
//
// 事件有两种：
//
//	{"type":"text","text":"..."}       可以立即转发给客户端的普通文本
//	{"type":"done","outcome":"...",...} 流结束，带最终结果
type streamTextEvent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type streamDoneEvent struct {
	Type      string            `json:"type"`
	Outcome   string            `json:"outcome"`
	Calls     []toolbridge.Call `json:"calls"`
	Error     string            `json:"error,omitempty"`
	ErrorKind string            `json:"error_kind,omitempty"`
	Retryable bool              `json:"retryable,omitempty"`
}

func (s *server) handleParseStream(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sess, ok := s.restore(w, q.Get("session_id"), q.Get("request_id"), q.Get("nonce"))
	if !ok {
		return
	}

	sp, err := sess.NewStreamParser()
	if err != nil {
		s.failErr(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)

	body := http.MaxBytesReader(w, r.Body, maxStreamBytes)
	defer body.Close()

	buf := make([]byte, 4096)
	var parseErr error

	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			safe, err := sp.Write(buf[:n])
			if len(safe) > 0 {
				if encErr := enc.Encode(streamTextEvent{Type: "text", Text: string(safe)}); encErr != nil {
					return // 客户端断开
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
			if err != nil {
				parseErr = err
				break
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				parseErr = readErr
			}
			break
		}
	}

	// 冲刷噪声过滤的行缓冲：配置了过滤规则时，最后不足一行的内容
	// 还扣在里面，不发出去客户端就少了一截正文。
	if tail := sp.Flush(); len(tail) > 0 {
		_ = enc.Encode(streamTextEvent{Type: "text", Text: string(tail)})
	}

	res := sp.Close()
	done := streamDoneEvent{Type: "done", Outcome: string(res.Outcome), Calls: res.Calls}
	if done.Calls == nil {
		done.Calls = []toolbridge.Call{}
	}
	if parseErr != nil {
		done.Error = parseErr.Error()
		done.ErrorKind = string(toolbridge.KindOf(parseErr))
		done.Retryable = toolbridge.KindOf(parseErr) == toolbridge.ErrParseFailed
	}
	_ = enc.Encode(done)
	if flusher != nil {
		flusher.Flush()
	}
}

/* ---------- /healthz ---------- */

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// 规格十章：健康检查不得泄露 API Key、完整工具定义、上游私有 URL
	// 或会话 nonce。这里只报「活着」和协议版本。
	s.json(w, http.StatusOK, map[string]string{
		"status":           "ok",
		"protocol_version": "1",
	})
}

/* ---------- 辅助 ---------- */

// restore 用调用方带回来的 nonce 重建会话。
//
// 无状态的关键：服务端不记任何东西，会话完全由这三个值确定。
func (s *server) restore(w http.ResponseWriter, sessionID, requestID, nonce string) (*toolbridge.Session, bool) {
	if sessionID == "" || requestID == "" || nonce == "" {
		s.fail(w, http.StatusBadRequest, "bad_request",
			"session_id、request_id 与 nonce 必填（nonce 来自 /v1/compile 的响应）")
		return nil, false
	}
	sess, err := s.bridge.RestoreSession(sessionID, requestID, nonce)
	if err != nil {
		s.fail(w, http.StatusBadRequest, "bad_nonce", err.Error())
		return nil, false
	}
	return sess, true
}

func (s *server) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	body := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	defer body.Close()

	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		// 拒绝未知字段而不是忽略：调用方拼错一个字段名时，
		// 静默忽略会让它以为设置生效了，然后困惑于行为不符预期。
		s.fail(w, http.StatusBadRequest, "bad_request", "请求体解析失败："+err.Error())
		return false
	}
	return true
}

func (s *server) json(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *server) fail(w http.ResponseWriter, code int, kind, msg string) {
	s.json(w, code, map[string]string{"error": msg, "error_kind": kind})
}

func (s *server) failErr(w http.ResponseWriter, err error) {
	kind := toolbridge.KindOf(err)
	code := http.StatusBadRequest
	if kind == toolbridge.ErrConfig {
		code = http.StatusInternalServerError
	}
	s.fail(w, code, string(kind), err.Error())
}

// loadNoiseFilters 从文件读取上游噪声正则，每行一条，# 开头是注释。
//
// 做成外部文件而不是命令行参数：正则里全是需要转义的字符，
// 塞进命令行会被 shell 吃掉一半。
func loadNoiseFilters(path string) ([]*regexp.Regexp, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []*regexp.Regexp
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		re, err := regexp.Compile(line)
		if err != nil {
			return nil, fmt.Errorf("第 %d 行的正则无效：%w", i+1, err)
		}
		out = append(out, re)
	}
	return out, nil
}
