"""
最小接入示例：给一个 FastAPI 反代加上工具调用能力。

场景：上游是纯文本模型（不支持 function calling），但客户端按 OpenAI
Chat Completions 协议发请求，期待拿到 tool_calls。

接入 Texvoke 只需要两处改动，共二十来行：
  1. 转发给上游之前：把 tools 换成一段 system prompt
  2. 拿到模型回复之后：把文本换回 tool_calls

先启动 sidecar：
    utr-server -addr 127.0.0.1:8757

再跑这个示例：
    pip install fastapi uvicorn httpx
    python fastapi_proxy.py

依赖只有 httpx，没有任何 Texvoke 的 SDK——接的是普通 HTTP。
"""

import uuid
from typing import Any

import httpx
from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse

UTR = "http://127.0.0.1:8757"

# 换成你自己的纯文本上游。
UPSTREAM = "https://your-text-only-model.example.com/api/chat"

app = FastAPI()
client = httpx.AsyncClient(timeout=120)


async def utr_compile(session_id: str, request_id: str, tools: list[dict],
                      query: str = "") -> dict:
    """把工具定义换成 system prompt。

    返回的 nonce 要保存下来，解析时必须带回去——它是这一轮的协议信号，
    编译与解析两边必须用同一个，否则模型看到的格式和解析器认的格式对不上。
    """
    r = await client.post(f"{UTR}/v1/compile", json={
        "session_id": session_id,
        "request_id": request_id,
        # tools 用 {name, description, input_schema} 的形状。
        # 三种客户端协议的工具定义形状各不相同，归一化是你的事——
        # 你比框架更清楚自己面对的是哪种客户端。
        "tools": tools,
        "query": query,
    })
    r.raise_for_status()
    return r.json()


async def utr_parse(session_id: str, request_id: str, nonce: str, text: str) -> dict:
    """把模型输出换回工具调用。

    注意它不会因为「模型写错了格式」而返回 HTTP 错误——那是一次成功的分析，
    结论在 outcome 字段里。只有服务本身出问题才会是非 200。
    """
    r = await client.post(f"{UTR}/v1/parse", json={
        "session_id": session_id,
        "request_id": request_id,
        "nonce": nonce,
        "text": text,
    })
    r.raise_for_status()
    return r.json()


def normalize_tools(raw: list[dict]) -> list[dict]:
    """把 OpenAI Chat Completions 的工具定义转成 sidecar 要的形状。"""
    out = []
    for t in raw or []:
        fn = t.get("function") or {}
        if not fn.get("name"):
            continue
        out.append({
            "name": fn["name"],
            "description": fn.get("description", ""),
            "input_schema": fn.get("parameters") or {"type": "object", "properties": {}},
        })
    return out


@app.post("/v1/chat/completions")
async def chat_completions(req: Request):
    body = await req.json()
    session_id = req.headers.get("x-session-id", str(uuid.uuid4()))
    request_id = str(uuid.uuid4())

    messages = body.get("messages", [])
    tools = normalize_tools(body.get("tools"))

    nonce = ""
    if tools:
        # ---- 接入点 1：把工具定义编译进 system prompt ----
        last_user = next(
            (m.get("content", "") for m in reversed(messages) if m.get("role") == "user"),
            "",
        )
        compiled = await utr_compile(session_id, request_id, tools,
                                     query=last_user if isinstance(last_user, str) else "")
        nonce = compiled["nonce"]

        # 工具被截断时值得记一笔：模型「没用那个工具」的原因可能是它没看见。
        if compiled["tools_dropped"]:
            print(f"[warn] {compiled['tools_dropped']} 个工具未进入 prompt")

        messages = [{"role": "system", "content": compiled["system_prompt"]}] + messages

    # ---- 中间：照常转发给你的上游。这一段完全是你自己的逻辑 ----
    upstream_reply = await call_upstream(body.get("model", ""), messages)

    if not nonce:
        # 没有工具时不必解析，直接返回。
        return JSONResponse(openai_response(body, upstream_reply, []))

    # ---- 接入点 2：把模型输出解析回工具调用 ----
    parsed = await utr_parse(session_id, request_id, nonce, upstream_reply)

    if parsed["outcome"] == "malformed" and parsed.get("retryable"):
        # 模型格式写错了。这里可以把 parsed["error"] 反馈给模型重试一次，
        # 但要有次数上限——规格明确反对无限重试。示例里直接放行。
        print(f"[warn] 模型格式不合规：{parsed['error']}")

    return JSONResponse(openai_response(body, parsed["text"], parsed["calls"]))


async def call_upstream(model: str, messages: list[dict]) -> str:
    """把请求发给你的纯文本上游，返回它的完整回复。

    这里是示意。真实实现取决于你的上游长什么样。
    """
    r = await client.post(UPSTREAM, json={"model": model, "messages": messages})
    r.raise_for_status()
    return r.json()["reply"]


def openai_response(req_body: dict, text: str, calls: list[dict]) -> dict[str, Any]:
    """拼一个 Chat Completions 响应。

    注意 finish_reason 与 tool_calls 必须一致：带着调用却报 stop，
    客户端 SDK 会直接忽略这些调用。
    """
    message: dict[str, Any] = {"role": "assistant", "content": text or None}
    if calls:
        message["tool_calls"] = [{
            "id": c["id"],
            "type": "function",
            "function": {
                "name": c["name"],
                # Chat Completions 里 arguments 是「内含 JSON 的字符串」，
                # 不是对象。写成对象官方 SDK 会解析失败。
                "arguments": _json_str(c["arguments"]),
            },
        } for c in calls]

    return {
        "id": f"chatcmpl-{uuid.uuid4().hex[:24]}",
        "object": "chat.completion",
        "created": 0,
        "model": req_body.get("model", ""),
        "choices": [{
            "index": 0,
            "message": message,
            "finish_reason": "tool_calls" if calls else "stop",
        }],
    }


def _json_str(v: Any) -> str:
    import json
    return v if isinstance(v, str) else json.dumps(v, ensure_ascii=False)


if __name__ == "__main__":
    import uvicorn
    uvicorn.run(app, host="127.0.0.1", port=8000)
