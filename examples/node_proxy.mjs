/**
 * 最小接入示例：给一个 Node 反代加上工具调用能力。
 *
 * 与 fastapi_proxy.py 是同一件事的 Node 版本，用来说明接入面与语言无关——
 * 两边都是普通 HTTP 调用，没有任何 Texvoke 的 SDK。
 *
 * 这一份额外演示**流式**接入：信号出现之前的文本边到边转发给客户端，
 * 不必等模型把话说完。非流式版本看 Python 那份。
 *
 * 先启动 sidecar：
 *   utr-server -addr 127.0.0.1:8757
 *
 * 再跑：
 *   node examples/node_proxy.mjs
 */

import http from "node:http";
import { randomUUID } from "node:crypto";

const UTR = "http://127.0.0.1:8757";

// 换成你自己的纯文本上游。
const UPSTREAM = "https://your-text-only-model.example.com/api/chat";

/* ---------- 两个接入点 ---------- */

/**
 * 把工具定义换成 system prompt。
 *
 * 返回的 nonce 要保存下来，解析时必须带回去——它是这一轮的协议信号，
 * 编译与解析两边必须用同一个，否则模型看到的格式和解析器认的对不上。
 */
async function utrCompile(sessionId, requestId, tools, query = "") {
  const r = await fetch(`${UTR}/v1/compile`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({
      session_id: sessionId,
      request_id: requestId,
      tools,
      query,
    }),
  });
  if (!r.ok) throw new Error(`compile 失败 ${r.status}: ${await r.text()}`);
  return r.json();
}

/**
 * 流式解析：一边喂模型输出，一边拿回可以安全转发的文本。
 *
 * onText 收到的每一段都是**确定不属于协议**的普通文本，可以立即写给客户端。
 * 可能属于协议信号的字节会被扣住，直到能确定它不是——发出去的字符收不回来，
 * 所以宁可晚一点也不能错。
 */
async function utrParseStream(sessionId, requestId, nonce, upstreamStream, onText) {
  const url = `${UTR}/v1/parse/stream?session_id=${encodeURIComponent(sessionId)}` +
    `&request_id=${encodeURIComponent(requestId)}&nonce=${encodeURIComponent(nonce)}`;

  const r = await fetch(url, {
    method: "POST",
    body: upstreamStream,
    duplex: "half", // Node 的 fetch 发送流时必须带这个
  });
  if (!r.ok) throw new Error(`parse 失败 ${r.status}: ${await r.text()}`);

  // 响应是 NDJSON：一行一个事件。
  const reader = r.body.getReader();
  const decoder = new TextDecoder();
  let buf = "";
  let done = null;

  for (;;) {
    const { value, done: finished } = await reader.read();
    if (finished) break;
    buf += decoder.decode(value, { stream: true });

    let nl;
    while ((nl = buf.indexOf("\n")) >= 0) {
      const line = buf.slice(0, nl).trim();
      buf = buf.slice(nl + 1);
      if (!line) continue;

      const ev = JSON.parse(line);
      if (ev.type === "text") onText(ev.text);
      else if (ev.type === "done") done = ev;
    }
  }
  return done;
}

/* ---------- 工具定义归一化 ---------- */

/** 把 OpenAI Chat Completions 的工具定义转成 sidecar 要的形状。 */
function normalizeTools(raw) {
  const out = [];
  for (const t of raw || []) {
    const fn = t.function || {};
    if (!fn.name) continue;
    out.push({
      name: fn.name,
      description: fn.description || "",
      input_schema: fn.parameters || { type: "object", properties: {} },
    });
  }
  return out;
}

/* ---------- 反代主体 ---------- */

const server = http.createServer(async (req, res) => {
  if (req.method !== "POST" || !req.url.startsWith("/v1/chat/completions")) {
    res.writeHead(404).end();
    return;
  }

  const body = JSON.parse(await readBody(req));
  const sessionId = req.headers["x-session-id"] || randomUUID();
  const requestId = randomUUID();

  let messages = body.messages || [];
  const tools = normalizeTools(body.tools);
  let nonce = "";

  // ---- 接入点 1：把工具定义编译进 system prompt ----
  if (tools.length) {
    const lastUser = [...messages].reverse().find((m) => m.role === "user");
    const compiled = await utrCompile(sessionId, requestId, tools,
      typeof lastUser?.content === "string" ? lastUser.content : "");
    nonce = compiled.nonce;

    // 工具被截断时值得记一笔：模型「没用那个工具」的原因可能是它没看见。
    if (compiled.tools_dropped) {
      console.warn(`[warn] ${compiled.tools_dropped} 个工具未进入 prompt`);
    }
    messages = [{ role: "system", content: compiled.system_prompt }, ...messages];
  }

  // ---- 中间：照常转发给你的上游。这一段完全是你自己的逻辑 ----
  const upstream = await fetch(UPSTREAM, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ model: body.model, messages, stream: true }),
  });

  // 客户端要的是 SSE。
  res.writeHead(200, {
    "content-type": "text/event-stream",
    "cache-control": "no-store",
  });

  const id = `chatcmpl-${randomUUID().replace(/-/g, "").slice(0, 24)}`;
  const chunk = (delta, finish = null) =>
    `data: ${JSON.stringify({
      id, object: "chat.completion.chunk", created: 0, model: body.model,
      choices: [{ index: 0, delta, finish_reason: finish }],
    })}\n\n`;

  res.write(chunk({ role: "assistant" }));

  if (!nonce) {
    // 没有工具时不必解析，原样透传。
    for await (const piece of upstream.body) res.write(chunk({ content: String(piece) }));
    res.write(chunk({}, "stop"));
    res.end("data: [DONE]\n\n");
    return;
  }

  // ---- 接入点 2：边解析边转发 ----
  const done = await utrParseStream(sessionId, requestId, nonce, upstream.body, (text) => {
    // 这段文本确定不属于协议，可以立即发给客户端。
    res.write(chunk({ content: text }));
  });

  if (done?.calls?.length) {
    done.calls.forEach((c, i) => {
      res.write(chunk({
        tool_calls: [{
          index: i, id: c.id, type: "function",
          function: {
            name: c.name,
            // Chat Completions 里 arguments 是「内含 JSON 的字符串」，
            // 不是对象。写成对象官方 SDK 会解析失败。
            arguments: typeof c.arguments === "string"
              ? c.arguments : JSON.stringify(c.arguments),
          },
        }],
      }));
    });
  }

  if (done?.error) {
    console.warn(`[warn] 解析结局 ${done.outcome}：${done.error}`);
  }

  // finish_reason 必须与 tool_calls 一致：带着调用却报 stop，
  // 客户端 SDK 会直接忽略这些调用。
  res.write(chunk({}, done?.calls?.length ? "tool_calls" : "stop"));
  res.end("data: [DONE]\n\n");
});

function readBody(req) {
  return new Promise((resolve, reject) => {
    let s = "";
    req.on("data", (c) => {
      s += c;
      // 请求体要有上限，否则一个构造的大请求就能吃光内存。
      if (s.length > 32 * 1024 * 1024) reject(new Error("请求体过大"));
    });
    req.on("end", () => resolve(s));
    req.on("error", reject);
  });
}

server.listen(8000, "127.0.0.1", () => {
  console.log("反代已启动：http://127.0.0.1:8000/v1/chat/completions");
  console.log("确保 utr-server 在 127.0.0.1:8757 上跑着");
});
