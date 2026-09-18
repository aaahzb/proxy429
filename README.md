# Proxy429

[English](#english) | [中文](#中文)

---

## English

A local LLM API proxy that keeps AI coding agents (Claude Code, Codex CLI)
unaffected by flaky model providers.

- **Automatic retry with exponential backoff** on 429/5xx — including
  rate-limit errors hidden inside HTTP-200 SSE streams — with SSE keep-alive
  pings so the client never times out while the proxy retries.
- **Model routing**: wildcard routing across providers, dedicated routes for
  safety-classifier and fast-mode requests, multimodal and web-search
  fallbacks.
- **Protocol translation**: OpenAI Responses ↔ Anthropic Messages — tool
  calling, reasoning-mode mapping (adaptive / budget / effort), and
  byte-level key-order preservation so provider prompt caches stay valid.
- **Live observability**: a local web console showing in-flight streams,
  token and cache-hit telemetry, and per-request diagnostics.
- **Cross-platform tray app** (Windows / macOS / Linux), single binary,
  ~40% test code.

### Motivation

I use AI coding agents all day. Running Claude Code against Volcano ARK's
GLM endpoint, I kept hitting 429 throttling — the built-in retry was slow
and capped, but I noticed that manually disconnecting and reconnecting
resumed the stream immediately. The failures were transient; somebody just
needed to retry patiently. So I built this proxy to do exactly that.

Naturally, the proxy itself is built with the same AI agents: I define the
problems and judge the results as a heavy daily user; the agents write the
code. I'm a quant fund manager, not a Go developer — a tool for AI agents,
built by AI agents, directed by someone who depends on them all day.

### Quick Start

1. Build with `build.sh` (outputs `release/proxy429`, or
   `release/Proxy429.app` on macOS).
2. Run it — a config file is generated on first launch.
3. Point your agent at `http://127.0.0.1:8080` (Claude Code) or the
   Responses endpoint (Codex CLI). Details in the docs below.

### Documentation

Full design documentation (Chinese): [docs/DESIGN.md](docs/DESIGN.md) —
retry semantics, routing, protocol translation, cache-preserving rewrites,
observability. Also: [使用说明](docs/使用说明.md) ·
[Windows 快速部署](docs/Windows快速部署说明.md) ·
[开机自启](docs/开机自启设置说明.md)

### Acknowledgments

- [cc-switch](https://github.com/farion1231/cc-switch) (MIT, © 2025 Jason
  Young) — the Responses translation test suites are ported from its Rust
  tests; the thinking-mode mapping is aligned with cc-switch 3.20.0; the
  `[cc-switch: omitted N bytes]` marker is kept for client compatibility.

### License

[MIT](LICENSE) © 2026 aaahzb

---

## 中文

一个本地 LLM API 代理：夹在 Claude Code / Codex 和上游模型提供商之间，
自动吃掉 429/5xx 限流（指数退避重试，含藏在 200 流里的错误），支持模型路由、
双协议翻译，带实时网页监控台。

### 为什么有这个

我整天用 AI 编程 agent 干活。在火山方舟的 GLM 上跑 Claude Code 时老被
429 打断——自带重试又慢又有次数限制，但我发现手动断开重连立刻就能恢复：
失败是暂时的，只是缺个有耐心重试的东西。于是有了这个代理。

这个代理本身也是用同样的 AI agent 造的：我定义问题、作为重度用户验收，
agent 写代码。我是量化基金经理，不是 Go 开发者——为 AI agent 造的工具，
由 AI agent 造，由天天靠它们干活的人指挥。

### 文档

完整设计文档见 [docs/DESIGN.md](docs/DESIGN.md)（重试语义、路由、协议翻译、
保缓存改写、可观测性）。另有 [使用说明](docs/使用说明.md) ·
[Windows 快速部署](docs/Windows快速部署说明.md) ·
[开机自启](docs/开机自启设置说明.md)

### 致谢与协议

- [cc-switch](https://github.com/farion1231/cc-switch)（MIT, © 2025 Jason
  Young）：Responses 翻译测试集移植自其 Rust 测试；thinking 映射与其
  3.20.0 对齐；协议标记为兼容沿用。
- 本项目以 [MIT](LICENSE) © 2026 aaahzb 开源。
