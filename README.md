# Proxy429

一个本地 Anthropic 兼容 HTTP 代理，专门用来吃掉上游的 429 / 5xx——遇到限流就自动指数退避重试，让 Claude Code 完全无感。

## 工作原理

Claude Code 的所有请求先发到本地代理（`127.0.0.1:8080`），代理原样转发给上游模型提供商：

- **纯字节转发**：因为是 Anthropic 兼容接口，请求/响应头和体都原样透传，不做任何解析改写。
- **自动重试（两种触发）**：
  - **情况 A**：上游 HTTP 状态码是 `429/500/502/503/504` 或网络错误 → 重试。
  - **情况 B**：状态码是 **200**，但响应体里藏着错误事件（`event: error` / `rate_limit` / `overloaded` / `"type":"error"` 等）→ 也重试。这点很关键——有些上游（含部分 Anthropic 兼容服务）限流时不返回 429 状态码，而是返回 200 把错误塞在 SSE 流里，只看状态码会漏掉。
- **总预算**：`total_budget_ms` 限制总重试时长，超时后放弃并把最后一个响应透传给 Claude Code，避免 Claude Code 自己先超时。
- **首字节超时内部重发**：`upstream_header_timeout_ms`（默认 70s）限制"等上游首字节"的时长。超时认为请求卡住，**内部自动重发**（不报错给 Claude Code）；重试用尽才透传 503 交给 Claude Code 自行重试。注意只限等首字节、不砍流式 Body（长输出不受影响）。
- **流式透传**：每读到一点就 `Flush`，保证 SSE 增量输出，不会攒成一坨。判断错误时只偷看响应体开头，没消费的字节会补回去继续转发，不丢数据。
- **关闭上游压缩**：代理强制 `Accept-Encoding: identity`，这样响应体是明文，才能做错误字符串匹配；代价是带宽略增。

## 文件结构

| 文件 | 作用 |
|------|------|
| `main.go` | 代理主程序 |
| `vt_windows.go` / `vt_other.go` | 跨平台启用终端 ANSI 转义（状态行原地刷新用，build tag 分平台） |
| `main_test.go` | SSE 解析 / 状态行格式化的单元测试 |
| `config.json` | 运行配置（监听地址、上游、重试策略、分类器开关等） |
| `go.mod` | Go 模块定义 |
| `README.md` | 文档 |
| `test/mock_429.go` | 本地测试用 mock 服务器（见「本地测试」） |
| `test/config_test.json` | 本地测试用配置 |
| `proxy429.exe` | 编译产物（.gitignore 忽略，不入库） |

## 配置说明（config.json）

当前已配置为火山方舟 ARK Coding Plan：

```json
{
  "listen": "127.0.0.1:8080",
  "upstream": "https://ark.cn-beijing.volces.com/api/plan",
  "max_retries": 5,
  "base_delay_ms": 500,
  "max_delay_ms": 20000,
  "total_budget_ms": 120000,
  "retry_status_codes": [429, 500, 502, 503, 504],
  "respect_retry_after": true,
  "classifier_thinking_disabled": true,
  "classifier_system_prefix": "You are a security monitor",
  "classifier_max_tokens": 0,
  "upstream_header_timeout_ms": 70000,
  "log_request_detail": false,
  "live_stats": true
}
```

- **listen**：本地监听地址端口，Claude Code 连这里。
- **upstream**：上游 ARK 的 Anthropic 兼容 Base URL。已带 `/api/plan` 前缀，Claude Code 自带的 `/v1/messages` 会被拼在后面，最终端点为 `https://ark.cn-beijing.volces.com/api/plan/v1/messages`（已实测返回 401 鉴权错误，证明路径正确）。
- **max_retries**：最多重试次数。
- **base_delay_ms / max_delay_ms**：指数退避的起步等待和上限。
- **total_budget_ms**：总重试预算，超过即放弃。
- **retry_status_codes**：触发重试的状态码。
- **respect_retry_after**：是否优先听上游 `Retry-After` 头。
- **classifier_thinking_disabled**：是否对安全分类器请求关掉 thinking（见下节）。
- **classifier_system_prefix**：分类器请求的 system 提示词前缀，用于识别。
- **classifier_max_tokens**：命中分类器后把 max_tokens 压到这个值（加速）。**注意：值太小（如 512）会让分类器的 thinking 被截断、Claude Code 收不到有效的安全判断，表现为 Bash 被拒且不给原因**。推荐 `0`（不压，用请求原 max_tokens；关 thinking 后输出很短，仍快速返回）。
- **upstream_header_timeout_ms**：等上游首字节的最长时间（毫秒）。超过则认为请求卡住，**内部自动重发**（不报错给 Claude Code），重试用尽才透传 503 交给 Claude Code 自行重试。默认 `70000`（70s）。用首字节超时而非整体超时，只卡"等响应开头"而不砍掉长流式输出。
- **log_request_detail**：是否打印每个请求的 stream/tools/system 前缀（诊断分类器指纹用，默认关）。
- **live_stats**：是否开启实时状态行（见「实时状态行」一节）。未配置默认开；非终端（stderr 重定向到文件）自动关，不会污染日志。

## 用法

### 1. 启动代理

在本目录打开 PowerShell（这个窗口要保持开着）：

```powershell
.\proxy429.exe
```

看到 `代理启动: 监听 http://127.0.0.1:8080 -> 转发到 https://ark.cn-beijing.volces.com/api/plan` 就说明跑起来了。

> 默认读同目录的 `config.json`，也可以用 `-config` 指定别的配置文件。本地 mock 测试用 `test/config_test.json`，详见下文「本地测试」一节。

### 2. 让 Claude Code 走代理

另开一个终端。**ARK 必须用 `ANTHROPIC_AUTH_TOKEN`**（变成 `Authorization: Bearer xxx`，ARK 只认这个头；用 `ANTHROPIC_API_KEY` 会 401）：

PowerShell：
```powershell
$env:ANTHROPIC_BASE_URL = "http://127.0.0.1:8080"
$env:ANTHROPIC_AUTH_TOKEN = "你在火山方舟控制台获取的 API Key"
claude
```

CMD：
```cmd
set ANTHROPIC_BASE_URL=http://127.0.0.1:8080
set ANTHROPIC_AUTH_TOKEN=你的API Key
claude
```

### 重新编译（改了 main.go 后）

```powershell
go build -o proxy429.exe
```

## 密钥传递（ARK 专属）

代理原样透传 header。ARK 的 Anthropic 兼容端点**只认 `Authorization: Bearer` 头**（已通过 401 错误码 `AuthN_MissOrInvalidAuthorizationHeader` 验证），对应 Claude Code 的 `ANTHROPIC_AUTH_TOKEN` 环境变量。**不要用 `ANTHROPIC_API_KEY`**（它会变成 `x-api-key` 头，ARK 不认，会一直 401）。

## 排错：代理"没反应"/看不到重试日志

新版本加了**全量请求日志**，每个进来的请求都会打印 `[请求]`、每次上游返回都会打印 `[上游]`、每次重试都会打印 `[重试]`。所以一眼就能定位问题：

**启动代理后，在另一个终端跑 `claude`，然后看代理窗口：**

- **看到 `[请求] POST /v1/messages ...`** → 请求已经进代理，代理在工作。接着看上游状态码日志：
  - `[尝试 N] 上游响应状态码: 429` 然后重试 → 正常在重试，若最后仍 429 说明重试耗尽，把 `config.json` 的 `max_retries` / `total_budget_ms` 调大。
  - `[尝试 N] 上游响应状态码: 200` 但 `→ 状态码 200 但响应体含错误` → 上游把限流错误塞在 200 响应体里了，代理也在重试（情况 B）。
  - `[尝试 N] 上游响应状态码: 401` → 鉴权方式错了，改用 `ANTHROPIC_AUTH_TOKEN`（见下）。
- **完全没有 `[请求]` 日志** → **Claude Code 根本没走代理**，它的 429 是直接从 ARK 拿的。这是最常见的"没反应"原因，按下面修。

### Claude Code 没走代理的常见原因

1. **环境变量没设在跑 `claude` 的那个终端里**。必须在同一个 PowerShell 窗口里先 `$env:ANTHROPIC_BASE_URL=...` 再 `claude`，换个窗口就没了。
2. **`~/.claude/settings.json` 里写了直连 ARK 的 `ANTHROPIC_BASE_URL`**（按 ARK 官方教程配的就常有）。这个文件里的值会被 shell 环境变量覆盖，但如果你没在 shell 里设，claude 就直连 ARK 了。要么删掉/改成代理地址，要么每次在 shell 里设环境变量覆盖它。
3. **claude 登录了 Anthropic 账号**。`claude /logout` 退出，改用 `ANTHROPIC_AUTH_TOKEN` 环境变量鉴权。
4. **验证办法**：跑 `claude` 之前在同一个终端执行 `echo $env:ANTHROPIC_BASE_URL`（PowerShell）或 `echo %ANTHROPIC_BASE_URL%`（CMD），必须输出 `http://127.0.0.1:8080` 才算设上了。

### 鉴权别用错

ARK 只认 `Authorization: Bearer` 头 → 必须用 `ANTHROPIC_AUTH_TOKEN`，**不要**用 `ANTHROPIC_API_KEY`（会变成 `x-api-key` 头，ARK 返回 401，错误码 `AuthN_MissOrInvalidAuthorizationHeader`）。代理是透传的，两种都行，但 ARK 只收 Bearer。

## 分类器请求自动关 thinking

Claude Code 跑 Bash 前会用模型做一次"安全分类"。这个分类请求如果带着 thinking，模型要想很久（可能 30s+），撞上 Claude Code 的超时就会报 `glm-5.2 is temporarily unavailable, auto mode cannot determine the safety of Bash`，Bash 全被挡住。

代理会**认出这个分类器请求并强制关掉它的 thinking**，让分类 2-3 秒返回：

- **识别**：只按 system 提示词前缀（默认 `You are a security monitor`）严格匹配，正常对话不会误伤。先用 `bytes.Contains` 预筛，正常请求不做 JSON 解析，开销极低。
- **改写**：命中后设 `thinking:{type:"disabled"}` + `reasoning_effort:"none"` + 删 `reasoning`（三字段双保险，覆盖 Anthropic / OpenAI 两种格式），并把 `max_tokens` 压到 `classifier_max_tokens`。
- **保留 key 顺序**：改写用 `json.Decoder` 流式定位目标字段在原 body 的字节位置，再做文本替换--只动 `thinking`/`reasoning_effort`/`reasoning`/`max_tokens` 这几个字段，其余字节（含 key 顺序、空格、格式）原样保留，不整体 `Unmarshal`+`Marshal`（那会让 Go 按字典序重排所有 key，可能影响上游缓存命中）。原 body 没有的字段（如 `reasoning_effort`）追加到闭合 `}` 之前。
- **Content-Length**：改写后 body 变长，代理会按新 body 重算长度（`copyHeaders` 跳过原 Content-Length），不会截断。

### 怎么确认生效

1. 跑 `claude` 触发一次 Bash 操作，看代理窗口有没有 `[改写] 命中分类器请求` 日志。有 → 分类器走代理了且已改写。
2. 如果没看到 `[改写]` 但 Bash 还是慢/失败：把 `config.json` 的 `log_request_detail` 改成 `true`，再触发一次，看 `[详情]` 日志里那个非流式请求的 `sys=` 前缀到底是什么，把 `classifier_system_prefix` 改成它。
3. 如果 ARK 的 glm-5.2 不认 `thinking:{type:"disabled"}`（改写了但分类还是慢），目前没有完美办法——三字段已经一起塞了，多余的会被上游忽略。可以先观察 `[改写]` 之后 Bash 是否还报 unavailable。

> 注意：这只治"分类器因 thinking 太慢撞超时"。如果分类器返回的是 429/503（限流），那走的是上面的重试逻辑，两套各管各的。

ARK 的 Base URL 带 `/api/plan` 前缀，但因为 Claude Code 自带 `/v1/messages` 路径，代理用 `Upstream + r.URL.Path` 拼接正好得到 `…/api/plan/v1/messages`，是 ARK 的正确端点（已实测）。所以**只要上游期望标准 Anthropic 路径 `/v1/messages` 接在 Base URL 后面，当前逻辑就无需改动**。

只有当上游期望的路径不是简单的「Base URL + `/v1/...`」时（比如它要 `/anthropic/messages` 而不是 `/anthropic/v1/messages`），才需要改 `main.go` 里的拼接逻辑。

## 实时状态行

透传响应时，代理会在终端最后一行**原地刷新**一个状态行（不新增日志行），实时显示流量与 token：

```
[流式] 活跃1 | 流出 2.3KB (12KB/s) | 缓存命中 0 | 输入 24 | 输出 80 | 重试 0 | 分类器 0
```

- **活跃**：当前正在透传的流数量。
- **流出 / 速率**：累计已转发字节 + 最近 100ms 的字节速率。每转发一段 SSE 就涨，**流过程中实时跳动**，是「正在迸出」的直接体感。
- **缓存命中 / 输入 / 输出**：从 SSE 的 `usage` 解析的累计 token。ARK 的 `usage` 不含 `cache_creation_input_tokens`，故无「缓存写入」一项。
- **重试**：启动至今因状态码命中 `retry_status_codes` 而重试的累计次数（每重试一次 +1；预算耗尽放弃的不计）。
- **分类器**：启动至今命中分类器请求并关掉 thinking 的累计次数。

> **关于 token 实时性**：ARK（及标准 Anthropic）只在流**末尾**的 `message_delta` 事件里发一次 `usage`，流过程中的 `content_block_delta` / `thinking_delta` 不带 token 计数。所以 token 字段在流过程中保持不变，到流结束才一次性更新为准确值。要看「正在迸出」的实时变化，看「流出」和「速率」——它们随字节转发实时跳。

实现要点：

- 多个并发请求的计数**聚合**到全局，不会互相覆盖；`output_tokens` 是单流累积值，用每流增量累加，所以并发流也不会把彼此的计数冲掉。
- 状态行用 `\r` 回车 + `\033[K` 清行原地刷新；log 输出前会先清掉状态行，所以日志往上滚、状态行始终停在最后一行，互不干扰。
- 仅在终端下显示：stderr 重定向到文件（如 `nohup` / 后台运行）时自动关闭，不会把状态行写进日志文件。
- Windows 上启动时会调用 `SetConsoleMode` 启用控制台 VT 处理，让 ANSI 转义在 cmd / PowerShell 里也生效（见 `vt_windows.go`）。
- 关闭方式：`config.json` 里设 `"live_stats": false`。

## Windows 托盘（最小化到托盘）

仅 Windows。双击 `proxy429.exe` 运行后，控制台点最小化会缩到系统托盘，不再占任务栏：

- **双击托盘图标**：恢复并前置控制台窗口。
- **右键托盘图标**：菜单含 `显示窗口` / `打开配置文件` / `刷新重载配置` / `退出代理`。
- **刷新重载配置**：重新读取 `config.json` 并原子替换运行时配置，同时清空所有累计统计（缓存命中/输入/输出/重试/分类器计数归零），效果等同于"关闭程序重新打开"的配置与统计。注意不会重新监听端口——若改了 `listen` 端口仍需重启程序。
- **explorer 重启自动恢复**：`explorer.exe` 崩溃重启会清空托盘，代理响应 `TaskbarCreated` 广播自动重建图标，无需重启代理。
- **状态灯图标**：托盘图标三态变色--**灰色**(空闲，无请求) / **黄色**(已发上游、等首字节) / **绿色**(流式转发中)。并行请求时显示高优先级(绿>黄>灰)。每 200ms 检查一次状态，state/active/waiting 任一变化时更新图标和悬停文字(同一状态不重复刷新)；explorer 重启重建后也会立即恢复当前颜色。
- **悬停 tooltip**：鼠标移到托盘图标上显示多行文字，带当前数量。空闲时 `Proxy429` / `idle`；有请求时按状态分行：`active N` / `waiting N`，两态并存时都显示(如 `Proxy429` / `active 2` / `waiting 3`)。数量变化也会刷新，所以能实时看到几个流在跑。

实现要点：

- 平台隔离用构建标签：`tray_windows.go`（`//go:build windows`）含全部实现，`tray_other.go`（`//go:build !windows`）提供空 `setupTray`，非 Windows 不编译、不生效。
- 纯标准库 `syscall` 直调 `user32`/`shell32`/`kernel32`/`gdi32`（`Shell_NotifyIcon`、`RegisterClassEx`、`CreateWindowEx`、消息循环、`CreateBitmap`+`CreateIconIndirect` 生成图标），无第三方依赖；状态灯图标用 GDI 动态生成 16x16 纯色位图(32bpp BGRA + 单色 AND 掩码)，灰/黄/绿三色切换时 `NIM_MODIFY`(带 `NIF_TIP` 同步更新 `szTip` 多行文字)更新并 `DestroyIcon` 销毁旧图标，避免 GDI 句柄泄漏。
- 消息循环跑在 `runtime.LockOSThread()` 固定的 goroutine 里，不阻塞代理主循环。
- 最小化检测用 200ms 轮询 `IsIconic`，避开 subclass 控制台窗口的不稳定。

## 本地测试

`test/` 目录里是可以脱离 ARK 本地复测用的文件：

- `test/mock_429.go`：mock 服务器，用 `?mode=` 切换三种上游行为——`429`（状态码重试）、`bodyerr`（200+体内错误重试）、`ok`（正常透传，带 `usage` 和 `message_delta`，能在状态行看到 token 跳动）。还会打印收到的 `thinking`/`reasoning_effort`/`max_tokens`，验证分类器改写是否生效。
- `test/config_test.json`：测试配置，指向本地 mock，重试间隔小、关掉了 `Retry-After`，方便快速复测。

复测流程（项目根目录执行）：

```powershell
# 终端1：起 mock（监听 9099）
go run ./test

# 终端2：起代理，指向 mock
.\proxy429.exe -config test/config_test.json

# 终端3：发请求测三种情况
curl -i -X POST "http://127.0.0.1:8081/v1/messages?mode=429" -d '{"model":"t","messages":[{"role":"user","content":"hi"}]}'
curl -i -X POST "http://127.0.0.1:8081/v1/messages?mode=bodyerr" -d '{"model":"t","messages":[{"role":"user","content":"hi"}]}'
curl -i -X POST "http://127.0.0.1:8081/v1/messages?mode=ok" -d '{"model":"t","messages":[{"role":"user","content":"hi"}]}'
```

看代理窗口的 `[改写]`/`[重试]`/`[透传]` 日志确认行为。测完 `taskkill /F /IM proxy429.exe` 并关掉 mock。

## 观察限流

日志会打印每次重试（`[重试 N] 状态码 429, 等待 ...`），跑一段时间就能看出这家提供商到底多爱 429。
