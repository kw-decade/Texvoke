// Command texvoke 是 Texvoke 的一体化网关：一条命令把任何纯文本上游
// 变成支持工具调用的 chatcompletion / messages / responses 端点。
//
// 它替代前身实现的全部职责。接入方不再写胶水：
//
//	texvoke serve \
//	  --listen 127.0.0.1:8756 \
//	  --upstream https://your-upstream.example.com/v1/chat/completions \
//	  --format openai-chat \
//	  --max-tools 10 \
//	  --always-include "Bash,Read,Write,Edit,Glob,Grep"
//
// 之后 Codex / Claude Code / 任何 OpenAI 兼容客户端直连即可用工具。
//
// 端点（客户端以为自己在跟原生 API 说话）：
//
//	POST /v1/chat/completions   OpenAI Chat Completions
//	POST /v1/messages           Anthropic Messages
//	POST /v1/responses          OpenAI Responses
//	GET  /healthz
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kw-decade/Texvoke/internal/gateway"
	"github.com/kw-decade/Texvoke/internal/serving"
	"github.com/kw-decade/Texvoke/internal/upstream"
	"github.com/kw-decade/Texvoke/pkg/toolbridge"
)

func main() {
	var (
		listen    = flag.String("listen", "127.0.0.1:18199", "监听地址")
		upstreamU = flag.String("upstream", "", "上游端点 URL（必填）")
		format    = flag.String("format", "openai-chat", "上游格式：openai-chat")
		model     = flag.String("model", "", "覆盖发给上游的模型名，为空保留客户端的")
		apiKey    = flag.String("api-key", os.Getenv("TEXVOKE_API_KEY"), "上游 Bearer 凭据；也可用环境变量 TEXVOKE_API_KEY")
		maxTools  = flag.Int("max-tools", 10, "进入 Prompt 的工具数上限")
		alwaysInc = flag.String("always-include", "", "必须进清单的工具名，逗号分隔")
		maxDesc   = flag.Int("max-tool-desc", 9000, "工具描述字节上限")
		extra     = flag.String("extra-instructions", "", "追加在协议说明后的补充指令")
		budget    = flag.Duration("request-budget", 0, "单次请求的墙钟总上限，0 用默认 10m；负值不设限")
		allowRem  = flag.Bool("allow-remote", false, "允许监听非环回地址（需要显式开启）")
		verbose   = flag.Bool("v", false, "输出调试日志")
	)
	flag.Parse()
	// 唯一子命令 serve；直接裸跑也行（文档统一写 serve，容错不写死）。
	if flag.NArg() > 0 && flag.Arg(0) != "serve" {
		fmt.Fprintf(os.Stderr, "texvoke: 未知子命令 %q（只有 serve）\n", flag.Arg(0))
		os.Exit(2)
	}

	if *upstreamU == "" {
		fmt.Fprintln(os.Stderr, "texvoke: --upstream 必填（例如 https://api.example.com/v1/chat/completions）")
		os.Exit(2)
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	// 默认只监听环回，与 sidecar 同一道栏、同一个判据。
	// 这里的风险比 sidecar 更高：texvoke 手里握着上游凭据，暴露到网络等于
	// 把你的 API Key 变成公共代理。没有认证机制，所以只能靠这道栏。
	if !*allowRem && !serving.LoopbackOnly(*listen) {
		logger.Error("拒绝监听非环回地址", "listen", *listen,
			"提示", "本服务没有认证机制且持有上游凭据，确需暴露请显式加 -allow-remote 并自行做好访问控制")
		os.Exit(1)
	}

	adapter, err := buildAdapter(*format, *upstreamU, *apiKey)
	if err != nil {
		logger.Error("上游适配器创建失败", "error", err)
		os.Exit(1)
	}

	bridge, err := toolbridge.New(toolbridge.Config{})
	if err != nil {
		logger.Error("初始化失败", "error", err)
		os.Exit(1)
	}

	gw, err := gateway.New(gateway.Config{
		Bridge:            bridge,
		Adapter:           adapter,
		UpstreamModel:     *model,
		AlwaysInclude:     splitList(*alwaysInc),
		MaxTools:          *maxTools,
		MaxToolDescBytes:  *maxDesc,
		ExtraInstructions: *extra,
		RequestBudget:     *budget,
		Log:               logger,
	})
	if err != nil {
		logger.Error("网关初始化失败", "error", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", handle(gw, logger, "chat"))
	mux.HandleFunc("POST /v1/messages", handle(gw, logger, "anthropic"))
	mux.HandleFunc("POST /v1/responses", handle(gw, logger, "responses"))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	httpSrv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		logger.Info("texvoke serve 已启动",
			"listen", *listen,
			"upstream", adapter.Name(),
			"endpoints", "/v1/chat/completions /v1/messages /v1/responses /healthz")
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
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

// handle 把 HTTP 层薄薄地包在 Gateway 外面：读体、限长、分发、写回。
// 编排逻辑一个字都不在这里——它们都在 internal/gateway 有测试。
func handle(gw *gateway.Gateway, log *slog.Logger, proto string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "读取请求体失败："+err.Error())
			return
		}
		stream := isStreamRequest(body)

		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			if err := gw.HandleSSE(r.Context(), proto, body, w); err != nil {
				// 头已发出：错误只能进事件流。与 sidecar 行为一致。
				fmt.Fprintf(w, "event: error\ndata: {\"error\":%q}\n\n", err.Error())
			}
			return
		}

		out, err := gw.Handle(r.Context(), proto, body)
		if err != nil {
			log.Warn("请求处理失败", "protocol", proto, "error", err.Error())
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
	}
}

// isStreamRequest 探测客户端是否要求流式。只看 body 的 stream 字段——
// 三协议这个字段名一致。
func isStreamRequest(body []byte) bool {
	var probe struct {
		Stream bool `json:"stream"`
	}
	return json.Unmarshal(body, &probe) == nil && probe.Stream
}

func buildAdapter(format, endpointURL, apiKey string) (upstream.Adapter, error) {
	switch strings.ToLower(format) {
	case "openai-chat":
		return upstream.OpenAIChat{Endpoint: endpointURL, APIKey: apiKey}, nil
	default:
		return nil, fmt.Errorf("未知 format %q（当前支持 openai-chat）", format)
	}
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	fmt.Fprintf(w, `{"error":{"message":%q,"type":"gateway_error"}}`, msg)
}
