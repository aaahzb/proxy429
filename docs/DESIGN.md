# Proxy429 设计文档

> 本文档随代码同步更新，记录每个机制「为什么这么做、踩过什么坑」。
> 项目入口与英文摘要见根目录 README.md。

一个本地 Anthropic 兼容 HTTP 代理，专门用来吃掉上游的 429 / 5xx——遇到限流就自动指数退避重试，让 Claude Code 完全无感。

## 工作原理

Claude Code 的所有请求先发到本地代理（`127.0.0.1:8080`），代理原样转发给上游模型提供商：

- **原样透传**：Anthropic 兼容接口，请求/响应头和体原样转发，不做解析改写。例外：分类器请求关掉 thinking、路由命中时改 model 名/上游/API key、fast 路由命中时移除 speed 字段并注入响应 headers（见「分类器请求自动关 thinking」「路由功能」两节）。
- **自动重试（三种触发）**：
  - **情况 0**：网络层错误（连接失败、首字节超时）-> 重试。客户端断开（ctx 取消）则立即停止重试、直接结束，不再白烧上游配额。
  - **情况 A**：上游 HTTP 状态码是 `429/500/502/503/504` → 重试。
  - **情况 B**：状态码是 **200**，但响应体里藏着错误事件（`event: error` / `rate_limit` / `overloaded` / `"type":"error"` 等）→ 也重试。这点很关键——有些上游（含部分 Anthropic 兼容服务）限流时不返回 429 状态码，而是返回 200 把错误塞在 SSE 流里，只看状态码会漏掉。
  - **情况 C**：正常响应 → 直接透传，记录首字延迟 / 流式时长 / output_tokens 入滑动窗口。
- **总预算**：`total_budget_s` 限制总重试时长，超时后放弃并把最后一个响应透传给 Claude Code，避免 Claude Code 自己先超时。
- **首字节超时内部重发**：`upstream_header_timeout_s`（默认 70s）限制"等上游首字节"的时长。超时认为请求卡住，**内部自动重发**（不报错给 Claude Code）；重试用尽才透传 503 交给 Claude Code 自行重试。注意只限等首字节、不砍流式 Body（长输出不受影响）。
- **流式透传**：每读到一点就 `Flush`，保证 SSE 增量输出，不会攒成一坨。判断错误时只偷看响应体开头，没消费的字节会补回去继续转发，不丢数据。
- **关闭上游压缩**：代理强制 `Accept-Encoding: identity`，这样响应体是明文，才能做错误字符串匹配；代价是带宽略增。

## 文件结构

| 文件 | 作用 |
|------|------|
| `main.go` | 代理主程序（含 `resolveConfigPath` 配置路径解析、`reloadConfig` 热重载、`logRing` 内存日志缓冲） |
| `tray.go` | 跨平台托盘/菜单栏（`fyne.io/systray`）：菜单（仅「查看日志」+「退出代理」两项，全平台一致）、状态灯图标、tooltip、状态轮询 |
| `logview.go` | 网页控制台（全平台唯一 UI）：挂 `/__logs`，状态/日志/配置三标签；同端口本地路由 `/__logs/data`、`/__config`、`/__reload`，仅本机访问 |
| `main_test.go` / `tray_test.go` | 单元测试（SSE 解析 / 状态采样 / 托盘状态灯与 tooltip） |
| `config.json` | 运行配置（监听地址、上游、重试策略、分类器开关等）。首次运行无配置时从 `//go:embed config.example.json` 自动生成 |
| `config.example.json` | 内嵌的配置模板，首次启动自动生成 `config.json` 用 |
| `go.mod` | Go 模块定义 |
| `README.md` | 文档 |
| `test/mock_429.go` | 本地测试用 mock 服务器（见「本地测试」） |
| `test/config_test.json` | 本地测试用配置 |
| `release/Proxy429.app` / `release/proxy429.exe` / `release/proxy429` | 编译产物（`build.sh` 按宿主平台输出；darwin 打包成 `.app`、windows 用 GUI 子系统、linux 纯二进制；`.gitignore` 忽略，不入库） |

## 配置说明（config.json）

当前已配置为火山方舟 ARK Coding Plan：

```json
{
  "listen": "127.0.0.1:8080",
  "upstream": "https://ark.cn-beijing.volces.com/api/plan",
  "max_retries": 5,
  "base_delay_s": 0.5,
  "max_delay_s": 20,
  "total_budget_s": 120,
  "retry_status_codes": [429, 500, 502, 503, 504],
  "respect_retry_after": true,
  "classifier_thinking_disabled": true,
  "classifier_max_tokens": 0,
  "upstream_header_timeout_s": 70,
  "log_request_detail": false,
  "recent_sample_window": 20,
  "convertAlltoStream": false,
  "responses_listen": "",
  "routes": []
}
```

- **listen**：本地监听地址端口，Claude Code 连这里。
- **upstream**：上游 ARK 的 Anthropic 兼容 Base URL。已带 `/api/plan` 前缀，Claude Code 自带的 `/v1/messages` 会被拼在后面，最终端点为 `https://ark.cn-beijing.volces.com/api/plan/v1/messages`（已实测返回 401 鉴权错误，证明路径正确）。`routes` 里有 `pattern:"*"` 兜底规则时可为空（所有带 model 的请求都被兜底路由接管，默认 upstream 用不到）；为空且无兜底时，未命中路由的请求直接 502，配置加载/重载日志会警告。
- **max_retries**：最多重试次数。
- **base_delay_s / max_delay_s**：指数退避的起步等待和上限（秒）。
- **total_budget_s**：总重试预算（秒），超过即放弃。
- **retry_status_codes**：触发重试的状态码。
- **respect_retry_after**：是否优先听上游 `Retry-After` 头。
- **classifier_thinking_disabled**：是否对安全分类器请求关掉 thinking（见下节）。
- **classifier_max_tokens**：命中分类器后把 max_tokens 压到这个值（加速）。**注意：值太小（如 512）会让分类器的 thinking 被截断、Claude Code 收不到有效的安全判断，表现为 Bash 被拒且不给原因**。推荐 `0`（不压，用请求原 max_tokens；关 thinking 后输出很短，仍快速返回）。
- **upstream_header_timeout_s**：等上游首字节的最长时间（秒）。超过则认为请求卡住，**内部自动重发**（不报错给 Claude Code），重试用尽才透传 503 交给 Claude Code 自行重试。默认 `70`（70s）。用首字节超时而非整体超时，只卡"等响应开头"而不砍掉长流式输出。
- **ping_interval_s**：429 重试时向客户端发 SSE ping 保活的间隔（秒）。代理重试期间客户端收不到上游数据，长时间无数据会触发 Claude Code 超时报 API error；代理定期发 ping 保活避免此问题。默认 `5`；`0` 用默认。详见「429 重试保活」一节。
- **log_request_detail**：是否打印每个请求的 stream/tools/system 前缀（诊断分类器指纹用，默认关）。
- **recent_sample_window**：网页控制台「状态」标签「首字」「tok/s」取最近多少次请求的样本做统计（滑动窗口）。默认 `20`；改大更平滑、改小更跟手。仅统计正常透传（情况 C）的流。
- **convertAlltoStream**：全局流式化开关（默认 `false`）。开启后，所有非流式请求（`stream:false` 或省略）被代理悄悄改为流式发给上游——网页控制台实时可见吐字，首字延迟与 tok/s 统计与普通流式请求一致。请求方无感知：代理把上游流完整收完后，**原样重建**非流式 JSON 一次性返回（所有内容块按流里原样拼回，含搜索结果 `encrypted_content`），调用方拿到的仍是它预期的非流式响应。流中途断开（未见 `message_stop`）时未向客户端写任何内容，代理整体重试。仅作用于 Anthropic Messages 请求（`/v1/messages`）；已是流式的请求与搜索摘要模式不受影响。详见「全局流式化」一节。
- **responses_listen**：OpenAI Responses API 监听口（空 = 不启用；配置模板默认演示 `127.0.0.1:8081`）。设为如 `127.0.0.1:8081` 后，代理在该地址额外开一个 Responses API 端点（`/v1/responses`），把 Responses 协议请求翻译成 Anthropic Messages 走主管线（路由/重试/网页监控全部生效），响应再翻译回 Responses 协议。供 Codex CLI 等只说 Responses 协议的工具接入 Anthropic 上游。改动保存重载即生效（监听口随配置动态启停）。详见「Responses API 监听口」一节。
- **routes**：模型路由规则数组，按 `pattern` 通配匹配请求的 model 名，命中则改走指定上游（换 URL/API/model）。未配置或空数组则不路由，所有请求走默认 `upstream`。详见「路由功能」一节。
- **classifier_route**：分类器请求专用路由（对象，与 `routes` 平级）。命中分类器（安全判断）的请求无视原 model 统一路由到指定 `url`/`api`/`model`；未配置则分类器请求仍按 model 走 `routes`（兼容）。详见「路由功能」一节。
- **fast_route**：fast 模式请求专用路由（对象，与 `routes` 平级）。检测到 `"speed":"fast"` 的非分类器请求统一路由到指定 `url`/`api`/`model`；未配置则不干预（兼容）。详见「路由功能」一节。
- **multimodal_fallback**：多模态兜底路由（对象，与 `routes` 平级）。请求带图片却命中 `text_only` 纯文本模型时，自动改走此处指定的 `url`/`api`/`model`；未配置则不兜底（透传给纯文本模型，由上游处理）。详见「路由功能」一节。
- **search_fallback**：搜索兜底路由（对象，与 `routes` 平级）。请求带搜索工具却命中 `no_search` 不支持搜索上游时，自动改走此处指定的 `url`/`api`/`model`；未配置则不兜底。详见「路由功能」一节。
- **log_file**：日志文件路径（可选）。**默认空**：日志只进内存环形缓冲（`logRing`，500 行，供网页控制台「日志」标签轮询）+ stderr；windowsgui 子系统或无终端时 stderr 为空操作，即不落盘。设了非空值才同时追加写入此文件，方便留存排查或 RemoteApp 等无控制台场景复制查看。改了需重启代理生效（网页「配置」标签保存重载不会重开日志文件）。

## 用法

### 1. 启动代理

`build.sh` 产出的二进制在 `release/` 下（见「重新编译」），双击或命令行启动均可，启动后驻留托盘/菜单栏，**不需要保持终端窗口开着**：

```powershell
# Windows（GUI 子系统，不弹控制台窗口）
.\release\proxy429.exe
```
```bash
# macOS（.app 菜单栏应用，无 Dock 图标）
open release/Proxy429.app
# Linux
./release/proxy429
```

启动日志（含 `代理启动 vc639d56-1606: 监听 http://127.0.0.1:8080 -> 转发到 https://ark.cn-beijing.volces.com/api/plan`，`v` 后是版本号 = git 短 hash + 构建时分）进内存环形缓冲，可在网页控制台「日志」标签查看；`log_file` 非空时也写入文件。

> 默认按 `resolveConfigPath` 解析配置路径：`-config` 标志 > 当前目录 `./config.json`（若存在）> `os.UserConfigDir()/proxy429/config.json`（macOS `~/Library/Application Support/proxy429/`、Linux `~/.config/proxy429/`、Windows `%AppData%/proxy429/`）。首次运行无配置时从内嵌的 `config.example.json` 自动生成。本地 mock 测试用 `-config test/config_test.json`，详见下文「本地测试」一节。
>
> 启动后状态栏出现状态灯图标（灰/黄/绿）：macOS 在菜单栏（`.app` 打包，`LSUIElement=true` 无 Dock 图标）、Windows/Linux 在系统托盘。右键菜单全平台一致，仅 `查看日志`（浏览器打开网页控制台）+ `退出代理` 两项。Windows 用 GUI 子系统（`-H=windowsgui`）构建，启动不弹控制台窗口，纯托盘运行。

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

macOS / Linux（bash/zsh）：
```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8080"
export ANTHROPIC_AUTH_TOKEN="你在火山方舟控制台获取的 API Key"
claude
```

### 重新编译（改了 main.go 后）

用项目根的 `build.sh`，它会自动把版本号（git 短 hash + 构建时分）注入二进制，并按宿主平台输出对应产物到 `release/`：

- **darwin** -> `release/Proxy429.app`（`LSUIElement=true` 菜单栏应用，无 Dock 图标）+ ad-hoc codesign（首次启动需 Finder 右键「打开」过 Gatekeeper）
- **windows** -> `release/proxy429.exe`（链接器 `-H=windowsgui`，GUI 子系统，启动不弹控制台窗口，纯托盘运行）
- **linux** -> `release/proxy429`

同时把 `使用说明.md`（若存在）复制进 `release/`。

```bash
bash build.sh
```

> 不要直接 `go build`--那样版本号会是 `dev`，网页控制台/启动日志显示 `vdev`，没法区分跑的是哪个构建。`build.sh` 核心等价于：
>
> ```bash
> # macOS（托盘走 cgo，需 clang，macOS 自带）
> CGO_ENABLED=1 go build -buildvcs=false -ldflags "-X main.Version=$(git rev-parse --short HEAD)-$(date +%H%M)" -o release/proxy429 .
> # Windows（托盘纯 Go，免 C 编译器；-H=windowsgui 走 GUI 子系统不弹控制台）
> CGO_ENABLED=0 go build -buildvcs=false -ldflags "-X main.Version=$(git rev-parse --short HEAD)-$(date +%H%M) -H=windowsgui" -o release/proxy429.exe .
> # Linux（托盘走 D-Bus，纯 Go）
> CGO_ENABLED=0 go build -buildvcs=false -ldflags "-X main.Version=$(git rev-parse --short HEAD)-$(date +%H%M)" -o release/proxy429 .
> ```
>
> **跨平台与 cgo**：托盘库 `fyne.io/systray` 只有 macOS 需 cgo（Cocoa/AppKit，macOS 自带 clang 满足），Windows 和 Linux 均为纯 Go（Linux 走 D-Bus），可免 C 工具链直接交叉编译。即 `GOOS=windows CGO_ENABLED=0 go build` 和 `GOOS=linux CGO_ENABLED=0 go build` 在 macOS 上可直接产出对应平台二进制。

## 密钥传递（ARK 专属）

代理原样透传 header。ARK 的 Anthropic 兼容端点**只认 `Authorization: Bearer` 头**（已通过 401 错误码 `AuthN_MissOrInvalidAuthorizationHeader` 验证），对应 Claude Code 的 `ANTHROPIC_AUTH_TOKEN` 环境变量。**不要用 `ANTHROPIC_API_KEY`**（它会变成 `x-api-key` 头，ARK 不认，会一直 401）。

## 排错：代理"没反应"/看不到重试日志

新版本加了**全量请求日志**，每个进来的请求打印 `[请求]`、每次上游返回打印 `[尝试 N] 上游响应状态码`、重试时打印 `→ 状态码 X，等待 Y 后重试`。所以一眼就能定位问题：

**启动代理后，在另一个终端跑 `claude`，然后看网页控制台「日志」标签（托盘右键「查看日志」，或浏览器开 `http://127.0.0.1:8080/__logs`）：**

- **看到 `[请求] #N POST /v1/messages model=xxx (body=N字节) 来自 ...`** → 请求已经进代理，代理在工作。接着看上游状态码日志：
  - `[尝试 N] 上游响应状态码: 429` 接着 `→ 状态码 429，等待 ... 后重试` → 正常在重试，若最后仍 429 说明重试耗尽，把 `config.json` 的 `max_retries` / `total_budget_s` 调大。
  - `[尝试 N] 上游响应状态码: 200` 接着 `→ 状态码 200 但响应体含错误` → 上游把限流错误塞在 200 响应体里了，代理也在重试（情况 B）。
  - `[尝试 N] 上游响应状态码: 401` → 鉴权方式错了，改用 `ANTHROPIC_AUTH_TOKEN`（见下）。
- **完全没有 `[请求]` 日志** → **Claude Code 根本没走代理**，它的 429 是直接从 ARK 拿的。这是最常见的"没反应"原因，按下面修。

- **HEAD 探测请求**（Claude Code 启动时发 `HEAD /`、`HEAD /api/hello` 探测连通性）：代理直接返回 200，不建 flight、不转上游、不打 `[请求]` 日志。所以日志和流列表里看不到这类请求是正常的，不代表代理没工作。

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

1. 跑 `claude` 触发一次 Bash 操作，看网页控制台「日志」标签有没有 `[改写] 命中分类器请求` 日志。有 → 分类器走代理了且已改写。
2. 如果没看到 `[改写]` 但 Bash 还是慢/失败：把 `config.json` 的 `log_request_detail` 改成 `true`，再触发一次，看 `[详情]` 日志里那个非流式请求的 `sys=` 前缀到底是什么。分类器前缀已硬编码为 `You are a security monitor`（main.go 常量 `classifierSystemPrefix`），若 Claude Code 升级后换了前缀，需改此常量重编译。
3. 如果 ARK 的 glm-5.2 不认 `thinking:{type:"disabled"}`（改写了但分类还是慢），目前没有完美办法——三字段已经一起塞了，多余的会被上游忽略。可以先观察 `[改写]` 之后 Bash 是否还报 unavailable。

> 注意：这只治"分类器因 thinking 太慢撞超时"。如果分类器返回的是 429/503（限流），那走的是上面的重试逻辑，两套各管各的。

ARK 的 Base URL 带 `/api/plan` 前缀，但因为 Claude Code 自带 `/v1/messages` 路径，代理用 `Upstream + r.URL.Path` 拼接正好得到 `…/api/plan/v1/messages`，是 ARK 的正确端点（已实测）。所以**只要上游期望标准 Anthropic 路径 `/v1/messages` 接在 Base URL 后面，当前逻辑就无需改动**。

只有当上游期望的路径不是简单的「Base URL + `/v1/...`」时（比如它要 `/anthropic/messages` 而不是 `/anthropic/v1/messages`），才需要改 `main.go` 里的拼接逻辑。

## 路由功能

按请求的 model 名把请求路由到不同上游（换 URL + API key + model 名），支持多组、`*` 通配。**未配置 `routes`（或空数组）时完全不路由，所有请求走默认 `upstream`，行为和没这功能一样。**

配置示例（把 `claude-opus*` 开头的请求改走 DeepSeek，model 名换成 `deepseek-V4-pro`）：

```json
"routes": [
  {
    "pattern": "claude-opus*",
    "url": "https://api.deepseek.com",
    "api": "sk-deepseek-xxx",
    "model": "deepseek-V4-pro",
    "text_only": true
  }
]
```

- **pattern**：模型名通配符，仅支持 `*`（匹配任意长度任意字符，含空）。`claude-opus*` 命中 `claude-opus-4-8`/`claude-opus`；`*opus` 匹配后缀；`claude-*` 匹配前缀；`a*b*c` 要求中间出现 b。无 `*` 则精确匹配。多条规则按数组顺序匹配，**第一个命中的生效**，没有"更具体优先"的排序--宽通配会截胡窄通配：`*opus*` 写在 `*opus-4*` 前面时，`claude-opus-4-8` 会先命中 `*opus*`，`*opus-4*` 永不触发；要让更具体的 pattern 生效，把它写在前面。
- **url**：目标上游 Base URL（覆盖默认 `upstream`）。Claude Code 的 `/v1/messages` 会拼在后面，拼接规则和默认 upstream 一致。
- **api**：目标 API key，设为 `Authorization: Bearer` 头。命中后会**删掉客户端原带的 `Authorization` 与 `x-api-key`**（避免把 ARK 的 token 透传到 DeepSeek 之类），再设新 key。留空则透传客户端原 token。
- **model**：替换成的目标模型名（改写请求体 `"model"` 字段的值，长度变化自动重算 Content-Length）。留空则不改 model。
- **text_only**：布尔，标记目标模型**仅支持纯文本**。设为 `true` 后，若该请求带图片，会自动改走 `multimodal_fallback` 兜底（详见「图片路由」一节）。不设或 `false` 则不兜底。
- **no_search**：布尔，标记目标上游**不支持搜索**。设为 `true` 后，若该请求带搜索工具，会自动改走 `search_fallback` 兜底（详见「搜索路由」一节）。不设或 `false` 则不兜底。
- **enhance_search**（可选对象，省略即不启用）：配成 `{}` 或填子字段即启用**增强搜索**——主力支持搜索（`no_search` 不为 `true`）时，带搜索工具的请求不调主力，改用本 route 的 `url`/`api`/`model` 走 kimi 摘要模式（详见「增强搜索」一节）。子字段：`summary_thinking`（默认 `false`）第2步摘要是否开 thinking；`summary_level`（默认 `low`）摘要详细程度 `low`/`mid`/`high`/`max`，档位与 `search_fallback.summary_level` 一致。
- **url_response_api**：字符串，原生 Responses API 上游 Base URL（省略或留空 = 不启用）。填了它之后，**Responses 监听口**（`responses_listen`）命中本路由的请求**不再翻译成 Anthropic**，Responses 原文直接透传到该字段指定的上游——适合 OpenAI 官方等原生完整实现 Responses 的上游，少一层翻译、缓存与计费口径和官方直连一致。只影响 Responses 口：Anthropic 口（Claude Code）命中同一条路由仍走 `url` 字段，互不影响。base 填到 `…/coding` 或 `…/v1` 即可（自动补 `/v1/responses`）；`api`/`model` 的 key 覆盖与 model 改写照常生效，重试与网页控制台监控统计（token 缓存拆分、工具标签、流渲染）也与翻译流一致。配了它时本路由的 `text_only`/`no_search`/`enhance_search` 对透传流不生效（那些是 Anthropic 上游的能力标记）。网页「API」列一眼区分协议来源：橙色 `[Anthropic]` = Anthropic 口流量，绿色 `[Response]` = 这种透传流，紫色 `[translate]` = 翻译流。**已知限制**（2026-09 实测）：透传不修改请求体（仅路由 `model` 改写照常），上游必须接受客户端原样发来的全部工具定义——Codex 桌面端恒带 `tool_search` 工具（0.152.1 起官方移除关闭开关；存在 MCP/插件延迟工具时自动附加，纯净 CLI 无延迟工具则不带），Kimi 的 Responses 端点目前拒绝该类型（400 `tool type "tool_search" is not supported`）：Kimi 路由暂走 `url` 翻译口，透传功能保留，待 Kimi 支持后再启用。
- **thinking**：字符串，声明**目标模型**的思考形态（省略或 `"auto"` = 按客户端发来的 model 名查内置映射表）。**仅 Responses 口翻译流生效**——Anthropic 口客户端直接发 Anthropic 格式，代理不改写其中的思考字段；`url_response_api` 透传流不翻译，同样不生效。为什么需要它：翻译时决定发 `adaptive+effort` 还是 `enabled+budget_tokens` 用的是客户端 model 名（路由改写 model 发生在翻译之后），别名叫 `claude-fable-5` 但实际路由到 Kimi 时会被误判成 adaptive 发给目标上游。配 `"budget"` 强制经典 `enabled+budget_tokens`（DeepSeek/Kimi 等）；配 `"adaptive"` 强制 `adaptive+effort` 档位（客户端名不在映射表、目标实为 adaptive 模型时）。填其他值配置加载直接报错。
命中时打 `[路由]` 日志，如 `[路由] #1 claude-opus-4-8 -> https://api.deepseek.com (model claude-opus-4-8 -> deepseek-V4-pro)`；`[请求]` 行仍显示路由前的原始 model 名。路由命中后的重试仍走同一目标上游（URL/API/model 不变）。网页控制台「配置」标签保存重载会重新读 `routes`，热生效。

> 路由只改 URL/API/model 三项，不改写请求体里的 thinking、messages 等其他字段（`thinking` 路由参数只在 Responses 翻译构造新请求体时决定思考形态，见上）；分类器关 thinking 的逻辑在路由之前执行，两者互不影响。

**响应 model 回改**：路由改写请求 model 后，上游返回的 SSE 响应里 `"model"` 字段值（可能是上游实际 model ID，如 `glm-5-2-260617`，与请求里写的 `glm-5.2` 不一定一致）会被**字段定位替换回原始 model 名**，不依赖字符串匹配。这样 Claude Code 看到的始终是它发出的原始 model，不会因保存了上游 model 名而在重启时报 `Session model ... could not be restored`。改写时打 `[改写] #N 响应流 model 回改 <上游实际model> -> <原始model>` 日志，显示上游真正返回的 model 名。该行为对 model 路由、分类器路由、fast 路由均生效。

### 分类器路由（classifier_route）

上面按 model 名路由是给正常对话请求分流用的。还有一种**只针对分类器请求**的路由模式：不管请求原本是什么 model，只要它命中分类器（即 Claude Code 工具调用前的安全判断请求，system 前缀匹配），就统一路由到指定上游。适合把这类轻量安全判断请求甩到便宜模型，省主模型额度。

配置（与 `routes` 平级，是一个对象，不是数组）：

```json
"classifier_route": {
  "url": "https://api.deepseek.com",
  "api": "sk-deepseek-xxx",
  "model": "deepseek-v4-flash"
}
```

- **url / api / model**：含义同 `routes` 里的同名字段。
- **优先级**：命中分类器且配了 `classifier_route` 时，**无视原 model**，走分类器路由，不再匹配 `routes`。若命中分类器但**没配** `classifier_route`，则回退到按 model 走 `routes`（兼容旧行为）。
- **独立性**：与 `classifier_thinking_disabled` 互不依赖--即使没开「关 thinking」，也能单独用分类器路由；反过来开了关 thinking 也能不配分类器路由。
- 命中时打 `[路由] #N 分类器 <原model> -> <url> (model <原> -> <目标>)`，带「分类器」标识以区别于普通 model 路由。同样走网页控制台热重载。

> 分类器路由的判定（system 前缀匹配）与 `[改写]` 关 thinking 用的是同一套识别逻辑，但分类器路由只判定、不改写 body，两者可独立开关。

### fast 路由（fast_route）

> **前置条件**：Claude Code 的 `/fast` 默认仅支持 Anthropic 官方 API。通过第三方代理使用时，需要先设置 `penguinModeOrgEnabled: true` 才能开启。项目里已提供 `enableFast.txt` 脚本，复制其内容到终端执行即可（或手动在 `~/.claude.json` 里加上 `"penguinModeOrgEnabled": true`）。

Claude Code `/fast` 模式在请求体里加 `"speed":"fast"` 字段、请求头加 `Anthropic-Beta: fast-mode-2026-02-01`。fast 路由检测到这个字段时，把非分类器请求统一甩到指定上游。

```json
"fast_route": {
  "url": "https://api.deepseek.com",
  "api": "sk-deepseek-你的key",
  "model": "deepseek-v4-pro"
}
```

- **触发条件**：请求体含 `"speed":"fast"` 且不是分类器请求。
- **改写行为**：命中后 **移除** `"speed":"fast"` 字段（上游不支持）、**删除** `Anthropic-Beta` 请求头（上游不认识），并在响应里**注入假的 fast 限流 headers**（`anthropic-fast-output-tokens-remaining: 999999` 等），让 Claude Code 认为 fast 模式可用。
- **优先级**：分类器路由 > **fast 路由** > model 路由。分类器请求即使带 `"speed":"fast"` 也不走 fast 路由。
- **未配置**：不做任何处理，`"speed":"fast"` 和 `Anthropic-Beta` 头原样透传给上游。
- 命中时打 `[路由] #N fast <原model> -> <url> (model <原> -> <目标>)`，带「fast」标识。同样走网页控制台热重载。

### 图片路由（multimodal_fallback）

某些上游模型只支持纯文本（如 DeepSeek-V4），收到带图片的请求会报错。图片路由解决这个问题：给纯文本模型的目标规则打上 `text_only: true` 标记，再配一个支持多模态的兜底上游；代理检测到请求带图片、且命中的是纯文本模型时，自动改走兜底上游。

配置（与 `routes` 平级，是一个对象）：

```json
"routes": [
  {
    "pattern": "claude-opus*",
    "url": "https://api.deepseek.com",
    "api": "sk-deepseek-xxx",
    "model": "deepseek-V4-pro",
    "text_only": true
  }
],
"multimodal_fallback": {
  "url": "https://your-multimodal-upstream.example.com",
  "api": "sk-mm-xxx",
  "model": "kimi-vl",
  "no_search": false
}
```

- **text_only**（`routes` 条目内）：标记该条目标模型仅支持纯文本。
- **url / api / model**（`multimodal_fallback` 内）：兜底多模态上游，含义同 `routes` 里的同名字段。
- **触发条件**：请求体含 Anthropic 图片内容块（`{"type":"image",...}`） **且** 命中的 `routes` 规则 `text_only: true` **且** 配了 `multimodal_fallback` **且** 请求不带搜索工具（带搜索时走 `search_fallback`）。四者同时满足才兜底。
- **改写行为**：命中兜底后，URL/API 改用 `multimodal_fallback` 的值，请求体 `"model"` 改写成兜底 model 名。响应里的 `"model"` 仍会**回改成原始 model 名**（和普通路由一样，见上文「响应 model 回改」），Claude Code 看到的还是它发出的原始 model。
- **不兜底的情况**：请求无图片；命中的规则 `text_only` 为 `false`/未设（目标模型自己支持多模态）；配了 `text_only: true` 但没配 `multimodal_fallback`（降级透传给纯文本模型，由上游处理）；请求带搜索工具（改走 `search_fallback`）。分类器路由、fast 路由不参与图片兜底。
- 命中时打 `[路由] #N 图片兜底 <原model> -> <兜底url> (model <原> -> <兜底model>)`，带「图片兜底」标识。同样走网页控制台热重载。

### 搜索路由（search_fallback）

有些上游不支持搜索（如火山），有些支持（如 DeepSeek、Kimi）。给不支持搜索的目标规则打上 `no_search: true` 标记，再配一个支持搜索的兜底上游；代理检测到请求带搜索工具、且命中的是不支持搜索的上游时，自动改走兜底上游。

"带搜索工具"特指请求 `tools` 里含 **Anthropic server-side** `web_search_*` 工具（如 `web_search_20250305`，由上游执行搜索）。**刻意不识别客户端侧 `WebSearch` 工具**——Claude Code 每个请求都带它的定义，无法据此区分是不是真要搜索，会误判所有对话为搜索请求。所以这套兜底主要服务于 Claude Desktop 等走服务端搜索的客户端；Claude Code CLI 的搜索由它自己执行，不经过这个兜底。

配置（与 `routes` 平级，是一个对象）：

```json
"routes": [
  {
    "pattern": "claude-opus*",
    "url": "https://volces.example.com",
    "api": "sk-volc-xxx",
    "model": "ark-opus",
    "no_search": true
  }
],
"search_fallback": {
  "url": "https://api.deepseek.com",
  "api": "sk-deepseek-xxx",
  "model": "deepseek-search",
  "text_only": true
}
```

- **no_search**（`routes` 条目内）：标记该条目标上游不支持搜索。
- **url / api / model**（`search_fallback` 内）：兜底支持搜索的上游，含义同 `routes` 里的同名字段。
- **summary_mode**（`search_fallback` 内，可选，默认 `false`）：`true` 启用搜索摘要模式（见下节）；`false` 走老行为（整请求转走兜底上游）。
- **summary_thinking**（`search_fallback` 内，可选，默认 `false`）：`summary_mode` 下第2步摘要是否开 thinking。
- **summary_level**（`search_fallback` 内，可选，默认 `low`）：`summary_mode` 下摘要详细程度。`low`=简短摘要（`max_tokens=2048`）；`mid`=中等详细，含关键事实与数据点（`4096`）；`high`=详尽，含全部数据点/引文/上下文（`8192`）；`max`=在 `high` 基础上，遇到步骤/方法/代码/公式必须完完整整逐字复述（`16384`，`full` 为同义别名）。
- **触发条件**：请求带搜索工具 **且** 命中的 `routes` 规则 `no_search: true` **且** 配了 `search_fallback`。
- **改写行为**：同图片兜底，URL/API/model 改写，响应 model 回改成原始 model 名。
- 命中时打 `[路由] #N 搜索兜底 <原model> -> <兜底url> (model <原> -> <兜底model>)`，带「搜索兜底」标识。同样走网页控制台热重载。

#### 搜索摘要模式（summary_mode）

默认（`summary_mode` 不设或 `false`）：命中 `no_search` 上游时，**整请求转走** `search_fallback` 上游（它自己支持搜索，直接出结果）。回答用的是兜底模型而非主力模型。

`summary_mode: true` 走另一种路径：**主力不换**，用搜索上游当"搜索+摘要服务员"，代理分两步自建 Kimi 格式响应直接返回客户端，不调用主 ark：

1. **step1 搜索**：把原始请求（`web_search` 工具，model 改成 `search_fallback.model`）发给搜索上游，非流式拿回 `server_tool_use` + `web_search_tool_result`（标题/URL）。
2. **step2 摘要**：把 step1 结果作为上下文回传**同一搜索上游**，流式生成每条结果的明文摘要。详细程度由 `summary_level` 控制（`low`/`mid`/`high`/`max`，默认 `low`），`max_tokens` 随档位递增（2048/4096/8192/16384）；`max` 档遇到步骤/方法/代码/公式会完整逐字复述。可选 `summary_thinking: true` 开 thinking 提升质量。
3. **组装返回**：按 Kimi 格式拼 `server_tool_use` -> `web_search_tool_result` -> 摘要文本的 SSE 流返回客户端。

这样 Claude Desktop 的下拉搜索列表（读 `web_search_tool_result` 的标题/URL）和模型回答（读摘要文本）都能正常工作，且摘要比上游自带的更详细。step1/step2 任一失败时自动降级为整请求转走 `search_fallback`（老行为），不会报错中断。

适用：搜索上游支持 `web_search` 但自带摘要不够详细，或想让摘要格式可控。注意搜索上游必须能返回标准 `server_tool_use` + `web_search_tool_result` 块（Kimi 可以；DeepSeek 视接口而定）。

```json
"search_fallback": {
  "url": "https://api.kimi.com/coding/",
  "api": "sk-kimi-xxx",
  "model": "kimi-for-coding",
  "summary_mode": true,
  "summary_thinking": false,
  "summary_level": "low"
}
```

- 命中时打 `[路由] #N 搜索摘要模式 <原model> -> <兜底url>` 与 `[搜索摘要] #N ...` 日志。

### 增强搜索（routes 内 enhance_search）

上面 `search_fallback.summary_mode` 处理的是「主力不支持搜索」的兜底场景。**增强搜索**处理反过来：主力本身**支持搜索**，但你不想用主力自带搜索，想让代理用 kimi 摘要模式（step1 搜索 + step2 摘要）来回答。

在 `routes` 条目内加 `enhance_search` 对象（省略即不启用）：

- **触发条件**：请求带搜索工具 **且** 命中的 `routes` 规则 `no_search` 不为 `true`（即支持搜索）**且** 该规则配了 `enhance_search`。
- **行为**：不调主力，用**该 route 自己的 `url`/`api`/`model`** 走 kimi 摘要模式（同上 step1+step2+自构响应），摘要参数用该 route 的 `enhance_search.summary_level`/`enhance_search.summary_thinking`。
- **与 `search_fallback.summary_mode` 区别**：后者是 `no_search:true` 时的兜底，搜索上游用 `search_fallback` 配的；前者是 `no_search:false` 的主力主动改走摘要，搜索上游用 `routes` 条目自己配的。两者互斥：`no_search:true` 走 `search_fallback`，`no_search:false` + `enhance_search` 走增强搜索。
- **降级**：step1/step2 失败时降级为整请求转走该 route 上游（主力正常透传）。

```json
"routes": [
  {
    "pattern": "claude-haiku*",
    "url": "https://api.deepseek.com",
    "api": "sk-deepseek-xxx",
    "model": "deepseek-V4-pro",
    "enhance_search": {
      "summary_thinking": false,
      "summary_level": "mid"
    }
  }
]
```

- 命中时打 `[路由] #N 增强搜索 <原model> -> <route url> (model <原> -> <route model>)`。

### 图片 + 搜索同时出现

请求若同时带图片和搜索工具，代理**按搜索处理**：整个请求走 `search_fallback`（`summary_mode` 则两步摘要），不管请求体里是否含图片。没有证据表明实际会出现"多模态+搜索"的组合，所以不为它单独找"既认图又能搜"的兜底；若搜索上游不支持图片，带图过去可能被上游报错，代理原样透传。

- 搜索请求 -> 一律 `search_fallback`（不管含不含图片）。
- 纯图片请求（无搜索）-> `multimodal_fallback`；没配则透传原 route（上游不支持图片则报错，代理透传）。

> 能力兜底只在 model 路由分支触发，分类器路由、fast 路由不参与。两个兜底都未配时退回原行为。

## 全局流式化（convertAlltoStream）

分类器、搜索 step1 等**非流式请求**在网页控制台上看不到吐字、没有首字/tok/s 统计——上游按非流式直接返回整段 JSON，代理原样透传，页面只能看到"一次性到达"。

`convertAlltoStream`（顶层配置，默认 `false`）开启后，代理把**所有**非流式请求悄悄改为流式发给上游：

- **请求侧**：`stream:false`（或省略）的 `/v1/messages` 请求，body 里 `stream` 字段被改写为 `true` 再发上游（流式定位 + 文本替换，不改动其它字段）。
- **网页监控**：上游按流式回 SSE 时，代理边收边 tee 到在途流页面——实时可见吐字，首字延迟、流式时长、tok/s 统计与普通流式请求完全一致。
- **回传侧（客户端无感知）**：代理把整个流收完（直到 `message_stop`），**原样重建**非流式 message JSON--所有内容块（文本、thinking、`server_tool_use`、`web_search_tool_result` 含 `encrypted_content`、`tool_use` 参数等）按流里原样保留拼回，一次性以 `application/json` 返回。调用方不知道自己的非流式请求被改成过流式。
- **model 回写**：路由改写过 model 的请求，重建时 model 字段回写客户端原始 model（与流式透传一致）。
- **可靠性**：流中途断开（未见 `message_stop`）时**未向客户端写任何字节**，代理按重试节奏整体重发（重发流式请求、重收一次流）；重试等待期间**不发 SSE ping 保活**（会污染非流式响应，静默等待）。重试用尽透传 502。
- **影响范围**：只作用于 Anthropic Messages 请求（`/v1/messages`）；已是流式的请求、搜索摘要模式（自构响应）不受影响。上游没按流式回（返回普通 JSON）则直接透传。

## Responses API 监听口（responses_listen）

`responses_listen`（顶层配置；空 = 不启用，配置模板演示值 `127.0.0.1:8081`）让代理在指定地址额外开一个 **OpenAI Responses API** 端点，把只说 Responses 协议的工具（Codex CLI 等）接到任意 Anthropic 上游：

```json
"responses_listen": "127.0.0.1:8081"
```

- **接入方式**：工具指向 `http://127.0.0.1:8081/v1`，按 Responses 协议 POST `/v1/responses`（`/responses` 也认）。请求里的 model 名照常参与主管线路由匹配——在 `routes` 里加一条对应 pattern（如 `gpt-5*`）即可指定走哪个 Anthropic 上游、改写成什么模型。
- **Codex CLI 接入**：一键搞定——网页控制台「配置」标签下方按当前编辑框**实时生成** Windows / macOS·Linux 两行终端命令（DeepSeek 文档同款格式：地址取 `responses_listen`；`routes` 每个 pattern 的代表名全部写进 Codex `/model` 菜单（纯 `*` 兜底路由的代表名固定叫 `Fallback`；`pattern` 不允许全字叫 `Fallback` 或 `fast_route`——保留名，撞名的路由不生效也不进菜单，网页会红字提醒；配了 `fast_route` 且带 `model` 时菜单追加 `fast_route` 条目，选中即走 fast 通道），下拉选中项为默认模型；脚本由本代理烤制下发、零交互），复制到对应终端回车即运行；或运行仓库根目录的交互版 `codex-setup.ps1`（Windows）/ `codex-setup.sh`（macOS/Linux）（仿 DeepSeek 官方脚本：备份后外科手术式改写 config.toml、写模型目录、选 9 还原）。手动配置：编辑 `~/.codex/config.toml`（Windows 为 `%USERPROFILE%\.codex\config.toml`）——顶层写 `model_provider = "proxy429"`、`model = "gpt-5-codex"`（参与路由匹配与 thinking 查表）、`preferred_auth_method = "apikey"` 与 `forced_login_method = "api"`（免去官方账号登录），再加 `[model_providers.proxy429]` 段：`base_url = "http://127.0.0.1:8081/v1"`、`wire_api = "responses"`、`experimental_bearer_token = "任意占位"`（代理不校验 token，真实 key 由路由 `api` 注入）。结构与 cc-switch 接管 Codex 时写入的一致；改完重启 Codex（config.toml 不热加载）。注意系统代理坑：Windows 系统代理开启时 Codex 走该代理且不认其例外清单，`127.0.0.1` 的请求会被劫到代理服务器报 503（代理侧零日志）——Windows 一键脚本已自动写入用户级 `NO_PROXY`（含 `127.0.0.1`）绕过，macOS/Linux 脚本只做体检并给出 `export NO_PROXY=...` 提示，手动配置需自行 `setx NO_PROXY "localhost,127.0.0.1,::1"` 后重启 Codex。多模型切换：加 `[profiles.名字]` 各设 `model`（`codex --profile 名字` 启动）或临时 `codex --model 名字`，代理按模型名路由、无需改动。逐步教程见 `使用说明.md`「让 Codex CLI 走代理」。
- **翻译**：`instructions`/system 消息 → `system`；扁平 `input[]` 重新嵌套成 Anthropic messages（`function_call` 并入 assistant 的 tool_use、连续 `function_call_output` 合并进一条 user 的 tool_result，不完整工具轮自动丢弃、首条非 user 自动补前导）；`max_output_tokens` → `max_tokens`（缺省 32000）。
- **thinking 映射**（与 cc-switch 3.20.0 的 thinking_optimizer 完全一致）：`reasoning.effort` 按模型分类走两条路径——adaptive 模型（fable-5/mythos-5/mythos-preview/sonnet-5/opus-4-8/4-7/4-6/sonnet-4-6，子串匹配）翻成 `thinking:{"type":"adaptive"}` + `output_config.effort`（low/medium/high/max），其中 fable-5/mythos-5/mythos-preview/sonnet-5 不带 effort 也默认开；fable-5/mythos-5 关不掉 thinking，显式 `effort:"none"` 翻成 adaptive + `effort:"low"`。其余模型翻成 `thinking:{"type":"enabled","budget_tokens":N}`（low 2048 / medium 8192 / high 16384 / xhigh·max·ultra 24576，上限压到 max_tokens 一半、不足 1024 不开）。工具续轮缺签名 thinking 回放、或 thinking 与强制 tool_choice 冲突时按 cc-switch 同款规则降级/报错。查表用客户端发来的 model 名（路由改写之前）。完整映射表见 `使用说明.md`「Responses 翻译映射表」。
- **工具体系**（与 cc-switch 3.20.0 对齐）：function 工具与 `web_search` 托管工具 → Anthropic tools（web_search 映射 `web_search_20250305`，cc-switch 反而是丢弃的）；`custom` freeform 工具（如 Codex 的 apply_patch）→ 包装成 `{"input": string}` 的 JSON Schema，原始工具定义内嵌 description，响应拆包回 `custom_tool_call`（流式走 `custom_tool_call_input.done` 事件）；`namespace`（MCP）工具 → 子工具拍平成 `ns__name`（超 64 字节截断加 sha256 后缀），响应还原成带 `namespace` 字段的 function_call；`tool_search` → 固定代理工具。工具结果里的图片媒体（MCP image 块、JSON 字符串嵌套、整串 data URL）自动剥离成 Anthropic image 块而非字符串化；`input_file` → document 块（附件认不出标准形态——blob:/file: 本地 URL、file_id 云端引用等——时序列化成文本兜底，不静默丢）；`tool_choice` 全形状映射（required/auto/none/function/custom/tool_search，未知形状降级 auto）；Anthropic 模型 Read 工具调用的 `pages:""` 怪癖自动清理。
- **响应**：Anthropic 内容块实时翻回 Responses 事件/对象——text → message 项（`output_text.delta`）、thinking → reasoning 项（摘要文本 + `encrypted_content`）、tool_use → function_call/custom_tool_call/tool_search_call 项（按工具注册表还原身份）、搜索结果 → `web_search_call` 项；usage 合并（缓存读计入 `input_tokens_details.cached_tokens`，缓存写计入 `cache_write_tokens`）。客户端 `stream:true` 拿 SSE 事件流，`stream:false` 拿一次性 JSON。Kimi（k3-256k 等）在请求带 web_search 工具时响应里以 `Search results for query: ` 开头的搜索回声行（连同其后的 query 一起）会被翻译层整行删除——转发给客户端与回放给上游的历史同套规则，避免 query 回声在上下文里累积、诱发连续同类搜索；剥完为空的文本块整体丢弃，无参 server_tool_use、空 web_search_tool_result 等空搜索结构两个方向都不产出、不回放；客户端历史回放的 `web_search_call` 调用项（哪怕带 query/来源）也一律不上行——其 id 是代理自造（`ws_`+响应 id）、上游搜索注册表从未登记，上行必 400 并连坐信封还原的真搜索对被 fail-soft 一起剥掉（生产实证：搜索块出生 33 秒的追问即被拒），搜索内容的唯一回放载体是搜索信封。模型模仿历史把多条裸前言重复粘在正文开头时仍会剥光前言留下正文。剥离是确定性纯文本规则、逐请求逐消息无条件应用（不含回声的文本一字节不动），请求体逐轮保持一致，前缀缓存不受影响。客户端界面不再堆搜索回声行，真实搜索以结果链接（sources）照常翻译为 `web_search_call` 项。
- **思考块信封**：Anthropic 签名 thinking 块被 base64 自封装进 reasoning 项的 `encrypted_content`（前缀 `p429-ant-thinking-v1:`），客户端下轮回放历史时还原成 thinking 块发给上游——多轮工具调用的思考链不丢，且自包含、不依赖上游解密。
- **搜索块信封**：一次搜索的 `server_tool_use` + `web_search_tool_result` 成对块（含全部 `encrypted_content` 正文）同样自封装进一个 reasoning 项（前缀 `p429-ant-search-v2:`）随响应发给客户端；封入时把调用块的 id 归一为结果块的 `srvtoolu_` id——Kimi 流式搜索给的调用块 id 是 `tool_` 开头、搜索注册表从未登记，原样回放必 400 `tool_call_id is not found`（受控实验：改写后同体 200），非流式搜索两者天生一致不受影响。下轮客户端原样回放时还原成完整 Anthropic 搜索块上行——模型据此直接读上次搜索到的正文，追问不再原关键字重搜（客户端历史里的 `web_search_call` 调用项只是展示件、不回放，见上段）。信封内存归属信息（上游 url + key 哈希 + 生成时的模型名），且整个 payload 用 api key 派生的掩码异或混淆（前缀 `p429-ant-search-v2:`；混淆非加密，求性能——客户端历史里不躺明文 url/key 信息，key 不对异或出来不是 JSON 自然跳过，换 key 后旧信封自动作废）：本请求路由预测与信封不同源时跳过还原（还原也解不开，省 token）；模型不同不拦——实测同 endpoint 同 key 跨模型回放照常解密。信封带封入时刻，但代理不设固定存活期上限——实测封入 1.7 小时的搜索 id 仍存活，固定上限会误杀活信封。取而代之的是每对话自适应水位：还原后上游报 `400 tool_call_id`（搜索 id 注册表有存活期，旧对话的搜索块可能已过期）时，代理自动剥掉回放的搜索块立即重试一次（不占重试预算、400 不透给客户端）——无感降级为「没还原」，模型需要时会重新搜；同时把本次被剥信封里最老的封入时刻记为该对话的「水位」（只升不降），此后不比水位新的信封直接不还原（主动剥块，不撞 400、不占重试）——注册表按龄淘汰，同龄与更老的必死，水位随每次兜底自动逼近真实存活期。无 ts 的老信封（v2 初版）视同最老：有水位时一并剥，无水位时乐观还原。被剥过块的流在状态页显示红色 `[剥N]`（N = 本流剥掉的回放搜索块总数，含水位剥与 400 兜底剥）：在途流挂在 model 列，最近完成流挂在缓存命中率后（如 `81%[剥2]`）；其中多少是水位剥的、多少是 400 兜底剥的，拆分看日志 `[剥块]`/`[兜底]` 行。空搜索（无 query）不封信封。
- **管线复用**：翻译层把请求内部转交给主 `/v1/messages` handler，上游永远走流式——路由（pattern/分类器/fast/多模态/搜索兜底）、429 重试保活、网页控制台在途流监控与统计全部照常生效。
- **原生透传（routes 条目 `url_response_api`）**：命中路由配了该字段时，Responses 口的请求**跳过翻译**，Responses 原文直接发到该字段指定的原生 Responses 上游（如 OpenAI 官方 `https://api.openai.com`，自动补 `/v1/responses`）——少一层翻译，缓存与计费口径和官方直连一致。只是不翻译而已：路由的 `api`/`model` 覆盖、429 重试保活、网页监控（在途流渲染、token 缓存拆分统计——Responses 的 `input_tokens` 含缓存总量，自动拆成"未命中输入 + 缓存命中"与 Anthropic 口径对齐、工具调用标签）全部照常。透传流在网页「API」列显示绿色 `[Response]`（翻译流是紫色 `[translate]`，Anthropic 口流量是橙色 `[Anthropic]`）。只影响 Responses 口，Anthropic 口命中同一条路由仍走 `url`。注意 Kimi 目前拒绝 Codex 恒带的 `tool_search` 工具（400），待其支持后启用。详见「路由功能」一节的 `url_response_api` 字段说明。
- **限制**：改动保存重载即生效（监听口动态启停，不像主 `listen` 那样需重启）；与主端口一样永远仅本机可连；端口被占用只告警禁用、不影响主代理。不支持 computer use 类计算机操作工具（无对应客户端与上游，cc-switch 同样不支持）、`store:true` 服务端状态与 `/v1/models` 列举。

## 429 重试保活（SSE ping）

代理遇到 429/5xx 自动重试时，重试期间不会向 Claude Code 发任何字节（还没连上成功响应）。Claude Code 流式请求长时间收不到数据会触发客户端超时，报 "API error" 并自带重试 0/10--这时代理还在重试，两边各干各的。

为避免此问题，代理在**首次重试时**向客户端发一个 `200` + SSE 流开头，并在 backoff 等待期间每 `ping_interval_s` 秒发一个 Anthropic 标准 `event: ping` 保活事件。Claude Code 收到 ping 即认为连接活着，不会超时。重试成功后无缝接上上游的正常 SSE 流（`message_start` 等跟在 ping 后，客户端忽略 ping）。

- **backoff 可中断**：重试等待改用 `select` 监听客户端断开，客户端超时/取消时代理立即停止重试（旧版 `time.Sleep` 不可中断，客户端断了还在傻睡 + 白烧上游配额）。
- **重试用尽兜底**：已发 `200` 保活头后无法再改状态码透传 429，改发一个 SSE `event: error`（`overloaded_error`）让 Claude Code 识别错误。正常 429 暂时代理能重试成功，不触发；只有持续限流用尽才走这里。
- **正常请求零影响**：无 429 时全程不发 ping，走原透传逻辑，不多发任何字节。
- 日志：`[保活] #N 重试中(状态码 429)，开启 SSE ping 保活` / `[保活] #N 客户端已断开，停止重试`。

> 该机制假设 Claude Code 超时是"无数据超时"（ping 能重置）。若实测 ping 保活开启后 Claude Code 仍超时，说明是别的超时类型，需进一步排查。

## 网页控制台

代理的**唯一 UI** 是一个浏览器网页控制台，全平台一致（不再有终端分屏 TUI）。挂在代理同端口的 `/__logs` 路径，仅本机访问（`isLocalRequest` 限制 `127.0.0.1`/`::1`/`localhost`，远程请求返回 403，即使代理 `listen` 在 `0.0.0.0` 暴露到内网也不会泄露日志/配置）。三个标签：

```
状态卡片（示例，实际为网页渲染）：
v c639d56-1606  active 3 | waiting 1 | bytesForward 2.3KB | rate 12KB/s
cacheRead 0 | input 24 | output 80 | retries 0 | classifiers 0
avgFirstByte 1.23s | tps 45.6

在途流：
#  model      阶段        字节    状态
1  glm-5.2    转发中      4.2KB   200
2  glm-5.2    等首字      0B      -
3  glm-5.2    等首字      0B      -
```

- **状态**：状态卡片 + 在途流表格。卡片字段：`active`/`waiting`（进行中/等首字）、`bytesForward`（累计转发字节）、`rate`（每秒字节速率，用两次轮询间增量算）、`cacheRead`/`input`/`output`（从 SSE `usage` 解析的累计 token）、`retries`（累计重试次数）、`classifiers`（累计命中分类器特征次数，无论是否分流/关思考都计；卡片悬停看命中/关思考两数、点击放大明细）、`classifierNoThink`（其中实际关思考的改写次数）、`avgFirstByte`（最近 `recent_sample_window` 次平均首字延迟）、`tps`（加权 token 吞吐）。在途流表格列：`#`/model/阶段/字节/状态。首列状态灯（⚪请求 / 🟡等首字节 / 🟢转发中）旁显示当前灯色已持续的秒数，只在灯变色时清零（黄灯内的路由与多次重试不单独清零）；黄灯内正在等首字节时，状态灯与总时长之间另有 `[尝试N: Xs]` = 本次尝试已等待的时长（每次尝试重新起算；重试退避中显示 `[退避中]`），黄灯总时长 = 各次尝试 + 退避之和。最近完成流表的「缓存年龄」列按会话+路由显示该会话该路由最近一次缓存写入距现在过了多久（m:ss 递增；上游缓存真实存活期是动态的，这列不再猜倒计时，只说明这份缓存是多久前写的，还能不能用请对照「缓存命中」弹窗的实测区间判断）——同会话同路由的最新一条流显示年龄，被更新的同键流刷新后旧行显示 `-`，无会话标识的流（count_tokens 探针、裸 API 无 metadata）恒 `-`；年龄从流开始时刻起算，会话标识取客户端请求自带字段（Claude Code 的 `metadata.user_id` 内 `session_id`、Codex 的 `prompt_cache_key`），代理只读不改；黄灯（等待首字节）阶段被下游断开的 499 流不刷新缓存（本行显 `-`，锚停留再上一次同键流），绿灯（流式中）断开的 499 照常刷新（本行作为新锚）；同会话同路由有在途流正在吐字（绿灯转发中）时缓存实际刚被刷新，该键最新完成行冻结显示 `[m:ss]`（方括号内为刷新那一刻旧锚的年龄，数字不变；列头问号有悬停说明），新锚等该流完成后生效；点击「缓存命中」卡片，弹窗底部「实测缓存时间」表列出各上游实测的缓存存活时间（模型、URL、下界 ≥、≥形成、上界 <、<形成、观测次数；按 URL+模型归类，不看其他参数：同会话相邻流后条命中率 ≥95% 记区间下界「至少活了间隔那么久」取最大值，前条命中过后条命中率 <50% 记区间上界「没活过间隔那么久」取最小值——不看严格归零，系统提示词等公共前缀的残留命中不算活着；两侧矛盾时（上游缓存时间中途变化或被提前驱逐）以较新观测为准、被否一侧作废重测、观测次数同步归零重计；「≥形成」「<形成」两列 = 各自界数值形成至今的时长（m:ss 递增），该界数值变化（含作废重测）即重新起算，只新增支撑观测、数值不变时不重置，无该界观测时随界同显 -；纯展示、内存态，重启或清空统计即清零）；开始时刻距今 5 分钟内的「会话+路由最新一条」完成流不会被「保留完成流 N」挤出列表（行数可因此超 N）。在途流与最近完成流表的「API」列把协议来源与思考值合一格：名 = 协议来源（橙 `[Anthropic]`=Anthropic 口原生、紫 `[translate]`=Responses 口翻译、绿 `[Response]`=Responses 口原生透传），名后 `[值]` = **实际发给上游**的思考配置最短形态（翻译映射、分类器关思考等代理改写全部生效后的最终口径），**其颜色 = 词汇口径**（思考是针对上游的：非透传流上游收到的都是 Anthropic 格式，所以紫名后跟橙值）——橙=Anthropic thinking（直连原样或翻译映射后）、绿=Responses reasoning（仅透传）；`关`=thinking 关或 effort none/off、`开 N`=enabled+budget_tokens N、`adaptive`=自适应无档、`low`/`high`/`max` 等档位词=adaptive 的 effort 或（绿色时）reasoning.effort 原值、无 `[值]`=请求体未带思考字段；如 Codex 发 effort high 翻译到 Anthropic 上游显 `[translate][开 16384]`（紫名橙值）。搜索摘要模式触发时，step1/step2 会作为独立子流显示在在途流表格（model 列标「搜索step1·模型」「搜索step2·模型」），完成后进入「最近完成的流」，可点击查看透传内容。完成流状态码列中非 200 的状态码加方括号显示（如 `[499]`、`[400]`），一眼挑出异常流；重试/预算用尽的流代理会向下游透传兜底 error 事件（overloaded_error），状态码列显 `[重试尽]`；发生过退避重试的流在状态码后追加金色 `[重试N次]`（N = 重试次数，如 `200[重试2次]`；`[重试尽]` 时同样带，可对照 `max_retries` 看是否打满）；499 = 上游响应没发完这个连接就结束了（最常见是下游主动取消——取消会传导成上游断连，上游提供商后台同样记 499，两边口径一致；nginx 惯例 client closed request）；上游完整发完后下游才断开的（Codex 收完 response.completed 即关连接）仍记 200，与上游后台一致。在途流（随转发实时增加）与最近完成流的 model 列后都会以 `[Read*1][Edit*3]` 形式（金色）列出该流响应中调用过的工具（`web_search` 等服务端工具也计，按首次出现顺序；同名 N 次合并显 `*N`（原始次数），单次调用显 `*1`，参数结构体为空的单次调用显 `*0`——如 Kimi 空搜索的无参调用，一眼区分空搜索与真搜索）。点击在途流/完成流的行回看该流：默认看输出（「显示解析/显示原始」切换）；「看请求体」回看导致该流的下游请求体（JSON 自动美化，非完整 JSON 按原文显示）。浏览一律只给前 256KB；勾选「储存完整结构体」（默认关，重启复位）后，新开始的请求额外记录完整请求体与输出（不设上限，占内存），查看器出现「下载请求体/下载输出」按钮可下载完整文件（JSON 美化后保存，非 JSON 按原文），以及「交互式JSON」按钮——把请求体/输出渲染成可按键折叠展开的 JSON 树（默认全部折叠，点键名行懒展开；输出是 SSE 事件流时解析成事件数组再成树），取消勾选立即清空已存的完整副本、下载与交互按钮消失。数据残缺的流不会静默当成完整版：请求体只剩截断版的「下载请求体」置灰（悬停见原因），无完整输出副本的「下载输出」置灰，交互式JSON 对这两类直接提示不看。Responses 翻译口的流记录的是翻译成 Anthropic 后的请求体。「清空统计」按钮清空上述累计统计与最近完成的流列表（在途流与流的 # 编号不清，避免与在途流撞号；内存日志另有「清空日志」按钮）。
- **日志**：最近 500 行日志（`logRing` 内存环形缓冲），自动滚到底、粘性滚动（手动向上滚时暂停跟随，回到底部恢复）。无翻页键/滚轮冲突，纯浏览器原生滚动。
- **配置**：配置文件编辑器。载入当前 `config.json` 内容（`GET /__config` 返回 `{path, content, exists}`），保存时 `POST /__config` 先 `json.Unmarshal` 进 `Config` 校验 JSON 合法性，**非法 JSON 直接返回 400 且不写盘**（避免把损坏配置写到磁盘导致下次启动失败），合法才写文件并调 `reloadConfig()` 热生效；另有「仅重载」按钮 `POST /__reload` 只调 `reloadConfig` 不改文件。

页面每 500ms 轮询一次 `/__logs/data`（返回最近日志 + 全量状态计数 + 在途流列表 JSON）。**关闭浏览器标签页即隐藏，代理继续运行不受影响**。

**状态卡片字段含义**：
- **v版本**：exe 版本号 = git 短 hash + 构建时分（如 `c639d56-1606`），用于确认跑的是哪个 exe。用 `build.sh` 构建才会注入，直接 `go build` 会显示 `dev`。
- **active / waiting**：当前进行中的请求数（active = 已发上游、转发中或重试等待；waiting = 等首字节阶段）。
- **bytesForward / rate**：累计已转发字节 + 每秒更新一次的字节速率。每转发一段 SSE 就涨，是「正在迸出」的直接体感。
- **cacheRead / input / output**：从 SSE 的 `usage` 解析的累计 token（另统计 `cache_creation` 缓存写入，ARK 的 `usage` 不含 `cache_creation_input_tokens`，按 0 计）。「缓存命中」卡片 = cache_read / (input + cache_read + cache_creation)，与 Claude Code 的 cache hit 算法一致：缓存写入不算命中、但计入总输入，漏掉它会虚高命中率。点卡片可看按上游模型分组的明细（含写入量）。**中途断开的流（客户端按 Esc、网络掉线）不计入聚合**：它们只收到 `message_start` 的预估 usage（Kimi 实测 `start.input` 含 cache_read 且 `start.cr=0`，真实拆分在 `message_delta` 才到），计入会把整个上下文算成未命中输入、显著拉低命中率；这类流整体回滚已计的增量，日志标 `[中断]`。
- **retries**：启动至今的累计重试次数（每重试一次 +1；含状态码 429/5xx、首字节超时/网络错误、200 体内错误三种触发；预算耗尽放弃的不计）。
- **classifiers**：启动至今命中分类器（安全判断）特征的请求数——无论是否分流 classifier_route、是否关思考都计。状态页「分类器」卡片悬停可看到命中总量与关思考改写数两个数，点击打开明细弹窗（含关思考占比），交互与「重试」卡片相同。
- **classifierNoThink**：其中实际被改写关掉 thinking 的累计次数（`classifier_thinking_disabled` 开启时才会发生；已是关思考形态的请求不产生改写，不计）。
- **avgFirstByte**：最近 `recent_sample_window` 次请求的**平均首字延迟**（秒）--从请求发出到上游吐出第一个 body 数据字节。只统计正常透传（情况 C）的流；收到第一个字节即入窗更新，不必等整流结束。
- **tps**：最近 `recent_sample_window` 次请求的**加权 token 吞吐**--Σoutput_tokens / Σ流式时长（第一个字节到最后一个字节）。用加权而非简单平均，避免短请求拉偏整体吞吐。

> **关于 token 实时性**：ARK（及标准 Anthropic）只在流**末尾**的 `message_delta` 事件里发一次 `usage`，流过程中的 `content_block_delta` / `thinking_delta` 不带 token 计数。所以 token 字段在流过程中保持不变，到流结束才一次性更新为准确值。要看「正在迸出」的实时变化，看 `bytesForward`/`rate` 和在途流表格里的字节--它们随转发实时跳。

实现要点：

- 日志统一进内存环形缓冲 `logRing`（500 行），供网页「日志」标签轮询；同时写 stderr（windowsgui 子系统或无终端时 stderr 为空操作，不落盘）。`log_file` 非空时再追加写入文件。
- `reloadConfig()` 返回 error，网页「配置」标签保存重载失败时能把错误回显给用户；行为不变（重读配置、原子替换、清空累计统计、不重新监听端口）。
- 端点全走 `isLocalRequest` 守卫：`GET /__config`、`POST /__config`（校验 + 写盘 + 重载）、`POST /__reload`（仅重载）、`GET /__logs`（HTML 页）、`GET /__logs/data`（状态 + 日志 JSON）。

## 系统托盘 / 菜单栏（跨平台）

启动后状态栏出现状态灯图标：macOS 在菜单栏（`.app` 打包，`LSUIElement=true` 无 Dock 图标）、Windows/Linux 在系统托盘。功能：

- **右键图标**：菜单仅 `查看日志` + `退出代理` 两项，**全平台一致**（不再有「显示窗口」「打开配置文件」「刷新重载配置」--配置编辑与重载已移入网页控制台）。
- **查看日志**：用默认浏览器打开网页控制台 `http://<listen>/__logs`（状态/日志/配置三标签，见上节）；**关闭浏览器标签页即隐藏，代理继续运行不受影响**。仅本机可访问（非 `127.0.0.1`/`::1`/`localhost` 请求返回 403）。
- **状态灯图标**：三态变色--**灰色**(空闲，无请求) / **黄色**(已发上游、等首字节) / **绿色**(流式转发中)。并行请求时显示高优先级(绿>黄>灰)。每 200ms 检查一次状态，state/active/waiting 任一变化时更新图标和悬停文字。
- **悬停 tooltip**：鼠标移到图标上显示多行文字，带当前数量。空闲时 `Proxy429` / `idle`；有请求时按状态分行：`active N` / `waiting N`，两态并存时都显示。数量变化也会刷新，所以能实时看到几个流在跑。

平台差异：

| 平台 | 托盘位置 | 托盘实现 | cgo |
|------|----------|----------|-----|
| Windows | 系统托盘 | `fyne.io/systray`（纯 Go syscall） | 否 |
| macOS | 菜单栏 | `fyne.io/systray`（Cocoa/AppKit） | 是（需 clang，macOS 自带） |
| Linux | 状态区 | `fyne.io/systray`（D-Bus StatusNotifier） | 否 |

> 三平台代理功能完全一致（重试、路由、分类器、保活、网页控制台统计）；唯一差异是托盘位置（macOS 菜单栏 vs Windows/Linux 系统托盘）。Windows 用 GUI 子系统（`-H=windowsgui`）构建，启动不弹控制台窗口，纯托盘运行；日志看网页控制台或 `log_file`。

实现要点：

- 跨平台托盘用 `fyne.io/systray`：`tray.go` 一套代码管菜单/状态灯/tooltip/状态轮询，`systray.Run` 占主线程（macOS 要求 UI 事件循环在主线程），HTTP 服务在 goroutine 里并发跑。
- 状态灯图标纯 Go 生成（无 cgo/无 GDI）：画 32x32 抗锯齿实心圆，macOS/Linux 编码成 PNG，Windows 封装成 BMP-entry ICO（`LoadImageW` 必定支持）。

## 本地测试

`test/` 目录里是可以脱离 ARK 本地复测用的文件：

- `test/mock_429.go`：mock 服务器，用 `?mode=` 切换三种上游行为——`429`（状态码重试）、`bodyerr`（200+体内错误重试）、`ok`（正常透传，带 `usage` 和 `message_delta`，能在网页控制台「状态」标签看到 token 跳动）。还会打印收到的 `thinking`/`reasoning_effort`/`max_tokens`，验证分类器改写是否生效。
- `test/config_test.json`：测试配置，指向本地 mock，重试间隔小、关掉了 `Retry-After`，方便快速复测。

复测流程（项目根目录执行）：

```bash
# 终端1：起 mock（监听 9099）
go run ./test

# 终端2：起代理，指向 mock（Windows 用 .\proxy429.exe）
./proxy429 -config test/config_test.json

# 终端3：发请求测三种情况
curl -i -X POST "http://127.0.0.1:8081/v1/messages?mode=429" -d '{"model":"t","messages":[{"role":"user","content":"hi"}]}'
curl -i -X POST "http://127.0.0.1:8081/v1/messages?mode=bodyerr" -d '{"model":"t","messages":[{"role":"user","content":"hi"}]}'
curl -i -X POST "http://127.0.0.1:8081/v1/messages?mode=ok" -d '{"model":"t","messages":[{"role":"user","content":"hi"}]}'
```

看网页控制台「日志」标签的 `[改写]`/`[尝试 N]`/`[完成]` 日志确认行为。测完关掉代理（托盘菜单「退出代理」；或 macOS/Linux `Ctrl+C`、Windows `taskkill /F /IM proxy429.exe`）并关掉 mock。

## 观察限流

日志会打印每次重试（`[尝试 N] 上游响应状态码: 429` 接着 `→ 状态码 429，等待 ... 后重试`），跑一段时间就能看出这家提供商到底多爱 429。
