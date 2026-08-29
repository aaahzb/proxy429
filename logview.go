package main

import (
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
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
	remotePath        = "/__remote"        // POST 切换 allow_remote（开/关内网访问），仅本机可操作
	configsPath       = "/__configs"       // GET 列出当前配置目录下所有 .json（供切换）
	switchPath        = "/__switch"        // POST 切换到指定配置文件并即时生效
	newConfigPath     = "/__newconfig"     // POST 新建配置文件（空白模板）并切换
	codexSetupPath    = "/__codexsetup"    // GET codex-setup.ps1 模板（配置页实时生成 Codex 脚本用）
	renameConfigPath  = "/__renameconfig"  // POST 重命名配置文件
	delConfigPath     = "/__delconfig"     // POST 删除配置文件（不允许删当前在用的）
	resetStatsPath    = "/__resetstats"    // POST 清空累计统计（切换配置不再自动清）
	clearLogsPath     = "/__clearlogs"     // POST 清空内存日志缓冲
	flightPath        = "/__flight"        // GET 在途流透传内容（?id=N，已完成流也查此）
	recentFlightsPath = "/__recentflights" // GET 最近完成的流列表（摘要）
	finishedCapPath   = "/__finishedcap"   // POST 设置保留完成流个数 N
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
		lastRate = int64(float64(b-prevRateBytes) / dt)
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
	RouteReason  int32  `json:"routeReason"` // 路由原因：0=透传 1=pattern 2=分类器 3=fast 4=多模态 5=搜索
	Translated   string `json:"translated"`  // 翻译口来源（"responses"），model 列显示 [translate] 前缀
	SearchPrompt string `json:"searchPrompt,omitempty"`
}

// logData 是 /__logs/data 返回的 JSON：最近日志 + 全量状态计数 + 在途流列表。
type logData struct {
	Lines        []string          `json:"lines"`
	Active       int               `json:"active"`
	Waiting      int               `json:"waiting"`
	Version      string            `json:"version"`
	CacheRead    int64             `json:"cacheRead"`
	InputTokens  int64             `json:"inputTokens"`
	OutputTokens int64             `json:"outputTokens"`
	ModelStats   []modelUsageEntry `json:"modelStats"`
	BytesForward int64             `json:"bytesForward"`
	Rate         int64             `json:"rate"` // bytes/s
	Retries      int64             `json:"retries"`
	Classifiers  int64             `json:"classifiers"`
	AvgFirstByte float64           `json:"avgFirstByte"` // ms
	Tps          float64           `json:"tps"`          // tok/s
	Flights      []flightInfo      `json:"flights"`
	Listen       string            `json:"listen"`      // 当前监听地址
	AllowRemote  bool              `json:"allowRemote"` // 是否允许内网访问
	CurrentCfg   string            `json:"currentCfg"`  // 当前生效的配置文件名
	FinishedCap  int32             `json:"finishedCap"` // 保留完成流个数 N（状态页可改）
}

// logDataHandler 返回最近 maxLogBuf 行日志和全量状态（JSON），供页面每 500ms 轮询。
func logDataHandler(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	c := cfg.Load()
	d := logData{Lines: logBuf.window(0, maxLogBuf), Version: Version, Listen: c.Listen, AllowRemote: c.AllowRemote, CurrentCfg: filepath.Base(currentConfigPath()), FinishedCap: finishedCap.Load()}
	stats.mu.Lock()
	d.Active = stats.active
	d.Waiting = stats.waiting
	d.CacheRead = stats.cacheRead
	d.InputTokens = stats.inputTokens
	d.OutputTokens = stats.outputTokens
	stats.mu.Unlock()
	d.ModelStats = stats.snapshotModelStats()
	d.BytesForward = stats.bytesForward.Load()
	d.Rate = computeRate(d.BytesForward)
	d.Retries = stats.statusRetries.Load()
	d.Classifiers = stats.classifierRewrites.Load()
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
			RouteReason:  f.routeReason.Load(),
			Translated:   f.translated,
			SearchPrompt: f.searchPrompt,
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
	if err := os.WriteFile(currentConfigPath(), body, 0644); err != nil {
		http.Error(w, "写入文件失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := reloadConfig(); err != nil {
		// 文件已保存但重载失败（极少见，因上面已校验过）：旧配置仍在跑，告知用户。
		http.Error(w, "已保存但重载失败: "+err.Error(), http.StatusInternalServerError)
		return
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

// remoteHandler 切换 allow_remote（开/关内网访问转发通道）。仅本机可操作--
// 即便 allow_remote 已开，远程请求调这个端点也返回 403，防止内网恶意设备自己开锁。
// 切换后立即更新内存配置（即时生效），并落盘以便重启后保持。
func remoteHandler(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "解析失败: "+err.Error(), http.StatusBadRequest)
		return
	}
	cur := cfg.Load()
	nc := *cur // 浅拷贝（routes 等切片仍共享底层数组，但只改 AllowRemote 字段，安全）
	nc.AllowRemote = body.Enabled
	cfg.Store(&nc)
	if err := persistConfig(&nc); err != nil {
		// 内存已生效但落盘失败：告知用户，避免下次重启回退成无感状态。
		http.Error(w, "已生效但写盘失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("[访问] allow_remote=%v (由本机 %s 设置)", body.Enabled, r.RemoteAddr)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "allowRemote": body.Enabled})
}

// persistConfig 把配置写回 currentConfigPath()（规范化的 2 空格 JSON）。
// 供 allow_remote 开关落盘用：读当前内存配置、改字段、写回。routes/keys 等字段均保留。
func persistConfig(c *Config) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(currentConfigPath(), data, 0644)
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

// codexSetupHandler 返回内嵌的 codex-setup.ps1 模板原文（含 BOM，下载可直接运行）。
// 配置页 JS 取回后按当前编辑框内容替换 $BAKED_BASE_URL / $BAKED_MODEL 锚点实时生成。
// 限本机访问（与其余 /__* 管理端点同规则）。
func codexSetupHandler(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(codexSetupPS1)
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
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// resetStatsHandler 清空累计统计（bytes/tokens/retries/classifierRewrites/延迟样本）。
// 切换配置不再自动清统计，需手动点「清空统计」按钮。限本机访问。
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

// recentFlightsHandler 返回最近完成流列表（摘要，不含 content），最新在前。限本机访问。
func recentFlightsHandler(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	finishedMu.Lock()
	out := make([]map[string]any, 0, len(finished))
	for i := len(finished) - 1; i >= 0; i-- {
		ff := finished[i]
		out = append(out, map[string]any{
			"id":          ff.id,
			"model":       ff.model,
			"routeReason": ff.routeReason,
			"translated":  ff.translated,
			"status":      ff.status,
			"bytes":       ff.bytes,
			"stage":       ff.stage,
			"ended":       ff.ended.Format("15:04:05"),
			"hitRate":     cacheHitRate(ff.cacheRead, ff.inTokens),
			"cacheRead":   ff.cacheRead,
			"inTokens":    ff.inTokens,
			"firstByte":   fmtFirstByte(ff.firstByteMs),
			"tps":         fmtTps(ff.tps),
			"searchPrompt": ff.searchPrompt,
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
	for len(finished) > b.N {
		finished = finished[1:]
	}
	finishedMu.Unlock()
	log.Printf("[完成流] 保留个数设为 %d", b.N)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "n": b.N})
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
  #log { white-space:pre-wrap; word-break:break-word; line-height:1.45; }
  #cfg { width:100%; min-height:60vh; background:#1e1e1e; color:#d4d4d4; border:1px solid #333; border-radius:4px; padding:8px; font:inherit; }
  #codexBox { margin-top:14px; border-top:1px solid #2a2a2a; padding-top:10px; }
  #codexBox .t { color:#9a9a9a; margin-bottom:6px; }
  #codexBox .dim { color:#777; font-size:12px; margin-top:4px; }
  #codexPs { width:100%; min-height:220px; background:#181818; color:#b8b8b8; border:1px solid #333; border-radius:4px; padding:8px; font:12px/1.4 Consolas,monospace; }
  #codexModelCustom { background:#1e1e1e; color:#d4d4d4; border:1px solid #333; border-radius:3px; padding:4px 6px; font:inherit; }
  #codexEnable { background:#2a2410; border:1px solid #6b5d1f; border-radius:4px; padding:8px 10px; margin-bottom:8px; color:#d8c27a; }
  #codexEnable .dim { color:#9a8a55; font-size:12px; margin-top:4px; }
  #codexGen.off { opacity:.35; pointer-events:none; }
  button { background:#264f78; color:#d4d4d4; border:1px solid #3a6ea5; border-radius:3px; padding:5px 12px; cursor:pointer; font:inherit; }
  button:hover { background:#2f6090; }
  button.ghost { background:#333; border-color:#444; }
  button.warn { background:#6a4a1a; border-color:#8a6a2a; color:#ffd87a; }
  button.warn:hover { background:#7a5a2a; }
  button.danger { background:#5a2a2a; border-color:#7a3a3a; color:#ffb0b0; }
  button.danger:hover { background:#6a3a3a; }
  button.armed { background:#8a2a2a; border-color:#a73a3a; color:#ffb0b0; animation:pulse 1s infinite; }
  @keyframes pulse { 0%,100%{opacity:1} 50%{opacity:0.55} }
  #access { background:#252526; border:1px solid #333; border-radius:4px; padding:8px 12px; margin-bottom:10px; display:flex; gap:10px; align-items:center; flex-wrap:wrap; }
  #access .lbl { color:#9a9a9a; }
  #access .val { font-weight:bold; }
  .ax-safe { color:#4ec9b0; } .ax-warn { color:#d9b34a; } .ax-open { color:#c75; }
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
  <div id="access">
    <span class="lbl">访问控制：</span>
    <span class="val" id="axStatus">…</span>
    <span class="lbl" id="axListen"></span>
    <button id="remoteBtn" class="warn">开启内网访问</button>
  </div>
  <div><span class="st idle" id="status">● 连接中</span> <span id="curCfg" style="margin-left:10px;color:#888"></span></div>
  <div class="cards" id="cards"></div>
  <div style="margin:6px 0"><button id="resetStatsBtn" class="ghost">清空统计</button>
    <label style="margin-left:12px;color:#9a9a9a">保留完成流: <input id="finishedCapInput" type="number" min="0" max="200" value="10" style="width:50px;background:#1e1e1e;color:#d4d4d4;border:1px solid #333;border-radius:3px;padding:2px 4px;font:inherit"></label>
    <button id="finishedCapBtn" class="ghost">设置</button>
    <span id="finishedCapMsg"></span>
  </div>
  <div>在途流 <label style="margin-left:8px;color:#9a9a9a;font-weight:normal"><input type="checkbox" id="autoTrackChk" checked>自动跟踪最新</label> <label style="color:#9a9a9a;font-weight:normal">最多显示 <input type="number" id="maxFlightsInput" min="1" max="20" value="3" style="width:40px;background:#1e1e1e;color:#d4d4d4;border:1px solid #333;border-radius:3px;padding:2px 4px;font:inherit"> 个</label></div>
  <table id="flights"><thead><tr><th></th><th>#</th><th>model</th><th>字节</th><th>状态</th></tr></thead><tbody></tbody></table>
  <div id="flightViewWrap" style="display:none;margin-top:8px">
    <div>流 #<span id="flightViewId"></span> 输出 <button id="flightViewRawBtn" class="ghost">显示原始</button> <button id="flightViewClose" class="ghost">关闭</button></div>
    <div id="flightView" style="max-height:300px;overflow:auto;background:#1e1e1e;border:1px solid #333;padding:8px"></div>
  </div>
  <div style="margin-top:10px">最近完成的流</div>
  <table id="finishedFlights"><thead><tr><th>#</th><th>model</th><th>字节</th><th>状态码</th><th>缓存命中</th><th>首字</th><th>tok/s</th><th>结束</th></tr></thead><tbody></tbody></table>
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
      <p>工具里的 model 名照常参与路由匹配：在 routes 加一条如 <code>gpt-5*</code> 即可指定上游与改写模型。改动需重启；访问控制与主端口同规则（allow_remote=false 时仅本机）。</p>
      <p>翻译规则与 cc-switch 3.20.0 一致：<code>reasoning.effort</code> 按模型分类映射——adaptive 模型（fable-5/mythos-5/mythos-preview/sonnet-5/opus-4-8/4-7/4-6/sonnet-4-6）翻成 <code>thinking:adaptive</code> + <code>output_config.effort</code>（fable-5/mythos-5 关不掉 thinking，显式 none 翻成 effort:low）；其余模型翻成 budget_tokens（low 2048 / medium 8192 / high 16384 / xhigh·max·ultra 24576）。查表用客户端发来的 model 名（路由改写之前），想让表生效就把客户端 model 直接填目标模型名。工具映射（function/custom/namespace/tool_search/web_search/input_file）与完整映射表见使用说明.md「Responses 翻译映射表」。</p>
      <p><b>Codex CLI 接入</b>：最省事——本控制台「配置」标签下方按编辑框实时生成 codex-setup.ps1（地址取 <code>responses_listen</code>；routes 每个 pattern 的代表名全部写进 Codex <code>/model</code> 菜单，下拉选中项为默认模型；复制后粘贴进 PowerShell 即运行，零交互，可下载 .ps1）。仓库根目录另有交互版 <code>codex-setup.ps1</code>（仿 DeepSeek 官方脚本：选模型、备份后改写 config.toml、写模型目录、可一键还原）。手动：编辑 <code>~/.codex/config.toml</code>——顶层 <code>model_provider = "proxy429"</code>、<code>model = "gpt-5-codex"</code>、<code>preferred_auth_method = "apikey"</code> + <code>forced_login_method = "api"</code>（免官方登录），加 <code>[model_providers.proxy429]</code> 段（<code>base_url = "http://127.0.0.1:8081/v1"</code>、<code>wire_api = "responses"</code>、<code>experimental_bearer_token</code> 填任意占位串）。改完重启 Codex。逐步教程见使用说明.md「让 Codex CLI 走代理」。</p>
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
      <p>命中率 = cache_read / (input + cache_read)。高命中率（90%+）主要来自上游模型（DeepSeek/Kimi）的原生 context caching，代理只透传 cache_read_input_tokens，不做额外缓存优化。点击状态页「缓存命中」卡片可看按真实上游模型分组的明细。</p>
      <h3>统计字段</h3>
      <ul>
      <li><b>首字</b>：从发出请求到收到首个输出字节耗时（ms）。</li>
      <li><b>tok/s</b>：流式输出速率 = 输出 token 数 / 流式耗时。</li>
      <li><b>缓存命中</b>：见上。</li>
      </ul>
      <h3>配置管理</h3>
      <ul>
      <li>配置页可新建 / 重命名 / 删除 / 切换配置文件。</li>
      <li>切到配置页后自动每 3 秒刷新文件列表，增删配置文件无需手动按「刷新列表」。</li>
      <li>当前生效的配置不可删除。</li>
      </ul>
      <h3>访问控制</h3>
      <p>默认仅本机访问。点「开启内网访问」放开到局域网（需二次确认，按钮变红再点一次生效）。</p>
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
    <div class="t">Codex 一键配置脚本（Windows）—— 按上方编辑框实时生成，地址取自 responses_listen，模型目录取自 routes 的 pattern</div>
    <div id="codexEnable" style="display:none">
      当前配置没有 responses_listen，Responses API 监听口未启用。要为本配置增加 Responses API 功能吗？
      <button id="codexEnableBtn">是，添加并保存</button>
      <span id="codexEnableMsg"></span>
      <div class="dim">会在上方 JSON 的 "listen" 行后加一行 "responses_listen": "127.0.0.1:8081"（端口可改）并立即保存；监听口只在代理启动时创建，保存后还需重启代理才生效。</div>
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
      <button id="codexCopyBtn" class="ghost">复制脚本</button>
      <button id="codexDlBtn" class="ghost">下载 .ps1</button>
      <span id="codexMsg"></span>
    </div>
    <textarea id="codexPs" readonly spellcheck="false" placeholder="模板加载中…"></textarea>
    <div class="dim">用法：复制后直接粘贴进 PowerShell 窗口回车即运行（等价 irm|iex），或下载后以 powershell -ExecutionPolicy Bypass -File 运行。脚本免交互、免官方登录、token 占位（真实 key 由上面路由的 api 注入）。下拉列出 routes 每个 pattern 的一个代表名（route.model 能命中 pattern 时用真名，否则用去 * 的 pattern）：这些名字全部写进 Codex 的 /model 菜单，选中项为默认模型。改了 responses_listen 需先「保存并重载」并重启代理后再用生成的脚本。</div>
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
      ensurePsTemplate();
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
// translated 非空（Responses 监听口翻译进来的流）时最前面再加 [translate]。
function modelCell(model, prompt, routeReason, translated){
  var tag = routeTag(routeReason);
  if(tag) model = tag + model;
  if(translated) model = '<span style="color:#c586c0">[translate]</span>' + model;
  if(!prompt) return '<td>'+model+'</td>';
  var esc = String(prompt).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
  return '<td title="'+esc+'">'+model+'</td>';
}
// 路由原因 -> model 列前缀标签（与 main.go route* 枚举对齐：0透传 1pattern 2分类器 3fast 4多模态 5搜索）。
// 透传不加前缀；标签淡蓝色与 model 链区分。
function routeTag(r){
  var tags = ['','[Pattern]','[分类器]','[Fast]','[多模态]','[搜索]'];
  if(!tags[r]) return '';
  return '<span style="color:#7ec8e3">'+tags[r]+'</span>';
}
// 在途流状态灯：转发中(绿)/等待首字节(黄)/请求阶段(白)，用 emoji 与「仅本机访问」状态灯同等大小。
function flightDot(f){
  if(f.stage===3) return '🟢';
  if(f.stage>=1) return '🟡';
  return '⚪';
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
  var curText = null, curThink = null;
  function textBlock(){ if(!curText){ curText = {type:'text', text:''}; blocks.push(curText); } return curText; }
  function thinkBlock(){ if(!curThink){ curThink = {type:'think', text:''}; blocks.push(curThink); } return curThink; }
  raw.split('\n').forEach(function(line){
    if(line.indexOf('data:') !== 0) return;
    var p = line.slice(5).trim();
    if(p === '' || p === '[DONE]') return;
    var j;
    try { j = JSON.parse(p); } catch(e) { return; }
    // error 事件：流内错误（限流/overloaded 等），显示错误信息
    if(j.type === 'error' && j.error){
      blocks.push({type:'error', msg: j.error.message || j.error.type || 'error'});
      curText = null; curThink = null;
      return;
    }
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
      var items = (b.results||[]).map(function(r){ return '<div>· '+esc(r.title||'')+' <span class="ss-url">'+esc(r.url||'')+'</span></div>'; }).join('');
      html += '<div class="ss-search">🔍 搜索结果 ('+(b.results||[]).length+')'+items+'</div>';
    }
  });
  return html;
}
// 点击在途流/完成流行：切到单流模式（保持 autoTrack 勾选，由 selectedFlight 优先决定显示），显示该流。
function selectFlight(id){
  // 手选单流：不取消 autoTrack 勾选（点行查看时勾选保持），
  // 仅设 selectedFlight 让轮询切到单流分支；autoTrack 重新勾选时再清手选回 grid。
  selectedFlight = id;
  flightEnded = false;
  lastRaw = '';
  var fv = document.getElementById('flightView');
  fv.innerHTML = '';
  fv.style.display = 'block';
  fv.style.gridTemplateColumns = '';
  fv.style.whiteSpace = 'pre-wrap';
  fv.style.wordBreak = 'break-all';
  fv.textContent = '加载中…';
  document.getElementById('flightViewWrap').style.display = 'block';
  document.getElementById('flightViewId').textContent = id;
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
    // 状态灯
    stEl.className = 'st ' + (d.active>0?'active':d.waiting>0?'wait':'idle');
    stEl.textContent = d.active>0?'● 流式中':d.waiting>0?'● 等待首字节':'● 空闲';
    document.getElementById('curCfg').textContent = d.currentCfg ? ('配置: '+d.currentCfg) : '';
    // 访问控制（listen + allow_remote 决定真实暴露状态）
    updateAccess(d);
    // 统计卡片
    cardsEl.innerHTML =
      card('活跃', d.active) + card('等待', d.waiting) +
      card('流出', fmtBytes(d.bytesForward)) + card('速率', fmtBytes(d.rate)+'/s') +
      '<div class="card" id="cacheCard" style="cursor:pointer"><div class="k">缓存命中</div><div class="v">'+cacheHitPct(d.cacheRead, d.inputTokens)+'</div></div>' + card('输入', fmtNum(d.inputTokens)) +
      card('输出', fmtNum(d.outputTokens)) + '<div class="card" id="retryCard" style="cursor:pointer"><div class="k">重试</div><div class="v">'+d.retries+'</div></div>' +
      card('分类器', d.classifiers) + card('首字', (d.avgFirstByte/1000).toFixed(2)+'s') +
      card('tok/s', d.tps.toFixed(1));
    // 缓存命中明细：缓存最新 modelStats，tooltip/modal 打开时实时刷新
    latestModelStats = d.modelStats || [];
    if(document.getElementById('cacheTip').style.display !== 'none') refreshCacheTip();
    if(document.getElementById('cacheModal').style.display !== 'none') refreshCacheModal();
    if(document.getElementById('retryTip').style.display !== 'none') refreshRetryTip();
    if(document.getElementById('retryModal').style.display !== 'none') refreshRetryModal();
    // 在途流
    const fs = d.flights || [];
    flightsBody.innerHTML = fs.map(f =>
      '<tr style="cursor:pointer" onclick="selectFlight('+f.id+')"><td>'+flightDot(f)+'</td><td>#'+f.id+'</td>'+modelCell(f.model,f.searchPrompt,f.routeReason,f.translated)+'<td>'+fmtBytes(f.bytes)+'</td><td>'+flightStatus(f)+'</td></tr>'
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
      if(!flightEnded){
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
        document.querySelector('#finishedFlights tbody').innerHTML = (rfd.list||[]).map(function(f){
          return '<tr style="cursor:pointer" onclick="selectFlight('+f.id+')"><td>#'+f.id+'</td>'+modelCell(f.model,f.searchPrompt,f.routeReason,f.translated)+'<td>'+fmtBytes(f.bytes)+'</td><td>'+(f.status||'-')+'</td><td>'+(f.hitRate||'-')+'</td><td>'+(f.firstByte||'-')+'</td><td>'+(f.tps||'-')+'</td><td>'+f.ended+'</td></tr>';
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
    stEl.textContent='● 已断开（代理可能已退出）';
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
    genPs();
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

// ---- Codex 一键脚本：取内嵌模板，按编辑框的 responses_listen + routes + 所选模型实时生成 ----
let psTemplate = null; // /__codexsetup 取回的模板文本（已去 BOM）
let codexModelList = [{name:'gpt-5-codex',src:'gpt-5*'},{name:'claude-fable-5',src:''}]; // 上次成功推导的目录模型
async function ensurePsTemplate(){
  if(psTemplate !== null) return;
  try{
    const t = await (await fetch('/__codexsetup',{cache:'no-store'})).text();
    psTemplate = t.replace(/^\uFEFF/, '');
    genPs();
  }catch(e){ document.getElementById('codexMsg').innerHTML = '<span class="err">模板加载失败: '+e+'</span>'; }
}
// responses_listen 为空 = Responses 口未启用：生成区整组置灰，只留「添加该功能」入口。
// 绝不回落默认地址——本机 8081 可能被别的程序占用，Codex 指过去会出莫名错误。
function setCodexUiEnabled(on){
  document.getElementById('codexEnable').style.display = on ? 'none' : '';
  document.getElementById('codexGen').classList.toggle('off', !on);
  if(!on) document.getElementById('codexPs').value = '# Responses API 未启用：点上方「是，添加并保存」后，这里才会生成脚本';
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
  genPs();
  document.getElementById('codexMsg').innerHTML = '<span class="ok">已添加并保存 responses_listen（端口可在上方 JSON 改）。<b>重启代理</b>后监听口才生效，之后 Codex 才能连上</span>';
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
// （thinking 查表更准），否则用去掉 * 的 pattern 代表名（* 可匹配 0 字符，必命中）；纯 * 兜底 route.model。
// JSON 暂无法解析返回 null（保持现有清单不动，打字途中不闪）。
function deriveCodexModels(){
  let c;
  try{ c = JSON.parse(document.getElementById('cfg').value); }catch(e){ return null; }
  const out = [];
  for(const r of (c.routes || [])){
    if(!r || typeof r.pattern !== 'string' || !r.pattern) continue;
    const m = (typeof r.model === 'string') ? r.model.trim() : '';
    const name = (m && matchPat(r.pattern, m)) ? m : (r.pattern.replace(/\*/g, '') || m);
    // 逗号是 BAKED_CATALOG 分隔符、引号/反斜杠会破坏 PS 字符串：这类名字跳过
    if(name && !/['"\s\\,]/.test(name) && !out.some(o => o.name === name)) out.push({name: name, src: r.pattern});
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
    return (m && !/['"\\\s]/.test(m)) ? m : codexModelList[0].name;
  }
  return v;
}
function genPs(){
  if(psTemplate === null) return;
  let c;
  try{ c = JSON.parse(document.getElementById('cfg').value); }
  catch(e){
    // 打字途中解析失败：不动现有内容，只提示
    document.getElementById('codexMsg').innerHTML = '<span class="err">配置 JSON 暂无法解析，脚本保持上次有效内容</span>';
    return;
  }
  const addr = String(c.responses_listen || '').trim();
  if(!addr || /['"\s]/.test(addr)){ document.getElementById('codexMsg').textContent = ''; setCodexUiEnabled(false); return; }
  document.getElementById('codexMsg').textContent = '';
  setCodexUiEnabled(true);
  const baseUrl = 'http://' + addr + '/v1';
  const derived = deriveCodexModels();
  if(derived !== null && derived.length &&
      derived.map(o => o.name).join('|') !== codexModelList.map(o => o.name).join('|')){
    codexModelList = derived;
    refreshCodexModels(codexModelList);
  }
  // 目录 = routes 各 pattern 的代表名全集 + 选中项（脚本侧还会并入已存在的目录条目）
  const cat = codexModelList.map(o => o.name);
  const sel = codexModelChoice();
  if(sel !== '__restore__' && !cat.includes(sel)) cat.push(sel);
  document.getElementById('codexPs').value = psTemplate
    .replace("$BAKED_BASE_URL = ''", "$BAKED_BASE_URL = '" + baseUrl + "'")
    .replace("$BAKED_MODEL    = ''", "$BAKED_MODEL    = '" + sel + "'")
    .replace("$BAKED_CATALOG  = ''", "$BAKED_CATALOG  = '" + cat.join(',') + "'");
}
document.getElementById('cfg').addEventListener('input', genPs);
document.getElementById('codexModel').onchange = () => {
  document.getElementById('codexModelCustom').style.display =
    (document.getElementById('codexModel').value === '__custom__') ? '' : 'none';
  genPs();
};
document.getElementById('codexModelCustom').addEventListener('input', genPs);
document.getElementById('codexCopyBtn').onclick = async () => {
  try{
    await navigator.clipboard.writeText(document.getElementById('codexPs').value);
    document.getElementById('codexMsg').innerHTML = '<span class="ok">已复制，粘贴到 PowerShell 窗口回车即运行</span>';
  }catch(e){ document.getElementById('codexMsg').innerHTML = '<span class="err">复制失败: '+e+'（可手动全选脚本框复制）</span>'; }
};
document.getElementById('codexDlBtn').onclick = () => {
  // 下载必须带 BOM：PS 5.1 把无 BOM 的 .ps1 按 ANSI(GBK) 读，中文会乱码
  const blob = new Blob(['\uFEFF' + document.getElementById('codexPs').value], {type:'text/plain;charset=utf-8'});
  const a = document.createElement('a');
  a.href = URL.createObjectURL(blob);
  a.download = 'codex-setup.ps1';
  a.click();
  URL.revokeObjectURL(a.href);
};

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
  if(!confirm('确定清空所有累计统计？')) return;
  try{
    const r = await fetch('/__resetstats',{method:'POST'});
    if(!r.ok) alert('失败: '+await r.text());
  }catch(e){ alert('失败: '+e); }
};

// ---- 设置保留完成流个数 N ----
document.getElementById('finishedCapBtn').onclick = async () => {
  var n = parseInt(document.getElementById('finishedCapInput').value, 10);
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
  var fv = document.getElementById('flightView');
  fv.innerHTML = '';
  fv.style.gridTemplateColumns = '';
  // 由轮询根据 autoTrack 决定显示 grid 或隐藏，这里不强制隐藏
};
// 自动跟踪最新流开关：勾选时清手选单流，回 grid；取消勾选则隐藏
document.getElementById('autoTrackChk').onchange = function(){
  autoTrack = this.checked;
  if(autoTrack){
    selectedFlight = 0;
    flightEnded = false;
    lastRaw = '';
  }
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
    // 手选单流：用 lastRaw 重渲染
    if(lastRaw){
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

// ---- 访问控制：allow_remote 开关（开启需二次确认）----
const axStatus = document.getElementById('axStatus');
const axListen = document.getElementById('axListen');
const remoteBtn = document.getElementById('remoteBtn');
let remoteArmed = false;       // 是否处于"再点一次确认"的待确认状态
let armedTimer = null;

function isLoopback(listen){
  return /^127\./.test(listen) || listen.startsWith('::1') || listen.startsWith('localhost') || listen.startsWith('[::1');
}

// poll 每 500ms 调一次，按 listen + allow_remote 算真实暴露状态并刷新按钮。
// 待确认（remoteArmed）时不覆盖按钮，让"再点一次"的红框保留到用户操作或超时。
function updateAccess(d){
  const lb = isLoopback(d.listen);
  axListen.textContent = 'listen: ' + d.listen;
  if(lb){
    axStatus.className = 'val ax-safe';
    axStatus.textContent = d.allowRemote ? '🟢 仅本机访问（listen 是环回）' : '🟢 仅本机访问';
  } else if(!d.allowRemote){
    axStatus.className = 'val ax-warn';
    axStatus.textContent = '🟡 listen 非环回但已拦截远程（安全）';
  } else {
    axStatus.className = 'val ax-open';
    axStatus.textContent = '🔴 内网可访问（远程设备能用你的 key 转发）';
  }
  if(remoteArmed) return; // 待确认中：保留红框按钮，不被轮询覆盖
  if(d.allowRemote){
    remoteBtn.className = 'danger';
    remoteBtn.textContent = '内网访问已开启 · 点击关闭';
  } else {
    remoteBtn.className = 'warn';
    remoteBtn.textContent = '开启内网访问';
  }
}

function disarmRemote(){
  remoteArmed = false;
  if(armedTimer){ clearTimeout(armedTimer); armedTimer = null; }
  remoteBtn.className = 'warn';
  remoteBtn.textContent = '开启内网访问';
}

async function postRemote(enabled){
  try{
    const r = await fetch('/__remote',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({enabled})});
    if(!r.ok){ axStatus.textContent = '操作失败: ' + await r.text(); return; }
    // 成功后 poll 会自动刷新状态；无需手动改按钮
  }catch(e){ axStatus.textContent = '操作失败: '+e; }
}

remoteBtn.onclick = () => {
  // 当前已开启 -> 直接关（关闭是安全操作，无需二次确认）
  if(remoteBtn.classList.contains('danger')){
    postRemote(false);
    return;
  }
  // 当前关闭 -> 开启需二次确认：第一次点进入待确认（红框脉冲），3s 内再点才生效
  if(!remoteArmed){
    remoteArmed = true;
    remoteBtn.className = 'armed';
    remoteBtn.textContent = '⚠️ 再点一次确认开放（3 秒内）';
    armedTimer = setTimeout(disarmRemote, 3000);
    return;
  }
  // 第二次点：真正开启
  disarmRemote();
  postRemote(true);
};

// ---- 缓存命中明细：hover tooltip + 点击放大弹窗 ----
var latestModelStats = [];
function cacheHitPct(cr, input){ return (input+cr)>0 ? (cr*100/(input+cr)).toFixed(1)+'%' : '-'; }
// cacheRowsHTML 生成明细表格；big=true 用大字号（弹窗用）
function cacheRowsHTML(big){
  if(!latestModelStats.length) return '<div style="color:#888">暂无数据</div>';
  var fs = big ? '14px' : '12px';
  var h = '<table style="border-collapse:collapse;width:100%;font-size:'+fs+'"><thead><tr style="color:#9a9a9a;text-align:left">'+
    '<th style="padding:3px 12px 3px 0">模型</th><th style="padding:3px 12px 3px 0;text-align:right">命中率</th>'+
    '<th style="padding:3px 12px 3px 0;text-align:right">命中token</th><th style="padding:3px 12px 3px 0;text-align:right">未命中token</th>'+
    '<th style="padding:3px 0;text-align:right">输出token</th></tr></thead><tbody>';
  latestModelStats.forEach(function(m){
    h += '<tr style="color:#d4d4d4"><td style="padding:3px 12px 3px 0">'+esc(m.model)+'</td>'+
      '<td style="padding:3px 12px 3px 0;text-align:right">'+cacheHitPct(m.cacheRead, m.input)+'</td>'+
      '<td style="padding:3px 12px 3px 0;text-align:right">'+fmtNum(m.cacheRead)+'</td>'+
      '<td style="padding:3px 12px 3px 0;text-align:right">'+fmtNum(m.input)+'</td>'+
      '<td style="padding:3px 0;text-align:right">'+fmtNum(m.output)+'</td></tr>';
  });
  return h + '</tbody></table>';
}
function refreshCacheTip(){ document.getElementById('cacheTip').innerHTML = '<div style="color:#9a9a9a;margin-bottom:4px">缓存命中明细（按上游模型）</div>' + cacheRowsHTML(false); }
function refreshCacheModal(){
  var tCr=0, tIn=0;
  latestModelStats.forEach(function(m){ tCr+=m.cacheRead; tIn+=m.input; });
  document.getElementById('cacheModalTotal').textContent = '总计 '+cacheHitPct(tCr, tIn);
  document.getElementById('cacheModalBody').innerHTML = cacheRowsHTML(true);
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
</script>
</body>
</html>`
