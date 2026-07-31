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
	logViewerPath = "/__logs"
	logDataPath   = "/__logs/data"
	configPath    = "/__config" // GET 取配置内容、POST 保存并重载
	reloadPath    = "/__reload" // POST 仅重载（不改动文件）
	remotePath    = "/__remote" // POST 切换 allow_remote（开/关内网访问），仅本机可操作
	configsPath   = "/__configs" // GET 列出当前配置目录下所有 .json（供切换）
	switchPath    = "/__switch"  // POST 切换到指定配置文件并即时生效
	newConfigPath    = "/__newconfig"    // POST 新建配置文件（空白模板）并切换
	renameConfigPath = "/__renameconfig" // POST 重命名配置文件
	delConfigPath    = "/__delconfig"    // POST 删除配置文件（不允许删当前在用的）
	resetStatsPath   = "/__resetstats"   // POST 清空累计统计（切换配置不再自动清）
	clearLogsPath    = "/__clearlogs"    // POST 清空内存日志缓冲
	flightPath       = "/__flight"       // GET 在途流透传内容（?id=N，已完成流也查此）
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
	RouteReason int32  `json:"routeReason"` // 路由原因：0=透传 1=pattern 2=分类器 3=fast 4=多模态 5=搜索
}

// logData 是 /__logs/data 返回的 JSON：最近日志 + 全量状态计数 + 在途流列表。
type logData struct {
	Lines        []string     `json:"lines"`
	Active       int          `json:"active"`
	Waiting      int          `json:"waiting"`
	Version      string       `json:"version"`
	CacheRead    int64        `json:"cacheRead"`
	InputTokens  int64        `json:"inputTokens"`
	OutputTokens int64        `json:"outputTokens"`
	BytesForward int64        `json:"bytesForward"`
	Rate         int64        `json:"rate"` // bytes/s
	Retries      int64        `json:"retries"`
	Classifiers  int64        `json:"classifiers"`
	AvgFirstByte float64      `json:"avgFirstByte"` // ms
	Tps          float64      `json:"tps"`          // tok/s
	Flights      []flightInfo `json:"flights"`
	Listen       string       `json:"listen"`      // 当前监听地址
	AllowRemote  bool         `json:"allowRemote"` // 是否允许内网访问
	CurrentCfg   string       `json:"currentCfg"`  // 当前生效的配置文件名
	FinishedCap  int32        `json:"finishedCap"` // 保留完成流个数 N（状态页可改）
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
			ID:          f.id,
			Model:       model,
			Phase:       int(f.phase.Load()),
			Bytes:       f.bytes.Load(),
			Status:      f.status,
			Stage:       f.stage.Load(),
			Attempt:     f.attempt.Load(),
			RouteReason: f.routeReason.Load(),
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

// readConfigTemplate 读取空白配置模板：依次尝试当前配置目录、可执行文件目录、工作目录的
// config.example.json，都找不到则返回 "{}"。供「新建配置」作为初始内容。
func readConfigTemplate() []byte {
	candidates := []string{filepath.Join(filepath.Dir(currentConfigPath()), "config.example.json")}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "config.example.json"))
	}
	candidates = append(candidates, "config.example.json")
	for _, p := range candidates {
		if data, err := os.ReadFile(p); err == nil {
			return data
		}
	}
	return []byte("{}\n")
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
	if configFilePath == oldFull {
		configFilePath = newFull
	}
	configMu.Unlock()
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
			"id":     ff.id,
			"model":  ff.model,
			"status": ff.status,
			"bytes":  ff.bytes,
			"stage":  ff.stage,
			"ended":     ff.ended.Format("15:04:05"),
			"hitRate":     cacheHitRate(ff.cacheRead, ff.inTokens),
			"cacheRead":   ff.cacheRead,
			"inTokens":    ff.inTokens,
			"firstByte":   fmtFirstByte(ff.firstByteMs),
			"tps":         fmtTps(ff.tps),
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
</style>
</head>
<body>
<div id="bar">
  <div class="tab active" data-tab="status">状态</div>
  <div class="tab" data-tab="logs">日志</div>
  <div class="tab" data-tab="config">配置</div>
  <span id="hint">关闭此标签页即隐藏 · 代理继续运行</span>
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
  <div style="margin-bottom:6px"><span id="count">0</span> 行 · 滚轮翻历史，自动滚到底</div>
  <pre id="log"></pre>
</div>
<button id="clearLogsBtn" class="ghost" style="position:fixed;right:16px;bottom:16px;z-index:20;display:none">清空日志</button>

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
</div>

<script>
const logEl = document.getElementById('log');
const stEl  = document.getElementById('status');
const cardsEl = document.getElementById('cards');
const flightsBody = document.querySelector('#flights tbody');
let stick = true;
let cfgLoaded = false;
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
    if (t.dataset.tab === 'config' && !cfgLoaded) loadConfig();
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
        if(idx[j.index]) idx[j.index].input += d.partial_json;
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
      card('缓存命中', fmtNum(d.cacheRead)) + card('输入', fmtNum(d.inputTokens)) +
      card('输出', fmtNum(d.outputTokens)) + card('重试', d.retries) +
      card('分类器', d.classifiers) + card('首字', (d.avgFirstByte/1000).toFixed(2)+'s') +
      card('tok/s', d.tps.toFixed(1));
    // 在途流
    const fs = d.flights || [];
    flightsBody.innerHTML = fs.map(f =>
      '<tr style="cursor:pointer" onclick="selectFlight('+f.id+')"><td>'+flightDot(f)+'</td><td>#'+f.id+'</td><td>'+f.model+'</td><td>'+fmtBytes(f.bytes)+'</td><td>'+flightStatus(f)+'</td></tr>'
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
          }
        });
        // 拉取每个在途流内容（独立更新各自 cell）
        show.forEach(function(f){
          fetch('/__flight?id='+f.id,{cache:'no-store'}).then(function(r){return r.ok?r.text():null;}).then(function(raw){
            if(raw===null) return;
            autoRaws[f.id] = raw;
            var pre = fv.querySelector('[data-id="'+f.id+'"] .cellPre');
            if(pre){
              var atBottom = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 2;
              if(flightViewRaw){ pre.textContent = raw; } else { var h = parseSSEHTML(raw); pre.innerHTML = h || '<span class="ss-empty">（未解析出内容，点「显示原始」查看）</span>'; }
              if(atBottom) pre.scrollTop = pre.scrollHeight;
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
          return '<tr style="cursor:pointer" onclick="selectFlight('+f.id+')"><td>#'+f.id+'</td><td>'+f.model+'</td><td>'+fmtBytes(f.bytes)+'</td><td>'+(f.status||'-')+'</td><td>'+(f.hitRate||'-')+'</td><td>'+(f.firstByte||'-')+'</td><td>'+(f.tps||'-')+'</td><td>'+f.ended+'</td></tr>';
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
    const dc = await rc.json();
    const sel = document.getElementById('cfgSelect');
    sel.innerHTML = (dc.files||[]).map(f => '<option value="'+f+'"'+(f===dc.current?' selected':'')+'>'+f+'</option>').join('');
  }catch(e){
    document.getElementById('cfgMsg').innerHTML = '<span class="err">加载失败: '+e+'</span>';
  }
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
    const sel = document.getElementById('cfgSelect');
    sel.innerHTML = (d.files||[]).map(f => '<option value="'+f+'"'+(f===d.current?' selected':'')+'>'+f+'</option>').join('');
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

document.getElementById('cfgDelBtn').onclick = async () => {
  const sel = document.getElementById('cfgSelect');
  // 用 prompt 输入要删的文件名（默认下拉当前值，可改成任意配置）：
  // 下拉切换会即时生效，选中项=当前生效配置删不掉，故删除目标不能绑死在下拉选中上。
  const name = prompt('输入要删除的配置文件名（不可删除当前生效的配置）：', sel.value);
  if(!name) return;
  if(!confirm('确定删除「'+name+'」？此操作不可恢复。')) return;
  setSwitchMsg('', '删除中…');
  try{
    const r = await fetch('/__delconfig',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({name:name})});
    if(r.ok){ setSwitchMsg('ok', '已删除 '+name); loadConfig(); }
    else { setSwitchMsg('err', '失败: '+await r.text()); }
  }catch(e){ setSwitchMsg('err', '失败: '+e); }
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
</script>
</body>
</html>`
