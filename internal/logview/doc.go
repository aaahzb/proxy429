package logview

// logViewerDocZH is the Chinese usage-doc body, rendered from docs/console-doc/doc-zh.html by
// tools/docgen (go run ./tools/docgen); edit the HTML fragment, not this file, and keep section
// ids and data-group boundaries in lockstep with logViewerDocEN. doc_test.go enforces that
// parity, the fragment/generated sync and the config-example key coverage.
const logViewerDocZH = `<div class="docHead">
  <div class="docTitle">使用文档</div>
  <input id="docSearch" class="docSearch" type="search" placeholder="搜索参数或功能（429、缓存、thinking…）" oninput="docFilter(this.value)" onkeydown="docSearchKey(event)">
  <span id="docSearchInfo" class="docSearchInfo" data-label="节匹配"></span>
  <button class="ghost" type="button" onclick="docClose()">关闭</button>
</div>
<div class="docMain">
  <nav class="docNav" id="docNav"></nav>
  <div class="docBody doc" id="docBody">
    <div class="docNoResult" id="docNoResult">没有匹配的章节。试试更短的关键词。</div>

    <section class="docSec" id="doc-what" data-group="开始使用">
      <h3>这是什么</h3>
      <p>Proxy429 是一个跑在本机的 Anthropic 兼容代理。AI 客户端把请求发给它，它再转发给一个或多个上游，并把过程完整显示在网页控制台里。</p>
      <p>它主要做三件事。</p>
      <ul>
        <li><b>扛住 429 和 5xx</b>：按指数退避自动重试，客户端通常感觉不到上游抖动。</li>
        <li><b>按模型名分流</b>：用 <code>routes</code> 把不同模型名的请求送到不同上游和不同 key。</li>
        <li><b>协议翻译</b>：把 Codex CLI 的 OpenAI Responses 请求翻译成 Anthropic Messages，让 Codex 也能用 Anthropic 上游。</li>
      </ul>
      <pre data-copy="复制" data-done="已复制">Claude Code ── Anthropic ──▶ 127.0.0.1:8080 ┐
Codex CLI   ── Responses ──▶ 127.0.0.1:8081 ┴─▶ routes / 兜底 ──▶ 上游</pre>
      <p>控制台地址是 <code>http://127.0.0.1:8080/__logs</code>，状态、日志和配置都在里面。所有监听口都只接受本机连接。</p>
    </section>

    <section class="docSec" id="doc-quickstart" data-group="开始使用">
      <h3>快速上手</h3>
      <ol class="docSteps">
        <li><b>启动代理</b>。Windows 双击 <code>proxy429.exe</code>；macOS 双击 <code>Proxy429.app</code>（首次被 Gatekeeper 拦截时右键选「打开」）；Linux 运行 <code>./proxy429</code>。启动后只有托盘或菜单栏图标，没有窗口。</li>
        <li><b>打开控制台</b>。点托盘菜单「查看日志」，或直接访问 <code>http://127.0.0.1:8080/__logs</code>。</li>
        <li><b>接 Claude Code</b>。在与 <code>claude</code> 同一个终端里执行：
          <pre data-copy="复制" data-done="已复制">export ANTHROPIC_BASE_URL="http://127.0.0.1:8080"
export ANTHROPIC_AUTH_TOKEN="your API key"
claude</pre>
          路由条目带 <code>api</code> 时，这里的 token 可以填任意非空串，真实 key 由代理注入。只认真实 <code>Authorization: Bearer</code> 的上游（如火山 ARK）必须用 <code>ANTHROPIC_AUTH_TOKEN</code>，不能用 <code>ANTHROPIC_API_KEY</code>，否则会 401。</li>
        <li><b>接 Codex CLI</b>。先在配置页确认 <code>responses_listen</code> 已启用，再到配置页底部的「Codex 一键配置」，复制对应系统的命令执行，然后重启 Codex。细节见「Codex 与 Responses 翻译」。</li>
      </ol>
      <p><b>验证</b>：状态页的「在途流」出现请求，日志页出现 <code>[route]</code> 行，就说明链路通了。</p>
      <p><b>配置文件在哪</b>：首次启动会在用户目录生成一份带占位 key 的配置。</p>
      <div class="docTableWrap"><table>
        <tr><th>平台</th><th>路径</th></tr>
        <tr><td>Windows</td><td><code>%AppData%\proxy429\config.json</code></td></tr>
        <tr><td>macOS</td><td><code>~/Library/Application Support/proxy429/config.json</code></td></tr>
        <tr><td>Linux</td><td><code>~/.config/proxy429/config.json</code></td></tr>
      </table></div>
      <p>启动目录下有 <code>./config.json</code> 时优先用它，调试很方便。也可以启动时加 <code>-config &lt;路径&gt;</code> 指定文件。日常修改建议走控制台的「配置」页。</p>
    </section>

    <section class="docSec" id="doc-console" data-group="开始使用">
      <h3>界面导航</h3>
      <p>顶部有「状态」「日志」「配置」三个标签。右侧是「文档」按钮和语言切换。关闭浏览器标签页只是隐藏控制台，代理继续运行。</p>
      <ul>
        <li><b>状态</b>：统计卡片、在途流、最近完成的流。点任意一行可以查看该流的请求体和输出。卡片和徽标的含义见「状态页与统计」和「徽标词典」。</li>
        <li><b>日志</b>：实时滚动日志。内存只保留最近 500 行，需要留存请配置 <code>log_file</code>。</li>
        <li><b>配置</b>：直接编辑 <code>config.json</code>，保存后立即生效。见「修改配置」。</li>
      </ul>
    </section>

    <section class="docSec" id="doc-config-edit" data-group="配置与路由">
      <h3>修改配置</h3>
      <p>配置页可以新建、重命名、删除和切换配置文件，文件列表每 3 秒自动刷新。当前生效的配置不能删除。 <button class="ghost" type="button" style="padding:2px 10px;font-size:12px" onclick="docOpenConfig()">打开配置页</button></p>
      <p><b>保存并重载</b>会先校验 JSON，再把文件写盘并立即生效。<b>仅重载</b>只是从磁盘重新读取，不改文件。校验不通过时不会写盘，错误会指出具体位置。</p>
      <p><code>listen</code> 只在启动时绑定。修改主监听端口后要退出并重启代理。<code>responses_listen</code> 例外，保存后监听口随配置即时启停。</p>
      <h4>共享变量 vars</h4>
      <p>多个路由用同一个 key 时，把 key 写进顶层 <code>vars</code>，路由里用 <code>$NAME</code> 或 <code>${NAME}</code> 引用。换 key 只改一处。</p>
      <pre data-copy="复制" data-done="已复制">"vars": { "DEEPSEEK_KEY": "sk-..." },
"routes": [
  { "pattern": "*opus*", "url": "https://api.deepseek.com", "api": "$DEEPSEEK_KEY", "model": "deepseek-v4-pro" }
]</pre>
      <ul>
        <li>文件里任意字符串值都可以引用变量，api 和 url 都行。文件保留引用原文，加载时再解析。</li>
        <li>引用未定义的变量会拒绝加载或保存，并给出路径，例如 <code>routes[0].api</code>。</li>
        <li>变量值必须是字符串，空串也算。变量可以再引用变量，只支持一层。</li>
        <li><code>$</code> 后面不是合法名字时原样保留，例如 <code>$5</code>。</li>
      </ul>
      <h4>预览 lazyApply</h4>
      <p>配置页的「预览 lazyApply 生效后 JSON」只读展示补完默认值后的配置，不会写盘。「复制到编辑框」把结果放回编辑器，仍需自己保存。规则见「全局默认 lazyApply」。</p>
      <p><b>访问控制</b>：转发口和管理端点（<code>/__*</code>）永远只接受本机连接。即使把 <code>listen</code> 写成 <code>0.0.0.0</code>，局域网设备也连不上，也看不到控制台。</p>
    </section>

    <section class="docSec" id="doc-routes" data-group="配置与路由">
      <h3>路由基础</h3>
      <p><code>routes</code> 按模型名通配匹配。<code>*</code> 匹配任意长度的任意字符，也可以匹配空串。命中后，<code>url</code>、<code>api</code>、<code>model</code> 三项会覆盖请求。</p>
      <pre data-copy="复制" data-done="已复制">"routes": [
  { "pattern": "*opus*",  "url": "https://upstream-a", "api": "sk-aaa", "model": "strong-model" },
  { "pattern": "*haiku*", "url": "https://upstream-b", "api": "sk-bbb", "model": "cheap-model" },
  { "pattern": "*",       "url": "https://upstream-a", "api": "sk-aaa", "model": "strong-model" }
]</pre>
      <ul>
        <li><b>顺序决定结果</b>：按数组顺序匹配，第一个命中的生效，没有「更具体优先」。<code>*opus*</code> 写在 <code>*opus-4*</code> 前面时，<code>claude-opus-4-8</code> 只会命中前者。具体规则写在前面，<code>*</code> 兜底放最后。</li>
        <li><b>pattern 可以写数组</b>：<code>"pattern": ["*fable*", "*opus*"]</code> 等价于把同一条路由复制多份，每份一个 pattern。其余字段共享，位置和数组顺序就是匹配顺序。空数组或非字符串成员会在加载时报错。</li>
        <li><b>保留名</b>：纯 <code>*</code> 兜底路由在 Codex 菜单里固定叫 <code>Fallback</code>，fast 通道叫 <code>fast_route</code>。任何 pattern 不能整串正好等于这两个名字，撞名的路由不生效也不进菜单；pattern 是数组时只禁用撞名的那一项，其余成员照常生效。<code>Fall*</code> 这类通配不受影响。</li>
        <li><b>effort 钉住思考档位</b>：路由写了 <code>effort</code>（<code>low</code>/<code>mid</code>/<code>high</code>/<code>max</code>，<code>mid</code> 按 <code>medium</code> 发给上游）时，在钉住点（路由落定后、搜索改写与原生 off-&gt;low 升级之前）请求体顶层 thinking 为 adaptive 的，代理把顶层 <code>output_config.effort</code> 强制改成该档位，客户端原本的档位被忽略；已经是该档位的不动也不出徽标。请求体不是 adaptive 形态（budget、关思考、没有 thinking 字段）时一个字节都不动，明确关思考永不被钉住（convertOff2Low 的静默升级在两个口都保持自己的 low），不写 <code>effort</code> 也完全不动。只有真正服务的路由算数：分类器、fast、图片和搜索兜底、Responses 透传各走各的路。状态页 API 列用青绿 <code>[from-&gt;to]</code> 徽标显示最后一次改写的前后值（更晚的搜索改写拥有徽标；400 兜底回关思考时徽标清空）。状态页模型列同时显示配置档位：<code>原始模型 -&gt; [档位] 目标模型</code>，模型未被改写时附在原名后成 <code>原始模型 [档位]</code>（如 <code>claude-fable -&gt; [high] deepseek-flash</code>）；档位用上游词汇（<code>mid</code> 显示为 <code>medium</code>），请求体未被改写也照样显示——是否真的改写过看 API 列徽标。顶层 <code>compactEffort</code>（<code>low</code>/<code>mid</code>/<code>high</code>/<code>max</code>）对上下文压缩请求（Claude Code 的 compact、Codex CLI 的 checkpoint compaction）做同样的钉档，而且优先级高于路由 <code>effort</code>：压缩请求一律用 <code>compactEffort</code> 的档位，路由钉住的档位在这些请求上让位；未设置时行为与原来完全一致。压缩流的模型列用 compactEffort 档位占据 <code>[档位]</code> 槽位（被覆盖的路由 <code>[档位]</code> 换成 compactEffort 档位，即 <code>原始模型 -&gt; [档位] 目标模型</code> 的同一位置），实际改写过同样看 API 列的 <code>[from-&gt;to]</code> 徽标。</li>
      </ul>
      <p>命中了哪条路由，看日志的 <code>[route]</code> 行，或状态页 model 列的 <code>[Pattern]</code>、<code>[分类器]</code>、<code>[Fast]</code>、<code>[多模态]</code>、<code>[搜索]</code> 前缀。</p>
    </section>

    <section class="docSec" id="doc-fallback" data-group="配置与路由">
      <h3>能力兜底</h3>
      <p>命中一个路由后，如果上游缺少请求需要的能力，代理会自动换到兜底上游。搜索和图片的处理顺序不同。</p>
      <ul>
        <li><b>搜索请求</b>（带 <code>web_search</code> 工具）：命中的路由标了 <code>no_search</code> 时，整条请求转给 <code>search_fallback</code>，由兜底上游自己搜索和回答。没配 <code>search_fallback</code> 就透传原路由，上游可能报错。</li>
        <li><b>图片请求</b>（不含搜索）：命中的路由标了 <code>text_only</code> 且请求里含图片时，转给 <code>multimodal_fallback</code>。没配就透传原路由，上游可能不认图片。</li>
        <li><b>搜索优先</b>：同时带搜索和图片的请求一律按搜索处理，走 <code>search_fallback</code>。</li>
      </ul>
      <p>兜底路由也使用 <code>url</code>/<code>api</code>/<code>model</code> 结构，并支持部分行为字段，例如 <code>thinking</code> 和 <code>convertOff2Low</code>。发生兜底后，生效的是兜底路由自己的字段；搜索类字段按逐字段规则覆盖，详见「参数总表」。</p>
      <p>想让支持搜索的上游自己做搜索加摘要，见「搜索增强与空搜守卫」。想把分类器请求或 fast 请求单独分流，见「专用路由」。</p>
    </section>

    <section class="docSec" id="doc-special" data-group="配置与路由">
      <h3>专用路由</h3>
      <h4>classifier_route</h4>
      <p>Claude Code 的安全判断和 Codex 的审批评估会发出特征请求。命中特征的请求无视原模型，直接走 <code>classifier_route</code>，适合甩给便宜模型，省主模型额度。</p>
      <ul>
        <li><code>classifier_thinking</code> 可选 <code>off</code>、<code>low</code> 或不写。不写就保持请求原样。<code>off</code> 把请求改写成关思考，分类返回更快。<code>low</code> 先关思考，再按 <code>convertOff2Low</code> 的机制悄悄升级成 low 发给上游，回传剥掉思考块，客户端无感。</li>
        <li><code>classifier_thinking_cc</code> 和 <code>classifier_thinking_codex</code> 按来源分别覆盖共享值。</li>
        <li>分类器请求的 max_tokens 很小（guardian 可能只有 64）。<code>low</code> 策略需要 1024 token 地板时，代理会在上游侧把 max_tokens 增加 1024 来补足，客户端无感。</li>
      </ul>
      <p>整体顺序：分类器特征请求先走 <code>classifier_route</code>，带 <code>"speed":"fast"</code> 的请求再走 <code>fast_route</code>，其余按 <code>routes</code> 通配匹配，最后落到 <code>upstream</code>。</p>
      <p>状态页的「分类器」卡片统计命中数，悬停看分来源明细，点击放大。</p>
      <h4>fast_route</h4>
      <p>请求带 <code>"speed":"fast"</code> 时走 <code>fast_route</code>。Claude Code 的 <code>/fast</code> 和 Codex 菜单里的 <code>fast_route</code> 项都会触发。命中后代理会给响应补上快速的限流头，让客户端保持快速模式。</p>
    </section>

    <section class="docSec" id="doc-lazyapply" data-group="配置与路由">
      <h3>全局默认 lazyApply</h3>
      <p><code>lazyApply</code> 是一个全局默认值包。它只给路由类对象本来就支持的字段补值，效果等同于把该键写进每个没有显式设置它的对象。它不会新增字段，也不会创建未配置的可选路由。</p>
      <ul>
        <li><b>逐字段优先级</b>：局部显式值（包括 <code>false</code> 和空串）&gt; <code>lazyApply</code> &gt; 内置默认。</li>
        <li><b>连接三件套可继承</b>：<code>url</code>、<code>api</code>、<code>model</code> 会从 lazyApply 继承（加载时写进每个省略该键的对象），所以上游只写一次，路由可以简化成 <code>{"pattern": "*sonnet*", "effort": "high"}</code>，<code>fast_route</code> 甚至可以写成 <code>{}</code>。局部写了的连接键仍然优先；写空串即停止继承：<code>"api": ""</code> 透传客户端 token、<code>"model": ""</code> 不改模型，<code>"url": ""</code> 表示不换上游——routes 条目照常生效并转发默认 upstream，classifier_route 只做思考改写，fast_route / multimodal_fallback / search_fallback 不启用、请求走正常路由。<code>pattern</code> 与 <code>url_response_api</code> 不继承，永远逐对象写。</li>
        <li><b>effort 只能逐路由写</b>：<code>routes[]</code> 的 <code>effort</code> 钉住不参与继承，lazyApply 不提供这个字段，避免一个全局默认把所有路由的档位一起钉死。顶层 <code>compactEffort</code> 是唯一的全局档位设置，但它只影响上下文压缩请求，不会碰其他流量。</li>
        <li><b>enhance_search 整体覆盖</b>：局部写了就整块生效，不会和 lazyApply 里的对象合并。</li>
        <li><b>个别对象可以退出</b>：在对象里写 <code>"noLazyApply": true</code>，它就不继承任何默认值，自己显式写的字段照常生效。</li>
        <li><b>会参与优先级链</b>：继承来的 <code>claudeSearch*</code> 视同局部写入，会压过 <code>search_fallback</code> 自己的值。想用 fallback 的值，就在该路由上写 <code>noLazyApply</code>。</li>
      </ul>
      <p>受支持的字段见「参数总表」的 lazyApply 一节。配置页的预览按钮可以只读核对展开结果。</p>
    </section>

    <section class="docSec" id="doc-retry" data-group="配置与路由">
      <h3>连接与重试</h3>
      <ul>
        <li><b>listen</b>：Anthropic 主监听地址，模板是 <code>127.0.0.1:8080</code>。只在启动时绑定。</li>
        <li><b>upstream</b>：默认上游。没命中任何路由时用它。如果配置了纯 <code>*</code> 兜底路由，可以不写。</li>
        <li><b>responses_listen</b>：OpenAI Responses 监听地址，空表示关闭。保存后立即启停。</li>
        <li><b>重试参数</b>：<code>max_retries</code> 是最大重试次数，不含首次尝试。<code>base_delay_s</code> 是首次退避秒数，之后按 2 的幂递增，并叠加最多 30% 的随机抖动。<code>max_delay_s</code> 是单次退避上限，写 0 会把退避截成 0，等于不等待。<code>total_budget_s</code> 是整条请求的重试总预算。<code>retry_status_codes</code> 列出触发重试的上游状态码，空数组等于不按状态码重试。</li>
        <li><b>respect_retry_after</b>：上游返回 <code>Retry-After</code> 头时优先按它等待。</li>
        <li><b>upstream_header_timeout_s</b>：等待上游首字节的上限。超时按内部重发处理，不写等于 70 秒。</li>
        <li><b>ping_interval_s</b>：重试等待期间发给客户端的 SSE 保活间隔，不写等于 5 秒。非流式请求不发送保活。</li>
      </ul>
      <p>重试用尽时，代理会向下游发一个 <code>overloaded_error</code> 事件。状态码列显示 <code>[重试尽]</code>；发生过退避重试的流显示 <code>[重试N次]</code>。</p>
      <p>日志相关：<code>log_request_detail</code> 记录请求体细节；<code>log_file</code> 把日志追加到磁盘文件；<code>recent_sample_window</code> 是状态行「首字」和「tok/s」的最近样本窗口，不写等于 20。</p>
    </section>

    <section class="docSec" id="doc-codex" data-group="进阶功能">
      <h3>Codex 与 Responses 翻译</h3>
      <p>把顶层 <code>responses_listen</code> 设为 <code>127.0.0.1:8081</code> 后，代理会在这个地址额外提供 <code>/v1/responses</code>。Codex CLI 只懂 OpenAI Responses 协议，请求会先翻译成 Anthropic Messages，走和 Claude Code 完全相同的主管线，再把响应翻译回 Responses。</p>
      <h4>接入 Codex</h4>
      <p>最省事的方式是配置页底部的「Codex 一键配置」。两行命令按上方编辑框实时生成，自动备份并改写 <code>~/.codex/config.toml</code>，还会写模型目录。下拉选「（还原默认 Codex 配置）」得到的命令可以撤销。改完重启 Codex。</p>
      <p>手动配置如下。</p>
      <pre data-copy="复制" data-done="已复制">model_provider = "proxy429"
model = "gpt-5-codex"
forced_login_method = "api"

[model_providers.proxy429]
name = "Proxy429 Local Proxy"
base_url = "http://127.0.0.1:8081/v1"
wire_api = "responses"
experimental_bearer_token = "proxy429"</pre>
      <p><code>base_url</code> 必须带 <code>/v1</code>，<code>wire_api</code> 必须是 <code>responses</code>。token 只是占位，代理不校验，真实 key 由命中路由的 <code>api</code> 注入。</p>
      <p><b>Codex 报 503 且代理侧零日志</b>：系统代理把发往 127.0.0.1 的请求劫走了。让 <code>NO_PROXY</code> 包含 <code>127.0.0.1</code> 后重启 Codex。Windows 和 macOS 的一键脚本会自动写入并持久化，Linux 脚本会给出提示。</p>
      <h4>模型名与思考映射</h4>
      <p>Codex 发来的 model 名照常参与 routes 匹配。思考形态的查表用的是客户端原始 model 名，发生在路由改写之前。想让映射表生效，就把 Codex 的 model 直接填成目标模型名。</p>
      <p>模型分两类。adaptive 类按 <code>thinking:adaptive</code> 加 <code>output_config.effort</code> 表达思考，包括 <code>fable-5</code>、<code>mythos-5</code>、<code>mythos-preview</code>、<code>sonnet-5</code>、<code>opus-4-8</code>、<code>opus-4-7</code>、<code>opus-4-6</code>、<code>sonnet-4-6</code>。其余模型按 <code>budget_tokens</code> 表达。</p>
      <div class="docTableWrap"><table>
        <tr><th>Codex reasoning.effort</th><th>普通模型 → budget_tokens</th><th>adaptive 模型 → effort</th></tr>
        <tr><td>minimal / low</td><td>2048</td><td>low</td></tr>
        <tr><td>medium</td><td>8192</td><td>medium</td></tr>
        <tr><td>high</td><td>16384</td><td>high</td></tr>
        <tr><td>xhigh / max / ultra</td><td>24576</td><td>max</td></tr>
        <tr><td>none / off / disabled</td><td>关思考</td><td>能关的关思考；fable-5、mythos-5 改为 adaptive + low</td></tr>
        <tr><td>不带该字段</td><td>关思考</td><td>fable-5、mythos-5、mythos-preview、sonnet-5 默认开启；其余关</td></tr>
      </table></div>
      <p>识别不了的值按「不带该字段」处理。budget_tokens 还有两道保护：不超过 <code>max_tokens</code> 的一半，封顶后低于 1024 就不开启。<code>max_output_tokens</code> 默认 32000。</p>
      <p>当别名命中了 adaptive 表，实际路由目标却只支持 budget（例如别名叫 <code>claude-fable-5</code>，实际接到 Kimi），在该路由条目上写 <code>"thinking": "budget"</code> 或 <code>"adaptive"</code> 纠正。这个字段只对翻译流生效。</p>
      <h4>工具映射</h4>
      <div class="docTableWrap"><table>
        <tr><th>Responses 侧</th><th>Anthropic 侧</th></tr>
        <tr><td>function 工具</td><td>tools[]，保留 name、description、input_schema</td></tr>
        <tr><td>custom 或字符串工具</td><td>包成单参数 {input:string}，定义内联进描述；回传为 custom_tool_call</td></tr>
        <tr><td>namespace（MCP）工具</td><td>拆成 ns__name 子工具，超长时截断加哈希；回放时还原</td></tr>
        <tr><td>tool_search</td><td>代理固定工具（query、limit）</td></tr>
        <tr><td>web_search / web_search_preview</td><td>web_search_20250305</td></tr>
        <tr><td>input_file</td><td>document 块（URL 或 base64）</td></tr>
        <tr><td>input_image</td><td>image 块（base64 或 URL）</td></tr>
        <tr><td>function_call_output 等工具结果</td><td>tool_result 块，图片拆成原生图片块，超长 base64 截断</td></tr>
        <tr><td>parallel_tool_calls: false</td><td>tool_choice.disable_parallel_tool_use</td></tr>
      </table></div>
      <h4>思考、历史与信封</h4>
      <ul>
        <li>开着思考时，工具续轮的历史必须带可回放的签名思考块。<code>allowNoThinkBlock4Anthropic</code> 默认 true：先按所请档位照发，真被上游 400 才自动关思考重试一次，并且不剥思考块，让下一轮历史自愈。写 false 则一开始就关思考。</li>
        <li>强制 <code>tool_choice</code> 与思考互斥。能关思考的模型会关掉这次请求的思考并保留工具约束；<code>fable-5</code>、<code>mythos-5</code> 关不掉，直接报错。</li>
        <li>思考块和每次搜索的完整结果会被封进响应里的信封，下一轮客户端回放时还原，思考链和搜索正文都能继续用。搜索信封按上游和 key 混淆，换 key 或换上游后自动失效，客户端历史里不出现明文 URL 或 key。</li>
        <li>旧对话的搜索块在上游过期时，代理会剥掉回放块并重试一次，无感降级为重新搜索。剥离数量显示为 <code>[剥N]</code>。</li>
      </ul>
    </section>

    <section class="docSec" id="doc-search" data-group="进阶功能">
      <h3>搜索增强与空搜守卫</h3>
      <p>Claude Code 每次网页搜索会单独起一个子代理请求。<b>独立搜索请求</b>的判定标准是：最后一条 user 消息以 <code>Perform a web search for the query</code> 开头。只看最后一条，历史里引用这句话不算。命中判定的请求在 model 列显示青绿色 <code>[Search]</code>。</p>
      <h4>让上游自己写摘要：enhance_search 与 claudeSearchSummary</h4>
      <p><code>enhance_search</code> 配在 routes 条目上，并且该路由没有标 <code>no_search</code> 时生效。代理把摘要指令注入搜索请求本身，仍走该路由自己的上游，一次请求完成搜索和逐条摘要，响应原样透传。旧的 <code>summary_level</code> 控制详细度（low、mid、high、max），<code>summary_thinking: true</code> 把思考改成 low。</p>
      <p>新字段更灵活，可以挂在 routes 条目或 <code>search_fallback</code> 上。</p>
      <ul>
        <li><code>claudeSearchSummary</code>：<code>forceLow</code>、<code>forceMid</code>、<code>forceHigh</code> 按档注入摘要指令，<code>noForce</code> 不注入。设置后压过 <code>enhance_search</code> 的旧字段。</li>
        <li><code>claudeSearchThinking</code>：<code>off</code>、<code>low</code>、<code>mid</code>、<code>high</code>、<code>max</code> 改写这条搜索请求的思考档位。客户端本来没开思考时，回传的思考块会被剥掉，客户端看到的仍是原来的形态。</li>
      </ul>
      <p>注入摘要后，徽标从 <code>[Search]</code> 升级为 <code>[SearchSummary-档位]</code>。思考被改写时，API 列额外显示 <code>[原-&gt;新]</code>。</p>
      <p><code>search_fallback</code> 自身还保留旧字段：<code>summary_mode: true</code> 对该兜底路由的独立搜索请求注入摘要，档位取 <code>summary_level</code>（默认 low）；<code>summary_thinking: true</code> 把思考改成 low。对应设置了 <code>claudeSearchSummary</code> 或 <code>claudeSearchThinking</code> 时以新字段为准。</p>
      <p><b>优先级</b>：请求由路由自己服务时，用路由自己的值。整条请求转给 <code>search_fallback</code> 时，路由显式设置的字段逐字段压过 fallback，没设置的字段用 fallback 的值。</p>
      <h4>空搜守卫：noBullshitOnEmptyClaudeSearch</h4>
      <p>配在 routes 条目或 <code>search_fallback</code> 上，默认关闭。开启后，独立搜索请求如果返回了搜索结果块，但每条结果的内容都是空数组，代理会丢掉上游的整个回答（包括思考过程和编造正文），改发一段「没有搜到，请换关键词重试」的完整流。结果非空或形状不认识时保守放行。</p>
      <p>只对 Anthropic 原生口的独立搜索请求生效，不影响 Responses 流量。开启后，健康的上游 200 流也会缓冲到上游结束才转发。触发时日志记 <code>[search-guard]</code>，完成流状态码列显示红色 <code>[空搜]</code>。</p>
    </section>

    <section class="docSec" id="doc-thinking" data-group="进阶功能">
      <h3>思考改写 convertOff2Low</h3>
      <p>有些上游在关思考时会改用无思考的旧模型，例如 Kimi 文档里的 K2.8 Preview。把「关」悄悄升级成 low，可以让请求留在强模型上。<code>convertOff2Low</code> 就是做这件事的开关。</p>
      <ul>
        <li>取值：<code>"translate"</code> 只作用于 Responses 翻译口；<code>"all"</code> 同时作用于翻译口和 Anthropic 原生口；不写等于关闭。</li>
        <li>做法：下游明确关思考时，代理按 low 发给上游。adaptive 模型发 <code>adaptive</code> 加 <code>effort: low</code>；budget 模型发 <code>budget_tokens: 2048</code>，并受 max_tokens 一半和 1024 地板约束。回传时剥掉思考块，客户端看到的仍是关思考，usage 如实透传。</li>
        <li>放弃升级的情况：budget 模型连 1024 都放不下，或者请求强制了与思考互斥的 <code>tool_choice</code>。</li>
        <li>上游拒绝思考时，自动回退为关思考并重试一次。</li>
        <li>生效路由：翻译口按客户端 model 名预匹配路由；原生口按最终生效的路由，发生图片或搜索兜底交换后取兜底路由的值。</li>
      </ul>
      <p>徽标 <code>[off-&gt;low]</code> 双色显示在 API 列，前一段是下游口径，后一段是上游口径。历史兜底按所请档位如实显示，不加徽标。</p>
      <p><code>allowNoThinkBlock4Anthropic</code> 决定工具续轮历史缺少签名思考块时的策略：默认 true 先照发，被 400 才自动关思考重试一次；false 则一开始就关。显式关思考的请求不受这个开关影响，仍由 <code>convertOff2Low</code> 管。</p>
    </section>

    <section class="docSec" id="doc-streamify" data-group="进阶功能">
      <h3>全局流式化 convertAlltoStream</h3>
      <p>开启 <code>convertAlltoStream</code>（默认 false）后，所有非流式 Anthropic Messages 请求都会被代理悄悄改成流式发上游。代理收完整条流后，把内容块原样拼回非流式 JSON，一次性返回给客户端。调用方拿到的仍是它预期的非流式响应。</p>
      <p>好处是分类器这类一次性请求也能在控制台看到实时吐字、首字和 tok/s。</p>
      <ul>
        <li>只作用于 <code>/v1/messages</code>，已经是流式的请求不受影响。</li>
        <li>流中途断开（没见到 <code>message_stop</code>）时，代理还没向客户端写任何内容，可以整体重试。</li>
        <li>重试等待期间不发送 SSE 保活，避免污染非流式响应。</li>
      </ul>
    </section>

    <section class="docSec" id="doc-params" data-group="参数参考">
      <h3>参数总表</h3>
      <p>下面按配置对象列出全部键。类型里的 number 指 JSON 数字，可以是整数或小数。默认值只写内置默认；没有内置默认的键，含义由模板给的示例值说明。</p>
      <h4>顶层</h4>
      <div class="docTableWrap"><table>
        <tr><th>键</th><th>类型</th><th>默认</th><th>说明</th></tr>
        <tr><td><code>listen</code></td><td>string</td><td>127.0.0.1:8080（模板）</td><td>主监听地址。只在启动时绑定</td></tr>
        <tr><td><code>upstream</code></td><td>string</td><td>空</td><td>默认上游，没命中路由时使用</td></tr>
        <tr><td><code>vars</code></td><td>object</td><td>无</td><td>命名字符串变量，供 $NAME 和 ${NAME} 引用</td></tr>
        <tr><td><code>max_retries</code></td><td>number</td><td>0</td><td>最大重试次数，不含首次；0 等于只试一次</td></tr>
        <tr><td><code>base_delay_s</code></td><td>number</td><td>0</td><td>首次退避秒数，之后按 2 的幂递增</td></tr>
        <tr><td><code>max_delay_s</code></td><td>number</td><td>0</td><td>单次退避上限；0 等于不等待</td></tr>
        <tr><td><code>total_budget_s</code></td><td>number</td><td>0</td><td>整条请求的重试总预算秒数</td></tr>
        <tr><td><code>retry_status_codes</code></td><td>number[]</td><td>空数组</td><td>触发重试的上游状态码；空等于不按状态码重试</td></tr>
        <tr><td><code>respect_retry_after</code></td><td>bool</td><td>false</td><td>优先遵守上游的 Retry-After 头</td></tr>
        <tr><td><code>upstream_header_timeout_s</code></td><td>number</td><td>70</td><td>等待上游首字节的上限；0 也按 70 秒</td></tr>
        <tr><td><code>ping_interval_s</code></td><td>number</td><td>5</td><td>重试期间的 SSE 保活间隔；0 也按 5 秒</td></tr>
        <tr><td><code>log_request_detail</code></td><td>bool</td><td>false</td><td>日志记录请求体细节</td></tr>
        <tr><td><code>log_file</code></td><td>string</td><td>空</td><td>日志追加到该文件</td></tr>
        <tr><td><code>recent_sample_window</code></td><td>number</td><td>20</td><td>首字与 tok/s 的最近样本数；0 也按 20</td></tr>
        <tr><td><code>convertAlltoStream</code></td><td>bool</td><td>false</td><td>全局流式化，见同名小节</td></tr>
        <tr><td><code>compactEffort</code></td><td>string</td><td>空</td><td>low、mid、high 或 max：上下文压缩请求的思考档位，优先级高于路由 effort；模型列在 [档位] 槽位显示，见「路由」一节的 effort 说明</td></tr>
        <tr><td><code>responses_listen</code></td><td>string</td><td>空</td><td>Responses 监听口；空等于关闭</td></tr>
        <tr><td><code>allowNoThinkBlock4Anthropic</code></td><td>bool</td><td>true</td><td>历史缺签名思考块时，先照发再兜底</td></tr>
        <tr><td><code>routes</code></td><td>array</td><td>空</td><td>模型名通配的路由表，条目字段见下节</td></tr>
        <tr><td><code>fast_route</code></td><td>object</td><td>空</td><td>快速通道路由，字段见后文同名表格</td></tr>
        <tr><td><code>classifier_route</code></td><td>object</td><td>空</td><td>分类器专用路由，字段见后文同名表格</td></tr>
        <tr><td><code>multimodal_fallback</code></td><td>object</td><td>空</td><td>图片兜底路由，字段与 fast_route 相同</td></tr>
        <tr><td><code>search_fallback</code></td><td>object</td><td>空</td><td>搜索兜底路由，字段见后文同名表格</td></tr>
        <tr><td><code>lazyApply</code></td><td>object</td><td>无</td><td>共享默认值，字段见后文同名表格</td></tr>
      </table></div>
      <h4>routes[] 条目</h4>
      <div class="docTableWrap"><table>
        <tr><th>键</th><th>类型</th><th>默认</th><th>说明</th></tr>
        <tr><td><code>pattern</code></td><td>string 或 string[]</td><td>必填</td><td>模型名通配，* 匹配任意长度；数组表示或</td></tr>
        <tr><td><code>url</code></td><td>string</td><td>空</td><td>上游 base URL；省略时继承 lazyApply.url，仍为空 = 不换上游：该条目其余字段照常生效，请求转发默认 upstream</td></tr>
        <tr><td><code>api</code></td><td>string</td><td>空</td><td>上游 key，以 Authorization: Bearer 发送；空等于透传客户端 token</td></tr>
        <tr><td><code>model</code></td><td>string</td><td>空</td><td>改写后的模型名；空等于不改</td></tr>
        <tr><td><code>text_only</code></td><td>bool</td><td>false</td><td>上游不支持图片，图片请求转 multimodal_fallback</td></tr>
        <tr><td><code>no_search</code></td><td>bool</td><td>false</td><td>上游不支持搜索，搜索请求转 search_fallback</td></tr>
        <tr><td><code>thinking</code></td><td>string</td><td>auto</td><td>auto、adaptive 或 budget；只作用于 Responses 翻译口</td></tr>
        <tr><td><code>effort</code></td><td>string</td><td>空</td><td>low、mid、high 或 max；adaptive 请求的 output_config.effort 强制成该档，已在同档不改，空等于不改；状态页模型列以 [档位] 标签显示配置的档位</td></tr>
        <tr><td><code>convertOff2Low</code></td><td>string</td><td>空</td><td>空、translate 或 all</td></tr>
        <tr><td><code>enhance_search</code></td><td>object</td><td>无</td><td>摘要注入，子字段 summary_thinking 和 summary_level</td></tr>
        <tr><td><code>claudeSearchSummary</code></td><td>string</td><td>空</td><td>forceLow、forceMid、forceHigh 或 noForce</td></tr>
        <tr><td><code>claudeSearchThinking</code></td><td>string</td><td>空</td><td>off、low、mid、high 或 max</td></tr>
        <tr><td><code>noBullshitOnEmptyClaudeSearch</code></td><td>bool</td><td>false</td><td>空搜守卫</td></tr>
        <tr><td><code>noLazyApply</code></td><td>bool</td><td>false</td><td>不继承 lazyApply</td></tr>
      </table></div>
      <h4>classifier_route</h4>
      <div class="docTableWrap"><table>
        <tr><th>键</th><th>类型</th><th>默认</th><th>说明</th></tr>
        <tr><td><code>url</code></td><td>string</td><td>空</td><td>上游 base URL；省略时继承 lazyApply.url，仍为空 = 不换上游，只做思考改写</td></tr>
        <tr><td><code>api</code></td><td>string</td><td>空</td><td>上游 key；空等于透传客户端 token</td></tr>
        <tr><td><code>model</code></td><td>string</td><td>空</td><td>改写后的模型名</td></tr>
        <tr><td><code>classifier_thinking</code></td><td>string</td><td>空</td><td>空、off 或 low；见「专用路由」</td></tr>
        <tr><td><code>classifier_thinking_cc</code></td><td>string</td><td>空</td><td>Claude Code 来源的覆盖值</td></tr>
        <tr><td><code>classifier_thinking_codex</code></td><td>string</td><td>空</td><td>Codex 来源的覆盖值</td></tr>
        <tr><td><code>noLazyApply</code></td><td>bool</td><td>false</td><td>不继承 lazyApply</td></tr>
      </table></div>
      <h4>fast_route 与 multimodal_fallback</h4>
      <div class="docTableWrap"><table>
        <tr><th>键</th><th>类型</th><th>默认</th><th>说明</th></tr>
        <tr><td><code>url</code></td><td>string</td><td>空</td><td>上游 base URL；省略时继承 lazyApply.url，仍为空 = 该路不启用，请求走正常路由</td></tr>
        <tr><td><code>api</code></td><td>string</td><td>空</td><td>上游 key；空等于透传客户端 token</td></tr>
        <tr><td><code>model</code></td><td>string</td><td>空</td><td>改写后的模型名</td></tr>
        <tr><td><code>convertOff2Low</code></td><td>string</td><td>空</td><td>空、translate 或 all</td></tr>
        <tr><td><code>noLazyApply</code></td><td>bool</td><td>false</td><td>不继承 lazyApply</td></tr>
      </table></div>
      <h4>search_fallback</h4>
      <div class="docTableWrap"><table>
        <tr><th>键</th><th>类型</th><th>默认</th><th>说明</th></tr>
        <tr><td><code>url</code></td><td>string</td><td>空</td><td>上游 base URL；省略时继承 lazyApply.url，仍为空 = 不启用，需要兜底的请求退回原路由</td></tr>
        <tr><td><code>api</code></td><td>string</td><td>空</td><td>上游 key；空等于透传客户端 token</td></tr>
        <tr><td><code>model</code></td><td>string</td><td>空</td><td>改写后的模型名</td></tr>
        <tr><td><code>thinking</code></td><td>string</td><td>auto</td><td>auto、adaptive 或 budget；只作用于 Responses 翻译口</td></tr>
        <tr><td><code>convertOff2Low</code></td><td>string</td><td>空</td><td>空、translate 或 all</td></tr>
        <tr><td><code>summary_mode</code></td><td>bool</td><td>false</td><td>旧字段：注入摘要指令</td></tr>
        <tr><td><code>summary_thinking</code></td><td>bool</td><td>false</td><td>旧字段：把搜索请求的思考改成 low</td></tr>
        <tr><td><code>summary_level</code></td><td>string</td><td>low</td><td>旧字段：摘要详细度 low、mid、high 或 max</td></tr>
        <tr><td><code>claudeSearchThinking</code></td><td>string</td><td>空</td><td>off、low、mid、high 或 max</td></tr>
        <tr><td><code>claudeSearchSummary</code></td><td>string</td><td>空</td><td>forceLow、forceMid、forceHigh 或 noForce</td></tr>
        <tr><td><code>noBullshitOnEmptyClaudeSearch</code></td><td>bool</td><td>false</td><td>空搜守卫</td></tr>
        <tr><td><code>noLazyApply</code></td><td>bool</td><td>false</td><td>不继承 lazyApply</td></tr>
      </table></div>
      <h4>lazyApply</h4>
      <div class="docTableWrap"><table>
        <tr><th>键</th><th>类型</th><th>作用对象</th></tr>
        <tr><td><code>text_only</code></td><td>bool</td><td>routes[]</td></tr>
        <tr><td><code>no_search</code></td><td>bool</td><td>routes[]</td></tr>
        <tr><td><code>thinking</code></td><td>string</td><td>routes[] 与 search_fallback</td></tr>
        <tr><td><code>convertOff2Low</code></td><td>string</td><td>所有支持该字段的路由类对象</td></tr>
        <tr><td><code>enhance_search</code></td><td>object</td><td>routes[]</td></tr>
        <tr><td><code>summary_mode</code></td><td>bool</td><td>search_fallback</td></tr>
        <tr><td><code>summary_thinking</code></td><td>bool</td><td>search_fallback</td></tr>
        <tr><td><code>summary_level</code></td><td>string</td><td>search_fallback</td></tr>
        <tr><td><code>claudeSearchThinking</code></td><td>string</td><td>routes[] 与 search_fallback</td></tr>
        <tr><td><code>claudeSearchSummary</code></td><td>string</td><td>routes[] 与 search_fallback</td></tr>
        <tr><td><code>noBullshitOnEmptyClaudeSearch</code></td><td>bool</td><td>routes[] 与 search_fallback</td></tr>
        <tr><td><code>classifier_thinking</code></td><td>string</td><td>classifier_route</td></tr>
        <tr><td><code>classifier_thinking_cc</code></td><td>string</td><td>classifier_route</td></tr>
        <tr><td><code>classifier_thinking_codex</code></td><td>string</td><td>classifier_route</td></tr>
        <tr><td><code>url</code></td><td>string</td><td>routes[]、classifier_route、fast_route、multimodal_fallback 与 search_fallback</td></tr>
        <tr><td><code>api</code></td><td>string</td><td>routes[]、classifier_route、fast_route、multimodal_fallback 与 search_fallback</td></tr>
        <tr><td><code>model</code></td><td>string</td><td>routes[]、classifier_route、fast_route、multimodal_fallback 与 search_fallback</td></tr>
      </table></div>
    </section>

    <section class="docSec" id="doc-status" data-group="控制台参考">
      <h3>状态页与统计</h3>
      <p>状态页顶部是统计卡片：活跃/等待、发给上游、上游返回、缓存命中、输入（未命中）、输出、重试、分类器、首字、tok/s。在途流和最近完成流各一张表，点行查看内容。</p>
      <h4>指标定义</h4>
      <ul>
        <li><b>总时间</b>：从代理收到下游请求，到把响应全部发回下游。包含内部处理、首字等待、吐字和收尾。</li>
        <li><b>首字</b>：从发出请求到收到首个输出字节的耗时。</li>
        <li><b>tok/s</b>：输出 token 数除以流式耗时。</li>
        <li><b>token 入/出</b>：该流的总输入 / 输出 tokens，格式 <code>107k/4</code>。输入 = input + cache_read + cache_creation；在途时显示头部/尾部已到达的数值，完成后才准确；未收到的一侧显示 <code>-</code>，例如被下游取消、只拿到头部的流显示 <code>107k/-</code>。在途流该列位于 API 和「字节」之间；最近完成流用它替换「字节」列，鼠标悬停单元格显示该流字节数。</li>
        <li><b>缓存命中</b>：<code>cache_read / (input + cache_read + cache_creation)</code>。缓存写入不算命中，但计入总输入。高命中率主要来自上游模型的原生上下文缓存，代理只透传 usage。中断的流不参与缓存命中聚合，避免把半截上下文算成低命中。</li>
        <li><b>输入（未命中）</b>：只统计未命中缓存的新鲜 input token（中断的流不计入）；总输入 = 未命中 + 缓存读取 + 缓存写入，与各流 token 列「入」同构成。悬停卡片可见该说明。</li>
        <li><b>缓存年龄</b>：按会话和路由锚定，显示最近一次缓存写入距现在多久。同会话同路由有流正在吐字时，最新完成行冻结为方括号形式，表示缓存刚被刷新。开始时刻在 5 分钟内的最新行不会被「保留完成流 N」挤掉。没有会话标识的请求恒显示 <code>-</code>。</li>
        <li><b># 列</b>：<code>#80&lt;-73</code> 表示 80 号流的请求延续自 73 号流。同一个会话、且 73 号的消息链是 80 号的前缀时才会标注。裸 <code>#N</code> 表示没有父流，例如子代理首轮或 <code>/clear</code> 之后。</li>
      </ul>
      <p>点击「缓存命中」卡片，弹窗按真实上游模型拆分明细。底部「实测缓存时间」表按 URL 和模型归组，给出缓存存活时间的下界（≥）、上界（&lt;）、各界的形成时间和观测次数。输入不足 1024 token 的流不参与观测。</p>
      <p>点击「分类器」卡片看命中与改写明细，按 Claude Code 和 Codex 分列。</p>
      <p>「活跃度」打开最近 48 小时的热图，每格 15 分钟，可以切换按请求次数、输入+输出或输出着色。热图保存在配置文件旁的 <code>heatmap.dat</code>，重启不丢。</p>
      <p>「清空统计」清空累计统计、最近完成流和活跃度热图，不清在途流和编号，也不清日志。「保留完成流」控制列表行数，默认 10，最大 200。</p>
    </section>

    <section class="docSec" id="doc-badges" data-group="控制台参考">
      <h3>徽标词典</h3>
      <div class="docTableWrap"><table>
        <tr><th>徽标</th><th>位置</th><th>含义</th></tr>
        <tr><td><code>[Anthropic]</code> / <code>[translate]</code></td><td>API 列</td><td>协议来源：原生 Anthropic 口 / Responses 口翻译</td></tr>
        <tr><td><code>off</code> / <code>on N</code> / <code>adaptive</code> / <code>low</code> 等</td><td>API 列</td><td>实际发给上游的思考形态（行内就显示这组英文值）。名字是协议来源，值的颜色是 Anthropic 口径</td></tr>
        <tr><td><code>[off-&gt;low]</code></td><td>API 列</td><td>关思考被悄悄升级为 low，见「思考改写」</td></tr>
        <tr><td><code>[档位]</code></td><td>model 列</td><td>路由配置的 <code>effort</code> 钉住档位：显示成 <code>原始模型 -&gt; [档位] 目标模型</code>（模型未被改写时为 <code>原始模型 [档位]</code>），上游词汇（<code>mid</code> 显示为 <code>medium</code>）；请求体没被改写也显示，是否实际改写看 API 列的 <code>[from-&gt;to]</code> 徽标；配置了 compactEffort 的压缩流上换成 compactEffort 的档位（同一槽位，被覆盖的路由档位不显示）</td></tr>
        <tr><td><code>[from-&gt;to]</code></td><td>API 列</td><td>青绿色的请求体思考改写：路由 <code>effort</code> 钉住、压缩请求的 <code>compactEffort</code> 钉住或搜索子代理改写，显示最后一次改写的前后值（更晚的改写重新取前后值）；<code>from</code> 为空表示请求本来没有 thinking 字段（显示 auto）</td></tr>
        <tr><td><code>[Search]</code> / <code>[SearchSummary-档位]</code></td><td>model 列</td><td>独立搜索请求 / 已注入摘要</td></tr>
        <tr><td><code>[Compact]</code> / <code>[Compact 88%]</code></td><td>model 列</td><td>客户端上下文压缩请求：最后一条 user 消息以客户端压缩提示词开头（Claude Code 的 compact、Codex CLI 的 checkpoint compaction）时标记；历史里只是引用这些提示词不算。完成流把被压掉的比例折进徽标（如 <code>[Compact 88%]</code>），悬停显示完整结果 <code>170k-&gt;20k, 88%</code>：左侧是压缩前的总输入上下文（input + cache_read + cache_creation），右侧是摘要输出的 token 数；用量未解析时只显示 <code>[Compact]</code>。压缩请求的档位显示在模型列的 <code>[档位]</code> 槽位（见上），请求体实际被改写时另有 API 列的 <code>[from-&gt;to]</code> 徽标</td></tr>
        <tr><td><code>[Read*1]</code> <code>[Edit*3]</code></td><td>model 列</td><td>本次响应里的工具调用次数，按首次出现排序。空参数的单次调用显示 *0</td></tr>
        <tr><td><code>[count_tokens]</code></td><td>model 列</td><td>Claude Code 的 token 计数探针，响应只有 input_tokens，标记以免看着像空响应；不计入活跃度热图</td></tr>
        <tr><td><code>[剥N]</code></td><td>model 列或缓存命中列</td><td>剥掉的回放搜索块数量</td></tr>
        <tr><td><code>[重试N次]</code></td><td>状态码后</td><td>发生过 N 次退避重试</td></tr>
        <tr><td><code>[重试尽]</code></td><td>状态码列</td><td>重试或预算用尽，已向下游回传 overloaded_error</td></tr>
        <tr><td><code>[无stop]</code></td><td>状态码后</td><td>上游 200 流提前结束，没有终止事件</td></tr>
        <tr><td><code>[空搜]</code></td><td>状态码列</td><td>空搜守卫触发</td></tr>
        <tr><td><code>[尝试N: Xs]</code> / <code>[退避中]</code></td><td>在途流</td><td>当前尝试已等待的时长 / 正在退避</td></tr>
        <tr><td>状态灯 ⚪🟡🟢</td><td>在途流首列</td><td>请求 / 等首字节 / 转发中。旁边的秒数只在变色时清零</td></tr>
      </table></div>
      <p>状态码不是 200 时加方括号显示，例如 <code>[499]</code>、<code>[400]</code>。499 表示上游还没发完连接就结束了，最常见是下游主动取消；上游发完后下游才断开的仍记 200。</p>
    </section>

    <section class="docSec" id="doc-viewer" data-group="控制台参考">
      <h3>流查看器</h3>
      <p>点击在途流或最近完成流的任意一行，下方打开流查看器。内容按钮循环四种状态：请求体 → 输出[渲染] → 输出[流式原始] → 输出[非流式]。</p>
      <ul>
        <li><b>渲染</b>：把 SSE 事件流解析成可读文本。</li>
        <li><b>流式原始</b>：逐字显示 SSE 原文。</li>
        <li><b>非流式</b>：把事件流拼成最终 JSON。流没完整到达时会注明是部分快照。此状态下「交互式JSON」直接对拼好的对象成树。</li>
        <li><b>请求体</b>：复现产生这条流的请求。JSON 自动美化。</li>
      </ul>
      <p><b>链路两侧</b>：「链路」按钮在「代理↔上游」和「下游↔代理」之间切换。默认看代理实际发给上游的请求体和上游回来的原始流。只有两侧有差异的流才双存，例如翻译流和模型改写流。切到没有记录的一侧会自动回退，并在按钮旁提示。</p>
      <p>浏览最多显示前 256KB。勾选「储存完整结构体」后，新开始的请求会额外记录完整请求体和输出（不设上限，占内存），查看器出现下载按钮。取消勾选会立即清空已存的完整副本。该开关重启后复位为关闭。</p>
      <p>「交互式JSON」始终可用，不需要勾选储存。它把当前内容变成可折叠的 JSON 树，第一层默认展开，更深的层级点击懒展开，并跟随链路侧切换。数据残缺的流不会被冒充成完整版，下载按钮会置灰并说明原因。</p>
      <p>「自动跟踪最新」开启时，查看器跟随最新的在途流。「最多显示」控制并排显示的在途流数量。</p>
    </section>

    <section class="docSec" id="doc-faq" data-group="排障">
      <h3>常见问题</h3>
      <ul>
        <li><b>Claude Code 没走代理</b>：环境变量必须和 <code>claude</code> 在同一个终端里 export。用 <code>echo $ANTHROPIC_BASE_URL</code> 确认输出 <code>http://127.0.0.1:8080</code>。</li>
        <li><b>改了端口不生效</b>：<code>listen</code> 只在启动时绑定。退出代理再启动。<code>responses_listen</code> 支持热启停。</li>
        <li><b>控制台打不开</b>：多半是主端口被占用或配置有错。直接编辑配置文件修正 <code>listen</code> 后重启。排查启动错误可以临时设置 <code>"log_file": "proxy.log"</code>。</li>
        <li><b>看不到重试</b>：日志页应出现 <code>[attempt N] upstream response status: 429</code> 和 <code>[retry]</code> 行。没有这些行说明状态码不在 <code>retry_status_codes</code> 里，或重试预算已经用完。确定性 4xx（如 400 <code>invalid_request_error</code>：坏 JSON、未签名思考历史）不重试——语义兜底各试一次后直接带原错误体透传（keepalive 流已开时错误详情随 SSE 错误事件下发，状态列记真实状态码而不是 [重试尽]）；408/425/429 与 5xx 即使不在列表里也仍按瞬时错误走体内重试。</li>
        <li><b>启动后没有窗口</b>：正常。程序常驻托盘或菜单栏，从图标菜单打开控制台。</li>
        <li><b>Codex 报 503 且代理零日志</b>：系统代理劫持了 127.0.0.1。把 <code>127.0.0.1</code> 加进 <code>NO_PROXY</code>，重启 Codex。Windows 一键脚本会写用户级变量，macOS 脚本会写入 launchd 并持久化。</li>
        <li><b>macOS 首次打不开</b>：应用是临时签名。在 Finder 里右键选「打开」，确认一次即可。</li>
        <li><b>保存配置被拒</b>：看页面上的报错。常见原因是 JSON 语法错误、引用了未定义的变量、枚举值写错，或配置里还留着已删除的键。</li>
        <li><b>请求返回 502</b>：没有命中任何路由，且 <code>upstream</code> 为空；或者上游地址连不上。看日志的 <code>[route]</code> 行确认去向。</li>
        <li><b>想保留日志</b>：设置 <code>log_file</code>。默认只留在内存，供控制台最近 500 行使用。</li>
      </ul>
    </section>

    <section class="docSec" id="doc-removed" data-group="排障">
      <h3>已删除的配置键</h3>
      <p>下面这些旧版顶层键已经删除。配置里带着它们不影响启动，程序照常运行，但对应功能不会生效：托盘图标变红，日志页的启动警告会逐键说明替代写法。含旧键的保存在配置页会被拒绝。</p>
      <div class="docTableWrap"><table>
        <tr><th>旧键</th><th>替代写法</th></tr>
        <tr><td><code>classifier_thinking_disabled</code></td><td>在 <code>classifier_route</code> 里写 <code>"classifier_thinking": "off"</code></td></tr>
        <tr><td><code>classifier_max_tokens</code></td><td>直接删除。分类器请求的 max_tokens 不再被修改</td></tr>
        <tr><td><code>translateNone2Low</code></td><td>在 routes 条目、fast_route、multimodal_fallback 或 search_fallback 上写 <code>"convertOff2Low": "translate"</code> 或 <code>"all"</code></td></tr>
      </table></div>
    </section>
  </div>
</div>`

// logViewerDocEN is the English usage-doc body, rendered from docs/console-doc/doc-en.html by
// tools/docgen. The English page render replaces the doc placeholder with it before the
// static-copy replacer runs, so this fragment must contain no Chinese characters.
const logViewerDocEN = `<div class="docHead">
  <div class="docTitle">Usage Docs</div>
  <input id="docSearch" class="docSearch" type="search" placeholder="Search parameters or features (429, cache, thinking…)" oninput="docFilter(this.value)" onkeydown="docSearchKey(event)">
  <span id="docSearchInfo" class="docSearchInfo" data-label="sections"></span>
  <button class="ghost" type="button" onclick="docClose()">Close</button>
</div>
<div class="docMain">
  <nav class="docNav" id="docNav"></nav>
  <div class="docBody doc" id="docBody">
    <div class="docNoResult" id="docNoResult">No matching sections. Try a shorter keyword.</div>

    <section class="docSec" id="doc-what" data-group="Getting Started">
      <h3>What this is</h3>
      <p>Proxy429 is an Anthropic-compatible proxy that runs on your machine. AI clients send requests to it, it forwards them to one or more upstreams, and the whole exchange is visible in a web console.</p>
      <p>It does three things.</p>
      <ul>
        <li><b>Absorbs 429s and 5xx</b>: exponential-backoff retries, so the client usually never notices upstream hiccups.</li>
        <li><b>Routes by model name</b>: <code>routes</code> sends different model names to different upstreams and keys.</li>
        <li><b>Translates protocols</b>: turns Codex CLI's OpenAI Responses requests into Anthropic Messages, so Codex can use Anthropic upstreams.</li>
      </ul>
      <pre data-copy="Copy" data-done="Copied">Claude Code ── Anthropic ──▶ 127.0.0.1:8080 ┐
Codex CLI   ── Responses ──▶ 127.0.0.1:8081 ┴─▶ routes / fallback ──▶ upstream</pre>
      <p>The console lives at <code>http://127.0.0.1:8080/__logs</code>: status, logs and config all live there. Every listener accepts loopback connections only.</p>
    </section>

    <section class="docSec" id="doc-quickstart" data-group="Getting Started">
      <h3>Quick start</h3>
      <ol class="docSteps">
        <li><b>Start the proxy</b>. On Windows double-click <code>proxy429.exe</code>; on macOS double-click <code>Proxy429.app</code> (if Gatekeeper blocks the first launch, right-click and choose Open); on Linux run <code>./proxy429</code>. After launch there is only a tray or menu-bar icon, no window.</li>
        <li><b>Open the console</b>. Use the tray menu item "View logs", or open <code>http://127.0.0.1:8080/__logs</code> directly.</li>
        <li><b>Hook up Claude Code</b>. In the same terminal that runs <code>claude</code>:
          <pre data-copy="Copy" data-done="Copied">export ANTHROPIC_BASE_URL="http://127.0.0.1:8080"
export ANTHROPIC_AUTH_TOKEN="your API key"
claude</pre>
          When the route entry carries <code>api</code>, the token here can be any non-empty string; the proxy injects the real key. Upstreams that only honor a real <code>Authorization: Bearer</code> (e.g. Volcengine ARK) must use <code>ANTHROPIC_AUTH_TOKEN</code>, not <code>ANTHROPIC_API_KEY</code>, or you get a 401.</li>
        <li><b>Hook up Codex CLI</b>. First make sure <code>responses_listen</code> is enabled on the Config tab, then use "Codex one-line setup" at the bottom of the Config tab: copy the command for your OS, run it, and restart Codex. Details in "Codex &amp; Responses translation".</li>
      </ol>
      <p><b>Verify</b>: a request appears under In-flight on the Status tab and a <code>[route]</code> line appears on the Logs tab — the chain works.</p>
      <p><b>Where the config file lives</b>: the first launch generates one with placeholder keys in your user directory.</p>
      <div class="docTableWrap"><table>
        <tr><th>Platform</th><th>Path</th></tr>
        <tr><td>Windows</td><td><code>%AppData%\proxy429\config.json</code></td></tr>
        <tr><td>macOS</td><td><code>~/Library/Application Support/proxy429/config.json</code></td></tr>
        <tr><td>Linux</td><td><code>~/.config/proxy429/config.json</code></td></tr>
      </table></div>
      <p>If <code>./config.json</code> exists in the launch directory it wins — handy for debugging. You can also pass <code>-config &lt;path&gt;</code> at launch. For day-to-day changes, use the Config tab.</p>
    </section>

    <section class="docSec" id="doc-console" data-group="Getting Started">
      <h3>Console tour</h3>
      <p>The top bar has three tabs — Status, Logs, Config — plus the Docs button and the language switch on the right. Closing the browser tab only hides the console; the proxy keeps running.</p>
      <ul>
        <li><b>Status</b>: stat cards, in-flight streams, recently finished streams. Click any row to inspect that stream's request body and output. Card and badge meanings are in "Status tab &amp; stats" and "Badge dictionary".</li>
        <li><b>Logs</b>: live scrolling log. Memory keeps only the latest 500 lines; configure <code>log_file</code> to keep more.</li>
        <li><b>Config</b>: edit <code>config.json</code> directly; saving takes effect immediately. See "Editing config".</li>
      </ul>
    </section>

    <section class="docSec" id="doc-config-edit" data-group="Config &amp; Routing">
      <h3>Editing config</h3>
      <p>The Config tab can create, rename, delete and switch config files; the list refreshes every 3 seconds. The active config cannot be deleted. <button class="ghost" type="button" style="padding:2px 10px;font-size:12px" onclick="docOpenConfig()">Open Config tab</button></p>
      <p><b>Save &amp; reload</b> validates the JSON, writes the file, and applies it immediately. <b>Reload only</b> re-reads the file from disk without writing. Invalid JSON is never written; the error points at the exact spot.</p>
      <p><code>listen</code> binds at startup only. After changing the main port, exit and restart the proxy. <code>responses_listen</code> is the exception: the listener starts and stops with each save.</p>
      <h4>Shared variables: vars</h4>
      <p>When several routes share one key, put the key in the top-level <code>vars</code> and reference it from routes as <code>$NAME</code> or <code>${NAME}</code>. Rotating a key touches one place.</p>
      <pre data-copy="Copy" data-done="Copied">"vars": { "DEEPSEEK_KEY": "sk-..." },
"routes": [
  { "pattern": "*opus*", "url": "https://api.deepseek.com", "api": "$DEEPSEEK_KEY", "model": "deepseek-v4-pro" }
]</pre>
      <ul>
        <li>Any string value in the file can reference a variable, <code>api</code> and <code>url</code> included. The file keeps the reference verbatim; it is resolved on load.</li>
        <li>Referencing an undefined variable rejects load or save with a path, e.g. <code>routes[0].api</code>.</li>
        <li>Variable values must be strings; an empty string counts. Variables may reference other variables, one level deep.</li>
        <li>A <code>$</code> not followed by a valid name is left alone, e.g. <code>$5</code>.</li>
      </ul>
      <h4>Previewing lazyApply</h4>
      <p>"Preview JSON after lazyApply" shows the config with defaults filled in, read-only, never written. "Copy to editor" puts the result back into the editor; you still save it yourself. Rules are in "Global defaults: lazyApply".</p>
      <p><b>Access control</b>: the forwarding port and the admin endpoints (<code>/__*</code>) always accept loopback connections only. Even if you write <code>listen</code> as <code>0.0.0.0</code>, LAN devices cannot connect and cannot see the console.</p>
    </section>

    <section class="docSec" id="doc-routes" data-group="Config &amp; Routing">
      <h3>Routing basics</h3>
      <p><code>routes</code> wildcard-matches the model name. <code>*</code> matches any run of characters, including none. On a hit, <code>url</code>, <code>api</code> and <code>model</code> override the request.</p>
      <pre data-copy="Copy" data-done="Copied">"routes": [
  { "pattern": "*opus*",  "url": "https://upstream-a", "api": "sk-aaa", "model": "strong-model" },
  { "pattern": "*haiku*", "url": "https://upstream-b", "api": "sk-bbb", "model": "cheap-model" },
  { "pattern": "*",       "url": "https://upstream-a", "api": "sk-aaa", "model": "strong-model" }
]</pre>
      <ul>
        <li><b>Order decides</b>: entries match in array order and the first hit wins — there is no "more specific wins". If <code>*opus*</code> comes before <code>*opus-4*</code>, <code>claude-opus-4-8</code> only ever hits the former. Put specific rules first and the <code>*</code> catch-all last.</li>
        <li><b>pattern can be an array</b>: <code>"pattern": ["*fable*", "*opus*"]</code> equals copying the route once per pattern. All other fields are shared; position and array order are the match order. An empty array or a non-string member fails the load.</li>
        <li><b>Reserved names</b>: the pure <code>*</code> catch-all route is always called <code>Fallback</code> in the Codex menu, and the fast lane is <code>fast_route</code>. No pattern may be exactly one of these names; a colliding route neither takes effect nor enters the menu, and when pattern is an array only the colliding member is disabled. Wildcards like <code>Fall*</code> are unaffected.</li>
        <li><b>effort pins the thinking level</b>: when a route sets <code>effort</code> (<code>low</code>/<code>mid</code>/<code>high</code>/<code>max</code>; <code>mid</code> goes upstream as <code>medium</code>), a request whose body at the pin point (after routing, before the search rewrite and the native off-&gt;low upgrades) carries adaptive thinking gets its top-level <code>output_config.effort</code> forced to that level, ignoring what the client asked for; a body already at that level is left untouched with no badge. Non-adaptive bodies (budget, thinking off, no thinking field) stay byte-for-byte untouched, explicit thinking-off is never pinned (the convertOff2Low quiet upgrade keeps its own low on both ports), and an unset <code>effort</code> never touches anything. Only the route that actually serves the request counts: classifier, fast, image/search fallback and Responses-passthrough lanes use their own paths. The API column shows the last rewrite as a teal <code>[from-&gt;to]</code> badge (a later search rewrite re-badges; the 400 fallback back to thinking-off clears it). The model column also shows the configured level as <code>origModel -&gt; [level] targetModel</code> (appended as <code>origModel [level]</code> when routing didn't rewrite the model), in wire vocabulary (<code>mid</code> shows as <code>medium</code>), even when the body-side rewrite didn't apply; the badge remains the proof of an actual rewrite. The top-level <code>compactEffort</code> (<code>low</code>/<code>mid</code>/<code>high</code>/<code>max</code>) pins context-compaction requests (Claude Code compact, Codex CLI checkpoint compaction) the same way and beats every route <code>effort</code> pin on them; unset it and behavior stays exactly as before. On those requests the model column's <code>[level]</code> slot carries the compactEffort level in place of the overridden route pin (the same <code>origModel -&gt; [level] targetModel</code> position), and an actual rewrite still shows the API column's <code>[from-&gt;to]</code> badge.</li>
      </ul>
      <p>To see which route won, read the <code>[route]</code> line in the log, or the <code>[Pattern]</code>, <code>[Classifier]</code>, <code>[Fast]</code>, <code>[Multimodal]</code>, <code>[Search]</code> prefix on the model column of the Status tab.</p>
    </section>

    <section class="docSec" id="doc-fallback" data-group="Config &amp; Routing">
      <h3>Capability fallbacks</h3>
      <p>After a route matches, if the upstream lacks a capability the request needs, the proxy swaps to a fallback upstream. Search and images are handled in a fixed order.</p>
      <ul>
        <li><b>Search requests</b> (carrying the <code>web_search</code> tool): when the matched route is marked <code>no_search</code>, the whole request goes to <code>search_fallback</code> and that upstream searches and answers on its own. Without a <code>search_fallback</code>, the request passes to the original route, which may error.</li>
        <li><b>Image requests</b> (without search): when the matched route is marked <code>text_only</code> and the request carries images, it goes to <code>multimodal_fallback</code>. Without one, it passes to the original route, which may reject images.</li>
        <li><b>Search wins</b>: a request carrying both search and images is treated as search and goes to <code>search_fallback</code>.</li>
      </ul>
      <p>Fallback routes use the same <code>url</code>/<code>api</code>/<code>model</code> shape and support some behavior fields, e.g. <code>thinking</code> and <code>convertOff2Low</code>. Once a fallback kicks in, its own fields apply; search-specific fields follow the per-field rules in "Parameter reference".</p>
      <p>To let a search-capable upstream write its own summaries, see "Search enhancement &amp; empty-search guard". To split classifier or fast requests onto their own upstream, see "Dedicated routes".</p>
    </section>

    <section class="docSec" id="doc-special" data-group="Config &amp; Routing">
      <h3>Dedicated routes</h3>
      <h4>classifier_route</h4>
      <p>Claude Code's security checks and Codex's approval evaluations send signature requests. A matching request ignores its model and goes straight to <code>classifier_route</code> — a good place for a cheap model, sparing your main quota.</p>
      <ul>
        <li><code>classifier_thinking</code> takes <code>off</code>, <code>low</code>, or nothing. Unset leaves the request untouched. <code>off</code> rewrites the request to thinking-off for faster verdicts. <code>low</code> turns thinking off first, then quietly upgrades it to low through the <code>convertOff2Low</code> machinery upstream-side, stripping thinking blocks on the way back — the client notices nothing.</li>
        <li><code>classifier_thinking_cc</code> and <code>classifier_thinking_codex</code> override the shared value per source.</li>
        <li>Classifier requests carry tiny max_tokens (guardian can send as little as 64). When the <code>low</code> policy needs the 1024-token floor, the proxy raises max_tokens by 1024 upstream-side, invisibly to the client.</li>
      </ul>
      <p>Overall order: classifier signature requests go to <code>classifier_route</code> first, requests carrying <code>"speed":"fast"</code> go to <code>fast_route</code> next, everything else wildcard-matches <code>routes</code> and finally falls back to <code>upstream</code>.</p>
      <p>The Classifier card on the Status tab counts hits; hover for the per-source split and click to enlarge.</p>
      <h4>fast_route</h4>
      <p>Requests carrying <code>"speed":"fast"</code> take <code>fast_route</code>. Claude Code's <code>/fast</code> and the <code>fast_route</code> entry in the Codex menu both trigger it. On a hit the proxy adds the fast-lane rate-limit headers to the response, keeping the client in fast mode.</p>
    </section>

    <section class="docSec" id="doc-lazyapply" data-group="Config &amp; Routing">
      <h3>Global defaults: lazyApply</h3>
      <p><code>lazyApply</code> is a global default bundle. It only fills fields that a route-like object already supports, exactly as if you wrote the key into every object that does not set it explicitly. It never invents fields and never creates unconfigured optional routes.</p>
      <ul>
        <li><b>Per-field precedence</b>: explicit local value (including <code>false</code> and empty strings) &gt; <code>lazyApply</code> &gt; built-in default.</li>
        <li><b>Connection trio is inherited</b>: <code>url</code>, <code>api</code> and <code>model</code> come from lazyApply too (written into every object that omits the key, at load time), so the upstream is written once and a route can shrink to <code>{"pattern": "*sonnet*", "effort": "high"}</code>; <code>fast_route</code> may even be <code>{}</code>. A locally written connection key still wins; writing an empty string stops inheritance: <code>"api": ""</code> passes the client's token through, <code>"model": ""</code> keeps the client's model, and <code>"url": ""</code> means no reroute (a routes[] entry still applies its other fields and forwards to the default upstream, classifier_route keeps only its thinking rewrite, and fast_route / multimodal_fallback / search_fallback are not engaged, so the request follows normal routing). <code>pattern</code> and <code>url_response_api</code> are never inherited and stay per object.</li>
        <li><b>effort is written per route only</b>: a route's <code>effort</code> pin is never inherited; lazyApply does not offer the field, so a global default can never pin every route's level at once. The top-level <code>compactEffort</code> is the one global effort knob, but it only affects context-compaction requests, never normal traffic.</li>
        <li><b>enhance_search replaces whole</b>: a local value replaces the lazyApply object entirely; the two are never merged.</li>
        <li><b>Objects can opt out</b>: <code>"noLazyApply": true</code> inside an object stops all inheritance; its own explicit fields still apply.</li>
        <li><b>Inherited values join the precedence chain</b>: an inherited <code>claudeSearch*</code> counts as locally written and beats <code>search_fallback</code>'s own value. To use the fallback's value, write <code>noLazyApply</code> on that route.</li>
      </ul>
      <p>Supported fields are listed under lazyApply in "Parameter reference". The Config tab's preview button shows the expanded result read-only.</p>
    </section>

    <section class="docSec" id="doc-retry" data-group="Config &amp; Routing">
      <h3>Connection &amp; retries</h3>
      <ul>
        <li><b>listen</b>: the main Anthropic listener address; the template uses <code>127.0.0.1:8080</code>. Binds at startup only.</li>
        <li><b>upstream</b>: the default upstream, used when no route matches. Omit it if you configure a pure <code>*</code> catch-all route.</li>
        <li><b>responses_listen</b>: the OpenAI Responses listener address; empty means off. Starts and stops on save.</li>
        <li><b>Retry parameters</b>: <code>max_retries</code> is the maximum number of retries, excluding the first attempt. <code>base_delay_s</code> is the first backoff in seconds; later waits double, plus up to 30% random jitter. <code>max_delay_s</code> caps a single wait; 0 clips every wait to 0, i.e. no waiting. <code>total_budget_s</code> is the retry budget for the whole request. <code>retry_status_codes</code> lists the upstream status codes that trigger a retry; an empty array means no status-code retries.</li>
        <li><b>respect_retry_after</b>: when the upstream returns a <code>Retry-After</code> header, wait by it first.</li>
        <li><b>upstream_header_timeout_s</b>: how long to wait for the upstream's first byte. A timeout is handled as an internal resend; unset means 70 seconds.</li>
        <li><b>ping_interval_s</b>: SSE keep-alive interval sent to the client during retry waits; unset means 5 seconds. Non-streaming requests get no keep-alive.</li>
      </ul>
      <p>When retries run out, the proxy sends the client an <code>overloaded_error</code> event. The status column shows <code>[retries exhausted]</code>; streams that went through backoff retries show <code>[retried Nx]</code>.</p>
      <p>Logging: <code>log_request_detail</code> records request-body detail; <code>log_file</code> appends the log to a file; <code>recent_sample_window</code> is the recent-sample window for the TTFT and tok/s stats, unset means 20.</p>
    </section>

    <section class="docSec" id="doc-codex" data-group="Advanced">
      <h3>Codex &amp; Responses translation</h3>
      <p>Set the top-level <code>responses_listen</code> to <code>127.0.0.1:8081</code> and the proxy additionally serves <code>/v1/responses</code> at that address. Codex CLI only speaks OpenAI Responses; requests are translated to Anthropic Messages, travel the exact same main pipeline as Claude Code, and the response is translated back to Responses.</p>
      <h4>Hooking up Codex</h4>
      <p>The easiest path is "Codex one-line setup" at the bottom of the Config tab. Two commands are generated live from the editor above; they back up and rewrite <code>~/.codex/config.toml</code> and write the model catalog. Choosing "(Restore default Codex config)" in the dropdown yields a command that fully undoes it. Restart Codex afterwards.</p>
      <p>Manual configuration below.</p>
      <pre data-copy="Copy" data-done="Copied">model_provider = "proxy429"
model = "gpt-5-codex"
forced_login_method = "api"

[model_providers.proxy429]
name = "Proxy429 Local Proxy"
base_url = "http://127.0.0.1:8081/v1"
wire_api = "responses"
experimental_bearer_token = "proxy429"</pre>
      <p><code>base_url</code> must include <code>/v1</code> and <code>wire_api</code> must be <code>responses</code>. The token is a placeholder the proxy never checks; the real key is injected by the matched route's <code>api</code>.</p>
      <p><b>Codex reports 503 and the proxy sees nothing</b>: a system proxy is hijacking requests to 127.0.0.1. Put <code>127.0.0.1</code> into <code>NO_PROXY</code> and restart Codex. The Windows and macOS one-line scripts write and persist it automatically; the Linux script prints a hint.</p>
      <h4>Model names and thinking mapping</h4>
      <p>The model name Codex sends participates in routes matching as usual. The thinking-shape lookup uses the client's original model name and happens before route rewriting. For the mapping table to apply, set Codex's model to the target model name itself.</p>
      <p>Models come in two classes. Adaptive models express thinking as <code>thinking:adaptive</code> plus <code>output_config.effort</code>: <code>fable-5</code>, <code>mythos-5</code>, <code>mythos-preview</code>, <code>sonnet-5</code>, <code>opus-4-8</code>, <code>opus-4-7</code>, <code>opus-4-6</code>, <code>sonnet-4-6</code>. All others express it as <code>budget_tokens</code>.</p>
      <div class="docTableWrap"><table>
        <tr><th>Codex reasoning.effort</th><th>Regular model → budget_tokens</th><th>Adaptive model → effort</th></tr>
        <tr><td>minimal / low</td><td>2048</td><td>low</td></tr>
        <tr><td>medium</td><td>8192</td><td>medium</td></tr>
        <tr><td>high</td><td>16384</td><td>high</td></tr>
        <tr><td>xhigh / max / ultra</td><td>24576</td><td>max</td></tr>
        <tr><td>none / off / disabled</td><td>thinking off</td><td>off when the model allows it; fable-5 and mythos-5 become adaptive + low</td></tr>
        <tr><td>field absent</td><td>thinking off</td><td>on by default for fable-5, mythos-5, mythos-preview, sonnet-5; off for the rest</td></tr>
      </table></div>
      <p>Unrecognized values are treated as "field absent". budget_tokens has two guards: never more than half of <code>max_tokens</code>, and if the clamped value is under 1024, thinking is not opened. <code>max_output_tokens</code> defaults to 32000.</p>
      <p>When an alias hits the adaptive table but the actual routed target only supports budget (e.g. the alias is <code>claude-fable-5</code> while traffic lands on Kimi), write <code>"thinking": "budget"</code> or <code>"adaptive"</code> on that route entry to correct it. The field only affects translated streams.</p>
      <h4>Tool mapping</h4>
      <div class="docTableWrap"><table>
        <tr><th>Responses side</th><th>Anthropic side</th></tr>
        <tr><td>function tools</td><td>tools[], keeping name, description, input_schema</td></tr>
        <tr><td>custom or string tools</td><td>wrapped as a single-parameter {input:string}; the definition is inlined into the description; replayed as custom_tool_call</td></tr>
        <tr><td>namespace (MCP) tools</td><td>split into ns__name sub-tools, truncated with a hash when overlong; restored on replay</td></tr>
        <tr><td>tool_search</td><td>the proxy's fixed tool (query, limit)</td></tr>
        <tr><td>web_search / web_search_preview</td><td>web_search_20250305</td></tr>
        <tr><td>input_file</td><td>document blocks (URL or base64)</td></tr>
        <tr><td>input_image</td><td>image blocks (base64 or URL)</td></tr>
        <tr><td>function_call_output and other tool results</td><td>tool_result blocks; images split into native image blocks; overlong base64 truncated</td></tr>
        <tr><td>parallel_tool_calls: false</td><td>tool_choice.disable_parallel_tool_use</td></tr>
      </table></div>
      <h4>Thinking, history and the envelope</h4>
      <ul>
        <li>With thinking on, tool-continuation history must carry replayable signed thinking blocks. <code>allowNoThinkBlock4Anthropic</code> defaults to true: send at the requested level first, and only if the upstream returns 400 turn thinking off and retry once, without stripping thinking blocks so the next round's history heals itself. Set it to false to turn thinking off from the start.</li>
        <li>A forced <code>tool_choice</code> conflicts with thinking. Models that can turn thinking off get thinking disabled for this request while keeping the tool constraint; <code>fable-5</code> and <code>mythos-5</code> cannot, so the request errors.</li>
        <li>Thinking blocks and each search's full results are sealed into an envelope inside the response, restored when the client replays it next round, so the thinking chain and search bodies keep working. Search envelopes are obfuscated per upstream and key; after a key or upstream change they expire automatically, and plaintext URLs or keys never appear in client history.</li>
        <li>When an old conversation's search blocks have expired upstream, the proxy strips the replayed blocks and retries once, degrading invisibly to a fresh search. The strip count shows as <code>[strip N]</code>.</li>
      </ul>
    </section>

    <section class="docSec" id="doc-search" data-group="Advanced">
      <h3>Search enhancement &amp; empty-search guard</h3>
      <p>Every Claude Code web search runs as its own sub-agent request. A <b>standalone search request</b> is recognized by its final user message starting with <code>Perform a web search for the query</code>. Only the final message counts; the same phrase quoted in history does not. Recognized requests show a teal-green <code>[Search]</code> on the model column.</p>
      <h4>Let the upstream write summaries: enhance_search and claudeSearchSummary</h4>
      <p><code>enhance_search</code> lives on a routes entry and applies when that route is not marked <code>no_search</code>. The proxy injects the summary instruction into the search request itself, still on the route's own upstream, so one request does the search and per-result summaries, and the response passes through unchanged. The legacy <code>summary_level</code> controls detail (low, mid, high, max), and <code>summary_thinking: true</code> turns thinking into low.</p>
      <p>The newer fields are more flexible and can sit on a routes entry or on <code>search_fallback</code>.</p>
      <ul>
        <li><code>claudeSearchSummary</code>: <code>forceLow</code>, <code>forceMid</code>, <code>forceHigh</code> inject the summary instruction at that level; <code>noForce</code> injects nothing. Once set, it beats the legacy <code>enhance_search</code> fields.</li>
        <li><code>claudeSearchThinking</code>: <code>off</code>, <code>low</code>, <code>mid</code>, <code>high</code>, <code>max</code> rewrite thinking for this search request. If the client never asked for thinking, returned thinking blocks are stripped and the client sees its original shape.</li>
      </ul>
      <p>After a summary injection the badge upgrades from <code>[Search]</code> to <code>[SearchSummary-level]</code>. When thinking was rewritten, the API column additionally shows <code>[from-&gt;to]</code>.</p>
      <p><code>search_fallback</code> also keeps its own legacy fields: <code>summary_mode: true</code> injects the summary instruction for standalone search requests on that fallback, at <code>summary_level</code> (low by default); <code>summary_thinking: true</code> turns thinking into low. When <code>claudeSearchSummary</code> or <code>claudeSearchThinking</code> is set, the newer field wins.</p>
      <p><b>Precedence</b>: when a route serves the request itself, its own values apply. When the whole request diverts to <code>search_fallback</code>, fields the route set explicitly beat the fallback per field, and unset fields take the fallback's value.</p>
      <h4>Empty-search guard: noBullshitOnEmptyClaudeSearch</h4>
      <p>Configured on a routes entry or on <code>search_fallback</code>; off by default. When on, a standalone search request that returned search-result blocks whose every result body is an empty array makes the proxy discard the upstream's entire answer (thinking and fabricated prose included) and send a fixed "nothing was found, try other keywords" stream instead. Non-empty results, or shapes it does not recognize, pass through conservatively.</p>
      <p>It only applies to standalone search requests on the native Anthropic port and never touches Responses traffic. Once on, even a healthy upstream 200 stream is buffered until the upstream finishes before forwarding. Triggers log <code>[search-guard]</code> and mark the status column of the finished row with a red <code>[empty-search]</code>.</p>
    </section>

    <section class="docSec" id="doc-thinking" data-group="Advanced">
      <h3>Thinking rewrite: convertOff2Low</h3>
      <p>Some upstreams switch to a non-thinking older model when thinking is off, e.g. K2.8 Preview in Kimi's docs. Quietly upgrading "off" to low keeps the request on the strong model. <code>convertOff2Low</code> is that switch.</p>
      <ul>
        <li>Values: <code>"translate"</code> applies to the Responses translation port only; <code>"all"</code> covers both the translation port and the native Anthropic port; unset means off.</li>
        <li>How it works: when the client explicitly turns thinking off, the proxy sends low upstream. Adaptive models get <code>adaptive</code> plus <code>effort: low</code>; budget models get <code>budget_tokens: 2048</code>, subject to the half-of-max_tokens cap and the 1024 floor. Thinking blocks are stripped on the way back — the client still sees thinking off, while usage passes through truthfully.</li>
        <li>The upgrade is abandoned when a budget model cannot fit even 1024, or when the request forces a <code>tool_choice</code> that conflicts with thinking.</li>
        <li>If the upstream rejects thinking, the proxy falls back to thinking-off and retries once.</li>
        <li>Which route applies: the translation port pre-matches routes by the client's model name; the native port uses the finally effective route, taking the fallback route's value after an image or search fallback swap.</li>
      </ul>
      <p>The <code>[off-&gt;low]</code> badge shows two colors in the API column: the first part is the client-side vocabulary, the second the upstream-side one. History fallbacks are shown honestly at the requested level and carry no badge.</p>
      <p><code>allowNoThinkBlock4Anthropic</code> decides the strategy when tool-continuation history lacks signed thinking blocks: true (default) sends first and turns thinking off only after a 400; false turns it off from the start. Requests that explicitly turn thinking off are unaffected and stay governed by <code>convertOff2Low</code>.</p>
    </section>

    <section class="docSec" id="doc-streamify" data-group="Advanced">
      <h3>Global stream-ification: convertAlltoStream</h3>
      <p>With <code>convertAlltoStream</code> on (default false), every non-streaming Anthropic Messages request is quietly turned into a streaming request upstream. The proxy reads the whole stream, reassembles the content blocks into the non-streaming JSON, and returns it in one shot. The caller still gets the non-streaming response it expects.</p>
      <p>The benefit: one-shot requests like classifiers now show live token output, TTFT and tok/s in the console.</p>
      <ul>
        <li>Applies to <code>/v1/messages</code> only; requests already streaming are untouched.</li>
        <li>If the stream breaks mid-way (no <code>message_stop</code>), the proxy has not written anything to the client yet, so it can retry wholesale.</li>
        <li>No SSE keep-alive is sent during retry waits, avoiding pollution of the non-streaming response.</li>
      </ul>
    </section>

    <section class="docSec" id="doc-params" data-group="Reference">
      <h3>Parameter reference</h3>
      <p>Every key, grouped by config object. Type "number" means a JSON number, integer or decimal. The default column lists built-in defaults only; keys without one are explained by the template's example value.</p>
      <h4>Top level</h4>
      <div class="docTableWrap"><table>
        <tr><th>Key</th><th>Type</th><th>Default</th><th>Meaning</th></tr>
        <tr><td><code>listen</code></td><td>string</td><td>127.0.0.1:8080 (template)</td><td>Main listener address. Binds at startup only</td></tr>
        <tr><td><code>upstream</code></td><td>string</td><td>empty</td><td>Default upstream when no route matches</td></tr>
        <tr><td><code>vars</code></td><td>object</td><td>none</td><td>Named string variables for $NAME and ${NAME} references</td></tr>
        <tr><td><code>max_retries</code></td><td>number</td><td>0</td><td>Max retries excluding the first attempt; 0 means try once</td></tr>
        <tr><td><code>base_delay_s</code></td><td>number</td><td>0</td><td>First backoff in seconds; later waits double</td></tr>
        <tr><td><code>max_delay_s</code></td><td>number</td><td>0</td><td>Cap on a single wait; 0 means no waiting</td></tr>
        <tr><td><code>total_budget_s</code></td><td>number</td><td>0</td><td>Retry budget for the whole request, in seconds</td></tr>
        <tr><td><code>retry_status_codes</code></td><td>number[]</td><td>empty</td><td>Upstream status codes that trigger a retry; empty disables them</td></tr>
        <tr><td><code>respect_retry_after</code></td><td>bool</td><td>false</td><td>Honor the upstream's Retry-After header first</td></tr>
        <tr><td><code>upstream_header_timeout_s</code></td><td>number</td><td>70</td><td>Wait limit for the upstream's first byte; 0 also means 70</td></tr>
        <tr><td><code>ping_interval_s</code></td><td>number</td><td>5</td><td>SSE keep-alive interval during retries; 0 also means 5</td></tr>
        <tr><td><code>log_request_detail</code></td><td>bool</td><td>false</td><td>Log request-body detail</td></tr>
        <tr><td><code>log_file</code></td><td>string</td><td>empty</td><td>Append the log to this file</td></tr>
        <tr><td><code>recent_sample_window</code></td><td>number</td><td>20</td><td>Recent samples behind TTFT and tok/s; 0 also means 20</td></tr>
        <tr><td><code>convertAlltoStream</code></td><td>bool</td><td>false</td><td>Global stream-ification, see the section above</td></tr>
        <tr><td><code>compactEffort</code></td><td>string</td><td>empty</td><td>low, mid, high or max: thinking level for context-compaction requests, beating route effort; the model column shows it in the [level] slot, see the effort notes under "Routing"</td></tr>
        <tr><td><code>responses_listen</code></td><td>string</td><td>empty</td><td>Responses listener; empty means off</td></tr>
        <tr><td><code>allowNoThinkBlock4Anthropic</code></td><td>bool</td><td>true</td><td>Send history lacking signed thinking blocks first, fall back on 400</td></tr>
        <tr><td><code>routes</code></td><td>array</td><td>empty</td><td>Model-name wildcard routing table; entry fields in the next table</td></tr>
        <tr><td><code>fast_route</code></td><td>object</td><td>empty</td><td>Fast-lane route; fields in its table below</td></tr>
        <tr><td><code>classifier_route</code></td><td>object</td><td>empty</td><td>Classifier route; fields in its table below</td></tr>
        <tr><td><code>multimodal_fallback</code></td><td>object</td><td>empty</td><td>Image fallback route; same fields as fast_route</td></tr>
        <tr><td><code>search_fallback</code></td><td>object</td><td>empty</td><td>Search fallback route; fields in its table below</td></tr>
        <tr><td><code>lazyApply</code></td><td>object</td><td>none</td><td>Shared defaults; fields in its table below</td></tr>
      </table></div>
      <h4>routes[] entry</h4>
      <div class="docTableWrap"><table>
        <tr><th>Key</th><th>Type</th><th>Default</th><th>Meaning</th></tr>
        <tr><td><code>pattern</code></td><td>string or string[]</td><td>required</td><td>Model-name wildcard, * matches any run; an array means OR</td></tr>
        <tr><td><code>url</code></td><td>string</td><td>empty</td><td>Upstream base URL; omitted it inherits lazyApply.url, and still empty = no reroute: the entry's other fields still apply and the request forwards to the default upstream</td></tr>
        <tr><td><code>api</code></td><td>string</td><td>empty</td><td>Upstream key sent as Authorization: Bearer; empty passes the client token through</td></tr>
        <tr><td><code>model</code></td><td>string</td><td>empty</td><td>Rewritten model name; empty leaves it unchanged</td></tr>
        <tr><td><code>text_only</code></td><td>bool</td><td>false</td><td>Upstream cannot take images; image requests divert to multimodal_fallback</td></tr>
        <tr><td><code>no_search</code></td><td>bool</td><td>false</td><td>Upstream cannot search; search requests divert to search_fallback</td></tr>
        <tr><td><code>thinking</code></td><td>string</td><td>auto</td><td>auto, adaptive or budget; affects the Responses translation port only</td></tr>
        <tr><td><code>effort</code></td><td>string</td><td>empty</td><td>low, mid, high or max; forces output_config.effort for adaptive requests (a body already at the level is left alone), empty leaves the body untouched; the status page's model column shows the configured level as a [level] tag</td></tr>
        <tr><td><code>convertOff2Low</code></td><td>string</td><td>empty</td><td>Empty, translate or all</td></tr>
        <tr><td><code>enhance_search</code></td><td>object</td><td>none</td><td>Summary injection; subfields summary_thinking and summary_level</td></tr>
        <tr><td><code>claudeSearchSummary</code></td><td>string</td><td>empty</td><td>forceLow, forceMid, forceHigh or noForce</td></tr>
        <tr><td><code>claudeSearchThinking</code></td><td>string</td><td>empty</td><td>off, low, mid, high or max</td></tr>
        <tr><td><code>noBullshitOnEmptyClaudeSearch</code></td><td>bool</td><td>false</td><td>Empty-search guard</td></tr>
        <tr><td><code>noLazyApply</code></td><td>bool</td><td>false</td><td>Do not inherit lazyApply</td></tr>
      </table></div>
      <h4>classifier_route</h4>
      <div class="docTableWrap"><table>
        <tr><th>Key</th><th>Type</th><th>Default</th><th>Meaning</th></tr>
        <tr><td><code>url</code></td><td>string</td><td>empty</td><td>Upstream base URL; omitted it inherits lazyApply.url, and still empty = no reroute (thinking rewrite only)</td></tr>
        <tr><td><code>api</code></td><td>string</td><td>empty</td><td>Upstream key; empty passes the client token through</td></tr>
        <tr><td><code>model</code></td><td>string</td><td>empty</td><td>Rewritten model name</td></tr>
        <tr><td><code>classifier_thinking</code></td><td>string</td><td>empty</td><td>Empty, off or low; see "Dedicated routes"</td></tr>
        <tr><td><code>classifier_thinking_cc</code></td><td>string</td><td>empty</td><td>Override for the Claude Code source</td></tr>
        <tr><td><code>classifier_thinking_codex</code></td><td>string</td><td>empty</td><td>Override for the Codex source</td></tr>
        <tr><td><code>noLazyApply</code></td><td>bool</td><td>false</td><td>Do not inherit lazyApply</td></tr>
      </table></div>
      <h4>fast_route and multimodal_fallback</h4>
      <div class="docTableWrap"><table>
        <tr><th>Key</th><th>Type</th><th>Default</th><th>Meaning</th></tr>
        <tr><td><code>url</code></td><td>string</td><td>empty</td><td>Upstream base URL; omitted it inherits lazyApply.url, and still empty = the lane stays disengaged and the request follows normal routing</td></tr>
        <tr><td><code>api</code></td><td>string</td><td>empty</td><td>Upstream key; empty passes the client token through</td></tr>
        <tr><td><code>model</code></td><td>string</td><td>empty</td><td>Rewritten model name</td></tr>
        <tr><td><code>convertOff2Low</code></td><td>string</td><td>empty</td><td>Empty, translate or all</td></tr>
        <tr><td><code>noLazyApply</code></td><td>bool</td><td>false</td><td>Do not inherit lazyApply</td></tr>
      </table></div>
      <h4>search_fallback</h4>
      <div class="docTableWrap"><table>
        <tr><th>Key</th><th>Type</th><th>Default</th><th>Meaning</th></tr>
        <tr><td><code>url</code></td><td>string</td><td>empty</td><td>Upstream base URL; omitted it inherits lazyApply.url, and still empty = not engaged, diverted requests fall back to the original route</td></tr>
        <tr><td><code>api</code></td><td>string</td><td>empty</td><td>Upstream key; empty passes the client token through</td></tr>
        <tr><td><code>model</code></td><td>string</td><td>empty</td><td>Rewritten model name</td></tr>
        <tr><td><code>thinking</code></td><td>string</td><td>auto</td><td>auto, adaptive or budget; affects the Responses translation port only</td></tr>
        <tr><td><code>convertOff2Low</code></td><td>string</td><td>empty</td><td>Empty, translate or all</td></tr>
        <tr><td><code>summary_mode</code></td><td>bool</td><td>false</td><td>Legacy: inject the summary instruction</td></tr>
        <tr><td><code>summary_thinking</code></td><td>bool</td><td>false</td><td>Legacy: turn the search request's thinking into low</td></tr>
        <tr><td><code>summary_level</code></td><td>string</td><td>low</td><td>Legacy: summary detail, low / mid / high / max</td></tr>
        <tr><td><code>claudeSearchThinking</code></td><td>string</td><td>empty</td><td>off, low, mid, high or max</td></tr>
        <tr><td><code>claudeSearchSummary</code></td><td>string</td><td>empty</td><td>forceLow, forceMid, forceHigh or noForce</td></tr>
        <tr><td><code>noBullshitOnEmptyClaudeSearch</code></td><td>bool</td><td>false</td><td>Empty-search guard</td></tr>
        <tr><td><code>noLazyApply</code></td><td>bool</td><td>false</td><td>Do not inherit lazyApply</td></tr>
      </table></div>
      <h4>lazyApply</h4>
      <div class="docTableWrap"><table>
        <tr><th>Key</th><th>Type</th><th>Applies to</th></tr>
        <tr><td><code>text_only</code></td><td>bool</td><td>routes[]</td></tr>
        <tr><td><code>no_search</code></td><td>bool</td><td>routes[]</td></tr>
        <tr><td><code>thinking</code></td><td>string</td><td>routes[] and search_fallback</td></tr>
        <tr><td><code>convertOff2Low</code></td><td>string</td><td>Every route-like object that supports it</td></tr>
        <tr><td><code>enhance_search</code></td><td>object</td><td>routes[]</td></tr>
        <tr><td><code>summary_mode</code></td><td>bool</td><td>search_fallback</td></tr>
        <tr><td><code>summary_thinking</code></td><td>bool</td><td>search_fallback</td></tr>
        <tr><td><code>summary_level</code></td><td>string</td><td>search_fallback</td></tr>
        <tr><td><code>claudeSearchThinking</code></td><td>string</td><td>routes[] and search_fallback</td></tr>
        <tr><td><code>claudeSearchSummary</code></td><td>string</td><td>routes[] and search_fallback</td></tr>
        <tr><td><code>noBullshitOnEmptyClaudeSearch</code></td><td>bool</td><td>routes[] and search_fallback</td></tr>
        <tr><td><code>classifier_thinking</code></td><td>string</td><td>classifier_route</td></tr>
        <tr><td><code>classifier_thinking_cc</code></td><td>string</td><td>classifier_route</td></tr>
        <tr><td><code>classifier_thinking_codex</code></td><td>string</td><td>classifier_route</td></tr>
        <tr><td><code>url</code></td><td>string</td><td>routes[], classifier_route, fast_route, multimodal_fallback and search_fallback</td></tr>
        <tr><td><code>api</code></td><td>string</td><td>routes[], classifier_route, fast_route, multimodal_fallback and search_fallback</td></tr>
        <tr><td><code>model</code></td><td>string</td><td>routes[], classifier_route, fast_route, multimodal_fallback and search_fallback</td></tr>
      </table></div>
    </section>

    <section class="docSec" id="doc-status" data-group="Console Reference">
      <h3>Status tab &amp; stats</h3>
      <p>The Status tab opens with stat cards: Active/Waiting, To upstream, From upstream, Cache hit, Input (uncached), Output, Retries, Classifier, TTFT, tok/s. In-flight and recently finished streams each get a table; click a row to inspect it.</p>
      <h4>Metric definitions</h4>
      <ul>
        <li><b>Total</b>: from the proxy receiving the downstream request to the full response being sent back. Includes internal handling, first-byte wait, streaming and wrap-up.</li>
        <li><b>TTFT</b>: from sending the request to receiving the first output byte.</li>
        <li><b>tok/s</b>: output tokens divided by streaming time.</li>
        <li><b>token in/out</b>: the stream's total input / output tokens, formatted <code>107k/4</code>. Input = input + cache_read + cache_creation; in flight it shows what the head/tail has delivered so far and is exact only once the stream finishes, and a side never received shows <code>-</code> (a stream cancelled by the downstream after its head reads <code>107k/-</code>). On in-flight rows the column sits between API and Bytes; on finished rows it replaces the Bytes column, and hovering a cell shows that stream's byte count.</li>
        <li><b>Cache hit</b>: <code>cache_read / (input + cache_read + cache_creation)</code>. Cache writes do not count as hits but do count as input. A high hit rate comes mostly from the upstream model's native context cache; the proxy only passes usage through. Interrupted streams are excluded from the cache-hit aggregate so a half-finished context is not counted as a low hit.</li>
        <li><b>Input (uncached)</b>: only fresh input tokens that missed the cache (interrupted streams excluded); total input = uncached + cache read + cache write, the same composition as each stream's token in/out "in" value. Hover the card to see this note.</li>
        <li><b>Cache age</b>: anchored per session and route, showing how long ago the latest cache write happened. While a stream of the same session and route is streaming, the newest finished row freezes into brackets, meaning the cache was just refreshed. A newest row that started within the last 5 minutes is protected from the "Keep finished N" trim. Requests without a session marker always show <code>-</code>.</li>
        <li><b># column</b>: <code>#80&lt;-73</code> means stream 80 continues from stream 73. It is annotated only when the session matches and stream 73's message chain is a prefix of stream 80's. A bare <code>#N</code> means no parent, e.g. a sub-agent's first round or right after <code>/clear</code>.</li>
      </ul>
      <p>Clicking the Cache hit card opens a per-real-upstream-model breakdown. The "Measured cache lifetime" table at the bottom groups by URL and model and gives the lower bound (≥), upper bound (&lt;), how long each bound has stood, and observation counts. Streams with under 1024 input tokens are not sampled.</p>
      <p>Clicking the Classifier card shows hits and rewrite details, split by Claude Code and Codex.</p>
      <p>Activity opens a heatmap of the last 48 hours, one cell per 15 minutes, colorable by request count, input+output, or output. It is stored in <code>heatmap.dat</code> next to the config file and survives restarts.</p>
      <p>Clear stats zeroes the cumulative stats, the finished-streams list and the activity heatmap; in-flight streams, stream numbering and the log are untouched. "Keep finished" controls the list length, default 10, maximum 200.</p>
    </section>

    <section class="docSec" id="doc-badges" data-group="Console Reference">
      <h3>Badge dictionary</h3>
      <div class="docTableWrap"><table>
        <tr><th>Badge</th><th>Where</th><th>Meaning</th></tr>
        <tr><td><code>[Anthropic]</code> / <code>[translate]</code></td><td>API column</td><td>Protocol source: native Anthropic port / Responses port translated</td></tr>
        <tr><td><code>off</code> / <code>on N</code> / <code>adaptive</code> / <code>low</code> etc</td><td>API column</td><td>The thinking shape actually sent upstream. The name is the protocol source; the value's color is the Anthropic vocabulary</td></tr>
        <tr><td><code>[off-&gt;low]</code></td><td>API column</td><td>Thinking-off quietly upgraded to low, see "Thinking rewrite"</td></tr>
        <tr><td><code>[level]</code></td><td>model column</td><td>The route-configured <code>effort</code> pin, shown as <code>origModel -&gt; [level] targetModel</code> (<code>origModel [level]</code> when the model wasn't rewritten), in wire vocabulary (<code>mid</code> shows as <code>medium</code>); shown even when the body-side rewrite didn't apply — the API column badge proves an actual rewrite; on compaction rows pinned by <code>compactEffort</code> the slot carries that level instead (the overridden route level does not show)</td></tr>
        <tr><td><code>[from-&gt;to]</code></td><td>API column</td><td>Teal body-side thinking rewrite: a route <code>effort</code> pin, the <code>compactEffort</code> pin on a compaction request or a search sub-agent rewrite, showing the value before the last rewrite and the one sent upstream; an empty <code>from</code> means the request had no thinking field (shown as auto)</td></tr>
        <tr><td><code>[Search]</code> / <code>[SearchSummary-level]</code></td><td>model column</td><td>Standalone search request / summary injected</td></tr>
        <tr><td><code>[Compact]</code> / <code>[Compact 88%]</code></td><td>model column</td><td>Context-compaction badge for client compaction requests: marked when the request's final user message opens with a client compaction prompt (Claude Code compact, Codex CLI checkpoint compaction); prompts merely quoted in history do not count. A finished row folds the share compressed away into the badge (<code>[Compact 88%]</code>) and shows the full <code>170k-&gt;20k, 88%</code> on hover: left is the pre-compaction total input context (input + cache_read + cache_creation), right is the summary's output tokens; when usage was not parsed, the badge stays a plain <code>[Compact]</code>. The compactEffort level shows in the model column's <code>[level]</code> slot (see above), and an actually rewritten body also shows the API column's [from-&gt;to] badge</td></tr>
        <tr><td><code>[Read*1]</code> <code>[Edit*3]</code></td><td>model column</td><td>Tool-call counts in this response, in first-appearance order. A single call with empty arguments shows *0</td></tr>
        <tr><td><code>[count_tokens]</code></td><td>model column</td><td>Claude Code's token-count probe. Its response carries only input_tokens; the badge keeps it from looking like an empty response, and it is excluded from the activity heatmap</td></tr>
        <tr><td><code>[strip N]</code></td><td>model or cache-hit column</td><td>Number of replayed search blocks stripped</td></tr>
        <tr><td><code>[retried Nx]</code></td><td>after the status</td><td>Went through N backoff retries</td></tr>
        <tr><td><code>[retries exhausted]</code></td><td>status column</td><td>Retries or budget ran out; overloaded_error was sent downstream</td></tr>
        <tr><td><code>[no-stop]</code></td><td>after the status</td><td>The upstream 200 stream ended early without a terminal event</td></tr>
        <tr><td><code>[empty-search]</code></td><td>status column</td><td>The empty-search guard triggered</td></tr>
        <tr><td><code>[attempt N: Xs]</code> / <code>[backing off]</code></td><td>in-flight row</td><td>Time the current attempt has waited / backing off right now</td></tr>
        <tr><td>Status light ⚪🟡🟢</td><td>first in-flight column</td><td>Requesting / waiting for first byte / relaying. The seconds beside it reset only when the color changes</td></tr>
      </table></div>
      <p>Non-200 status codes show in brackets, e.g. <code>[499]</code>, <code>[400]</code>. 499 means the upstream connection ended before it finished sending — most often the client cancelled; an upstream that finished and then saw the client leave still counts as 200.</p>
    </section>

    <section class="docSec" id="doc-viewer" data-group="Console Reference">
      <h3>Stream viewer</h3>
      <p>Clicking any in-flight or finished row opens the stream viewer below. The content button cycles four states: Request body → Output [rendered] → Output [raw stream] → Output [non-stream].</p>
      <ul>
        <li><b>Rendered</b>: the SSE event stream parsed into readable text.</li>
        <li><b>Raw stream</b>: the verbatim SSE bytes.</li>
        <li><b>Non-stream</b>: the events assembled into the final JSON. An incomplete stream is labeled as a partial snapshot. In this state Interactive JSON builds its tree directly from the assembled object.</li>
        <li><b>Request body</b>: the request that produced this stream, pretty-printed JSON.</li>
      </ul>
      <p><b>Both link sides</b>: the Link button toggles between proxy↔upstream and client↔proxy. The default shows what the proxy actually sent upstream and what came back. Both sides are stored only when they differ, e.g. translated streams and model-rewritten streams. Switching to a side with no recording falls back automatically with a note beside the button.</p>
      <p>Browsing shows at most the first 256KB. With "Store full payloads" on, newly started requests additionally record the full request body and output (no cap, memory cost), and the viewer shows download buttons. Turning it off immediately purges stored full copies. The switch resets to off after a restart.</p>
      <p>Interactive JSON is always available and needs no storage switch. It turns the current content into a collapsible JSON tree: the first level is expanded, deeper levels lazily expand on click, and it follows link-side switches. A stream with incomplete data is never passed off as complete — download buttons grey out with the reason.</p>
      <p>With "Auto-track latest" on, the viewer follows the newest in-flight stream. "Show at most" controls how many in-flight streams are shown side by side.</p>
    </section>

    <section class="docSec" id="doc-faq" data-group="Troubleshooting">
      <h3>FAQ</h3>
      <ul>
        <li><b>Claude Code bypasses the proxy</b>: the environment variables must be exported in the same terminal that runs <code>claude</code>. Check with <code>echo $ANTHROPIC_BASE_URL</code> — it must print <code>http://127.0.0.1:8080</code>.</li>
        <li><b>A port change does not take effect</b>: <code>listen</code> binds at startup only — exit the proxy and start it again. <code>responses_listen</code> hot-starts and hot-stops.</li>
        <li><b>The console will not open</b>: usually the main port is taken or the config is broken. Fix <code>listen</code> by editing the config file directly and restart. To chase startup errors, temporarily set <code>"log_file": "proxy.log"</code>.</li>
        <li><b>No retries visible</b>: the Logs tab should show <code>[attempt N] upstream response status: 429</code> and <code>[retry]</code> lines. If they are missing, the status code is not in <code>retry_status_codes</code>, or the retry budget is already spent. A deterministic 4xx (e.g. a 400 <code>invalid_request_error</code>: malformed JSON, unsigned thinking history) is not retried — after the semantic fallbacks get their one shot, the upstream error passes through with its body intact (when a keepalive stream is already open, the detail rides inside the SSE error event and the status column records the real code, not [retries exhausted]); 408/425/429 and every 5xx keep the in-body retry even when not listed.</li>
        <li><b>No window after launch</b>: normal. The program lives in the tray or menu bar; open the console from the icon's menu.</li>
        <li><b>Codex reports 503 with zero proxy log</b>: a system proxy hijacked 127.0.0.1. Add <code>127.0.0.1</code> to <code>NO_PROXY</code> and restart Codex. The Windows one-line script writes a user-level variable; the macOS script persists it into launchd.</li>
        <li><b>macOS will not open the app the first time</b>: the app is ad-hoc signed. Right-click it in Finder and choose Open once.</li>
        <li><b>A config save is rejected</b>: read the on-page error. Common causes are JSON syntax errors, undefined variable references, wrong enum values, or removed keys left in the config.</li>
        <li><b>A request returns 502</b>: no route matched and <code>upstream</code> is empty, or the upstream address is unreachable. Check the <code>[route]</code> line in the log to see where it went.</li>
        <li><b>Keep logs</b>: set <code>log_file</code>. By default logs stay in memory only, feeding the console's latest 500 lines.</li>
      </ul>
    </section>

    <section class="docSec" id="doc-removed" data-group="Troubleshooting">
      <h3>Removed config keys</h3>
      <p>These old top-level keys are gone. A config carrying them still starts and runs normally, but the behavior stays inactive: the tray icon turns red, and startup warnings on the Logs tab explain each key's replacement. Saving a config that still contains a removed key is rejected on the Config tab.</p>
      <div class="docTableWrap"><table>
        <tr><th>Old key</th><th>Replacement</th></tr>
        <tr><td><code>classifier_thinking_disabled</code></td><td>Write <code>"classifier_thinking": "off"</code> inside <code>classifier_route</code></td></tr>
        <tr><td><code>classifier_max_tokens</code></td><td>Just delete it. Classifier-request max_tokens is no longer modified</td></tr>
        <tr><td><code>translateNone2Low</code></td><td>Write <code>"convertOff2Low": "translate"</code> or <code>"all"</code> on routes entries, fast_route, multimodal_fallback or search_fallback</td></tr>
      </table></div>
    </section>
  </div>
</div>`
