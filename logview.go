package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 网页控制台挂载路径（与代理同端口，仅本机访问）。
// 选 /__ 前缀：Anthropic API 走 /v1/...，不冲突；Go DefaultServeMux 精确匹配优先于 / 通配。
const (
	logViewerPath     = "/__logs"
	logDataPath       = "/__logs/data"
	configPath        = "/__config"        // GET 取配置内容、POST 保存并重载
	reloadPath        = "/__reload"        // POST 仅重载（不改动文件）
	configsPath       = "/__configs"       // GET 列出当前配置目录下所有 .json（供切换）
	switchPath        = "/__switch"        // POST 切换到指定配置文件并即时生效
	newConfigPath     = "/__newconfig"     // POST 新建配置文件（空白模板）并切换
	codexSetupPS1Path = "/__codexsetup.ps1" // GET 烤制后的 codex-setup.ps1（Windows，irm|iex 拉取）
	codexSetupSHPath  = "/__codexsetup.sh"  // GET 烤制后的 codex-setup.sh（macOS/Linux，curl|bash 拉取）
	renameConfigPath  = "/__renameconfig"  // POST 重命名配置文件
	delConfigPath     = "/__delconfig"     // POST 删除配置文件（不允许删当前在用的）
	resetStatsPath    = "/__resetstats"    // POST 清空累计统计（切换配置不再自动清）
	clearLogsPath     = "/__clearlogs"     // POST 清空内存日志缓冲
	flightPath        = "/__flight"        // GET 在途流透传内容（?id=N，已完成流也查此）
	flightReqPath     = "/__flightreq"      // GET 流的下游请求体原文（?id=N，在途/已完成都查）
	recentFlightsPath = "/__recentflights" // GET 最近完成的流列表（摘要）
	finishedCapPath   = "/__finishedcap"   // POST 设置保留完成流个数 N
	fullStorePath     = "/__fullstore"      // POST 开关「储存完整结构体」（记录完整请求体/输出供下载）
)

// isLocalRequest 限制只有本机浏览器能访问控制台。
// 即使代理 listen 在 0.0.0.0 暴露到内网，远程请求 /__* 也返回 403，避免日志/配置泄露。
func isLocalRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	return host == "127.0.0.1" || host == "::1" || host == "localhost" || strings.HasPrefix(host, "127.")
}

// ---- 实时速率（字节/秒）----
// 控制台每 500ms 轮询一次 /__logs/data，用两次轮询间的 bytesForward 增量算速率。
// 状态保存在包级变量，跨请求延续。

var (
	rateMu        sync.Mutex
	prevRateBytes int64
	prevRateT     time.Time
	lastRate      int64
)

// computeRate 根据累计字节 b 与上次采样计算 bytes/s，并更新基线。
func computeRate(b int64) int64 {
	rateMu.Lock()
	defer rateMu.Unlock()
	now := time.Now()
	if prevRateT.IsZero() {
		prevRateBytes, prevRateT = b, now
		return lastRate
	}
	dt := now.Sub(prevRateT).Seconds()
	if dt > 0.05 {
		delta := b - prevRateBytes
		if delta < 0 {
			delta = 0 // 「清空统计」后累计字节回零，负增量按 0 计（否则速率卡显示负值）
		}
		lastRate = int64(float64(delta) / dt)
		prevRateBytes, prevRateT = b, now
	}
	return lastRate
}

// logViewerHandler 返回控制台 HTML 页（状态/日志/配置三标签）。
func logViewerHandler(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(logViewerHTML))
}

// flightInfo 是单个在途流的展示信息（网页「状态」标签的表格用）。
type flightInfo struct {
	ID          uint64 `json:"id"`
	Model       string `json:"model"`
	Phase       int    `json:"phase"`       // 0=等待响应, 1=已收到响应（开始转发）
	Bytes       int64  `json:"bytes"`       // 已转发字节
	Status      int    `json:"status"`      // HTTP 状态码（stage=stageForward 时显示）
	Stage       int32  `json:"stage"`       // 当前阶段：0=请求 1=路由 2=尝试N 3=转发(显示状态码)
	Attempt     int32  `json:"attempt"`     // 当前尝试序号（1 起），stage=2 时显示「尝试N」
	AttemptMs    int64  `json:"attemptMs"`             // 当前尝试已等首字节的毫秒数（黄灯旁 [尝试N:Xs]）；-1=重试退避中（显示 [退避中]）
	RouteReason  int32  `json:"routeReason"` // 路由原因：0=透传 1=pattern 2=分类器 3=fast 4=多模态 5=搜索
	Translated   string `json:"translated"`  // 翻译口来源（"responses"=翻译 / "responses-raw"=原生透传），API 列显示 [translate]/[Response]
	CountTokens  bool   `json:"countTokens,omitempty"` // count_tokens 探针流，model 列显示 [count_tokens] 前缀
	SearchPrompt string `json:"searchPrompt,omitempty"`
	ReqTrunc     bool   `json:"reqTrunc,omitempty"` // 请求体被截断只剩前 256KB（下载按钮置灰）
	HasFull      bool   `json:"hasFull,omitempty"`  // 仍持有完整输出副本（下载输出/交互式JSON 可用）
	StageMs      int64  `json:"stageMs"`            // 当前灯色已持续的毫秒数（状态灯旁显示，灯色变化才清零）
	Tools        string `json:"tools,omitempty"`    // 工具调用标签（"[Read*1][Edit*3]"，无工具省略；随转发实时累积）
	Stripped     int    `json:"stripped,omitempty"` // 剥掉的回放搜索块总数（时间规则+400 兜底；0 省略），model 列红标 [剥N]
}

// logData 是 /__logs/data 返回的 JSON：最近日志 + 全量状态计数 + 在途流列表。
type logData struct {
	Lines        []string          `json:"lines"`
	Active       int               `json:"active"`
	Waiting      int               `json:"waiting"`
	Version      string            `json:"version"`
	CacheRead    int64             `json:"cacheRead"`
	CacheCreation int64            `json:"cacheCreation"`
	InputTokens  int64             `json:"inputTokens"`
	OutputTokens int64             `json:"outputTokens"`
	ModelStats   []modelUsageEntry `json:"modelStats"`
	CacheObs      []cacheObsRow     `json:"cacheObs"` // 实测缓存时间（按上游 URL+模型），缓存命中弹窗第二表
	BytesForward int64             `json:"bytesForward"`
	Rate         int64             `json:"rate"` // bytes/s
	Retries      int64             `json:"retries"`
	Classifiers  int64             `json:"classifiers"`
	AvgFirstByte float64           `json:"avgFirstByte"` // ms
	Tps          float64           `json:"tps"`          // tok/s
	Flights      []flightInfo      `json:"flights"`
	CurrentCfg   string            `json:"currentCfg"`  // 当前生效的配置文件名
	FinishedCap  int32             `json:"finishedCap"` // 保留完成流个数 N（状态页可改）
	FullStore     bool              `json:"fullStore"`   // 「储存完整结构体」开关（状态页可切，默认关）

	// ClassifierNoThink 是关思考改写次数：只在实际改 body 关 thinking 时 +1。
	// （Classifiers 是命中分类器特征的请求数：无论是否分流/关思考都计。）
	ClassifierNoThink int64 `json:"classifierNoThink"`
}

// logDataHandler 返回最近 maxLogBuf 行日志和全量状态（JSON），供页面每 500ms 轮询。
func logDataHandler(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	d := logData{Lines: logBuf.window(0, maxLogBuf), Version: Version, CurrentCfg: filepath.Base(currentConfigPath()), FinishedCap: finishedCap.Load(), FullStore: fullStore.Load()}
	stats.mu.Lock()
	d.Active = stats.active
	d.Waiting = stats.waiting
	d.CacheRead = stats.cacheRead
	d.CacheCreation = stats.cacheCreation
	d.InputTokens = stats.inputTokens
	d.OutputTokens = stats.outputTokens
	stats.mu.Unlock()
	d.ModelStats = stats.snapshotModelStats()
	d.CacheObs = snapshotCacheObs()
	d.BytesForward = stats.bytesForward.Load()
	d.Rate = computeRate(d.BytesForward)
	d.Retries = stats.statusRetries.Load()
	d.Classifiers = stats.classifierHits.Load()
	d.ClassifierNoThink = stats.classifierRewrites.Load()
	d.AvgFirstByte, d.Tps = stats.recentLatency()

	// 在途流列表：按 ID 排序（snapshot 已排序），展示 model 与转发进度。
	for _, f := range flights.snapshot() {
		model := f.origModel
		if f.targetModel != "" && f.targetModel != f.origModel {
			model = f.origModel + " -> " + f.targetModel
		}
		d.Flights = append(d.Flights, flightInfo{
			ID:           f.id,
			Model:        model,
			Phase:        int(f.phase.Load()),
			Bytes:        f.bytes.Load(),
			Status:       f.status,
			Stage:        f.stage.Load(),
			Attempt:      f.attempt.Load(),
			AttemptMs:    f.attemptMs(),
			RouteReason:  f.routeReason.Load(),
			Translated:   f.translated,
			CountTokens:  f.countTokens,
			SearchPrompt: f.searchPrompt,
			Tools:        f.toolCallsTag(),             // 工具调用实时累积，在途流 model 列随 500ms 轮询逐步出现
			Stripped:     int(f.searchStripped.Load()), // 时间规则剥块在流建立时即入账，400 兜底随转发增补
			ReqTrunc:     f.reqTrunc(),
			HasFull:      f.hasFullContent(),
			StageMs:      f.stageMs(),
		})
	}
	if d.Flights == nil {
		d.Flights = []flightInfo{}
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(d)
}

// configGetHandler 返回配置文件路径与原始内容（JSON），供「配置」标签载入编辑器。
func configGetHandler(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	data, err := os.ReadFile(currentConfigPath())
	content := ""
	if err == nil {
		content = string(data)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"path":    currentConfigPath(),
		"content": content,
		"exists":  err == nil,
	})
}

// configPostHandler 保存编辑后的配置并重载。
// 先用 json.Unmarshal 校验是合法 JSON 且符合 Config 结构，校验通过才写盘——
// 避免把损坏的配置写到磁盘导致下次启动失败。写盘后调 reloadConfig 生效。
func configPostHandler(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "读取请求体失败: "+err.Error(), http.StatusBadRequest)
		return
	}
	// 先校验：合法 JSON 且能解析进 Config（未知字段忽略，类型错误会失败）。
	var probe Config
	if err := json.Unmarshal(body, &probe); err != nil {
		http.Error(w, "JSON 解析失败，未保存: "+err.Error(), http.StatusBadRequest)
		return
	}
	_, statErr := os.Stat(currentConfigPath()) // 保存前不存在 → 本次保存会新建文件，清单变化
	if err := os.WriteFile(currentConfigPath(), body, 0644); err != nil {
		http.Error(w, "写入文件失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := reloadConfig(); err != nil {
		// 文件已保存但重载失败（极少见，因上面已校验过）：旧配置仍在跑，告知用户。
		http.Error(w, "已保存但重载失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if os.IsNotExist(statErr) {
		// 当前配置文件此前不存在（如被外部删除），本次保存新建了它：通知托盘重建子菜单
		notifyTrayCfgChanged()
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// reloadHandler 仅重载配置（不改动文件），供「仅重载」按钮使用。
func reloadHandler(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	if err := reloadConfig(); err != nil {
		http.Error(w, "重载失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// configsHandler 列出当前配置目录下所有 .json 文件，供网页「配置」标签下拉切换。
// 返回当前文件名、目录、可用文件列表。限本机访问。
func configsHandler(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	cur := currentConfigPath()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"current": filepath.Base(cur),
		"dir":     filepath.Dir(cur),
		"files":   listConfigFiles(),
	})
}

// switchHandler 切换到指定配置文件并即时生效。body: {"name":"work.json"}。
// name 必须是纯文件名（不含路径分隔符/..），防路径穿越越权切到目录外文件。限本机访问。
func switchHandler(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "解析失败: "+err.Error(), http.StatusBadRequest)
		return
	}
	name := body.Name
	// 防路径穿越：只允许纯文件名，不含分隔符或 ..
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		http.Error(w, "无效的配置文件名", http.StatusBadRequest)
		return
	}
	full := filepath.Join(filepath.Dir(currentConfigPath()), name)
	if err := switchConfig(full); err != nil {
		http.Error(w, "切换失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "current": name})
}

// validConfigName 校验并规范化配置文件名：只允许纯文件名（无路径分隔符/..），
// 自动补 .json 后缀。返回规范化名与是否合法。
func validConfigName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return "", false
	}
	if !strings.HasSuffix(name, ".json") {
		name += ".json"
	}
	return name, true
}

// readConfigTemplate 返回新建配置用的空白模板。
// 直接用编译期内嵌的 config.example.json（configExampleBytes），保证始终与 exe 同版本最新，
// 不读磁盘副本--磁盘上的 config.example.json 可能是旧版未随 exe 更新，会导致新建出旧模板。
// 想看/改参考模板，看 release 目录里的 config.example.json 即可。
func readConfigTemplate() []byte {
	return configExampleBytes
}

// 烤制锚点：与模板文件里的写法逐字符一致（codex_setup_test.go 锁定它们在模板中各出现一次）。
// 值只经白名单校验后替换进单引号空串 '' 的位置，不可能引号逃逸。
var codexPS1Anchors = [5]string{"$BAKED_BASE_URL = ''", "$BAKED_MODEL    = ''", "$BAKED_CATALOG  = ''", "$BAKED_CONTEXT_WINDOW  = ''", "$BAKED_COMPACT_PERCENT = ''"}
var codexSHAnchors = [5]string{"BAKED_BASE_URL=''", "BAKED_MODEL=''", "BAKED_CATALOG=''", "BAKED_CONTEXT_WINDOW=''", "BAKED_COMPACT_PERCENT=''"}

// slug 白名单：模型名/目录条目允许字符（路由 pattern 里的 * 也允许）；不含引号、空白、URL 分隔符
var codexSlugRE = regexp.MustCompile(`^[A-Za-z0-9._*/:+-]{1,100}$`)

// base 白名单：http(s) 地址（主机可 IPv4/域名/IPv6 括号，路径可选）
var codexBaseURLRE = regexp.MustCompile(`^https?://[A-Za-z0-9.:\[\]-]+(/[A-Za-z0-9._~/-]*)?$`)

// ctx/compact 白名单：纯数字串（烤进脚本后作为 JSON 数字写入模型目录，不能带引号）
var codexDigitsRE = regexp.MustCompile(`^[0-9]{1,7}$`)

// codexScriptHandler 把 query 参数烤进脚本模板的 BAKED 锚点后下发——配置页给用户的
// 一行命令（irm '<url>' | iex / bash <(curl -fsSL '<url>')）拉取的就是烤制版，粘贴即跑：
//
//	?model=<slug>     默认模型（__restore__ = 直接还原；空 = 脚本内交互菜单）
//	?base=<url>       Responses 监听口地址（空 = 脚本内询问或用默认值）
//	?catalog=a,b,c    写进 Codex /model 菜单的模型清单（空 = 脚本内置演示模型）
//	?ctx=262144       目录声明的上下文窗口（空 = 262144；范围 4096..2000000）
//	?compact=95       自动压缩触发百分比（空 = 95；范围 1..99）
//
// isSH=true 时额外把 CRLF 归一成 LF（防御 Windows 检出 autocrlf；bash 不认 CR），
// 否则（ps1）剥掉模板文件的 BOM：irm|iex 按 HTTP charset 解码不需要 BOM，
// 且实测 iex 遇到串首 U+FEFF 会把它粘进第一条命令名报错（BOM 只对双击/文件执行有用）。
// 限本机访问（与其余 /__* 管理端点同规则）。
func codexScriptHandler(tpl []byte, anchors [5]string, isSH bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLocalRequest(r) {
			http.Error(w, "forbidden (local only)", http.StatusForbidden)
			return
		}
		q := r.URL.Query()
		model := q.Get("model")
		base := q.Get("base")
		catalog := q.Get("catalog")
		ctxWin := q.Get("ctx")
		compact := q.Get("compact")
		if model != "" && model != "__restore__" && !codexSlugRE.MatchString(model) {
			http.Error(w, "model 参数含非法字符", http.StatusBadRequest)
			return
		}
		if base != "" && (len(base) > 200 || !codexBaseURLRE.MatchString(base)) {
			http.Error(w, "base 参数不是合法的 http(s) 地址", http.StatusBadRequest)
			return
		}
		if catalog != "" {
			slugs := strings.Split(catalog, ",")
			if len(slugs) > 64 || len(catalog) > 4000 {
				http.Error(w, "catalog 参数过长", http.StatusBadRequest)
				return
			}
			for _, s := range slugs {
				if !codexSlugRE.MatchString(s) {
					http.Error(w, "catalog 参数含非法字符: "+s, http.StatusBadRequest)
					return
				}
			}
		}
		// ctx/compact 烤进脚本后直接当 JSON 数字用，这里把范围卡死（窗口 4k..2M，压缩 1..99%）
		if ctxWin != "" {
			if n, err := strconv.Atoi(ctxWin); !codexDigitsRE.MatchString(ctxWin) || err != nil || n < 4096 || n > 2000000 {
				http.Error(w, "ctx 参数需为 4096..2000000 的整数", http.StatusBadRequest)
				return
			}
		}
		if compact != "" {
			if n, err := strconv.Atoi(compact); !codexDigitsRE.MatchString(compact) || err != nil || n < 1 || n > 99 {
				http.Error(w, "compact 参数需为 1..99 的整数", http.StatusBadRequest)
				return
			}
		}
		out := tpl
		if isSH {
			out = bytes.ReplaceAll(out, []byte("\r\n"), []byte("\n"))
		} else {
			out = bytes.TrimPrefix(out, []byte{0xEF, 0xBB, 0xBF})
		}
		for i, v := range [5]string{base, model, catalog, ctxWin, compact} {
			if v == "" {
				continue
			}
			a := anchors[i]
			// 锚点以空串 '' 结尾，换成带值的单引号串（值已过白名单，无单引号）
			out = bytes.Replace(out, []byte(a), []byte(strings.TrimSuffix(a, "''")+"'"+v+"'"), 1)
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(out)
	}
}

// newConfigHandler 新建配置文件：用 config.example.json 作为空白模板写入指定文件名，
// 创建成功后立即切换到新配置。文件已存在则拒绝。限本机访问。
func newConfigHandler(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "解析失败: "+err.Error(), http.StatusBadRequest)
		return
	}
	name, ok := validConfigName(body.Name)
	if !ok {
		http.Error(w, "无效的配置文件名（仅纯文件名，不含路径）", http.StatusBadRequest)
		return
	}
	full := filepath.Join(filepath.Dir(currentConfigPath()), name)
	if _, err := os.Stat(full); err == nil {
		http.Error(w, "文件已存在: "+name, http.StatusConflict)
		return
	}
	if err := os.WriteFile(full, readConfigTemplate(), 0644); err != nil {
		http.Error(w, "写入失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := switchConfig(full); err != nil {
		http.Error(w, "已创建但切换失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("[新建] %s", full)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "current": name})
}

// renameConfigHandler 重命名配置文件。若重命名的是当前在用的配置，同步更新 configFilePath，
// 使后续保存/重载指向新文件。目标名已存在则拒绝。限本机访问。
func renameConfigHandler(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "解析失败: "+err.Error(), http.StatusBadRequest)
		return
	}
	oldName, ok := validConfigName(body.Old)
	if !ok {
		http.Error(w, "无效的原文件名", http.StatusBadRequest)
		return
	}
	newName, ok := validConfigName(body.New)
	if !ok {
		http.Error(w, "无效的新文件名", http.StatusBadRequest)
		return
	}
	dir := filepath.Dir(currentConfigPath())
	oldFull := filepath.Join(dir, oldName)
	newFull := filepath.Join(dir, newName)
	if _, err := os.Stat(oldFull); err != nil {
		http.Error(w, "原文件不存在: "+oldName, http.StatusBadRequest)
		return
	}
	if _, err := os.Stat(newFull); err == nil {
		http.Error(w, "目标已存在: "+newName, http.StatusConflict)
		return
	}
	if err := os.Rename(oldFull, newFull); err != nil {
		http.Error(w, "重命名失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// 若重命名的是当前配置，更新 configFilePath，使后续读写指向新文件
	configMu.Lock()
	renamed := configFilePath == oldFull
	if renamed {
		configFilePath = newFull
	}
	configMu.Unlock()
	if renamed {
		writeActiveConfigState(newFull)
	}
	log.Printf("[重命名] %s -> %s", oldName, newName)
	// 文件名清单变了（若改的是当前配置，托盘勾选也跟着新名字走）：通知托盘重建子菜单
	notifyTrayCfgChanged()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "current": filepath.Base(currentConfigPath())})
}

// delConfigHandler 删除配置文件。不允许删除当前在用的配置（需先切换到别的）。
// 限本机访问。前端做两次确认防误操作。
func delConfigHandler(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "解析失败: "+err.Error(), http.StatusBadRequest)
		return
	}
	name, ok := validConfigName(body.Name)
	if !ok {
		http.Error(w, "无效的配置文件名", http.StatusBadRequest)
		return
	}
	full := filepath.Join(filepath.Dir(currentConfigPath()), name)
	if full == currentConfigPath() {
		http.Error(w, "不能删除当前在用的配置，请先切换到别的配置", http.StatusBadRequest)
		return
	}
	if _, err := os.Stat(full); err != nil {
		http.Error(w, "文件不存在: "+name, http.StatusBadRequest)
		return
	}
	if err := os.Remove(full); err != nil {
		http.Error(w, "删除失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("[删除] %s", full)
	// 通知托盘重建「切换配置」子菜单去掉被删项（与 switchConfig 成功路径同一个通知）
	notifyTrayCfgChanged()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// resetStatsHandler 清空累计统计（bytes/tokens/retries/classifierRewrites/延迟样本）
// 与「最近完成的流」列表。切换配置不再自动清统计，需手动点「清空统计」按钮。限本机访问。
func resetStatsHandler(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	clearStats(cfg.Load())
	log.Printf("[统计] 已清空")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// clearLogsHandler 清空内存日志缓冲（仅网页查看用，不影响 log_file 落盘文件）。限本机访问。
func clearLogsHandler(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	logBuf.clear()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// flightHandler 返回指定在途流最近透传的内容（SSE 原文），供网页点击在途流查看。
// 流不存在（已结束）返回 404。限本机访问。
func flightHandler(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	id, err := strconv.ParseUint(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		http.Error(w, "无效的 id", http.StatusBadRequest)
		return
	}
	if r.URL.Query().Get("full") == "1" {
		// 下载输出原文：完整内容只在「储存完整结构体」开启时记录。
		if !fullStore.Load() {
			http.Error(w, "储存完整结构体未开启", http.StatusForbidden)
			return
		}
		flights.mu.RLock()
		f0, ok0 := flights.m[id]
		flights.mu.RUnlock()
		var full []byte
		if ok0 {
			full = f0.snapshotFullContent()
		} else {
			finishedMu.Lock()
			for _, ff := range finished {
				if ff.id == id {
					full = ff.fullContent
					break
				}
			}
			finishedMu.Unlock()
		}
		if len(full) == 0 {
			http.Error(w, "该流未记录完整输出（开启前已开始或无透传内容）", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(full)
		return
	}
	flights.mu.RLock()
	f, ok := flights.m[id]
	flights.mu.RUnlock()
	var content []byte
	if ok {
		content = f.snapshotContent()
	} else {
		// 不在途：查最近完成流存档（content 是静态快照）。
		finishedMu.Lock()
		for _, ff := range finished {
			if ff.id == id {
				content = ff.content
				break
			}
		}
		finishedMu.Unlock()
		if content == nil {
			http.Error(w, "流已结束或不存在", http.StatusNotFound)
			return
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(content)
}

// flightReqHandler 返回指定流的下游请求体原文，供网页查看"什么请求导致这个流"。
// 在途/已完成都查；流不存在或未记录（搜索子流、body 读取失败）返回 404。限本机访问。
func flightReqHandler(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	id, err := strconv.ParseUint(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		http.Error(w, "无效的 id", http.StatusBadRequest)
		return
	}
	flights.mu.RLock()
	f, ok := flights.m[id]
	flights.mu.RUnlock()
	var body []byte
	if ok {
		body = f.snapshotReqBody()
	} else {
		// 不在途：查最近完成流存档（reqBody 是静态快照）。
		finishedMu.Lock()
		for _, ff := range finished {
			if ff.id == id {
				body = ff.reqBody
				break
			}
		}
		finishedMu.Unlock()
	}
	if body == nil {
		http.Error(w, "流不存在或未记录请求体", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

// fmtCacheAge 把缓存年龄（缓存创建至今的时长）格式化为 m:ss（≥1h 时 h:mm:ss）。
func fmtCacheAge(d time.Duration) string {
	s := int(d.Seconds())
	if h := s / 3600; h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, s%3600/60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// fmtObsDur 把实测间隔格式化为 mm:ss（"00:30" / "23:30"，分钟零垫两位；超 99 分钟自然扩展为 "102:00"）。
func fmtObsDur(d time.Duration) string {
	s := int(d.Seconds())
	return fmt.Sprintf("%02d:%02d", s/60, s%60)
}

// recentFlightsHandler 返回最近完成流列表（摘要，不含 content），最新在前。限本机访问。
// cacheAge 列语义：每锚定键（会话+路由）的最新一行显示缓存年龄（缓存创建至今的时长，递增）；
// 被同键更新行刷新的旧行与无会话标识的行（count_tokens/裸 API）显示 "-"；
// 同键有在途流吐字时冻结显示 [m:ss]（在途流开始那一刻旧锚的年龄，数字不变）。
func recentFlightsHandler(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	// 同键有在途流正在吐字（stage=3 转发中）时，该键缓存实际刚被上游刷新——上一完成行
	// 冻结显示刷新发生那一刻（在途流开始时刻）旧锚的年龄，形如 [4:32]；新锚等该在途流
	// 完成归档后正式生效。同键并发多条吐字流取最早开始的（首次刷新时刻）。
	streaming := make(map[string]time.Time)
	for _, f := range flights.snapshot() {
		if f.stage.Load() == 3 {
			if k := convKeyOf(f); k != "" {
				if ts, ok := streaming[k]; !ok || f.start.Before(ts) {
					streaming[k] = f.start
				}
			}
		}
	}
	finishedMu.Lock()
	now := time.Now()
	// 每键最新行下标（finished 升序，后者覆盖前者）。被裁后旧行可能"升级"为可见最新，
	// 显示它自己的年龄——存活期是动态的，年龄大只说明该会话很久没写缓存。
	latest := make(map[string]int)
	for i := range finished {
		if finished[i].convKey != "" {
			latest[finished[i].convKey] = i
		}
	}
	out := make([]map[string]any, 0, len(finished))
	for i := len(finished) - 1; i >= 0; i-- {
		ff := finished[i]
		cacheAge := "-"
		if ff.convKey != "" && latest[ff.convKey] == i {
			if ts, ok := streaming[ff.convKey]; ok {
				cacheAge = "[" + fmtCacheAge(ts.Sub(ff.start)) + "]"
			} else {
				cacheAge = fmtCacheAge(now.Sub(ff.start))
			}
		}
		out = append(out, map[string]any{
			"id":          ff.id,
			"model":       ff.model,
			"routeReason": ff.routeReason,
			"translated":  ff.translated,
			"countTokens": ff.countTokens,
			"status":      ff.status,
			"gaveUp":        ff.gaveUp,
			"bytes":       ff.bytes,
			"total":         fmtMs(ff.totalMs),
			"stage":       ff.stage,
			"ended":       ff.ended.Format("15:04:05"),
			"hitRate":     cacheHitRate(ff.cacheRead, ff.inTokens, ff.cacheCreation),
			"cacheAge":      cacheAge,
			"cacheRead":   ff.cacheRead,
			"cacheCreation": ff.cacheCreation,
			"inTokens":    ff.inTokens,
			"firstByte":   fmtFirstByte(ff.firstByteMs),
			"tps":         fmtTps(ff.tps),
			"searchPrompt": ff.searchPrompt,
			"tools":         ff.tools,
			"stripped":      ff.searchStripped,
			"reqTrunc":      ff.reqTruncated,
			"hasFull":       ff.fullContent != nil,
		})
	}
	finishedMu.Unlock()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"list": out})
}

// finishedCapHandler 设置保留完成流个数 N（0..200），并立即裁剪 finished。限本机访问。
func finishedCapHandler(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var b struct {
		N int `json:"n"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, "无效的 JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if b.N < 0 {
		b.N = 0
	}
	if b.N > 200 {
		b.N = 200
	}
	finishedCap.Store(int32(b.N))
	finishedMu.Lock()
	trimFinishedLocked()
	finishedMu.Unlock()
	log.Printf("[完成流] 保留个数设为 %d", b.N)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "n": b.N})
}

// fullStoreHandler 开关「储存完整结构体」：开启后新开始的请求记录完整请求体与输出
// （不设 256KB 上限，供下载）；关闭立即清空在途/存档里的完整副本（reqBody 截回 cap），
// 网页下载按钮随之消失。限本机访问。
func fullStoreHandler(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var b struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, "无效的 JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	fullStore.Store(b.Enabled)
	if !b.Enabled {
		// 关闭即释放完整副本：在途流逐个清理，存档同理。
		for _, f := range flights.snapshot() {
			f.purgeFull()
		}
		finishedMu.Lock()
		for i := range finished {
			finished[i].fullContent = nil
			if len(finished[i].reqBody) > flightContentCap {
				finished[i].reqBody = finished[i].reqBody[:flightContentCap]
				finished[i].reqTruncated = true
			}
		}
		finishedMu.Unlock()
	}
	log.Printf("[完整结构体] 储存开关设为 %v", b.Enabled)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "enabled": b.Enabled})
}

const logViewerHTML = `<!DOCTYPE html>
<html lang="zh">
<head>
<meta charset="utf-8">
<title>Proxy429 控制台</title>
<style>
  * { box-sizing: border-box; }
  body { margin:0; background:#1e1e1e; color:#d4d4d4; font-family:Menlo,Consolas,"Courier New",monospace; font-size:12px; }
  #bar { position:sticky; top:0; z-index:2; background:#252526; border-bottom:1px solid #333; display:flex; gap:0; align-items:stretch; }
  .tab { padding:8px 16px; color:#9a9a9a; cursor:pointer; border-right:1px solid #333; }
  .tab.active { color:#d4d4d4; background:#1e1e1e; border-bottom:2px solid #4ec9b0; }
  #hint { margin-left:auto; align-self:center; padding:0 12px; color:#6a6a6a; }
  .pane { display:none; padding:10px 14px; }
  .pane.active { display:block; }
  .st { font-weight:bold; }
  .idle { color:#969696; } .wait { color:#d9b34a; } .active { color:#4ec9b0; } .dead { color:#c75; }
  .cards { display:flex; flex-wrap:wrap; gap:8px; margin-bottom:12px; }
  .card { background:#252526; border:1px solid #333; border-radius:4px; padding:8px 12px; min-width:96px; }
  .card .k { color:#9a9a9a; font-size:11px; }
  .card .v { color:#d4d4d4; font-size:16px; margin-top:2px; }
  table { border-collapse:collapse; width:100%; }
  th,td { text-align:left; padding:4px 8px; border-bottom:1px solid #2a2a2a; }
  th { color:#9a9a9a; font-weight:normal; }
  .thq { display:inline-block; width:13px; height:13px; line-height:12px; margin-left:3px; border:1px solid #6a6a6a; border-radius:50%; color:#9a9a9a; font-size:10px; text-align:center; cursor:help; vertical-align:1px; }
  #log { white-space:pre-wrap; word-break:break-word; line-height:1.45; }
  #cfg { width:100%; min-height:60vh; background:#1e1e1e; color:#d4d4d4; border:1px solid #333; border-radius:4px; padding:8px; font:inherit; }
  #codexBox { margin-top:14px; border-top:1px solid #2a2a2a; padding-top:10px; }
  #codexBox .t { color:#9a9a9a; margin-bottom:6px; }
  #codexBox .dim { color:#777; font-size:12px; margin-top:4px; }
  #codexGen .cmdRow { display:flex; align-items:center; gap:6px; margin:4px 0; }
  #codexGen .cmdRow .os { flex:none; width:165px; color:#9a9a9a; font-size:12px; }
  #codexGen .cmdRow code { flex:1; min-width:0; background:#181818; color:#8ec88e; border:1px solid #333; border-radius:4px; padding:6px 8px; font:12px/1.4 Consolas,monospace; white-space:nowrap; overflow-x:auto; }
  #codexModelCustom { background:#1e1e1e; color:#d4d4d4; border:1px solid #333; border-radius:3px; padding:4px 6px; font:inherit; }
  #codexEnable { background:#2a2410; border:1px solid #6b5d1f; border-radius:4px; padding:8px 10px; margin-bottom:8px; color:#d8c27a; }
  #codexEnable .dim { color:#9a8a55; font-size:12px; margin-top:4px; }
  #codexGen.off { opacity:.35; pointer-events:none; }
  button { background:#264f78; color:#d4d4d4; border:1px solid #3a6ea5; border-radius:3px; padding:5px 12px; cursor:pointer; font:inherit; }
  button:hover { background:#2f6090; }
  button.ghost { background:#333; border-color:#444; }
  button.ghost:disabled { opacity:0.4; cursor:not-allowed; }
  button.danger { background:#5a2a2a; border-color:#7a3a3a; color:#ffb0b0; }
  button.danger:hover { background:#6a3a3a; }
  #cfgMsg { margin-left:10px; }
  .ok { color:#4ec9b0; } .err { color:#c75; }
  #cfgPath { color:#6a6a6a; margin-bottom:8px; }
  /* 解析视图内容块样式 */
  .ss-think { color:#9a9a9a; font-style:italic; white-space:pre-wrap; word-break:break-all; }
  .ss-tool { margin:4px 0; padding:4px 6px; background:#2a2a2a; border-left:3px solid #4ec9b0; white-space:pre-wrap; word-break:break-all; }
  .ss-tool b { color:#4ec9b0; }
  .ss-tool code { color:#9cdcfe; white-space:pre-wrap; word-break:break-all; }
  .ss-search { margin:4px 0; padding:4px 6px; background:#2a2515; border-left:3px solid #dcdcaa; white-space:pre-wrap; word-break:break-all; }
  .ss-search .ss-url { color:#569cd6; }
  .ss-empty { color:#9a9a9a; }
  .ss-error { margin:4px 0; padding:4px 6px; background:#3a1a1a; border-left:3px solid #f48771; color:#f48771; white-space:pre-wrap; word-break:break-all; }
  /* 交互式 JSON 树（流查看器「交互式JSON」按钮） */
  .jt-line { line-height:1.5; }
  .jt-kids { margin-left:18px; border-left:1px dotted #333; }
  .jt-head { cursor:pointer; user-select:none; }
  .jt-head:hover { background:#2a2a2a; }
  .jt-arrow { color:#888; display:inline-block; width:14px; }
  .jt-key { color:#9cdcfe; }
  .jt-str { color:#ce9178; white-space:pre-wrap; word-break:break-all; }
  .jt-num { color:#b5cea8; }
  .jt-bool { color:#569cd6; }
  .jt-null { color:#808080; }
  .jt-count { color:#808080; }
  /* 使用文档弹窗 */
  .doc h3 { color:#4ec9b0; margin:14px 0 4px; font-size:13px; }
  .doc p { margin:4px 0; color:#c0c0c0; }
  .doc ul { margin:4px 0 4px 18px; color:#c0c0c0; }
  .doc li { margin:2px 0; }
  .doc code { background:#2a2a2a; padding:1px 5px; border-radius:3px; color:#9cdcfe; }
  .doc b { color:#d4d4d4; }
  /* 删除配置弹窗 */
  #cfgDelModal { position:fixed; inset:0; background:rgba(0,0,0,.55); z-index:100; display:none; align-items:center; justify-content:center; }
  #cfgDelModal .box { background:#252526; border:1px solid #444; border-radius:6px; padding:16px; min-width:320px; }
  #cfgDelModal .t { margin-bottom:10px; color:#d4d4d4; }
  #cfgDelModal select { width:100%; background:#1e1e1e; color:#d4d4d4; border:1px solid #444; border-radius:3px; padding:5px; font:inherit; }
  #cfgDelModal .err { color:#c75; margin-top:8px; min-height:16px; }
  #cfgDelModal .btns { margin-top:14px; display:flex; gap:8px; justify-content:flex-end; }
</style>
</head>
<body>
<div id="bar">
  <div class="tab active" data-tab="status">状态</div>
  <div class="tab" data-tab="logs">日志</div>
  <div class="tab" data-tab="config">配置</div>
  <span id="hint">关闭此标签页即隐藏 · 代理继续运行</span>
  <button id="docBtn" class="ghost" style="align-self:center;margin:0 12px" onclick="document.getElementById('docModal').style.display='flex'">文档</button>
</div>

<div class="pane active" id="pane-status">
  <div style="margin-bottom:8px"><span class="st idle" id="status">⚪ 连接中</span> <span id="curCfg" style="margin-left:10px;color:#888"></span>
    <button id="resetStatsBtn" class="ghost" style="margin-left:16px;padding:2px 8px">清空统计</button>
    <label style="margin-left:12px;color:#9a9a9a">保留完成流: <input id="finishedCapInput" type="number" min="0" max="200" value="10" style="width:50px;background:#1e1e1e;color:#d4d4d4;border:1px solid #333;border-radius:3px;padding:2px 4px;font:inherit"></label>
    <button id="finishedCapBtn" class="ghost" style="padding:2px 8px">设置</button>
    <span id="finishedCapMsg"></span>
    <label style="margin-left:12px;color:#9a9a9a;font-weight:normal" title="开启后新开始的请求记录完整请求体与输出（不设 256KB 上限），流查看器出现「下载请求体/下载输出」按钮；关闭立即清空已存的完整副本，仅能浏览截断内容"><input type="checkbox" id="fullStoreChk"> 储存完整结构体</label>
  </div>
  <div class="cards" id="cards"></div>
  <div>在途流 <label style="margin-left:8px;color:#9a9a9a;font-weight:normal"><input type="checkbox" id="autoTrackChk" checked>自动跟踪最新</label> <label style="color:#9a9a9a;font-weight:normal">最多显示 <input type="number" id="maxFlightsInput" min="1" max="20" value="3" style="width:40px;background:#1e1e1e;color:#d4d4d4;border:1px solid #333;border-radius:3px;padding:2px 4px;font:inherit"> 个</label></div>
  <table id="flights"><thead><tr><th></th><th>#</th><th>model</th><th>API</th><th>字节</th><th>状态</th></tr></thead><tbody></tbody></table>
  <div id="flightViewWrap" style="display:none;margin-top:8px">
    <div>流 #<span id="flightViewId"></span> <span id="flightViewKind">输出</span> <button id="flightViewWhatBtn" class="ghost" style="display:none">看请求体</button> <button id="flightViewRawBtn" class="ghost">显示原始</button> <button id="flightViewDlBtn" class="ghost" style="display:none">下载请求体</button> <button id="flightViewDlOutBtn" class="ghost" style="display:none">下载输出</button> <button id="flightViewTreeBtn" class="ghost" style="display:none">交互式JSON</button> <button id="flightViewClose" class="ghost">关闭</button></div>
    <div id="flightView" style="max-height:300px;overflow:auto;background:#1e1e1e;border:1px solid #333;padding:8px"></div>
  </div>
  <div style="margin-top:10px">最近完成的流</div>
  <table id="finishedFlights"><thead><tr><th>#</th><th>model</th><th>API</th><th>字节</th><th>总时间</th><th>状态码</th><th>缓存命中</th><th>缓存年龄<span class="thq" title="该会话+路由最近一次缓存写入距现在的时长（m:ss 递增）。显示方括号且数字冻结（如 [4:32]）时：同会话同路由有一条正在进行的流刚刷新了缓存，方括号内是刷新那一刻的年龄；该流完成后恢复递增（新锚）。上游缓存真实存活期是动态的——点击「缓存命中」卡片，弹窗内有各上游（按 URL+模型归类）实测的缓存存活时间可对照">?</span></th><th>首字</th><th>tok/s</th><th>结束</th></tr></thead><tbody></tbody></table>
</div>

<div class="pane" id="pane-logs">
  <div style="margin-bottom:6px"><span id="count">0</span> 行 · 滚轮翻历史，自动滚到底 <span style="color:#9a9a9a">（内存仅留最近 500 行，新的覆盖旧的；不会随时间堆积。落盘日志另看 log_file）</span></div>
  <pre id="log"></pre>
</div>
<button id="clearLogsBtn" class="ghost" style="position:fixed;right:16px;bottom:16px;z-index:20;display:none">清空日志</button>

<div id="cacheTip" style="display:none;position:fixed;z-index:35;background:#1a1a1a;border:1px solid #555;border-radius:6px;padding:8px 10px;max-width:540px;box-shadow:0 4px 12px rgba(0,0,0,0.5);font-size:12px"></div>
<div id="cacheModal" style="display:none;position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:40;align-items:center;justify-content:center" onclick="if(event.target===this)this.style.display='none'">
  <div style="background:#1a1a1a;border:1px solid #555;border-radius:8px;padding:20px 24px;max-width:720px;max-height:80vh;overflow:auto;position:relative">
    <button class="ghost" style="position:absolute;top:10px;right:12px" onclick="document.getElementById('cacheModal').style.display='none'">关闭</button>
    <div style="font-size:15px;margin-bottom:12px;color:#d4d4d4">缓存命中明细（按真实上游模型） <span id="cacheModalTotal" style="color:#888;font-size:12px;margin-left:8px"></span></div>
    <div id="cacheModalBody"></div>
  </div>
</div>
<div id="retryTip" style="display:none;position:fixed;z-index:35;background:#1a1a1a;border:1px solid #555;border-radius:6px;padding:8px 10px;max-width:540px;box-shadow:0 4px 12px rgba(0,0,0,0.5);font-size:12px"></div>
<div id="retryModal" style="display:none;position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:40;align-items:center;justify-content:center" onclick="if(event.target===this)this.style.display='none'">
  <div style="background:#1a1a1a;border:1px solid #555;border-radius:8px;padding:20px 24px;max-width:720px;max-height:80vh;overflow:auto;position:relative">
    <button class="ghost" style="position:absolute;top:10px;right:12px" onclick="document.getElementById('retryModal').style.display='none'">关闭</button>
    <div style="font-size:15px;margin-bottom:12px;color:#d4d4d4">重试明细（按路由目标模型） <span id="retryModalTotal" style="color:#888;font-size:12px;margin-left:8px"></span></div>
    <div id="retryModalBody"></div>
  </div>
</div>
<div id="clsTip" style="display:none;position:fixed;z-index:35;background:#1a1a1a;border:1px solid #555;border-radius:6px;padding:8px 10px;max-width:540px;box-shadow:0 4px 12px rgba(0,0,0,0.5);font-size:12px"></div>
<div id="clsModal" style="display:none;position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:40;align-items:center;justify-content:center" onclick="if(event.target===this)this.style.display='none'">
  <div style="background:#1a1a1a;border:1px solid #555;border-radius:8px;padding:20px 24px;max-width:720px;max-height:80vh;overflow:auto;position:relative">
    <button class="ghost" style="position:absolute;top:10px;right:12px" onclick="document.getElementById('clsModal').style.display='none'">关闭</button>
    <div style="font-size:15px;margin-bottom:12px;color:#d4d4d4">分类器明细 <span id="clsModalTotal" style="color:#888;font-size:12px;margin-left:8px"></span></div>
    <div id="clsModalBody"></div>
  </div>
</div>

<div id="docModal" style="display:none;position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:40;align-items:center;justify-content:center" onclick="if(event.target===this)this.style.display='none'">
  <div style="background:#1a1a1a;border:1px solid #555;border-radius:8px;padding:20px 24px;max-width:780px;max-height:85vh;overflow:auto;position:relative">
    <button class="ghost" style="position:absolute;top:10px;right:12px" onclick="document.getElementById('docModal').style.display='none'">关闭</button>
    <div style="font-size:15px;margin-bottom:14px;color:#d4d4d4;font-weight:bold">使用文档</div>
    <div class="doc">
      <h3>全局流式化 convertAlltoStream</h3>
      <p>顶层配置 <code>convertAlltoStream</code>（默认 false）开启后，所有非流式请求（<code>stream:false</code> 或省略）都被代理悄悄改为流式发给上游：在途流页面实时可见吐字、统计首字与 tok/s。请求方无感知——代理把上游流完整收完后，<b>原样重建</b>非流式 JSON（所有内容块按流里原样拼回，含搜索结果 encrypted_content）一次性返回，调用方拿到的仍是它预期的非流式响应。流中途断开（未见 message_stop）时未向客户端写任何内容，代理整体重试。</p>
      <p>仅作用于 Anthropic Messages 请求（/v1/messages）；已是流式的请求、搜索摘要模式不受影响。重试等待期间不发 SSE 保活 ping（会污染非流式响应），静默等待。</p>
      <h3>Responses API 监听口 responses_listen</h3>
      <p>顶层配置 <code>responses_listen</code>（空 = 不启用；配置模板默认演示 <code>127.0.0.1:8081</code>）设为如 <code>127.0.0.1:8081</code> 后，代理在该地址额外开一个 OpenAI Responses API 端点（<code>/v1/responses</code>）：把 Codex CLI 等只说 Responses 协议的工具接到 Anthropic 上游。请求被翻译成 Anthropic Messages 走主管线（路由/重试/本控制台监控照常生效），响应翻译回 Responses（客户端 stream:true 拿 SSE 事件流，false 拿一次性 JSON）。</p>
      <p>工具里的 model 名照常参与路由匹配：在 routes 加一条如 <code>gpt-5*</code> 即可指定上游与改写模型。改动保存重载即生效（监听口随配置动态启停）；与主端口一样永远仅本机可连。</p>
      <p><b>原生透传 url_response_api</b>：route 条目配了 <code>url_response_api</code> 后，Responses 口命中该 route 的请求<b>不再翻译</b>，Responses 原文直接透传到该字段指定的原生 Responses 上游（base 填到 <code>…/coding</code> 或 <code>…/v1</code> 即可，代理自动补 <code>/v1/responses</code>；model 改写、key 覆盖、重试与本控制台监控统计照常）。适合 OpenAI 官方等原生完整实现 Responses 的上游——少一层翻译，缓存与计费口径和官方直连一致。已知限制：透传不改请求体（仅路由 model 改写照常），上游须接受客户端原样发来的全部工具——Codex 桌面端恒带的 tool_search 目前被 Kimi 拒绝（400），Kimi 待其支持后启用、期间走 url 翻译口。API 列一眼区分协议来源：橙色 <code>[Anthropic]</code> = Anthropic 口原生流量，绿色 <code>[Response]</code> = 这种透传流，紫色 <code>[translate]</code> = 翻译流。只影响 Responses 口：Anthropic 口（Claude Code）命中同一条 route 仍走该 route 的 <code>url</code> 字段，互不影响。</p>
      <p>翻译规则与 cc-switch 3.20.0 一致：<code>reasoning.effort</code> 按模型分类映射——adaptive 模型（fable-5/mythos-5/mythos-preview/sonnet-5/opus-4-8/4-7/4-6/sonnet-4-6）翻成 <code>thinking:adaptive</code> + <code>output_config.effort</code>（fable-5/mythos-5 关不掉 thinking，显式 none 翻成 effort:low）；其余模型翻成 budget_tokens（low 2048 / medium 8192 / high 16384 / xhigh·max·ultra 24576）。查表用客户端发来的 model 名（路由改写之前），想让表生效就把客户端 model 直接填目标模型名。工具映射（function/custom/namespace/tool_search/web_search/input_file）与完整映射表见使用说明.md「Responses 翻译映射表」。</p>
      <p><b>思考/搜索信封</b>：签名 thinking 块与每次搜索的完整结果块（含 encrypted_content 正文）被自封装进 reasoning 项的 encrypted_content 随响应发给客户端，下轮客户端回放历史时还原上行——思考链不丢，追问直接读上次搜索到的正文、不再原关键字重搜。搜索信封带 url+key 哈希归属且整体经 key 派生掩码混淆（客户端历史里不躺明文 url/key 信息）：换了上游或 key 就解不开不还原（省 token），换模型不拦（实测照常解密）；旧对话的搜索块在上游过期（报 tool_call_id）时代理自动剥掉回放块重试一次，无感降级为需要时重新搜。</p>
      <p><b>Codex CLI 接入</b>：最省事——本控制台「配置」标签下方给出 Windows / macOS·Linux 两行一键命令（DeepSeek 文档同款格式，按编辑框实时生成：地址取 <code>responses_listen</code>；routes 每个 pattern 的代表名全部写进 Codex <code>/model</code> 菜单，下拉选中项为默认模型），复制到对应终端回车即运行，脚本由本代理实时烤制下发。仓库根目录另有交互版 <code>codex-setup.ps1</code>（Windows）与 <code>codex-setup.sh</code>（macOS/Linux）：选模型、备份后改写 config.toml、写模型目录、可一键还原。手动：编辑 <code>~/.codex/config.toml</code>——顶层 <code>model_provider = "proxy429"</code>、<code>model = "gpt-5-codex"</code>、<code>preferred_auth_method = "apikey"</code> + <code>forced_login_method = "api"</code>（免官方登录），加 <code>[model_providers.proxy429]</code> 段（<code>base_url = "http://127.0.0.1:8081/v1"</code>、<code>wire_api = "responses"</code>、<code>experimental_bearer_token</code> 填任意占位串）。改完重启 Codex。<b>503 且代理侧零日志</b>：系统代理或终端代理变量会把 127.0.0.1 的请求劫到代理服务器报 503——Windows 一键脚本安装时已自动写用户级 NO_PROXY（含 127.0.0.1）绕过；macOS 脚本自动把 NO_PROXY 写进 launchd 环境（GUI 应用与新终端窗口都生效）并安装登录项持久化（脚本选「还原」可撤销）；Linux 脚本只做体检并提示 <code>export NO_PROXY="localhost,127.0.0.1,::1"</code>；手动配置请自行 <code>setx NO_PROXY "localhost,127.0.0.1,::1"</code>（Windows）后重启 Codex。逐步教程见使用说明.md「让 Codex CLI 走代理」。</p>
      <h3>路由与能力兜底</h3>
      <p>请求按顺序匹配上游：classifier_route（分类器分流）→ fast_route（快速直连）→ routes（按 model pattern 匹配）。命中 route 后，若该上游能力不足（text_only 缺图片 / no_search 缺搜索）按以下处理：</p>
      <ul>
      <li>搜索请求（带 web_search）→ 走 search_fallback：开了 summary_mode 则代理做 step1 搜索 + step2 摘要两步自构响应，否则整请求转发给 search_fallback 上游自己搜索回答。两种都是同一个 search_fallback 配置，不是独立路由。</li>
      <li>纯图片请求（无搜索）→ 走 multimodal_fallback；没配则透传原 route。</li>
      <li>搜索没配 search_fallback、或纯图片没配 multimodal_fallback → 降级透传原 route（上游可能报错）。</li>
      </ul>
      <h3>参数速查</h3>
      <table style="width:100%;border-collapse:collapse;font-size:13px;color:#d4d4d4;margin:8px 0">
      <tr style="border-bottom:1px solid #555">
      <th style="text-align:left;padding:6px 8px">参数</th>
      <th style="text-align:left;padding:6px 8px">配在哪儿</th>
      <th style="text-align:left;padding:6px 8px">作用</th>
      <th style="text-align:left;padding:6px 8px">搭配 / 互斥</th>
      </tr>
      <tr style="border-bottom:1px solid #333">
      <td style="padding:6px 8px;vertical-align:top"><code>text_only</code></td>
      <td style="padding:6px 8px;vertical-align:top">routes[] 条目</td>
      <td style="padding:6px 8px;vertical-align:top">标记上游不支持图片</td>
      <td style="padding:6px 8px;vertical-align:top">含图请求改走 multimodal_fallback；没配则透传原 route</td>
      </tr>
      <tr style="border-bottom:1px solid #333">
      <td style="padding:6px 8px;vertical-align:top"><code>no_search</code></td>
      <td style="padding:6px 8px;vertical-align:top">routes[] 条目</td>
      <td style="padding:6px 8px;vertical-align:top">标记上游不支持搜索</td>
      <td style="padding:6px 8px;vertical-align:top">搜索请求改走 search_fallback；与 enhance_search 互斥（标了 no_search 则 enhance_search 不生效）</td>
      </tr>
      <tr style="border-bottom:1px solid #333">
      <td style="padding:6px 8px;vertical-align:top"><code>enhance_search</code></td>
      <td style="padding:6px 8px;vertical-align:top">routes[] 条目</td>
      <td style="padding:6px 8px;vertical-align:top">支持搜索时主动改走 kimi 摘要模式</td>
      <td style="padding:6px 8px;vertical-align:top">仅该 route 未标 no_search 时生效；与 search_fallback 互斥</td>
      </tr>
      <tr style="border-bottom:1px solid #333">
      <td style="padding:6px 8px;vertical-align:top"><code>url_response_api</code></td>
      <td style="padding:6px 8px;vertical-align:top">routes[] 条目</td>
      <td style="padding:6px 8px;vertical-align:top">Responses 口命中该 route 时原生透传（不翻译）到该 Responses 上游</td>
      <td style="padding:6px 8px;vertical-align:top">只影响 Responses 口；Anthropic 口仍走该 route 的 url；配了它该 route 的 text_only/no_search/enhance_search 对透传流不生效</td>
      </tr>
      <tr style="border-bottom:1px solid #333">
      <td style="padding:6px 8px;vertical-align:top"><code>multimodal_fallback</code></td>
      <td style="padding:6px 8px;vertical-align:top">顶层</td>
      <td style="padding:6px 8px;vertical-align:top">图片兜底上游</td>
      <td style="padding:6px 8px;vertical-align:top">route 标 text_only 且请求含图时走它；纯图片无搜索才落这里</td>
      </tr>
      <tr>
      <td style="padding:6px 8px;vertical-align:top"><code>search_fallback</code></td>
      <td style="padding:6px 8px;vertical-align:top">顶层</td>
      <td style="padding:6px 8px;vertical-align:top">搜索兜底上游</td>
      <td style="padding:6px 8px;vertical-align:top">route 标 no_search 且请求含搜索时走它；summary_mode=true 代理做 step1+step2 自构响应，=false 整请求转发给上游自己搜索回答</td>
      </tr>
      </table>
      <h3>pattern 顺序</h3>
      <p>routes 按数组顺序匹配，第一个命中的生效，无"更具体优先"排序。宽通配会截胡窄通配--<code>*opus*</code> 写在 <code>*opus-4*</code> 前面时，<code>claude-opus-4-8</code> 先命中 <code>*opus*</code>，<code>*opus-4*</code> 永不触发；要让更具体的 pattern 生效，写在前面。</p>
      <h3>图片多模态</h3>
      <p>route 标了 <code>text_only</code> 且请求含图片时触发兜底。route 本身支持图片（未标 text_only）则直接走，不触发。</p>
      <ul>
      <li>配了 multimodal_fallback：改走 mf（正常多模态兜底）。</li>
      <li>没配 multimodal_fallback：透传给原 route 模型（上游不支持图片会报错，代理原样透传）。</li>
      </ul>
      <p>搜索请求（带 web_search）一律走 search_fallback，不管请求体是否含图片--没有证据表明会同时出现多模态+搜索。</p>
      <h3>增强搜索 enhance_search</h3>
      <p>route 配 <code>enhance_search</code> 后，<b>仅当该 route 支持搜索（未标 <code>no_search</code>）</b>时生效：收到带 web_search 的请求不调主力，改走两步：</p>
      <ul>
      <li>step1：用本 route 上游做非流式搜索，拿到 web_search_tool_result。</li>
      <li>step2：搜索结果 + 用户原始问题（搜索意图）组合成指令，流式生成逐条摘要（Result N: ...）。</li>
      </ul>
      <p>与 <code>search_fallback</code> 互斥：route 支持搜索（未标 <code>no_search</code>）走 enhance_search；route 标 <code>no_search</code> 缺搜索才走 <code>search_fallback</code>。</p>
      <p><code>summary_level</code>：low（简短）/ mid（中等）/ high（详尽）/ max（含代码公式逐字复述），控制详细度与 max_tokens。<code>summary_thinking</code>：step2 是否开 thinking。</p>
      <h3>缓存命中</h3>
      <p>命中率 = cache_read / (input + cache_read + cache_creation)，与 Claude Code 的 cache hit 算法一致：缓存写入不算命中、但计入总输入。高命中率（90%+）主要来自上游模型（DeepSeek/Kimi）的原生 context caching，代理只透传 usage，不做额外缓存优化。点击状态页「缓存命中」卡片可看按真实上游模型分组的明细（含写入量），弹窗底部另有「实测缓存时间」表（按上游 URL+模型归组，规则见「统计字段」的「缓存年龄」条目）。中途断开的流（按 Esc、网络掉线）不计入聚合：它们只有 message_start 的预估 usage（部分上游的 start.input 含 cache_read 且 start.cr=0），计入会污染命中率，日志里标 [中断]。</p>
      <h3>统计字段</h3>
      <ul>
      <li><b>总时间</b>：从代理收到下游请求到把响应全部发回下游的总耗时 = 内部处理（翻译/路由/缓冲）+ 首字等待 + 吐字 + 收尾内部处理。</li>
      <li><b>首字</b>：从发出请求到收到首个输出字节耗时（ms）。</li>
      <li><b>tok/s</b>：流式输出速率 = 输出 token 数 / 流式耗时。</li>
      <li><b>缓存命中</b>：见上。</li>
      <li><b>分类器</b>（状态卡片）：卡面数字 = 启动至今命中分类器（Claude Code 安全判断）特征的请求数——无论是否分流到 classifier_route、是否关思考都计，反映安全判断请求量。鼠标移上卡片看明细（命中总量 / 关思考改写次数），点击放大弹窗（含关思考占比）；「关思考」= 其中实际被改写关掉 thinking 的次数（仅 classifier_thinking_disabled 开启时发生；已是关思考形态的请求不产生改写，不计）。</li>
      <li><b>缓存年龄</b>（最近完成流表）：按会话+路由锚定，显示该会话该路由最近一次缓存写入距现在过了多久（m:ss 递增）——上游缓存真实存活期是动态的，这列不再猜倒计时，只告诉你"这份缓存是多久前写的"，还能不能用请对照「缓存命中」弹窗的实测区间判断。同会话同路由最新一条流显示年龄；被更新的同键流刷新后旧行显示 <code>-</code>；无会话标识的流（count_tokens 探针、不带 metadata 的裸调用）恒 <code>-</code>。黄灯（等待首字节）阶段被下游主动断开的 499 流视同未刷新缓存：本行显 <code>-</code>、该键锚停留再上一次同键流；绿灯（流式中）断开的 499 说明上游已在吐字、缓存已写，照常作为新锚。同会话同路由有<b>在途流正在吐字</b>（绿灯转发中）时，缓存实际刚被刷新——该键最新完成行冻结显示 <code>[m:ss]</code>（方括号内为刷新那一刻旧锚的年龄，数字不变），新锚等该在途流完成归档后生效。会话标识来自客户端请求自带字段（Claude Code 的 metadata.user_id 内 session_id / Codex 的 prompt_cache_key），代理只读不改。年龄从流开始时刻起算（缓存写入/刷新发生在上游处理输入时）。<b>保留规则</b>：开始时刻距今 5 分钟内的"会话+路由最新一条"完成流不被「保留完成流 N」挤掉（列表行数可因此超 N）；显示 <code>-</code> 或超窗口的行照常先进先出裁剪。<b>点击「缓存命中」卡片</b>，弹窗底部「实测缓存时间」表列出各上游实测的缓存存活时间：按 URL+模型名归为一类（不看其他参数），同会话相邻两条流后条命中率 ≥95% 记一次区间下界「至少活了间隔那么久」（取最大值），前条命中过而后条命中率 &lt;50% 记一次区间上界「没活过间隔那么久」（取最小值；不看严格归零——系统提示词等公共前缀的残留缓存命中不算活着）；间隔按两条流各自的开始时刻算；输入总量（input+cache_read+cache_creation）不足 1024 token 的流不观测（小请求噪声大）；两侧观测矛盾时（上游缓存时间中途变化、或缓存被提前驱逐）以较新的观测为准、被否的一侧作废重测、观测次数同步归零重计；「结论形成」列 = 当前上下界数值形成至今的时长（m:ss 递增），任一界数值变化（含作废重测）即重新起算，只新增支撑观测、数值不变时不重置；实测只作展示，纯内存态（重启/清空统计即清零）。</li>
      <li><b>model 列工具标签</b>：响应中调用过的工具以 [Read*1][Edit*3] 形式金色跟在 model 后（web_search 等服务端工具也计）；在途流随转发实时增加，完成流保留最终快照；同名 N 次合并显 *N（原始次数），单次调用显 *1，参数结构体为空的单次调用显 *0（如空搜索），顺序按首次出现。<b>剥块红标 [剥N]</b>：该流剥掉的回放搜索块总数（信封封入超 1 小时或老于本对话水位 → 时间规则直接剥不还原；还原后仍被上游拒 → 400 兜底剥光重试）。在途流挂在 model 列工具标签后，完成流改挂「缓存命中」列（如 81%[剥2]），同一标记两处只出现一处；只有发生过剥块的流才显示。拆分（时间规则剥多少、400 兜底剥多少）看日志 [剥块]/[兜底] 行；被剥后客户端无感（收不到 400），模型失去旧搜索上下文时可能重搜。</li>
      <li><b>状态灯时长</b>：在途流表格首列状态灯（⚪请求 / 🟡等首字节 / 🟢转发中）旁的秒数 = 当前灯色已持续的时间；只在灯变色时清零——黄灯内部的路由、多次重试不单独清零，保证"黄灯亮了多久"连续真实。黄灯内正在等首字节时，状态灯与总时长之间另有 [尝试N: Xs] = 本次尝试已等待的时长（每次尝试重新起算；重试退避中显示 [退避中]）——黄灯总时长 = 各次尝试 + 退避之和，两者对照即可看出是否已在重试。</li>
      <li><b>499</b>：状态码列中非 200 的状态码加方括号显示（如 [499]、[400]），一眼挑出异常流。重试/预算用尽时代理会向下游透传兜底 error 事件（overloaded_error），此类流状态码列显 [重试尽]（点击行可回看该兜底事件）。499 口径与上游提供商后台一致——上游响应没发完连接就结束了记 499（最常见是下游主动取消，取消会传导成上游断连；nginx 惯例 client closed request）；上游完整发完后下游才断开的（Codex 收完 response.completed 即关连接）仍记 200。</li>
      </ul>
      <h3>流查看</h3>
      <p>点击在途流/最近完成流的行可看该流内容：默认看输出（「显示解析/显示原始」切换）；「看请求体」回看导致这个流的下游请求体（JSON 自动美化，非完整 JSON 按原文显示）。浏览一律只给前 256KB。勾选「储存完整结构体」（默认关，重启复位）后，新开始的请求额外记录完整请求体与输出（不设上限，占内存），查看器出现「下载请求体/下载输出」按钮可下载完整文件（JSON 美化后保存，非 JSON 按原文），以及「交互式JSON」按钮——把请求体/输出渲染成可按键折叠展开的 JSON 树（默认全部折叠，点键名行懒展开；输出是 SSE 事件流时解析成事件数组再成树）；取消勾选立即清空已存的完整副本、下载与交互按钮消失。数据残缺的流不会静默当成完整版：请求体只剩截断版的「下载请求体」置灰（悬停见原因），无完整输出副本的「下载输出」置灰，交互式JSON 对这两类直接提示不看。Responses 翻译口的流记录的是翻译成 Anthropic 后的请求体。</p>
      <h3>配置管理</h3>
      <ul>
      <li>配置页可新建 / 重命名 / 删除 / 切换配置文件。</li>
      <li>切到配置页后自动每 3 秒刷新文件列表，增删配置文件无需手动按「刷新列表」。</li>
      <li>当前生效的配置不可删除。</li>
      </ul>
      <h3>访问控制</h3>
      <p>转发通道与管理端点（/__*）都永远仅本机可连（127.0.0.1/::1）：误把 listen / responses_listen 设成 0.0.0.0 也不会把转发通道暴露给内网。</p>
    </div>
  </div>
</div>

<div class="pane" id="pane-config">
  <div id="cfgPath"></div>
  <div style="margin-bottom:8px">
    配置文件: <select id="cfgSelect"></select>
    <button id="cfgRefreshBtn" class="ghost">刷新列表</button>
    <button id="cfgNewBtn" class="ghost">新建</button>
    <button id="cfgRenameBtn" class="ghost">重命名</button>
    <button id="cfgDelBtn" class="ghost">删除</button>
    <span id="cfgSwitchMsg"></span>
  </div>
  <textarea id="cfg" spellcheck="false"></textarea>
  <div style="margin-top:8px">
    <button id="saveBtn">保存并重载</button>
    <button id="reloadBtn" class="ghost">仅重载（不改动文件）</button>
    <span id="cfgMsg"></span>
  </div>
  <div id="codexBox">
    <div class="t">Codex 一键配置 —— 下面两行命令按上方编辑框实时生成（地址取自 responses_listen，模型目录取自 routes 的 pattern），在对应终端粘贴回车即运行</div>
    <div id="codexEnable" style="display:none">
      当前配置没有 responses_listen，Responses API 监听口未启用。要为本配置增加 Responses API 功能吗？
      <button id="codexEnableBtn">是，添加并保存</button>
      <span id="codexEnableMsg"></span>
      <div class="dim">会在上方 JSON 的 "listen" 行后加一行 "responses_listen": "127.0.0.1:8081"（端口可改）并立即保存；监听口随保存即时启动，不用重启代理。</div>
    </div>
    <div id="codexGen">
    <div style="margin-bottom:6px">
      默认模型: <select id="codexModel">
        <option value="gpt-5-codex">gpt-5-codex（gpt-5*）</option>
        <option value="claude-fable-5">claude-fable-5</option>
        <option value="__custom__">自定义…</option>
        <option value="__restore__">（还原默认 Codex 配置）</option>
      </select>
      <input id="codexModelCustom" placeholder="自定义模型名" style="display:none">
      &nbsp;上下文窗口: <input id="codexCtx" type="number" value="262144" min="4096" max="2000000" style="width:84px" title="写进模型目录每个条目的上下文窗口（tokens）">
      &nbsp;压缩阈值%: <input id="codexCompact" type="number" value="95" min="1" max="99" style="width:56px" title="上下文用到该百分比时 Codex 自动压缩（95 = 用到 95% 触发）">
      <span id="codexMsg"></span>
    </div>
    <div class="cmdRow"><span class="os">Windows（PowerShell）</span><code id="codexCmdWin"></code><button id="codexCopyWin" class="ghost">复制</button></div>
    <div class="cmdRow"><span class="os">macOS / Linux（终端）</span><code id="codexCmdSh"></code><button id="codexCopySh" class="ghost">复制</button></div>
    <div class="dim">命令从本代理拉取按当前配置烤好的脚本并直接执行（免交互、免官方登录、token 占位——真实 key 由上面路由的 api 注入）；脚本先备份再改写 ~/.codex/config.toml 并写模型目录，下拉选「（还原默认 Codex 配置）」得到的命令可完全撤销。下拉列出 routes 每个 pattern 的一个代表名（route.model 能命中 pattern 时用真名，否则用去 * 的 pattern；纯 * 兜底路由固定叫 Fallback——它接住任意模型名，显示某个真实 model 名会误导）：这些名字全部写进 Codex 的 /model 菜单，选中项为默认模型；pattern 不允许全字叫 Fallback 或 fast_route（保留名，撞名的路由不生效也不进菜单）；配了 fast_route 且带 model 时菜单追加 fast_route 条目，选中即走 fast 通道（等效请求带 speed:"fast"）。上下文窗口与压缩阈值写进目录的每个条目（Codex 用到该百分比时自动压缩上下文，顶层 model_context_window 等覆盖键会被脚本清掉以免压过目录声明）。改了 responses_listen 点「保存并重载」后监听口即按新地址生效，命令跟着变。</div>
    </div>
  </div>
</div>

<div id="cfgDelModal">
  <div class="box">
    <div class="t">选择要删除的配置（当前生效的配置不可删除）：</div>
    <select id="cfgDelSelect"></select>
    <div class="err" id="cfgDelMsg"></div>
    <div class="btns">
      <button id="cfgDelCancelBtn" class="ghost">取消</button>
      <button id="cfgDelConfirmBtn" class="danger">确认删除</button>
    </div>
  </div>
</div>

<script>
const logEl = document.getElementById('log');
const stEl  = document.getElementById('status');
const cardsEl = document.getElementById('cards');
const flightsBody = document.querySelector('#flights tbody');
let stick = true;
let cfgLoaded = false;
let cfgPollTimer = null; // 配置页激活时定时轮询配置文件列表，切走清掉
let lastCfgFiles = null; // 上次配置列表签名，未变化跳过重绘避免下拉闪烁
let selectedFlight = 0; // 当前查看的在途流 id（0=未查看）
let autoTrack = true; // 勾选时 poll 自动跟踪最新在途流（id 最大=最新），不勾选则用户手选
let maxFlights = 3; // 自动跟踪多流模式下最多并排显示多少个在途流（最新 N 个），防止太多太细
let flightViewRaw = false; // 查看区显示模式：false=解析文本，true=原始 SSE
let flightEnded = false; // 选中的流是否已结束（结束后停止拉取，保留最后内容）
let flightViewWhat = 'resp'; // 手选单流查看内容：'resp'=输出流，'req'=下游请求体（仅手选单流可切）
let lastReqRaw = ''; // 当前选中流的下游请求体原文缓存（切到 req 时拉一次；请求体静态不变）
let fullStoreOn = false; // 「储存完整结构体」开关（以服务端为准）：开=可下载完整请求体/输出
let flightFlags = {}; // 流 id -> {rt: 请求体被截断, hf: 仍持有完整输出}，poll 时在途/完成两表下发，用于置灰下载按钮
let flightViewTree = false; // 单流交互式 JSON 树视图：true=查看区显示可折叠树（点击时的静态快照，poll 不刷新）
let lastRaw = ''; // 单流模式：最后一次拉到的 raw SSE（模式切换重渲染 + 流结束后保留）
let autoRaws = {}; // 自动跟踪多流模式：每个在途流 id -> 最近 raw（模式切换重渲染用）

// 标签切换
document.querySelectorAll('.tab').forEach(t => {
  t.onclick = () => {
    document.querySelectorAll('.tab').forEach(x => x.classList.remove('active'));
    document.querySelectorAll('.pane').forEach(x => x.classList.remove('active'));
    t.classList.add('active');
    document.getElementById('pane-' + t.dataset.tab).classList.add('active');
    document.getElementById('clearLogsBtn').style.display = (t.dataset.tab === 'logs') ? 'block' : 'none';
    if (t.dataset.tab === 'logs') stick = true, window.scrollTo(0, document.body.scrollHeight);
    if (cfgPollTimer){ clearInterval(cfgPollTimer); cfgPollTimer=null; }
    if (t.dataset.tab === 'config'){
      if(!cfgLoaded) loadConfig(); else loadConfigList();
      cfgPollTimer = setInterval(loadConfigList, 3000);
      genCodexCmds();
    }
  };
});

window.addEventListener('scroll', () => {
  stick = (window.innerHeight + window.scrollY) >= (document.body.scrollHeight - 30);
});

function fmtBytes(n){
  if(n<1024) return n+'B';
  if(n<1048576) return (n/1024).toFixed(1)+'KB';
  return (n/1048576).toFixed(2)+'MB';
}
// model 列单元格：有 searchPrompt 时整个 td 加 title，hover 显示系统原生提示词（宽度自适应、无滚动条）。
// routeReason 非 0（透传）时前置 [标签]，一眼看出是什么原因路由的（如 [Fast]claude-sonnet-5 -> glm-5.2）。
// countTokens 为 true（/v1/messages/count_tokens 探针）时前置灰色 [count_tokens]：
// 这类流原始内容只有 {"input_tokens":N}，没标记看着像空响应。
// API 来源（[Anthropic]/[Response]/[translate]）在独立 API 列显示，见 apiCell，不在本列。
function modelCell(model, prompt, routeReason, countTokens, tools, stripped){
  var tag = routeTag(routeReason);
  if(tag) model = tag + model;
  if(countTokens) model = '<span style="color:#9d9d9d">[count_tokens]</span>' + model;
  // 工具调用标签（"[Read*1][Edit*3]"）：跟在 model 链后，金色区分；来自服务端 toolCallsTag 的已格式化串，转义后插入
  // （注意：本函数内局部变量不能叫 esc——var 声明提升会遮蔽全局 esc()，上面这行就会拿不到函数）
  if(tools) model = model + '<span style="color:#d7ba7d">' + esc(tools) + '</span>';
  // 剥块红标 [剥N]：该流剥掉的回放搜索块总数（时间规则+400 兜底）；只在在途流挂本列，
  // 完成流改挂缓存命中列（见行模板）——同一红标两处只出现一处，避免重复计数观感
  if(stripped > 0) model = model + '<span style="color:#f48771">[剥' + stripped + ']</span>';
  if(!prompt) return '<td>'+model+'</td>';
  var escP = String(prompt).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
  return '<td title="'+escP+'">'+model+'</td>';
}
// API 列单元格：该流的协议来源——
// 空 = Anthropic 口原生流量（Claude Code 等，橙色 [Anthropic]，#d97757 = Claude 珊瑚橙）；
// 'responses-raw' = Responses 口原生透传（路由配了 url_response_api，绿色 [Response]，#2bbf8a = ChatGPT 绿 #10a37f 的提亮版）；
// 'responses' = Responses 口翻译成 Anthropic 走主管线（紫色 [translate]，沿用旧 model 列前缀的颜色）。
function apiCell(translated){
  if(translated === 'responses-raw') return '<td><span style="color:#2bbf8a">[Response]</span></td>';
  if(translated) return '<td><span style="color:#c586c0">[translate]</span></td>';
  return '<td><span style="color:#d97757">[Anthropic]</span></td>';
}
// 路由原因 -> model 列前缀标签（与 main.go route* 枚举对齐：0透传 1pattern 2分类器 3fast 4多模态 5搜索）。
// 透传不加前缀；标签淡蓝色与 model 链区分。
function routeTag(r){
  var tags = ['','[Pattern]','[分类器]','[Fast]','[多模态]','[搜索]'];
  if(!tags[r]) return '';
  return '<span style="color:#7ec8e3">'+tags[r]+'</span>';
}
// 在途流状态灯：转发中(绿)/等待首字节(黄)/请求阶段(白)，用 emoji 与顶部连接状态灯同等大小。
function flightDot(f){
  if(f.stage===3) return '🟢';
  if(f.stage>=1) return '🟡';
  return '⚪';
}
// 状态灯旁的持续时长：当前灯色已点亮的秒数（服务端 stageMs，随 500ms 轮询走动）。
function fmtStageDur(ms){ return (ms/1000).toFixed(1)+'s'; }
// 黄灯旁的当前尝试计时，夹在状态灯与黄灯总时长之间：[尝试N: Xs]=本次尝试已等首字节的时长
// （每次尝试重新起算）；attemptMs=-1 表示重试退避中（下一次尝试未发出），显示 [退避中]；
// 非尝试阶段（白灯请求/黄灯路由/绿灯转发）不显示。
function attemptTag(f){
  if(f.stage!==2) return '';
  if(f.attemptMs==null || f.attemptMs<0) return ' [退避中]';
  return ' [尝试'+(f.attempt||1)+': '+fmtStageDur(f.attemptMs)+']';
}
// 在途流「状态」列：转发阶段显示状态码，其余阶段显示中文进度；
// 尝试及以后附加路由原因（如「尝试1·搜索」「200·透传」）。
// stage: 0=请求 1=路由 2=尝试N 3=转发(显示状态码)
// 转义 & < >，innerHTML 安全插入用户内容（tool input/搜索结果可能含 <>）。
function esc(s){
  return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');
}
// 解析 SSE 原文为 HTML：按到达顺序提取 text/thinking/tool_use/server_tool_use/web_search_tool_result，
// 每类独立成块渲染（tool 各起新行），"啥都渲染"。后端只 tee raw，解析放前端避免影响透传热路径。
function parseSSEHTML(raw){
  var blocks = [];          // 按顺序: {type:'text'|'think'|'tool'|'search', ...}
  var idx = {};             // content_block index -> block（input_json_delta 回填用）
  var rIdx = {};            // Responses 透传流：output_item id -> block（arguments delta / done 回填用）
  var curText = null, curThink = null;
  function textBlock(){ if(!curText){ curText = {type:'text', text:''}; blocks.push(curText); } return curText; }
  function thinkBlock(){ if(!curThink){ curThink = {type:'think', text:''}; blocks.push(curThink); } return curThink; }
  // Responses output 数组 → blocks（response.completed 兜底与非流式 response 对象共用）：
  // message→text、reasoning→think、function_call/custom_tool_call→tool、web_search_call→search。
  function renderResponsesOutput(output){
    output.forEach(function(it){
      if(!it || !it.type) return;
      if(it.type === 'message' && Array.isArray(it.content)){
        it.content.forEach(function(c){
          if(c && c.type === 'output_text' && c.text) blocks.push({type:'text', text:c.text});
        });
      } else if(it.type === 'reasoning' && Array.isArray(it.summary)){
        var t = it.summary.map(function(s){ return (s && s.text) || ''; }).join('');
        if(t) blocks.push({type:'think', text:t});
      } else if(it.type === 'function_call' || it.type === 'custom_tool_call'){
        blocks.push({type:'tool', name: it.name || it.type, input: it.arguments || it.input || ''});
      } else if(it.type === 'web_search_call'){
        var act = it.action || {};
        var srcs = (act.sources || []).map(function(s){ return {title:'', url: (s && s.url) || s || ''}; });
        blocks.push({type:'search', query: act.query || '', results: srcs});
      }
    });
  }
  raw.split('\n').forEach(function(line){
    if(line.indexOf('data:') !== 0) return;
    var p = line.slice(5).trim();
    if(p === '' || p === '[DONE]') return;
    var j;
    try { j = JSON.parse(p); } catch(e) { return; }
    // error 事件：流内错误（限流/overloaded 等），显示错误信息。
    // 兼认 Responses 的扁平 error 事件（{type:'error', code, message}，无嵌套 error 对象）。
    if(j.type === 'error' && (j.error || j.message)){
      blocks.push({type:'error', msg: (j.error && (j.error.message || j.error.type)) || j.message || 'error'});
      curText = null; curThink = null;
      return;
    }
    // ---- Responses 原生透传流（url_response_api）：事件名 response.*，须在通用 delta 兜底前分流 ----
    if(j.type === 'response.output_text.delta' && j.delta != null){ textBlock().text += j.delta; return; }
    if(j.type === 'response.reasoning_summary_text.delta' && j.delta != null){ thinkBlock().text += j.delta; return; }
    if(j.type === 'response.function_call_arguments.delta' && j.delta != null){
      // 按 item_id 回填；added 丢失/在途截断时退回最近 tool 块，再没有就新建
      var fb = rIdx[j.item_id];
      if(!fb){ for(var k = blocks.length-1; k >= 0; k--){ if(blocks[k].type === 'tool'){ fb = blocks[k]; break; } } }
      if(!fb){ fb = {type:'tool', name:'function_call', input:''}; blocks.push(fb); curText = null; curThink = null; }
      fb.input += j.delta;
      return;
    }
    if(j.type === 'response.output_item.added' || j.type === 'response.output_item.done'){
      var it = j.item;
      if(!it || !it.type) return;
      if(it.type === 'function_call' || it.type === 'custom_tool_call'){
        var rtb = rIdx[it.id];
        if(!rtb){ rtb = {type:'tool', name: it.name || it.type, input:''}; blocks.push(rtb); if(it.id) rIdx[it.id] = rtb; curText = null; curThink = null; }
        if(it.name) rtb.name = it.name; // done 事件里的 name 可能更全
        // done 时 arguments 兜底回填：delta 被截断/丢失时好歹显示完整入参；delta 已填过就不覆盖
        if(j.type === 'response.output_item.done' && !rtb.input && it.arguments) rtb.input = it.arguments;
      } else if(it.type === 'web_search_call'){
        var rsb = rIdx[it.id];
        if(!rsb){ rsb = {type:'search', query:'', results:[]}; blocks.push(rsb); if(it.id) rIdx[it.id] = rsb; curText = null; curThink = null; }
        var act = it.action || {};
        if(act.query && !rsb.query) rsb.query = act.query;
        if(act.sources && rsb.results.length === 0){
          rsb.results = act.sources.map(function(s){ return {title:'', url: (s && s.url) || s || ''}; });
        }
      } else if(it.type === 'reasoning' && Array.isArray(it.summary)){
        // 只在 added 时 summary 已带文本才兜底渲染（部分端点一次性给全）；
        // 流式场景 summary 起始为空、由 reasoning_summary_text.delta 填充，这里不重复渲染
        var rt = it.summary.map(function(s){ return (s && s.text) || ''; }).join('');
        if(rt && j.type === 'response.output_item.added') blocks.push({type:'think', text:rt});
      }
      return;
    }
    if(j.type === 'response.completed' && j.response){
      // 长流归档只留尾部（flightContentCap）：delta 全被截掉时，终局事件里的完整 output 是最后兜底
      if(blocks.length === 0 && Array.isArray(j.response.output)) renderResponsesOutput(j.response.output);
      return;
    }
    if(j.type === 'response.failed'){
      var fe = j.response && j.response.error;
      blocks.push({type:'error', msg: (fe && (fe.message || fe.code)) || 'response.failed'});
      curText = null; curThink = null;
      return;
    }
    if(j.type === 'response.incomplete') return; // 终局标记，内容已随 delta 渲染
    // content_block_start：tool_use / server_tool_use / web_search_tool_result 建块
    if(j.type === 'content_block_start' && j.content_block){
      var cb = j.content_block;
      if(cb.type === 'tool_use' || cb.type === 'server_tool_use'){
        var tb = {type:'tool', name: cb.name || cb.type, input: ''};
        blocks.push(tb); idx[j.index] = tb; curText = null; curThink = null;
      } else if(cb.type === 'web_search_tool_result'){
        var sb = {type:'search', results: cb.content || []};
        blocks.push(sb); idx[j.index] = sb; curText = null; curThink = null;
      } else if(cb.type === 'redacted_thinking'){
        blocks.push({type:'think', text: '[已编辑思考（加密）]'});
        curText = null; curThink = null;
      }
      return;
    }
    // content_block_delta：text/thinking 累加，input_json_delta 回填到对应 tool
    if(j.type === 'content_block_delta' && j.delta){
      var d = j.delta;
      if(d.type === 'text_delta' && d.text){ textBlock().text += d.text; }
      else if(d.type === 'thinking_delta' && d.thinking){ thinkBlock().text += d.thinking; }
      else if(d.type === 'input_json_delta' && d.partial_json != null){
        // idx[index] 可能不存在：content_block_start 丢失/类型非 tool_use/在途流被截断
        var tb = idx[j.index];
        if(!tb){ for(var k = blocks.length-1; k >= 0; k--){ if(blocks[k].type === 'tool'){ tb = blocks[k]; break; } } }
        if(!tb){ tb = {type:'tool', name:'tool_use', input:''}; blocks.push(tb); curText = null; curThink = null; }
        tb.input += d.partial_json;
      }
      return;
    }
    // 扁平 text_delta / thinking_delta（部分上游不包 content_block_delta，直接发 delta 事件）
    if(j.type === 'text_delta' && j.text != null){ textBlock().text += j.text; return; }
    if(j.type === 'thinking_delta' && j.thinking != null){ thinkBlock().text += j.thinking; return; }
    // 扁平 input_json_delta（部分上游不包 content_block_delta，直接发 input_json_delta 事件）
    if(j.type === 'input_json_delta' && j.partial_json != null){
      var tb = idx[j.index];
      if(!tb){
        for(var k = blocks.length-1; k >= 0; k--){ if(blocks[k].type === 'tool'){ tb = blocks[k]; break; } }
      }
      if(!tb){ tb = {type:'tool', name:'tool_use', input:''}; blocks.push(tb); curText = null; curThink = null; }
      tb.input += j.partial_json;
      return;
    }
    // 兜底：通用 delta（非 Anthropic 标准事件结构的流）
    if(j.delta){
      if(j.delta.text) textBlock().text += j.delta.text;
      else if(j.delta.thinking) thinkBlock().text += j.delta.thinking;
      else if(j.delta.content) textBlock().text += j.delta.content;
      else if(j.delta.reasoning_content) thinkBlock().text += j.delta.reasoning_content;
      return;
    }
    // OpenAI 风格
    if(j.choices && j.choices[0] && j.choices[0].delta){
      var dd = j.choices[0].delta;
      if(dd.content) textBlock().text += dd.content;
      else if(dd.reasoning_content) thinkBlock().text += dd.reasoning_content;
    }
  });
  // 非流式 JSON 兜底：无 data: 行（非 SSE）时，解析整个 body 为非流式响应。
  // 分类器等请求常返回非流式 JSON（stream:false 或上游偶发不流式）。
  if(blocks.length === 0){
    try {
      var nj = JSON.parse(raw.trim());
      if(Array.isArray(nj.content)){
        // Anthropic 非流式：{content:[{type:'text'|'thinking'|'tool_use'|'redacted_thinking'}]}
        nj.content.forEach(function(c){
          if(c.type === 'text' && c.text) blocks.push({type:'text', text:c.text});
          else if(c.type === 'thinking' && c.thinking) blocks.push({type:'think', text:c.thinking});
          else if(c.type === 'tool_use') blocks.push({type:'tool', name:c.name||'tool_use', input: JSON.stringify(c.input||{})});
          else if(c.type === 'redacted_thinking') blocks.push({type:'think', text:'[已编辑思考（加密）]'});
        });
      } else if(nj.choices && nj.choices[0] && nj.choices[0].message){
        // OpenAI 非流式：{choices:[{message:{content, reasoning_content}}]}
        var mc = nj.choices[0].message;
        if(mc.reasoning_content) blocks.push({type:'think', text:mc.reasoning_content});
        if(mc.content) blocks.push({type:'text', text: typeof mc.content === 'string' ? mc.content : JSON.stringify(mc.content)});
      } else if(nj.object === 'response' && Array.isArray(nj.output)){
        // Responses 非流式（透传客户端 stream:false）：{object:'response', output:[...]}
        renderResponsesOutput(nj.output);
        if(nj.error && nj.error.message) blocks.push({type:'error', msg: nj.error.message});
      } else if(nj.error && nj.error.message){
        blocks.push({type:'error', msg: nj.error.message});
      }
    } catch(e) {}
  }
  var html = '';
  blocks.forEach(function(b){
    if(b.type === 'text') html += '<div class="ss-text">'+esc(b.text)+'</div>';
    else if(b.type === 'think') html += '<div class="ss-think">'+esc(b.text)+'</div>';
    else if(b.type === 'tool') html += '<div class="ss-tool">🔧 <b>'+esc(b.name)+'</b> <code>'+esc(b.input)+'</code></div>';
    else if(b.type === 'error') html += '<div class="ss-error">⛔ <b>错误</b> '+esc(b.msg)+'</div>';
    else if(b.type === 'search'){
      var q = b.query ? ' <code>'+esc(b.query)+'</code>' : ''; // Responses web_search_call 带查询词
      var items = (b.results||[]).map(function(r){ return '<div>· '+esc(r.title||'')+' <span class="ss-url">'+esc(r.url||'')+'</span></div>'; }).join('');
      html += '<div class="ss-search">🔍 搜索结果'+q+' ('+(b.results||[]).length+')'+items+'</div>';
    }
  });
  return html;
}
// 查看区按钮/标签可见性：看请求体仅手选单流时出现；下载/交互按钮还要「储存完整结构体」开启
// （完整副本只在该模式下记录，关闭即被服务端清空）。该流数据残缺时下载按钮置灰带悬停提示：
// 请求体被截断（rt）的不能当完整版下载，无完整输出副本（!hf）的不能下载输出。
function updateFlightViewChrome(){
  var whatBtn = document.getElementById('flightViewWhatBtn');
  var dlBtn = document.getElementById('flightViewDlBtn');
  var dlOutBtn = document.getElementById('flightViewDlOutBtn');
  var treeBtn = document.getElementById('flightViewTreeBtn');
  var fl = flightFlags[selectedFlight] || {};
  if(selectedFlight){
    whatBtn.style.display = '';
    whatBtn.textContent = flightViewWhat==='req' ? '看输出' : '看请求体';
    document.getElementById('flightViewKind').textContent = flightViewWhat==='req' ? '请求体' : '输出';
    dlBtn.style.display = fullStoreOn ? '' : 'none';
    dlBtn.disabled = fl.rt === true;
    dlBtn.title = dlBtn.disabled ? '该流请求体只剩截断版（前 256KB），完整版未记录或已清空' : '';
    dlOutBtn.style.display = fullStoreOn ? '' : 'none';
    dlOutBtn.disabled = fl.hf === false;
    dlOutBtn.title = dlOutBtn.disabled ? '该流未记录完整输出（开启前已开始/已清空/无透传内容）' : '';
    treeBtn.style.display = fullStoreOn ? '' : 'none';
    treeBtn.textContent = flightViewTree ? '退出交互' : '交互式JSON';
  } else {
    whatBtn.style.display = 'none';
    dlBtn.style.display = 'none';
    dlOutBtn.style.display = 'none';
    treeBtn.style.display = 'none';
    document.getElementById('flightViewKind').textContent = '输出';
  }
}
// 请求体渲染：解析=JSON 美化（非完整 JSON 退回原文并注明），原始=逐字原文。
// 显示永远只给前 256KB：开关关闭时服务端本就截断；开启时服务端存完整体，显示截断、完整体走下载。
// rt（服务端下发）= 该流请求体只剩截断版：被 purge 截回的流不再提示「下载获取完整」。
function renderReqBody(){
  var fv = document.getElementById('flightView');
  var full = lastReqRaw;
  var rt = (flightFlags[selectedFlight]||{}).rt === true;
  var overCap = new TextEncoder().encode(full).length >= 262144;
  var note = '';
  if(rt) note = '（请求体超长，仅保留前 256KB）\n';
  else if(overCap) note = fullStoreOn ? '（仅显示前 256KB，点「下载请求体」获取完整）\n' : '（请求体超长，仅保留前 256KB）\n';
  var disp;
  if(flightViewRaw){ disp = full; }
  else {
    try{ disp = JSON.stringify(JSON.parse(full), null, 2); }
    catch(e){ disp = (rt ? '' : '（请求体非完整 JSON，按原文显示）\n') + full; }
  }
  if(fullStoreOn && !rt && disp.length > 262144) disp = disp.slice(0, 262144);
  fv.textContent = note + disp;
}
// renderRespFromLastRaw 用 lastRaw 按 解析/原始 重渲染输出视图（切回输出/退出交互树时用）。
function renderRespFromLastRaw(){
  var fv = document.getElementById('flightView');
  if(lastRaw === ''){
    fv.textContent = flightEnded ? '（该流无透传内容：失败/重试用尽/非流式）' : '加载中…';
  } else if(flightViewRaw){ fv.textContent = lastRaw; }
  else { var h = parseSSEHTML(lastRaw); fv.innerHTML = h || '<span class="ss-empty">（未解析出内容，点「显示原始」查看 SSE）</span>'; }
}
// ---- 交互式 JSON 树：默认全部折叠，点击键名行懒展开（子节点展开时才构建，大 JSON 不一次卡死）----
// jtPrimitiveHTML 渲染标量值（字符串带引号转义，null/数字/布尔着色）。
function jtPrimitiveHTML(v){
  if(v === null) return '<span class="jt-null">null</span>';
  if(typeof v === 'string') return '<span class="jt-str">'+esc(JSON.stringify(v))+'</span>';
  if(typeof v === 'number') return '<span class="jt-num">'+v+'</span>';
  if(typeof v === 'boolean') return '<span class="jt-bool">'+v+'</span>';
  return esc(String(v));
}
// jtNode 构建一个键值对行：容器（对象/数组）默认折叠成 {N 键}/[N 项] 摘要，点击展开时
// 懒构建子行、再点折叠回摘要；空容器与标量直接内联不可点。数组项的 key 传下标数字。
function jtNode(key, value){
  var line = document.createElement('div');
  line.className = 'jt-line';
  var keyHtml = key===null ? '' : '<span class="jt-key">'+esc(JSON.stringify(key))+'</span>: ';
  var isObj = value!==null && typeof value==='object';
  if(!isObj){
    line.innerHTML = '<span class="jt-arrow"></span>'+keyHtml+jtPrimitiveHTML(value);
    return line;
  }
  var isArr = Array.isArray(value);
  var keys = isArr ? value.map(function(_,i){return i;}) : Object.keys(value);
  var open = isArr?'[':'{', close = isArr?']':'}';
  if(keys.length===0){
    line.innerHTML = '<span class="jt-arrow"></span>'+keyHtml+open+close;
    return line;
  }
  var head = document.createElement('span');
  head.className = 'jt-head';
  var summary = '<span class="jt-arrow">▶</span>'+keyHtml+open+' <span class="jt-count">'+keys.length+(isArr?' 项':' 键')+'</span> '+close;
  head.innerHTML = summary;
  line.appendChild(head);
  var kids = null; // 展开后才构建的子节点容器；折叠时销毁
  head.onclick = function(){
    if(kids){
      kids.remove();
      kids = null;
      head.innerHTML = summary;
      return;
    }
    kids = document.createElement('div');
    kids.className = 'jt-kids';
    keys.forEach(function(k){ kids.appendChild(jtNode(k, value[k])); });
    var tail = document.createElement('div');
    tail.className = 'jt-line';
    tail.innerHTML = '<span class="jt-arrow"></span>'+close;
    kids.appendChild(tail);
    line.appendChild(kids);
    head.innerHTML = '<span class="jt-arrow">▼</span>'+keyHtml+open;
  };
  return line;
}
// jsonTreeRoot 用整值构建树根（根也无 key、默认折叠）。
function jsonTreeRoot(value){
  var root = document.createElement('div');
  root.appendChild(jtNode(null, value));
  return root;
}
// sseToJSONArray 把 SSE 原文解析成 [{event, data}, ...] 供交互式树查看；
// data 非 JSON 时保留原文行；全为注释/空块时返回空数组（表示不像 SSE）。
function sseToJSONArray(raw){
  var out = [];
  raw.split(/\r?\n\r?\n/).forEach(function(block){
    var ev = null, datas = [];
    block.split(/\r?\n/).forEach(function(l){
      if(l.indexOf('event:')===0) ev = l.slice(6).trim();
      else if(l.indexOf('data:')===0) datas.push(l.slice(5).trim());
    });
    if(ev===null && !datas.length) return;
    var d = datas.join('\n');
    try{ d = JSON.parse(d); }catch(e){}
    out.push({event: ev, data: d});
  });
  return out;
}
// 点击在途流/完成流行：切到单流模式（保持 autoTrack 勾选，由 selectedFlight 优先决定显示），显示该流。
function selectFlight(id){
  // 手选单流：不取消 autoTrack 勾选（点行查看时勾选保持），
  // 仅设 selectedFlight 让轮询切到单流分支；autoTrack 重新勾选时再清手选回 grid。
  selectedFlight = id;
  flightEnded = false;
  lastRaw = '';
  flightViewWhat = 'resp'; // 新手选默认看输出；请求体点「看请求体」再拉
  lastReqRaw = '';
  flightViewTree = false;
  var fv = document.getElementById('flightView');
  fv.innerHTML = '';
  fv.style.display = 'block';
  fv.style.gridTemplateColumns = '';
  fv.style.whiteSpace = 'pre-wrap';
  fv.style.wordBreak = 'break-all';
  fv.textContent = '加载中…';
  document.getElementById('flightViewWrap').style.display = 'block';
  document.getElementById('flightViewId').textContent = id;
  updateFlightViewChrome();
}
function flightStatus(f){
  var s;
  if(f.stage===3) s = f.status?f.status:'响应';
  else if(f.stage===2) s = '尝试'+(f.attempt||1);
  else if(f.stage===1) s = '路由';
  else s = '请求';
  if(f.stage>=2) s += '·' + routeLabel(f.routeReason);
  return s;
}
// 路由原因 -> 中文标签（与 main.go route* 枚举对齐：0透传 1pattern 2分类器 3fast 4多模态 5搜索）。
function routeLabel(r){
  return ['透传','pattern','分类器','fast','多模态','搜索'][r] || '透传';
}
function fmtNum(n){
  if(n<1000) return ''+n;
  if(n<1000000) return (n/1000).toFixed(1)+'k';
  return (n/1000000).toFixed(2)+'M';
}
function card(k,v){ return '<div class="card"><div class="k">'+k+'</div><div class="v">'+v+'</div></div>'; }

async function poll(){
  try{
    const r = await fetch('/__logs/data',{cache:'no-store'});
    const d = await r.json();
    // 储存完整结构体开关：以服务端为准（决定下载按钮显隐）
    fullStoreOn = !!d.fullStore;
    var fsChk = document.getElementById('fullStoreChk');
    if(fsChk.checked !== fullStoreOn) fsChk.checked = fullStoreOn;
    // 状态灯
    stEl.className = 'st ' + (d.active>0?'active':d.waiting>0?'wait':'idle');
    stEl.textContent = d.active>0?'🟢 流式中':d.waiting>0?'🟡 等待首字节':'⚪ 空闲';
    document.getElementById('curCfg').textContent = d.currentCfg ? ('配置: '+d.currentCfg) : '';
    // 统计卡片
    cardsEl.innerHTML =
      card('活跃', d.active) + card('等待', d.waiting) +
      card('流出', fmtBytes(d.bytesForward)) + card('速率', fmtBytes(d.rate)+'/s') +
      '<div class="card" id="cacheCard" style="cursor:pointer"><div class="k">缓存命中</div><div class="v">'+cacheHitPct(d.cacheRead, d.inputTokens, d.cacheCreation)+'</div></div>' + card('输入', fmtNum(d.inputTokens)) +
      card('输出', fmtNum(d.outputTokens)) + '<div class="card" id="retryCard" style="cursor:pointer"><div class="k">重试</div><div class="v">'+d.retries+'</div></div>' +
      '<div class="card" id="clsCard" style="cursor:pointer"><div class="k">分类器</div><div class="v">'+d.classifiers+'</div></div>' + card('首字', (d.avgFirstByte/1000).toFixed(2)+'s') +
      card('tok/s', d.tps.toFixed(1));
    // 缓存命中明细：缓存最新 modelStats / cacheObs / 分类器双计数，tooltip/modal 打开时实时刷新
    latestModelStats = d.modelStats || [];
    latestCacheObs = d.cacheObs || [];
    latestClsHits = d.classifiers || 0;
    latestClsNoThink = d.classifierNoThink || 0;
    if(document.getElementById('cacheTip').style.display !== 'none') refreshCacheTip();
    if(document.getElementById('cacheModal').style.display !== 'none') refreshCacheModal();
    if(document.getElementById('retryTip').style.display !== 'none') refreshRetryTip();
    if(document.getElementById('retryModal').style.display !== 'none') refreshRetryModal();
    if(document.getElementById('clsTip').style.display !== 'none') refreshClsTip();
    if(document.getElementById('clsModal').style.display !== 'none') refreshClsModal();
    // 在途流
    const fs = d.flights || [];
    fs.forEach(function(f){ flightFlags[f.id] = {rt:!!f.reqTrunc, hf:!!f.hasFull}; });
    flightsBody.innerHTML = fs.map(f =>
      '<tr style="cursor:pointer" onclick="selectFlight('+f.id+')"><td>'+flightDot(f)+attemptTag(f)+' '+fmtStageDur(f.stageMs)+'</td><td>#'+f.id+'</td>'+modelCell(f.model,f.searchPrompt,f.routeReason,f.countTokens,f.tools,f.stripped)+apiCell(f.translated)+'<td>'+fmtBytes(f.bytes)+'</td><td>'+flightStatus(f)+'</td></tr>'
    ).join('');
    // 在途流输出查看：手选单流优先，否则自动跟踪 grid，否则隐藏
    var fv = document.getElementById('flightView');
    var wrap = document.getElementById('flightViewWrap');
    if(selectedFlight){
      // 手选单流模式（点表格行触发，优先于自动跟踪；autoTrack 勾选状态保持）
      wrap.style.display = 'block';
      document.getElementById('flightViewId').textContent = selectedFlight;
      fv.style.display = 'block';
      fv.style.gridTemplateColumns = '';
      fv.style.whiteSpace = 'pre-wrap';
      fv.style.wordBreak = 'break-all';
      updateFlightViewChrome();
      if(!flightEnded && flightViewWhat==='resp' && !flightViewTree){ // req 模式/交互树看的是静态内容，切回输出前不拉流
        try{
          const fr = await fetch('/__flight?id='+selectedFlight,{cache:'no-store'});
          if(fr.ok){
            const atBottom = fv.scrollTop + fv.clientHeight >= fv.scrollHeight - 2;
            lastRaw = await fr.text();
            if(lastRaw === ''){
              fv.textContent = '（该流无透传内容：失败/重试用尽/非流式）';
            } else {
              if(flightViewRaw){ fv.textContent = lastRaw; } else { var h = parseSSEHTML(lastRaw); fv.innerHTML = h || '<span class="ss-empty">（未解析出内容，点「显示原始」查看 SSE）</span>'; }
            }
            if(atBottom) fv.scrollTop = fv.scrollHeight; // 用户滚到底则跟随，否则不动
            // 完成流（不在在途列表）内容是静态快照，拉一次即可，停止重复拉取。
            if(!fs.some(function(x){return x.id===selectedFlight;})) flightEnded = true;
          } else if(fr.status === 404){
            flightEnded = true;
            var endDiv = document.createElement('div');
            endDiv.className = 'ss-empty';
            endDiv.textContent = '-- 流已结束 --';
            fv.appendChild(endDiv);
          }
        }catch(e){}
      }
    } else if(autoTrack){
      // 自动跟踪：在途流并排 grid（1 个则单列全宽，N 个则 1×N 左右排列）
      // 最多显示 maxFlights 个（最新 N 个），防止太多太细
      updateFlightViewChrome();
      var show = fs.slice(-maxFlights);
      if(show.length > 0){
        wrap.style.display = 'block';
        document.getElementById('flightViewId').textContent = show.length+' 个在途流';
        fv.style.display = 'grid';
        fv.style.gridTemplateColumns = 'repeat('+show.length+', 1fr)';
        fv.style.gap = '4px';
        fv.style.whiteSpace = '';
        fv.style.wordBreak = '';
        // 移除不在显示范围内的 cell（已结束或超出最多个数）
        Array.from(fv.children).forEach(function(cell){
          if(!show.some(function(f){return String(f.id)===cell.dataset.id;})){
            delete autoRaws[cell.dataset.id];
            cell.remove();
          }
        });
        // 新增 cell
        show.forEach(function(f){
          if(!fv.querySelector('[data-id="'+f.id+'"]')){
            var cell = document.createElement('div');
            cell.dataset.id = f.id;
            cell.innerHTML = '<div style="color:#9a9a9a;margin-bottom:2px">流 #'+f.id+' '+(f.model||'')+'</div><pre class="cellPre" style="max-height:240px;overflow:auto;background:#1a1a1a;border:1px solid #333;padding:6px;white-space:pre-wrap;word-break:break-all;margin:0;font:inherit">加载中…</pre>';
            fv.appendChild(cell);
            var np = cell.querySelector('.cellPre');
            np.dataset.stick = '1'; // 默认跟踪底部；用户手动上滚后才停止跟随
            np.addEventListener('scroll', function(){
              np.dataset.stick = (np.scrollTop + np.clientHeight >= np.scrollHeight - 2) ? '1' : '0';
            });
          }
        });
        // 拉取每个在途流内容（独立更新各自 cell）
        show.forEach(function(f){
          fetch('/__flight?id='+f.id,{cache:'no-store'}).then(function(r){return r.ok?r.text():null;}).then(function(raw){
            if(raw===null) return;
            autoRaws[f.id] = raw;
            var pre = fv.querySelector('[data-id="'+f.id+'"] .cellPre');
            if(pre){
              if(flightViewRaw){ pre.textContent = raw; } else { var h = parseSSEHTML(raw); pre.innerHTML = h || '<span class="ss-empty">（未解析出内容，点「显示原始」查看）</span>'; }
              if(pre.dataset.stick === '1') pre.scrollTop = pre.scrollHeight;
            }
          }).catch(function(){});
        });
      } else {
        wrap.style.display = 'none';
        fv.innerHTML = '';
      }
    } else {
      // 未勾选自动跟踪，也无手选：隐藏查看区
      wrap.style.display = 'none';
      fv.innerHTML = '';
    }
    // 最近完成流列表（点击复用查看区回看其输出）
    try{
      const rf = await fetch('/__recentflights',{cache:'no-store'});
      if(rf.ok){
        const rfd = await rf.json();
        (rfd.list||[]).forEach(function(f){ flightFlags[f.id] = {rt:!!f.reqTrunc, hf:!!f.hasFull}; });
        document.querySelector('#finishedFlights tbody').innerHTML = (rfd.list||[]).map(function(f){
          return '<tr style="cursor:pointer" onclick="selectFlight('+f.id+')"><td>#'+f.id+'</td>'+modelCell(f.model,f.searchPrompt,f.routeReason,f.countTokens,f.tools)+apiCell(f.translated)+'<td>'+fmtBytes(f.bytes)+'</td><td>'+(f.total||'-')+'</td><td>'+(f.gaveUp?'[重试尽]':(f.status?(f.status===200?'200':'['+f.status+']'):'-'))+'</td><td>'+(f.hitRate||'-')+(f.stripped>0?'<span style="color:#f48771">[剥'+f.stripped+']</span>':'')+'</td><td>'+(f.cacheAge||'-')+'</td><td>'+(f.firstByte||'-')+'</td><td>'+(f.tps||'-')+'</td><td>'+f.ended+'</td></tr>';
        }).join('');
      }
    }catch(e){}
    // 回填保留个数（用户正在输入时不覆盖）
    var fcInp = document.getElementById('finishedCapInput');
    if(document.activeElement !== fcInp) fcInp.value = d.finishedCap;
    // 日志
    logEl.textContent = (d.lines||[]).join('\n');
    document.getElementById('count').textContent = d.lines?d.lines.length:0;
    // 日志页自动滚到底（仅日志标签；状态页推流时主窗口不应被拽到底）
    if(stick && document.querySelector('.tab.active').dataset.tab === 'logs') window.scrollTo(0, document.body.scrollHeight);
  }catch(e){
    stEl.className='st dead';
    stEl.textContent='🔴 已断开（代理可能已退出）';
  }
}
poll();
setInterval(poll, 500);

// 配置标签
// fillCfgLists 用 /__configs 的结果填充切换下拉 cfgSelect（全部，当前打勾）。
function fillCfgLists(dc){
  const sel = document.getElementById('cfgSelect');
  sel.innerHTML = (dc.files||[]).map(f => '<option value="'+f+'"'+(f===dc.current?' selected':'')+'>'+f+'</option>').join('');
}

async function loadConfig(){
  try{
    const [r, rc] = await Promise.all([
      fetch('/__config',{cache:'no-store'}),
      fetch('/__configs',{cache:'no-store'})
    ]);
    const d = await r.json();
    document.getElementById('cfgPath').textContent = d.path + (d.exists?'':'（文件不存在，保存将创建）');
    document.getElementById('cfg').value = d.content || '';
    cfgLoaded = true;
    genCodexCmds();
    const dc = await rc.json();
    fillCfgLists(dc);
    lastCfgFiles = JSON.stringify(dc.files);
  }catch(e){
    document.getElementById('cfgMsg').innerHTML = '<span class="err">加载失败: '+e+'</span>';
  }
}

// loadConfigList 只刷新配置文件下拉列表（不覆盖编辑器未保存内容），供配置页轮询/切回时用。
// 列表未变化时跳过重绘，避免下拉闪烁。
function loadConfigList(){
  fetch('/__configs',{cache:'no-store'}).then(r=>r.json()).then(d=>{
    const sig = JSON.stringify(d.files);
    if(sig !== lastCfgFiles){ fillCfgLists(d); lastCfgFiles = sig; }
  }).catch(()=>{});
}

function setMsg(cls, txt){ document.getElementById('cfgMsg').innerHTML = '<span class="'+cls+'">'+txt+'</span>'; }

document.getElementById('saveBtn').onclick = async () => {
  setMsg('', '保存中…');
  try{
    const r = await fetch('/__config',{method:'POST',body:document.getElementById('cfg').value});
    if(r.ok){ setMsg('ok', '已保存并重载'); const d = await r.json().catch(()=>{}); }
    else { const t = await r.text(); setMsg('err', '失败: '+t); }
  }catch(e){ setMsg('err', '失败: '+e); }
};
document.getElementById('reloadBtn').onclick = async () => {
  setMsg('', '重载中…');
  try{
    const r = await fetch('/__reload',{method:'POST'});
    if(r.ok) setMsg('ok','已重载'); else setMsg('err','失败: '+await r.text());
  }catch(e){ setMsg('err','失败: '+e); }
};

// ---- Codex 一键命令：按编辑框的 responses_listen + routes + 所选模型实时生成 ----
// 页面只给一行终端命令（DeepSeek 文档同款格式），脚本由服务端 /__codexsetup.ps1|sh
// 按 query 参数（base/model/catalog）烤制下发；值全是白名单字符，query 无需转义。
let codexModelList = [{name:'gpt-5-codex',src:'gpt-5*'},{name:'claude-fable-5',src:''}]; // 上次成功推导的目录模型
// responses_listen 为空 = Responses 口未启用：生成区整组置灰，只留「添加该功能」入口。
// 绝不回落默认地址——本机 8081 可能被别的程序占用，Codex 指过去会出莫名错误。
function setCodexUiEnabled(on){
  document.getElementById('codexEnable').style.display = on ? 'none' : '';
  document.getElementById('codexGen').classList.toggle('off', !on);
  if(!on){
    document.getElementById('codexCmdWin').textContent = '（Responses API 未启用：点上方「是，添加并保存」后这里才会生成命令）';
    document.getElementById('codexCmdSh').textContent = '（同上）';
  }
}
// 「是，添加并保存」：在编辑框 JSON 的 "listen" 行后插入 responses_listen，再走既有保存流程
document.getElementById('codexEnableBtn').onclick = () => {
  const ta = document.getElementById('cfg');
  const lines = ta.value.split('\n');
  let idx = lines.findIndex(l => /^\s*"listen"\s*:/.test(l));
  if(idx < 0) idx = lines.findIndex(l => l.indexOf('{') >= 0);
  if(idx < 0){ document.getElementById('codexEnableMsg').innerHTML = '<span class="err">找不到插入位置，请手动在配置 JSON 里加一行 "responses_listen": "127.0.0.1:8081",</span>'; return; }
  const indent = (lines[idx].match(/^\s*/) || [''])[0];
  lines.splice(idx + 1, 0, indent + '"responses_listen": "127.0.0.1:8081",');
  ta.value = lines.join('\n');
  document.getElementById('codexEnableMsg').textContent = '';
  document.getElementById('saveBtn').click();
  genCodexCmds();
  document.getElementById('codexMsg').innerHTML = '<span class="ok">已添加并保存 responses_listen（端口可在上方 JSON 改），监听口已随保存启动，现在就能用下面的命令</span>';
};
// 与 main.go matchModel 同语义：* 匹配任意长度（含 0）任意字符
function matchPat(p, n){
  const parts = String(p).split('*');
  if(parts.length === 1) return p === n;
  if(!n.startsWith(parts[0])) return false;
  n = n.slice(parts[0].length);
  for(let i = 1; i < parts.length - 1; i++){
    const idx = n.indexOf(parts[i]);
    if(idx < 0) return false;
    n = n.slice(idx + parts[i].length);
  }
  return n.endsWith(parts[parts.length - 1]);
}
// 从编辑框 routes 推导目录模型（Codex /model 菜单内容）：route.model 自己能命中 pattern 就用真名
// （thinking 查表更准），否则用去掉 * 的 pattern 代表名（* 可匹配 0 字符，必命中）；
// 纯 * 兜底路由固定叫 "Fallback"——它接住的是任意模型名，菜单里显示某个真实 model 名会误导。
// JSON 暂无法解析返回 null（保持现有清单不动，打字途中不闪）。
function deriveCodexModels(){
  let c;
  try{ c = JSON.parse(document.getElementById('cfg').value); }catch(e){ return null; }
  const out = [];
  for(const r of (c.routes || [])){
    if(!r || typeof r.pattern !== 'string' || !r.pattern) continue;
    if(r.pattern === 'Fallback' || r.pattern === 'fast_route') continue; // 保留名（* 兜底 / fast 通道在菜单里的名字）：全字撞名的路由不生效也不进菜单
    const m = (typeof r.model === 'string') ? r.model.trim() : '';
    const name = (r.pattern === '*') ? 'Fallback' : ((m && matchPat(r.pattern, m)) ? m : (r.pattern.replace(/\*/g, '') || m));
    // 名字要过服务端 slug 白名单（烤进脚本单引号串 + URL query 都不能有特殊字符）：不合规的跳过
    if(name && /^[A-Za-z0-9._*/:+-]+$/.test(name) && !out.some(o => o.name === name)) out.push({name: name, src: r.pattern});
  }
  // fast_route 也进菜单：条目名就是字面名 "fast_route"（翻译层认这个名字注入 speed:"fast" 走 fast 通道）；
  // 没配 model 不列——fast 分支要把 model 改写成 fast_route.model 发给上游
  const fr = c.fast_route;
  if(fr && typeof fr.url === 'string' && fr.url && typeof fr.model === 'string' && fr.model.trim()){
    if(!out.some(o => o.name === 'fast_route')) out.push({name: 'fast_route', src: 'fast_route'});
  }
  return out;
}
// 模型下拉按 routes 重建（选中项尽量保留）；只在清单变化时调用，避免打字途中闪烁
function refreshCodexModels(list){
  const sel = document.getElementById('codexModel');
  const prev = sel.value;
  sel.innerHTML = '';
  for(const o of list){
    const opt = document.createElement('option');
    opt.value = o.name;
    opt.textContent = (o.src && o.src !== o.name) ? o.name + '（' + o.src + '）' : o.name;
    sel.appendChild(opt);
  }
  const oc = document.createElement('option'); oc.value = '__custom__'; oc.textContent = '自定义…'; sel.appendChild(oc);
  const orr = document.createElement('option'); orr.value = '__restore__'; orr.textContent = '（还原默认 Codex 配置）'; sel.appendChild(orr);
  let found = false;
  for(const opt of sel.options){ if(opt.value === prev){ found = true; break; } }
  sel.value = found ? prev : sel.options[0].value;
  document.getElementById('codexModelCustom').style.display = (sel.value === '__custom__') ? '' : 'none';
}
function codexModelChoice(){
  const v = document.getElementById('codexModel').value;
  if(v === '__custom__'){
    const m = document.getElementById('codexModelCustom').value.trim();
    return /^[A-Za-z0-9._*/:+-]+$/.test(m) ? m : codexModelList[0].name;
  }
  return v;
}
// 拼两行终端命令；qs 的值全是白名单字符（slug 白名单 / http 地址），无需 URL 转义
function codexScriptCmds(qs){
  const o = location.origin;
  return {
    win: "irm '" + o + "/__codexsetup.ps1?" + qs + "' | iex",
    sh: "bash <(curl -fsSL '" + o + "/__codexsetup.sh?" + qs + "')"
  };
}
function genCodexCmds(){
  const ta = document.getElementById('cfg');
  if(!ta.value.trim()) return; // 配置尚未加载
  // 还原不碰代理：即使 responses_listen 缺失也直接给命令（也就无需「添加该功能」提示）
  if(codexModelChoice() === '__restore__'){
    document.getElementById('codexMsg').textContent = '';
    setCodexUiEnabled(true);
    const cmds = codexScriptCmds('model=__restore__');
    document.getElementById('codexCmdWin').textContent = cmds.win;
    document.getElementById('codexCmdSh').textContent = cmds.sh;
    return;
  }
  let c;
  try{ c = JSON.parse(ta.value); }
  catch(e){
    // 打字途中解析失败：不动现有内容，只提示
    document.getElementById('codexMsg').innerHTML = '<span class="err">配置 JSON 暂无法解析，命令保持上次有效内容</span>';
    return;
  }
  const addr = String(c.responses_listen || '').trim();
  if(!addr || /['"\s]/.test(addr)){ document.getElementById('codexMsg').textContent = ''; setCodexUiEnabled(false); return; }
  document.getElementById('codexMsg').textContent = '';
  setCodexUiEnabled(true);
  // 保留名检查：pattern 全字撞 "Fallback"/"fast_route" 的路由不生效（服务端路由时直接跳过），点名提醒改名
  const reservedHit = (c.routes || []).find(r => r && (r.pattern === 'Fallback' || r.pattern === 'fast_route'));
  if(reservedHit){
    document.getElementById('codexMsg').innerHTML = '<span class="err">⚠ pattern 不允许全字叫 "' + reservedHit.pattern + '"（Codex 菜单的保留名：* 兜底路由=Fallback、fast 通道=fast_route），撞名的路由不生效，请改名</span>';
  }
  const derived = deriveCodexModels();
  if(derived !== null && derived.length &&
      derived.map(o => o.name).join('|') !== codexModelList.map(o => o.name).join('|')){
    codexModelList = derived;
    refreshCodexModels(codexModelList);
  }
  // 目录 = routes 各 pattern 的代表名全集 + 选中项（脚本侧还会并入已存在的目录条目）
  const cat = codexModelList.map(o => o.name);
  const sel = codexModelChoice();
  if(!cat.includes(sel)) cat.push(sel);
  // 上下文窗口/压缩阈值随命令烤进脚本（写进模型目录每个条目）；输入非法时回落默认，
  // 服务端另有 4096..2M / 1..99 的白名单兜底
  let ctx = parseInt(document.getElementById('codexCtx').value, 10);
  if(!(ctx >= 4096 && ctx <= 2000000)) ctx = 262144;
  let compact = parseInt(document.getElementById('codexCompact').value, 10);
  if(!(compact >= 1 && compact <= 99)) compact = 95;
  const cmds = codexScriptCmds('base=' + 'http://' + addr + '/v1' + '&model=' + sel + '&catalog=' + cat.join(',') + '&ctx=' + ctx + '&compact=' + compact);
  document.getElementById('codexCmdWin').textContent = cmds.win;
  document.getElementById('codexCmdSh').textContent = cmds.sh;
}
document.getElementById('cfg').addEventListener('input', genCodexCmds);
document.getElementById('codexModel').onchange = () => {
  document.getElementById('codexModelCustom').style.display =
    (document.getElementById('codexModel').value === '__custom__') ? '' : 'none';
  genCodexCmds();
};
document.getElementById('codexModelCustom').addEventListener('input', genCodexCmds);
document.getElementById('codexCtx').addEventListener('input', genCodexCmds);
document.getElementById('codexCompact').addEventListener('input', genCodexCmds);
function codexCopyCmd(id, hint){
  return async () => {
    try{
      await navigator.clipboard.writeText(document.getElementById(id).textContent);
      document.getElementById('codexMsg').innerHTML = '<span class="ok">已复制，' + hint + '</span>';
    }catch(e){ document.getElementById('codexMsg').innerHTML = '<span class="err">复制失败: '+e+'（可手动选中命令复制）</span>'; }
  };
}
document.getElementById('codexCopyWin').onclick = codexCopyCmd('codexCmdWin', '粘贴到 PowerShell 窗口回车即运行');
document.getElementById('codexCopySh').onclick = codexCopyCmd('codexCmdSh', '粘贴到终端回车即运行');

// ---- 配置文件切换 ----
document.getElementById('cfgSelect').onchange = async () => {
  const name = document.getElementById('cfgSelect').value;
  const sw = document.getElementById('cfgSwitchMsg');
  sw.innerHTML = '切换中…';
  try{
    const r = await fetch('/__switch',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({name})});
    if(r.ok){
      const d = await r.json();
      sw.innerHTML = '<span class="ok">已切换到 '+d.current+'</span>';
      loadConfig();
    } else {
      sw.innerHTML = '<span class="err">失败: '+await r.text()+'</span>';
      loadConfig();
    }
  }catch(e){
    sw.innerHTML = '<span class="err">失败: '+e+'</span>';
  }
};
document.getElementById('cfgRefreshBtn').onclick = async () => {
  try{
    const r = await fetch('/__configs',{cache:'no-store'});
    const d = await r.json();
    fillCfgLists(d);
    document.getElementById('cfgSwitchMsg').innerHTML = '<span class="ok">列表已刷新</span>';
  }catch(e){
    document.getElementById('cfgSwitchMsg').innerHTML = '<span class="err">刷新失败: '+e+'</span>';
  }
};

// ---- 配置文件 新建/重命名/删除 ----
function setSwitchMsg(cls, txt){ document.getElementById('cfgSwitchMsg').innerHTML = '<span class="'+cls+'">'+txt+'</span>'; }

document.getElementById('cfgNewBtn').onclick = async () => {
  const name = prompt('新建配置文件名（无需 .json 后缀）：');
  if(!name) return;
  setSwitchMsg('', '新建中…');
  try{
    const r = await fetch('/__newconfig',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({name:name})});
    if(r.ok){ const d = await r.json(); setSwitchMsg('ok', '已新建并切换到 '+d.current); loadConfig(); }
    else { setSwitchMsg('err', '失败: '+await r.text()); }
  }catch(e){ setSwitchMsg('err', '失败: '+e); }
};

document.getElementById('cfgRenameBtn').onclick = async () => {
  const sel = document.getElementById('cfgSelect');
  // 旧名用 prompt 输入（默认下拉值，可改），跟下拉选中解耦——理由同删除。
  const old = prompt('重命名哪个配置（输入文件名）：', sel.value);
  if(!old) return;
  const newName = prompt('将「'+old+'」重命名为（无需 .json 后缀）：');
  if(!newName) return;
  setSwitchMsg('', '重命名中…');
  try{
    const r = await fetch('/__renameconfig',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({old:old,new:newName})});
    if(r.ok){ const d = await r.json(); setSwitchMsg('ok', '已重命名，当前 '+d.current); loadConfig(); }
    else { setSwitchMsg('err', '失败: '+await r.text()); }
  }catch(e){ setSwitchMsg('err', '失败: '+e); }
};

// 删除配置：点「删除」弹出窗口选要删的（排除当前生效的，当前删不掉），
// 与切换下拉 cfgSelect 解耦：下拉切换会即时生效，不能把删除目标绑在它上面。
const delModal = document.getElementById('cfgDelModal');
const delSel = document.getElementById('cfgDelSelect');
const delMsg = document.getElementById('cfgDelMsg');
function delModalShow(){ delModal.style.display = 'flex'; }
function delModalHide(){ delModal.style.display = 'none'; }

document.getElementById('cfgDelBtn').onclick = async () => {
  try{
    const r = await fetch('/__configs',{cache:'no-store'});
    const d = await r.json();
    const deletable = (d.files||[]).filter(f => f !== d.current);
    delSel.innerHTML = deletable.map(f => '<option value="'+f+'">'+f+'</option>').join('');
    delSel.disabled = deletable.length === 0;
    delMsg.textContent = deletable.length === 0 ? '当前目录下没有可删除的配置' : '';
    delModalShow();
  }catch(e){ setSwitchMsg('err', '加载配置列表失败: '+e); }
};
delModal.onclick = (ev) => { if(ev.target === delModal) delModalHide(); }; // 点遮罩关闭
document.getElementById('cfgDelCancelBtn').onclick = delModalHide;
document.getElementById('cfgDelConfirmBtn').onclick = async () => {
  const name = delSel.value;
  if(!name){ delMsg.textContent = '请先选择要删除的配置'; return; }
  if(!confirm('确定删除「'+name+'」？此操作不可恢复。')) return;
  if(!confirm('再次确认：真的要删除「'+name+'」吗？')) return;
  delMsg.textContent = '删除中…';
  try{
    const r = await fetch('/__delconfig',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({name:name})});
    if(r.ok){ delModalHide(); setSwitchMsg('ok', '已删除 '+name); loadConfig(); }
    else { delMsg.textContent = '失败: '+await r.text(); }
  }catch(e){ delMsg.textContent = '失败: '+e; }
};

// ---- 清空统计（切换配置不再自动清，独立按钮）----
document.getElementById('resetStatsBtn').onclick = async () => {
  if(!confirm('确定清空所有累计统计？（含最近完成的流列表；在途流与流编号不清）')) return;
  try{
    const r = await fetch('/__resetstats',{method:'POST'});
    if(!r.ok) alert('失败: '+await r.text());
  }catch(e){ alert('失败: '+e); }
};

// ---- 设置保留完成流个数 N ----
// 轮询每 0.5s 用服务端值回填输入框（输入框无焦点时）。点「设置」会先触发 mousedown 把焦点
// 移出输入框，若轮询恰好插在 mousedown 与 click 之间，输入框被刷回旧值、click 读到旧值——
// 所以 mousedown 时先把值存下，click 用存下的值，杜绝这个竞态。
var fcCapPending = null;
document.getElementById('finishedCapBtn').onmousedown = function(){
  fcCapPending = document.getElementById('finishedCapInput').value;
};
document.getElementById('finishedCapBtn').onclick = async () => {
  var n = parseInt(fcCapPending !== null ? fcCapPending : document.getElementById('finishedCapInput').value, 10);
  fcCapPending = null;
  if(isNaN(n) || n < 0) n = 0;
  if(n > 200) n = 200;
  document.getElementById('finishedCapInput').value = n;
  var msg = document.getElementById('finishedCapMsg');
  msg.innerHTML = '<span style="color:#9a9a9a">设置中…</span>';
  try{
    const r = await fetch('/__finishedcap',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({n:n})});
    if(r.ok){ const d = await r.json(); msg.innerHTML = '<span class="ok">已设为 '+d.n+'</span>'; }
    else { msg.innerHTML = '<span class="err">失败: '+await r.text()+'</span>'; }
  }catch(e){ msg.innerHTML = '<span class="err">失败: '+e+'</span>'; }
};

// ---- 清空日志（仅内存缓冲，不影响 log_file）----
document.getElementById('clearLogsBtn').onclick = async () => {
  try{
    const r = await fetch('/__clearlogs',{method:'POST'});
    if(!r.ok) alert('失败: '+await r.text());
  }catch(e){ alert('失败: '+e); }
};

// ---- 关闭在途流内容查看（同时停止自动跟踪，避免下次 poll 又被打开）----
document.getElementById('flightViewClose').onclick = () => {
  // 关闭手选单流：清 selectedFlight 回到自动跟踪/隐藏状态，但不动 autoTrack 勾选
  selectedFlight = 0;
  flightEnded = false;
  lastRaw = '';
  flightViewWhat = 'resp';
  lastReqRaw = '';
  flightViewTree = false;
  var fv = document.getElementById('flightView');
  fv.innerHTML = '';
  fv.style.gridTemplateColumns = '';
  updateFlightViewChrome();
  // 由轮询根据 autoTrack 决定显示 grid 或隐藏，这里不强制隐藏
};
// 自动跟踪最新流开关：勾选时清手选单流，回 grid；取消勾选则隐藏
document.getElementById('autoTrackChk').onchange = function(){
  autoTrack = this.checked;
  if(autoTrack){
    selectedFlight = 0;
    flightEnded = false;
    lastRaw = '';
    flightViewWhat = 'resp';
    lastReqRaw = '';
    flightViewTree = false;
  }
  updateFlightViewChrome();
};
// 最多并排显示多少个在途流（1..20）
document.getElementById('maxFlightsInput').onchange = function(){
  var n = parseInt(this.value, 10);
  if(isNaN(n) || n < 1) n = 1;
  if(n > 20) n = 20;
  this.value = n;
  maxFlights = n;
};
document.getElementById('flightViewRawBtn').onclick = () => {
  flightViewRaw = !flightViewRaw;
  document.getElementById('flightViewRawBtn').textContent = flightViewRaw ? '显示解析' : '显示原始';
  var fv = document.getElementById('flightView');
  if(selectedFlight){
    if(flightViewTree){ flightViewTree = false; updateFlightViewChrome(); } // 切 解析/原始 先退出交互树
    // 手选单流：请求体视图按请求体重渲染，输出视图用 lastRaw 重渲染
    if(flightViewWhat==='req'){
      if(lastReqRaw) renderReqBody();
    } else if(lastRaw){
      if(flightViewRaw){ fv.textContent = lastRaw; } else { var h = parseSSEHTML(lastRaw); fv.innerHTML = h || '<span class="ss-empty">（未解析出内容，点「显示原始」查看 SSE）</span>'; }
    }
  } else if(autoTrack){
    // 多流模式：用缓存的 autoRaws 重渲染每个 cell
    Array.from(fv.children).forEach(function(cell){
      var raw = autoRaws[cell.dataset.id];
      var pre = cell.querySelector('.cellPre');
      if(pre && raw !== undefined){
        if(flightViewRaw){ pre.textContent = raw; } else { var h = parseSSEHTML(raw); pre.innerHTML = h || '<span class="ss-empty">（未解析出内容，点「显示原始」查看）</span>'; }
      }
    });
  }
};
// ---- 看请求体/看输出切换（仅手选单流；请求体拉一次即静态，下载用同一缓存）----
document.getElementById('flightViewWhatBtn').onclick = async () => {
  if(!selectedFlight) return;
  var fv = document.getElementById('flightView');
  if(flightViewWhat === 'resp'){
    flightViewWhat = 'req';
    flightViewTree = false;
    updateFlightViewChrome();
    fv.textContent = '加载中…';
    try{
      const r = await fetch('/__flightreq?id='+selectedFlight,{cache:'no-store'});
      if(r.ok){
        lastReqRaw = await r.text();
        renderReqBody();
      } else {
        lastReqRaw = '';
        fv.textContent = '（'+await r.text()+'）'; // 404: 流不存在或未记录请求体
      }
    }catch(e){ fv.textContent = '（请求体拉取失败）'; }
    updateFlightViewChrome();
  } else {
    flightViewWhat = 'resp';
    flightViewTree = false;
    updateFlightViewChrome();
    // 回到输出视图：用 lastRaw 立即重渲染，未结束的流下一次 poll 继续刷新
    renderRespFromLastRaw();
  }
};
// ---- 交互式 JSON 树查看（仅「储存完整结构体」开启时按钮可见；默认全部折叠，点键展开）----
document.getElementById('flightViewTreeBtn').onclick = async () => {
  if(!selectedFlight) return;
  var fv = document.getElementById('flightView');
  if(flightViewTree){
    // 退出交互：回到当前 请求体/输出 的常规渲染
    flightViewTree = false;
    updateFlightViewChrome();
    if(flightViewWhat==='req'){ if(lastReqRaw) renderReqBody(); } else renderRespFromLastRaw();
    return;
  }
  // 取完整数据：请求体视图用已拉取的 lastReqRaw（开关开启时即完整体）；输出视图现拉 full=1。
  // 该流数据残缺（rt=截断版 / hf=false=无完整副本）时不拉取，直接提示——残缺内容成不了树。
  var fl = flightFlags[selectedFlight] || {};
  var text = '', err = '';
  if(flightViewWhat==='req'){
    text = lastReqRaw;
    if(fl.rt === true) err = '（该流请求体只剩截断版（前 256KB），不是完整 JSON，无法交互查看）';
    else if(!text) err = '（该流未记录请求体，可点「看输出」再点回重试）';
  } else if(fl.hf === false){
    err = '（该流未记录完整输出：开启前已开始/已清空/无透传内容）';
  } else {
    fv.textContent = '加载中…';
    try{
      const r = await fetch('/__flight?id='+selectedFlight+'&full=1',{cache:'no-store'});
      if(r.ok) text = await r.text();
      else err = '（'+await r.text()+'）';
    }catch(e){ err = '（完整输出拉取失败）'; }
  }
  flightViewTree = true;
  updateFlightViewChrome();
  if(err){ fv.textContent = err; return; }
  // 先按整个 JSON 解析；失败再按 SSE 事件流解析成数组；都不行则提示。
  var val, ok = false;
  try{ val = JSON.parse(text); ok = true; }
  catch(e){
    var arr = sseToJSONArray(text);
    if(arr.length){ val = arr; ok = true; }
  }
  if(!ok){ fv.textContent = '（内容不是 JSON 也不是 SSE 事件流，无法交互查看）'; return; }
  fv.innerHTML = '';
  fv.appendChild(jsonTreeRoot(val));
};
// ---- 下载（仅「储存完整结构体」开启时按钮可见）：点击才拉取完整内容，Blob 落盘 ----
// saveBlobText 落盘文本：能解析成 JSON 则美化（pretty，2 空格缩进）存 .json，否则逐字原文存 .txt。
function saveBlobText(text, base){
  var ext = '.txt';
  try{
    text = JSON.stringify(JSON.parse(text), null, 2);
    ext = '.json';
  }catch(e){}
  var a = document.createElement('a');
  a.href = URL.createObjectURL(new Blob([text], {type:'application/octet-stream'}));
  a.download = base+ext;
  a.click();
  setTimeout(function(){ URL.revokeObjectURL(a.href); }, 1000);
}
document.getElementById('flightViewDlBtn').onclick = async () => {
  if(!selectedFlight) return;
  try{
    const r = await fetch('/__flightreq?id='+selectedFlight,{cache:'no-store'});
    if(!r.ok){ alert('下载失败: '+await r.text()); return; }
    saveBlobText(await r.text(), 'flight-'+selectedFlight+'-request');
  }catch(e){ alert('下载失败: '+e); }
};
document.getElementById('flightViewDlOutBtn').onclick = async () => {
  if(!selectedFlight) return;
  try{
    const r = await fetch('/__flight?id='+selectedFlight+'&full=1',{cache:'no-store'});
    if(!r.ok){ alert('下载失败: '+await r.text()); return; }
    saveBlobText(await r.text(), 'flight-'+selectedFlight+'-output');
  }catch(e){ alert('下载失败: '+e); }
};
// ---- 储存完整结构体开关：POST 服务端，失败回滚勾选 ----
document.getElementById('fullStoreChk').onchange = async function(){
  var want = this.checked;
  try{
    const r = await fetch('/__fullstore',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({enabled:want})});
    if(!r.ok) throw await r.text();
    fullStoreOn = want;
  }catch(e){ alert('设置失败: '+e); this.checked = !want; }
  updateFlightViewChrome();
};

// ---- 缓存命中明细：hover tooltip + 点击放大弹窗 ----
var latestModelStats = [];
var latestCacheObs = [];
var latestClsHits = 0;    // 分类器命中总量（classifiers：无论是否分流/关思考都计）
var latestClsNoThink = 0; // 其中实际关思考的改写数（classifierNoThink）
function cacheHitPct(cr, input, cc){ const d=input+cr+(cc||0); return d>0 ? (cr*100/d).toFixed(1)+'%' : '-'; }
// cacheRowsHTML 生成明细表格；big=true 用大字号（弹窗用）
function cacheRowsHTML(big){
  if(!latestModelStats.length) return '<div style="color:#888">暂无数据</div>';
  var fs = big ? '14px' : '12px';
  var h = '<table style="border-collapse:collapse;width:100%;font-size:'+fs+'"><thead><tr style="color:#9a9a9a;text-align:left">'+
    '<th style="padding:3px 12px 3px 0">模型</th><th style="padding:3px 12px 3px 0;text-align:right">命中率</th>'+
    '<th style="padding:3px 12px 3px 0;text-align:right">命中token</th><th style="padding:3px 12px 3px 0;text-align:right">写入token</th>'+
    '<th style="padding:3px 12px 3px 0;text-align:right">未命中token</th><th style="padding:3px 0;text-align:right">输出token</th></tr></thead><tbody>';
  latestModelStats.forEach(function(m){
    h += '<tr style="color:#d4d4d4"><td style="padding:3px 12px 3px 0">'+esc(m.model)+'</td>'+
      '<td style="padding:3px 12px 3px 0;text-align:right">'+cacheHitPct(m.cacheRead, m.input, m.cacheCreation)+'</td>'+
      '<td style="padding:3px 12px 3px 0;text-align:right">'+fmtNum(m.cacheRead)+'</td>'+
      '<td style="padding:3px 12px 3px 0;text-align:right">'+fmtNum(m.cacheCreation||0)+'</td>'+
      '<td style="padding:3px 12px 3px 0;text-align:right">'+fmtNum(m.input)+'</td>'+
      '<td style="padding:3px 0;text-align:right">'+fmtNum(m.output)+'</td></tr>';
  });
  return h + '</tbody></table>';
}
function refreshCacheTip(){ document.getElementById('cacheTip').innerHTML = '<div style="color:#9a9a9a;margin-bottom:4px">缓存命中明细（按上游模型）</div>' + cacheRowsHTML(false); }
function refreshCacheModal(){
  var tCr=0, tIn=0, tCc=0;
  latestModelStats.forEach(function(m){ tCr+=m.cacheRead; tIn+=m.input; tCc+=(m.cacheCreation||0); });
  document.getElementById('cacheModalTotal').textContent = '总计 '+cacheHitPct(tCr, tIn, tCc);
  document.getElementById('cacheModalBody').innerHTML = cacheRowsHTML(true) + cacheObsHTML();
}
// 实测缓存时间表（按上游 URL+模型）：数据来自代理侧对同会话相邻流的实测，内存态
function cacheObsHTML(){
  if(!latestCacheObs.length) return '';
  var h = '<div style="color:#9a9a9a;margin:14px 0 4px">实测缓存时间（按上游 URL+模型）</div>'+
    '<table style="border-collapse:collapse;width:100%;font-size:14px"><thead><tr style="color:#9a9a9a;text-align:left">'+
    '<th style="padding:3px 12px 3px 0">模型</th><th style="padding:3px 12px 3px 0">URL</th>'+
    '<th style="padding:3px 12px 3px 0;text-align:right">缓存时间 ≥</th><th style="padding:3px 12px 3px 0;text-align:right">缓存时间 &lt;</th>'+
    '<th style="padding:3px 0;text-align:right">观测<span class="thq" title="支撑当前上下界的观测条数。上下界交叉时（新观测否定旧界——上游缓存时间中途变化或被提前驱逐），被否一侧作废重测，观测次数同步归零重计">?</span></th>'+
    '<th style="padding:3px 0 3px 12px;text-align:right">结论形成<span class="thq" title="当前上/下界数值形成至今的时长（m:ss 递增）。任一界的数值发生变化（含矛盾作废重测）就重新起算；只新增支撑观测、数值不变时不重置">?</span></th></tr></thead><tbody>';
  latestCacheObs.forEach(function(o){
    h += '<tr style="color:#d4d4d4"><td style="padding:3px 12px 3px 0">'+esc(o.model)+'</td>'+
      '<td style="padding:3px 12px 3px 0">'+esc(o.url)+'</td>'+
      '<td style="padding:3px 12px 3px 0;text-align:right">'+(o.alive||'-')+'</td>'+
      '<td style="padding:3px 12px 3px 0;text-align:right">'+(o.dead||'-')+'</td>'+
      '<td style="padding:3px 0;text-align:right">'+o.samples+'</td>'+
      '<td style="padding:3px 0 3px 12px;text-align:right">'+(o.age||'-')+'</td></tr>';
  });
  return h + '</tbody></table>';
}
// 事件委托到卡片容器：hover 显示 tooltip，click 打开放大弹窗
cardsEl.addEventListener('mouseover', function(e){ if(e.target.closest('#cacheCard')){ refreshCacheTip(); document.getElementById('cacheTip').style.display='block'; } });
cardsEl.addEventListener('mouseout', function(e){ if(e.target.closest('#cacheCard')){ document.getElementById('cacheTip').style.display='none'; } });
cardsEl.addEventListener('click', function(e){ if(e.target.closest('#cacheCard')){ refreshCacheModal(); document.getElementById('cacheModal').style.display='flex'; } });
// tooltip 跟随鼠标定位
document.addEventListener('mousemove', function(e){
  var tip = document.getElementById('cacheTip');
  if(tip.style.display !== 'none'){ tip.style.left = Math.min(e.clientX+12, window.innerWidth-560)+'px'; tip.style.top = (e.clientY+12)+'px'; }
  var rt = document.getElementById('retryTip');
  if(rt.style.display !== 'none'){ rt.style.left = Math.min(e.clientX+12, window.innerWidth-300)+'px'; rt.style.top = (e.clientY+12)+'px'; }
  var ct = document.getElementById('clsTip');
  if(ct.style.display !== 'none'){ ct.style.left = Math.min(e.clientX+12, window.innerWidth-560)+'px'; ct.style.top = (e.clientY+12)+'px'; }
});
// ---- 重试明细：hover tooltip + 点击放大弹窗（仿缓存命中）----
function retryRowsHTML(big){
  var rows = latestModelStats.filter(function(m){ return m.retries>0; });
  if(!rows.length) return '<div style="color:#888">暂无重试</div>';
  var fs = big ? '14px' : '12px';
  var h = '<table style="border-collapse:collapse;width:100%;font-size:'+fs+'"><thead><tr style="color:#9a9a9a;text-align:left">'+
    '<th style="padding:3px 12px 3px 0">模型</th><th style="padding:3px 0;text-align:right">重试次数</th></tr></thead><tbody>';
  rows.forEach(function(m){
    h += '<tr style="color:#d4d4d4"><td style="padding:3px 12px 3px 0">'+esc(m.model)+'</td><td style="padding:3px 0;text-align:right">'+m.retries+'</td></tr>';
  });
  return h + '</tbody></table>';
}
function refreshRetryTip(){ document.getElementById('retryTip').innerHTML = '<div style="color:#9a9a9a;margin-bottom:4px">重试明细（按路由目标模型）</div>' + retryRowsHTML(false); }
function refreshRetryModal(){
  var t=0;
  latestModelStats.forEach(function(m){ t+=m.retries; });
  document.getElementById('retryModalTotal').textContent = '总计 '+t;
  document.getElementById('retryModalBody').innerHTML = retryRowsHTML(true);
}
cardsEl.addEventListener('mouseover', function(e){ if(e.target.closest('#retryCard')){ refreshRetryTip(); document.getElementById('retryTip').style.display='block'; } });
cardsEl.addEventListener('mouseout', function(e){ if(e.target.closest('#retryCard')){ document.getElementById('retryTip').style.display='none'; } });
cardsEl.addEventListener('click', function(e){ if(e.target.closest('#retryCard')){ refreshRetryModal(); document.getElementById('retryModal').style.display='flex'; } });
// ---- 分类器明细：hover tooltip + 点击放大弹窗（仿重试）----
function clsRowsHTML(big){
  var fs = big ? '14px' : '12px';
  return '<table style="border-collapse:collapse;width:100%;font-size:'+fs+'"><tbody>'+
    '<tr style="color:#d4d4d4"><td style="padding:3px 12px 3px 0">命中分类器特征</td><td style="padding:3px 0;text-align:right">'+latestClsHits+'</td></tr>'+
    '<tr style="color:#d4d4d4"><td style="padding:3px 12px 3px 0">其中关思考改写</td><td style="padding:3px 0;text-align:right">'+latestClsNoThink+'</td></tr>'+
    '</tbody></table>';
}
function refreshClsTip(){ document.getElementById('clsTip').innerHTML = '<div style="color:#9a9a9a;margin-bottom:4px">分类器明细（命中=无论是否分流/关思考都计）</div>' + clsRowsHTML(false); }
function refreshClsModal(){
  document.getElementById('clsModalTotal').textContent = latestClsHits>0 ? '关思考占比 '+Math.round(latestClsNoThink*100/latestClsHits)+'%' : '';
  document.getElementById('clsModalBody').innerHTML = clsRowsHTML(true);
}
cardsEl.addEventListener('mouseover', function(e){ if(e.target.closest('#clsCard')){ refreshClsTip(); document.getElementById('clsTip').style.display='block'; } });
cardsEl.addEventListener('mouseout', function(e){ if(e.target.closest('#clsCard')){ document.getElementById('clsTip').style.display='none'; } });
cardsEl.addEventListener('click', function(e){ if(e.target.closest('#clsCard')){ refreshClsModal(); document.getElementById('clsModal').style.display='flex'; } });
</script>
</body>
</html>`
