package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fyne.io/systray"
)

// Config 是代理的配置结构，对应 config.json。
type Config struct {
	Listen                     string           `json:"listen"`
	Upstream                   string           `json:"upstream"`
	MaxRetries                 int              `json:"max_retries"`
	BaseDelaySec               float64          `json:"base_delay_s"`
	MaxDelaySec                float64          `json:"max_delay_s"`
	TotalBudgetSec             float64          `json:"total_budget_s"`
	RetryStatusCodes           []int            `json:"retry_status_codes"`
	RespectRetryAfter          bool             `json:"respect_retry_after"`
	ClassifierThinkingDisabled bool             `json:"classifier_thinking_disabled"`
	ClassifierSystemPrefix     string           `json:"classifier_system_prefix"`
	ClassifierMaxTokens        int              `json:"classifier_max_tokens"`     // 命中分类器后压 max_tokens 到此值；0 不压（推荐，避免 thinking 截断导致 Claude Code 收不到安全判断）
	UpstreamHeaderTimeoutSec   float64          `json:"upstream_header_timeout_s"` // 等上游首字节的最长时间（秒）；超时认为请求卡住、内部重发。0 用默认 70s
	PingIntervalSec            float64          `json:"ping_interval_s"`           // 429 重试时向客户端发 SSE ping 保活的间隔（秒）；0 用默认 5s
	LogRequestDetail           bool             `json:"log_request_detail"`
	LogFile                    string           `json:"log_file"`                      // 日志文件路径；非空则日志同时追加写入此文件，方便无控制台（如 RemoteApp）复制查看
	RecentSampleWindow         int              `json:"recent_sample_window"`          // 状态行"最近X次"延迟/吞吐统计窗口大小；0 用默认 20
	Routes                     []RouteRule      `json:"routes"`                        // 模型路由规则；空则不路由，走默认 upstream
	ClassifierRoute            *ClassifierRoute `json:"classifier_route,omitempty"`    // 分类器请求专用路由；命中分类器时无视原 model 统一路由到此处；空则不启用
	FastRoute                  *FastRoute       `json:"fast_route,omitempty"`          // fast 模式请求专用路由；检测到 "speed":"fast" 的非分类器请求统一路由到此处；空则不启用
	MultimodalFallback         *MultimodalRoute `json:"multimodal_fallback,omitempty"` // 多模态兜底路由；请求含图片却命中 text_only 纯文本模型时改走此处；空则不启用
	SearchFallback             *SearchRoute     `json:"search_fallback,omitempty"`     // 搜索兜底路由；请求带搜索工具却命中 no_search 上游时改走此处；空则不启用
}

// RouteRule 定义一条模型路由：命中的请求改走指定上游，并替换 model 名与 API key。
// Pattern 用 * 通配模型名；命中后 URL 覆盖默认 upstream，API 覆盖客户端 token，Model 替换请求体 model 字段。
type RouteRule struct {
	Pattern  string `json:"pattern"`   // 模型名通配符，仅支持 *（匹配任意长度任意字符），如 "claude-opus*"
	URL      string `json:"url"`       // 目标上游 Base URL，如 https://api.deepseek.com
	API      string `json:"api"`       // 目标 API key，设为 Authorization: Bearer；空则透传客户端原 token
	Model    string `json:"model"`     // 替换成的目标模型名；空则不改 model 字段
	TextOnly bool   `json:"text_only"` // 目标模型仅支持纯文本；请求含图片时改走 multimodal_fallback 兜底
	NoSearch bool   `json:"no_search"` // 目标上游不支持搜索；请求带搜索工具时改走 search_fallback 兜底
}

// ClassifierRoute 定义分类器请求的专用路由：命中分类器（安全判断）的请求无视原 model，
// 统一改走指定上游。专用于把 Claude Code 的轻量安全判断请求甩到便宜模型，省主模型额度。
type ClassifierRoute struct {
	URL   string `json:"url"`   // 目标上游 Base URL
	API   string `json:"api"`   // 目标 API key；空则透传客户端原 token
	Model string `json:"model"` // 替换成的目标模型名；空则不改 model 字段
}

// FastRoute 定义 fast 模式请求的专用路由：检测到 "speed":"fast" 的非分类器请求统一改走指定上游。
// Claude Code /fast 不换模型名，只加速出字；代理负责在响应里注入假的 fast 限流 headers。
type FastRoute struct {
	URL   string `json:"url"`   // 目标上游 Base URL
	API   string `json:"api"`   // 目标 API key；空则透传客户端原 token
	Model string `json:"model"` // 替换成的目标模型名；空则不改 model 字段
}

// MultimodalRoute 定义多模态兜底路由：当请求含图片却命中 text_only 的纯文本模型时，
// 自动改走此处指定的多模态上游。NoSearch 标记该兜底也不支持搜索，带搜索的图片请求会改走 search_fallback。
type MultimodalRoute struct {
	URL      string `json:"url"`       // 目标上游 Base URL
	API      string `json:"api"`       // 目标 API key；空则透传客户端原 token
	Model    string `json:"model"`     // 替换成的目标模型名；空则不改 model 字段
	NoSearch bool   `json:"no_search"` // 该图片兜底不支持搜索；带搜索的图片请求改走 search_fallback
}

// SearchRoute 定义搜索兜底路由：当请求带搜索工具却命中 no_search 的不支持搜索上游时，
// 自动改走此处指定的支持搜索的上游。TextOnly 标记该兜底也不支持图片，带图片的搜索请求会改走 multimodal_fallback。
type SearchRoute struct {
	URL      string `json:"url"`       // 目标上游 Base URL
	API      string `json:"api"`       // 目标 API key；空则透传客户端原 token
	Model    string `json:"model"`     // 替换成的目标模型名；空则不改 model 字段
	TextOnly bool   `json:"text_only"` // 该搜索兜底也不支持图片；带图片的搜索请求改走 multimodal_fallback
}

// Version 是代理版本号，编译时用 -ldflags "-X main.Version=<git-short>" 注入；默认 dev。
var Version = "dev"

var cfg atomic.Pointer[Config]

// configFilePath 是配置文件路径，main 启动时设置，托盘 reload 复用。
var configFilePath string

// loadConfig 读取并解析配置文件并应用默认值。main 启动与托盘 reload 共用。
func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	if c.ClassifierSystemPrefix == "" {
		c.ClassifierSystemPrefix = "You are a security monitor"
	}
	if c.UpstreamHeaderTimeoutSec <= 0 {
		c.UpstreamHeaderTimeoutSec = 70 // 默认 70s：等上游首字节超时则内部重发
	}
	if c.PingIntervalSec <= 0 {
		c.PingIntervalSec = 5 // 默认 5s：429 重试时向客户端发 SSE ping 保活的间隔
	}
	if c.RecentSampleWindow <= 0 {
		c.RecentSampleWindow = 20 // 默认统计最近 20 次请求的首字延迟与 token/s
	}
	return &c, nil
}

// configExampleBytes 是内嵌的默认配置模板，首次运行时写入用户配置目录。
//
//go:embed config.example.json
var configExampleBytes []byte

// resolveConfigPath 决定配置文件路径，优先级：
//  1. -config 显式指定（最高，直接用）
//  2. 当前目录 config.json 存在（终端在项目目录运行）
//  3. 用户配置目录 proxy429/config.json（.app 双击、安装后运行；首次运行自动生成）
//
// 第 3 条让 macOS .app 双击启动也能找到配置：.app 的 cwd 是 /，没有 ./config.json，
// 故落到 ~/Library/Application Support/proxy429/config.json（macOS）、
// ~/.config/proxy429/config.json（Linux）、%AppData%/proxy429/config.json（Windows）。
func resolveConfigPath(flagPath string) string {
	if flagPath != "" {
		return flagPath
	}
	if _, err := os.Stat("config.json"); err == nil {
		return "config.json"
	}
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		return "config.json"
	}
	appDir := filepath.Join(dir, "proxy429")
	p := filepath.Join(appDir, "config.json")
	if _, err := os.Stat(p); err != nil {
		// 首次运行：建目录并写入内嵌的默认配置模板，避免双击 .app 后无配置启动失败。
		// 用户随后在网页「配置」标签里改成自己的上游/key 即可。
		if mkErr := os.MkdirAll(appDir, 0755); mkErr == nil {
			if wErr := os.WriteFile(p, configExampleBytes, 0644); wErr == nil {
				log.Printf("[配置] 首次运行，已生成默认配置: %s", p)
			}
		}
	}
	return p
}

// reloadConfig 重新读取配置文件并原子替换全局 cfg，同时清空所有累计统计，
// 效果等同于"关闭程序重新打开"的配置与统计（但不重新监听端口）。
// 返回错误供网页「重载」按钮反馈；失败时全局 cfg 保持旧配置不变。
func reloadConfig() error {
	c, err := loadConfig(configFilePath)
	if err != nil {
		log.Printf("[重载] 失败: %v", err)
		return err
	}
	cfg.Store(c)
	// 清空累计统计（active 保留：它反映当前活跃流数，不是累计值，清了会让在跑的流结束时 active-- 变 -1）。
	stats.mu.Lock()
	stats.cacheRead = 0
	stats.inputTokens = 0
	stats.outputTokens = 0
	stats.mu.Unlock()
	stats.bytesForward.Store(0)
	stats.statusRetries.Store(0)
	stats.classifierRewrites.Store(0)
	stats.resetSampleCap(c.RecentSampleWindow) // 清空"最近X次"延迟/吞吐样本并按新容量重建
	log.Printf("[重载] 配置已重新加载，统计已清空: http://%s -> %s (最多重试 %d 次, 分类器关thinking=%v)",
		c.Listen, c.Upstream, c.MaxRetries, c.ClassifierThinkingDisabled)
	return nil
}

// client 不设总超时（流式响应可能很长）；首字节超时由 Transport.ResponseHeaderTimeout 控制
// （main 里按 upstream_header_timeout_s 设置），超时走情况0 重发。重试节奏靠 total_budget_s 控制。
var client = &http.Client{
	Timeout: 0,
}

// ---- 实时状态行 ----
// 多个并发请求的 token 计数聚合到全局 stats，由独立 goroutine 每 100ms 原地刷新
// 一行（\r 回行首 + \033[K 清行尾），不新增日志行。log 输出经过 clearLineWriter，
// 写 log 前先清状态行，让日志往上滚、状态行始终停在最后一行。

// throughputSample 记录单次成功流的流式时长与产出，流结束时入窗，供状态行计算 token/s。
// 首字延迟单独用 []int64 环形缓冲，收到第一个 body 字节即入窗，让“首字”实时更新、不必等流结束。
type throughputSample struct {
	streamMs     int64 // 流式时长：第一个字节 -> 最后一个字节
	outputTokens int64 // 本流 output_tokens
}

// liveStats 是所有流的聚合计数器，网页控制台与托盘状态灯用它展示实时状态。
type liveStats struct {
	mu                 sync.Mutex
	active             int          // 当前透传中的流数量
	waiting            int          // 已发上游、等首字节的请求数（状态灯黄）
	cacheRead          int64        // 累计缓存命中 token（cache_read_input_tokens）
	inputTokens        int64        // 累计输入 token
	outputTokens       int64        // 累计输出 token（各流当前累积值之和，随流增长）
	bytesForward       atomic.Int64 // 累计已转发字节，流过程中实时增长（ARK 不在流中发 token，用它体现实时迸出）
	statusRetries      atomic.Int64 // 启动至今的重试次数（含状态码/超时/网络错误/体内错误，每重试一次 +1）
	classifierRewrites atomic.Int64 // 启动至今命中分类器请求并关 thinking 的次数

	sampleMu  sync.Mutex         // 保护下面的滑动窗口（独立于 mu，避免长流持锁）
	fbSamples []int64            // 首字延迟环形缓冲（收到首字节即 push，状态行“首字”实时更新）
	fbHead    int                // fbSamples 下一个写入位置
	tpSamples []throughputSample // 吞吐环形缓冲（流结束 push），供计算 token/s
	tpHead    int                // tpSamples 下一个写入位置
	sampleCap int                // 环形缓冲容量（= RecentSampleWindow，两个窗口共享）
}

var stats liveStats

// resetSampleCap 重置滑动窗口容量并清空已有样本。main 启动与 reload 共用：
// 容量随配置变化，环形缓冲索引需同步归零。
func (s *liveStats) resetSampleCap(cap int) {
	s.sampleMu.Lock()
	defer s.sampleMu.Unlock()
	s.fbSamples = nil
	s.fbHead = 0
	s.tpSamples = nil
	s.tpHead = 0
	s.sampleCap = cap
}

// pushFirstByte 把首字延迟写入环形缓冲。收到第一个 body 字节即调用，
// 让状态行“首字”在流式返回的瞬间就更新，无需等整流结束。
func (s *liveStats) pushFirstByte(firstByteMs int64) {
	s.sampleMu.Lock()
	defer s.sampleMu.Unlock()
	if s.sampleCap <= 0 {
		return
	}
	if len(s.fbSamples) < s.sampleCap {
		s.fbSamples = append(s.fbSamples, firstByteMs)
		return
	}
	s.fbSamples[s.fbHead] = firstByteMs
	s.fbHead = (s.fbHead + 1) % s.sampleCap
}

// pushThroughput 把流式时长与 output_tokens 写入环形缓冲，流结束时调用，供计算 token/s。
func (s *liveStats) pushThroughput(streamMs, outputTokens int64) {
	s.sampleMu.Lock()
	defer s.sampleMu.Unlock()
	if s.sampleCap <= 0 {
		return
	}
	sm := throughputSample{streamMs: streamMs, outputTokens: outputTokens}
	if len(s.tpSamples) < s.sampleCap {
		s.tpSamples = append(s.tpSamples, sm)
		return
	}
	s.tpSamples[s.tpHead] = sm
	s.tpHead = (s.tpHead + 1) % s.sampleCap
}

// recentLatency 计算最近 X 次样本的平均首字延迟（毫秒）与 token/s（加权吞吐：
// ΣoutputTokens / Σ流式时长）。首字延迟从 fbSamples 算（收到首字节即入窗），
// token/s 从 tpSamples 算（流结束入窗）。无样本或总时长为 0 时对应项返回 0。
func (s *liveStats) recentLatency() (avgFirstByteMs float64, tps float64) {
	s.sampleMu.Lock()
	defer s.sampleMu.Unlock()
	if len(s.fbSamples) > 0 {
		var sumFB int64
		for _, v := range s.fbSamples {
			sumFB += v
		}
		avgFirstByteMs = float64(sumFB) / float64(len(s.fbSamples))
	}
	var sumST, sumOut int64
	for _, sm := range s.tpSamples {
		sumST += sm.streamMs
		sumOut += sm.outputTokens
	}
	if sumST > 0 {
		tps = float64(sumOut) / (float64(sumST) / 1000.0)
	}
	return
}

// toInt64 把 JSON 解析出的数字（float64 / json.Number 等）安全转成 int64。
func toInt64(v interface{}) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i
		}
	}
	return 0
}

// humanNum 把大数字压成 1.2k / 3.40M 形式，便于在单行里显示。
func humanNum(n int64) string {
	if n < 0 {
		n = 0
	}
	switch {
	case n < 1000:
		return strconv.FormatInt(n, 10)
	case n < 1000000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%.2fM", float64(n)/1000000)
	}
}

// humanBytes 把字节数压成 2.3KB / 1.20MB 形式，用于显示实时流量。
func humanBytes(n int64) string {
	if n < 0 {
		n = 0
	}
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.2fMB", float64(n)/(1024*1024))
	}
}

// maxLogBuf 是日志环形缓冲区最大行数，也是网页「查看日志」页可翻看的历史上限。
const maxLogBuf = 500

// flight 跟踪单个进行中的请求，供网页控制台展示在途流（#id + 耗时/字节）。
type flight struct {
	id          uint64       // 递增编号
	start       time.Time    // 请求发出时间
	phase       atomic.Int32 // 0=等待响应, 1=已收到响应（开始转发）
	status      int          // HTTP 状态码（phase=1 时有效）
	bytes       atomic.Int64 // 已转发字节数
	origModel   string       // 客户端原始 model；路由改写后用于响应流回改
	targetModel string       // 路由改写后的目标 model；非空且≠origModel 时 forward 会回改
	modelLogged atomic.Bool  // 是否已打 [改写] 日志，只打一次
}

// flightRegistry 管理所有进行中的请求。
type flightRegistry struct {
	mu     sync.RWMutex
	m      map[uint64]*flight
	nextID atomic.Uint64
}

var flights flightRegistry

func (fr *flightRegistry) register(f *flight) {
	fr.mu.Lock()
	fr.m[f.id] = f
	fr.mu.Unlock()
}

func (fr *flightRegistry) unregister(id uint64) {
	fr.mu.Lock()
	delete(fr.m, id)
	fr.mu.Unlock()
}

func (fr *flightRegistry) snapshot() []*flight {
	fr.mu.RLock()
	defer fr.mu.RUnlock()
	out := make([]*flight, 0, len(fr.m))
	for _, f := range fr.m {
		out = append(out, f)
	}
	// 按 ID 排序，保证渲染顺序稳定。
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// ---- 日志环形缓冲 ----

// logRing 保存最近的日志行，用于终端尺寸变化时重绘下半屏。
type logRing struct {
	mu    sync.Mutex
	lines []string
	head  int // 下一条写入位置
	len   int // 当前存储条数
}

var logBuf logRing

func (r *logRing) append(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.len < cap(r.lines) {
		r.lines = append(r.lines, line)
		r.len++
	} else {
		r.lines[r.head] = line
		r.head = (r.head + 1) % cap(r.lines)
	}
}

// length 返回当前存储的日志条数。
func (r *logRing) length() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.len
}

// window 返回从底部偏移 offset 行开始的 n 行日志（按时间顺序，旧到新）。
// offset=0 等价于 recent(n)；offset>0 表示跳过最新 offset 行再取 n 行。
func (r *logRing) window(offset, n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	total := r.len
	if total == 0 || n <= 0 {
		return nil
	}
	if offset >= total {
		offset = total - 1
	}
	end := total - offset // 窗口右端（exclusive，从最旧算起）
	start := end - n
	if start < 0 {
		start = 0
	}
	count := end - start
	if count <= 0 {
		return nil
	}
	out := make([]string, 0, count)
	capN := cap(r.lines)
	for i := start; i < end; i++ {
		idx := (r.head + i) % capN
		out = append(out, r.lines[idx])
	}
	return out
}

// ---- 日志写入 ----

// logBufAppender 只把日志存入环形缓冲（供网页「查看日志」页轮询读取），不向任何流输出。
// 程序无终端 UI（纯托盘 + 网页），日志统一进缓冲供网页查看；是否落盘由 log_file 决定。
type logBufAppender struct{}

func (logBufAppender) Write(p []byte) (int, error) {
	logBuf.append(string(p))
	return len(p), nil
}

// parseSSEStats 解析一行 SSE（data: {...}），把 usage 里的 token 数累加到全局 stats。
// output_tokens 是单流累积值，用增量（new - lastOutput）累加，避免多流互相覆盖。
func parseSSEStats(line []byte, lastOutput *int64) {
	s := bytes.TrimRight(line, "\r\n")
	if !bytes.HasPrefix(s, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(s, []byte("data:")))
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	var ev map[string]interface{}
	if json.Unmarshal(payload, &ev) != nil {
		return
	}
	var usage map[string]interface{}
	switch ev["type"] {
	case "message_start":
		if msg, ok := ev["message"].(map[string]interface{}); ok {
			usage, _ = msg["usage"].(map[string]interface{})
		}
	case "message_delta":
		usage, _ = ev["usage"].(map[string]interface{})
	}
	if usage == nil {
		return
	}
	stats.mu.Lock()
	defer stats.mu.Unlock()
	// cache_read / input_tokens 在 message_start 里一次性给出，
	// 直接累加（message_delta 一般不带这些字段，不会重复加）。
	// 注：ARK 的 usage 不含 cache_creation_input_tokens，故无"缓存写入"统计。
	stats.cacheRead += toInt64(usage["cache_read_input_tokens"])
	stats.inputTokens += toInt64(usage["input_tokens"])
	// output_tokens 是本流累积值：只把增量加到全局。
	if out := toInt64(usage["output_tokens"]); out > *lastOutput {
		stats.outputTokens += out - *lastOutput
		*lastOutput = out
	}
}

// shouldRetry 判断给定状态码是否在重试名单里。
func shouldRetry(code int) bool {
	c := cfg.Load()
	for _, rc := range c.RetryStatusCodes {
		if rc == code {
			return true
		}
	}
	return false
}

// computeBackoff 计算本次重试前的等待时间：
// 优先听上游 Retry-After 头（秒），否则用指数退避 + 抖动。
func computeBackoff(resp *http.Response, attempt int) time.Duration {
	c := cfg.Load()
	if c.RespectRetryAfter && resp != nil {
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil {
				return time.Duration(secs) * time.Second
			}
		}
	}
	backoffSec := c.BaseDelaySec * math.Pow(2, float64(attempt))
	if backoffSec > c.MaxDelaySec {
		backoffSec = c.MaxDelaySec
	}
	// 加抖动，避免同时重试造成惊群。
	jitter := backoffSec * 0.3 * rand.Float64()
	return time.Duration((backoffSec + jitter) * float64(time.Second))
}

// copyHeaders 把 src 的头复制到 dst，跳过逐跳头和 Content-Length。
// Content-Length 必须跳过：改写请求体后长度会变，让 Go 按 body 重新计算。
func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		if k == "Connection" || k == "Keep-Alive" ||
			k == "Transfer-Encoding" || k == "Upgrade" ||
			k == "Content-Length" {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// peekHead 读取响应体开头：读到第一个 SSE 事件结束（空行）、或读满 maxBytes、
// 或遇到 EOF 为止。返回已读到的字节（这些字节已从 br 消费，转发时要补回去）。
func peekHead(br *bufio.Reader, maxBytes int) []byte {
	var buf bytes.Buffer
	for buf.Len() < maxBytes {
		line, err := br.ReadBytes('\n')
		buf.Write(line)
		if err != nil {
			break // EOF 或出错，返回已读部分
		}
		// SSE 事件之间用空行分隔：已经有内容且遇到空行，说明第一个事件读完了。
		if len(bytes.TrimRight(line, "\r\n")) == 0 && buf.Len() > 2 {
			break
		}
	}
	return buf.Bytes()
}

// headHasError 判断响应开头是不是一个错误事件（限流等）。
func headHasError(head []byte) bool {
	s := strings.ToLower(string(head))
	return strings.Contains(s, "event: error") ||
		strings.Contains(s, "event:error") ||
		strings.Contains(s, `"type":"error"`) ||
		strings.Contains(s, `"type": "error"`) ||
		strings.Contains(s, "rate_limit") ||
		strings.Contains(s, "overloaded") ||
		strings.Contains(s, "too many requests")
}

// firstLine 取字节流第一行（截断到 200 字符），用于日志。
func firstLine(b []byte) string {
	s := string(b)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return strings.TrimSpace(s)
}

// truncate 把字符串压成一行并截断到 n 字符，用于日志。
func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// fieldSpan 记录顶层 JSON object 中某字段的字节位置（基于原始 body，含引号/冒号定位）。
// keyStart/keyEnd 是 key（含双引号）的范围；valStart/valEnd 是 value 的字节范围。
type fieldSpan struct {
	name     string
	keyStart int
	keyEnd   int
	valStart int
	valEnd   int
}

// locateTopFields 流式解析顶层 JSON object，返回各字段在原 body 的字节位置（保留出现顺序）。
// 用 json.Decoder 的 Token 读 key、Decode 读 value 到 RawMessage，再结合 InputOffset +
// RawMessage 内容校准出 value 字节范围。只匹配顶层 key，不误伤嵌套对象中的同名字段。
// 定位失败时返回 ok=false，调用方据此放行原 body（转发不受影响）。
func locateTopFields(body []byte) ([]fieldSpan, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, false
	}
	var spans []fieldSpan
	for dec.More() {
		tk, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, ok := tk.(string)
		if !ok {
			return nil, false
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, false
		}
		valStart, valEnd, ok := locateValueRange(body, v, dec.InputOffset())
		if !ok {
			return nil, false
		}
		keyStart, keyEnd, ok := findKeySpan(body, valStart, key)
		if !ok {
			return nil, false
		}
		spans = append(spans, fieldSpan{name: key, keyStart: keyStart, keyEnd: keyEnd, valStart: valStart, valEnd: valEnd})
	}
	return spans, true
}

// locateValueRange 用 Decoder 的 InputOffset 推算 value 字节范围，并用 RawMessage 内容校准。
// InputOffset 在 Decode 后通常指向 value 结束位置，但可能因 scanp/尾部空白差几字节；
// 故在附近 ±8 字节内做精确匹配（body 切片 == RawMessage），匹配不到则判定定位失败。
func locateValueRange(body, v []byte, off int64) (valStart, valEnd int, ok bool) {
	approx := int(off)
	for d := 0; d <= 8; d++ {
		for _, end := range []int{approx + d, approx - d} {
			start := end - len(v)
			if start >= 0 && end >= 0 && end <= len(body) && bytes.Equal(body[start:end], v) {
				return start, end, true
			}
		}
	}
	return 0, 0, false
}

// findKeySpan 从 value 起始位置向前找 "key": 模式，定位 key（含双引号）的字节范围。
// 目标字段名均为无转义的简单标识符，故直接字面量匹配；LastIndex 取最近一次出现。
func findKeySpan(body []byte, valStart int, key string) (keyStart, keyEnd int, ok bool) {
	needle := []byte(`"` + key + `":`)
	end := valStart
	if end > len(body) {
		end = len(body)
	}
	idx := bytes.LastIndex(body[:end], needle)
	if idx < 0 {
		return 0, 0, false
	}
	return idx, idx + len(key) + 2, true // +2 = 起始" + key + 闭合"
}

// classifierSystemMatches 判断 system 字段（原始字节）是否以指定前缀开头。
// system 可能是字符串，也可能是 Anthropic 风格的 [{type,text}] 数组。
func classifierSystemMatches(body []byte, spans []fieldSpan, prefix string) bool {
	for _, s := range spans {
		if s.name != "system" {
			continue
		}
		sysRaw := body[s.valStart:s.valEnd]
		var str string
		if err := json.Unmarshal(sysRaw, &str); err == nil {
			return strings.HasPrefix(str, prefix)
		}
		var arr []map[string]interface{}
		if err := json.Unmarshal(sysRaw, &arr); err == nil {
			for _, m := range arr {
				if t, _ := m["text"].(string); strings.HasPrefix(t, prefix) {
					return true
				}
			}
		}
		return false
	}
	return false
}

// fieldDeleteRange 计算删除字段（含 key、:、value）的字节范围，并正确处理前后逗号：
// 字段后是 , 则连同尾逗号一起删；否则若字段前是 ,（跳过空白后）则从前逗号删起。
// 避免删除后留下 ,, / {, / ,} 等非法结构。
func fieldDeleteRange(body []byte, f *fieldSpan) (int, int) {
	start := f.keyStart
	end := f.valEnd
	if end < len(body) && body[end] == ',' {
		return start, end + 1 // 删到尾逗号后
	}
	// 字段后是 }（或空白+}）：向前找前一字段的尾逗号
	i := start - 1
	for i >= 0 && (body[i] == ' ' || body[i] == '\t' || body[i] == '\n' || body[i] == '\r') {
		i--
	}
	if i >= 0 && body[i] == ',' {
		return i, end
	}
	return start, end
}

// lastCloseBrace 返回 body 中最后一个 } 的位置（跳过 trailing 空白），找不到返回 -1。
func lastCloseBrace(body []byte) int {
	for i := len(body) - 1; i >= 0; i-- {
		if body[i] == '}' {
			return i
		}
		if body[i] != ' ' && body[i] != '\t' && body[i] != '\n' && body[i] != '\r' {
			return -1
		}
	}
	return -1
}

// applyClassifierEdits 在原 body 上做文本替换/删除/追加，只动目标字段，其余字节原样保留。
// thinking->{"type":"disabled"}、reasoning_effort->"none"、reasoning->删除、max_tokens->数字；
// 原不存在的字段追加到顶层闭合 } 之前。编辑按 start 升序基于原始 body 分段拼接，互不干扰。
func applyClassifierEdits(body []byte, spans []fieldSpan, maxTokens int) []byte {
	have := map[string]*fieldSpan{}
	for i := range spans {
		s := &spans[i]
		switch s.name {
		case "thinking", "reasoning_effort", "reasoning", "max_tokens":
			have[s.name] = s
		}
	}

	type edit struct {
		start, end int
		repl       []byte
	}
	var edits []edit

	// 替换/删除已存在的目标字段
	if r, ok := have["reasoning"]; ok {
		ds, de := fieldDeleteRange(body, r)
		edits = append(edits, edit{ds, de, nil})
	}
	if t, ok := have["thinking"]; ok {
		edits = append(edits, edit{t.valStart, t.valEnd, []byte(`{"type":"disabled"}`)})
	}
	if re, ok := have["reasoning_effort"]; ok {
		edits = append(edits, edit{re.valStart, re.valEnd, []byte(`"none"`)})
	}
	if maxTokens > 0 {
		if m, ok := have["max_tokens"]; ok {
			edits = append(edits, edit{m.valStart, m.valEnd, []byte(strconv.Itoa(maxTokens))})
		}
	}

	// 追加原 body 不存在的字段：插到顶层闭合 } 之前（保留原字段顺序，新字段在末尾）
	var appendFields []byte
	if _, ok := have["thinking"]; !ok {
		appendFields = append(appendFields, []byte(`"thinking":{"type":"disabled"}`)...)
	}
	if _, ok := have["reasoning_effort"]; !ok {
		if len(appendFields) > 0 {
			appendFields = append(appendFields, ',')
		}
		appendFields = append(appendFields, []byte(`"reasoning_effort":"none"`)...)
	}
	if maxTokens > 0 {
		if _, ok := have["max_tokens"]; !ok {
			if len(appendFields) > 0 {
				appendFields = append(appendFields, ',')
			}
			appendFields = append(appendFields, []byte(`"max_tokens":`+strconv.Itoa(maxTokens))...)
		}
	}
	if len(appendFields) > 0 {
		braceOff := bytes.IndexByte(body, '{')
		closeOff := lastCloseBrace(body)
		if braceOff < 0 || closeOff <= braceOff {
			return body
		}
		// object 为空（{ } 间只有空白）则插到 { 后；否则在 } 前加前导逗号插入
		empty := true
		for i := braceOff + 1; i < closeOff; i++ {
			if c := body[i]; c != ' ' && c != '\t' && c != '\n' && c != '\r' {
				empty = false
				break
			}
		}
		if empty {
			edits = append(edits, edit{braceOff + 1, braceOff + 1, appendFields})
		} else {
			edits = append(edits, edit{closeOff, closeOff, append([]byte{','}, appendFields...)})
		}
	}

	if len(edits) == 0 {
		return body
	}

	// 按 start 升序分段拼接（基于原始 body，编辑互不干扰）
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	var buf bytes.Buffer
	prev := 0
	for _, e := range edits {
		if e.start < prev {
			continue // 防御：编辑重叠则跳过
		}
		buf.Write(body[prev:e.start])
		buf.Write(e.repl)
		prev = e.end
	}
	buf.Write(body[prev:])
	return buf.Bytes()
}

// isFastRequest 判断请求是否来自 Claude Code /fast 模式。
// 以请求体里显式包含的 "speed":"fast" 顶层字段为准；请求头里的 Anthropic-Beta 只做预检参考，
// 不触发路由，避免 /fast off 后仅因 header 残留就继续走 fast 路由。
func isFastRequest(body []byte, r *http.Request) bool {
	return getStringField(body, "speed") == "fast"
}

// removeSpeedField 从 JSON body 中移除顶层 "speed" 字段，保留合法 JSON 结构。
// 上游不支持 speed 字段，传过去可能导致拒绝或未定义行为。采用流式字段定位，
// 不受字段顺序、空白字符影响。
func removeSpeedField(body []byte) []byte {
	spans, ok := locateTopFields(body)
	if !ok {
		return body
	}
	for i, s := range spans {
		if s.name != "speed" {
			continue
		}
		return deleteFieldSpan(body, spans, i)
	}
	return body
}

// getStringField 从 body 的顶层 JSON object 中读取指定字段的字符串值；未找到或非字符串返回空。
func getStringField(body []byte, key string) string {
	spans, ok := locateTopFields(body)
	if !ok {
		return ""
	}
	for _, s := range spans {
		if s.name != key {
			continue
		}
		var v string
		if err := json.Unmarshal(body[s.valStart:s.valEnd], &v); err != nil {
			return ""
		}
		return v
	}
	return ""
}

// deleteFieldSpan 从顶层 JSON object 中删除第 idx 个字段，保持其余字段顺序与格式。
func deleteFieldSpan(body []byte, spans []fieldSpan, idx int) []byte {
	if len(spans) == 0 || idx < 0 || idx >= len(spans) {
		return body
	}
	s := spans[idx]
	// 待删除区间：从本字段 keyStart 开始，到本字段 valEnd 结束。
	// 之后要处理前后的逗号，使 JSON 仍合法。
	start := s.keyStart
	end := s.valEnd

	if idx == 0 {
		// 第一个字段：删除后应去掉后面紧跟的逗号（如果有）。
		if end < len(body) && body[end] == ',' {
			end++
		}
	} else {
		// 非首字段：前面有逗号，把逗号一起删掉（start 前移一位）。
		start--
	}

	out := make([]byte, 0, len(body)-(end-start))
	out = append(out, body[:start]...)
	out = append(out, body[end:]...)
	return out
}

// isClassifierRequest 判断 body 是否为分类器请求（system 字段前缀匹配）。
// 与 maybeRewriteClassifier 共用同一判定，但只判定不改写；供路由决策使用。
// 独立于 ClassifierThinkingDisabled：即使未开 thinking 改写，分类器路由仍可生效。
func isClassifierRequest(c *Config, body []byte) bool {
	if c.ClassifierSystemPrefix == "" {
		return false
	}
	if !bytes.Contains(body, []byte(c.ClassifierSystemPrefix)) {
		return false
	}
	spans, ok := locateTopFields(body)
	if !ok {
		return false
	}
	return classifierSystemMatches(body, spans, c.ClassifierSystemPrefix)
}

// maybeRewriteClassifier 命中分类器请求时关掉 thinking，让分类快速返回。
// 用 bytes.Contains 预筛，正常请求（不含前缀子串）不做 JSON 解析，开销极低。
// 命中后用 json.Decoder 流式定位目标字段的字节位置，再做文本替换--不整体重序列化，
// 未改字段原样保留字节（含 key 顺序与格式），避免影响上游缓存命中。
func maybeRewriteClassifier(body []byte) []byte {
	c := cfg.Load()
	if !c.ClassifierThinkingDisabled || c.ClassifierSystemPrefix == "" {
		return body
	}
	if !bytes.Contains(body, []byte(c.ClassifierSystemPrefix)) {
		return body
	}
	spans, ok := locateTopFields(body)
	if !ok {
		return body
	}
	if !classifierSystemMatches(body, spans, c.ClassifierSystemPrefix) {
		return body
	}
	newBody := applyClassifierEdits(body, spans, c.ClassifierMaxTokens)
	if len(newBody) == len(body) && bytes.Equal(newBody, body) {
		return body
	}
	log.Printf("[改写] 命中分类器请求，关闭 thinking (body %d->%d字节)", len(body), len(newBody))
	stats.classifierRewrites.Add(1) // 实时状态行计数：分类器关 thinking 次数
	return newBody
}

// logRequestDetail 解析请求体打印 stream/tools/system 前缀，用于诊断分类器指纹。
// 仅在 log_request_detail=true 时调用，默认关闭。
func logRequestDetail(r *http.Request, body []byte) {
	var p map[string]interface{}
	if err := json.Unmarshal(body, &p); err != nil {
		log.Printf("[详情] %s %s (非JSON)", r.Method, r.URL.Path)
		return
	}
	stream, _ := p["stream"].(bool)
	tools := 0
	toolNames := []string{}
	if arr, ok := p["tools"].([]interface{}); ok {
		tools = len(arr)
		for _, t := range arr {
			if m, ok := t.(map[string]interface{}); ok {
				name, _ := m["name"].(string)
				typ, _ := m["type"].(string)
				if name != "" {
					toolNames = append(toolNames, name)
				} else if typ != "" {
					toolNames = append(toolNames, typ)
				}
			}
		}
	}
	sysPrefix := ""
	switch s := p["system"].(type) {
	case string:
		sysPrefix = truncate(s, 60)
	case []interface{}:
		for _, blk := range s {
			if m, ok := blk.(map[string]interface{}); ok {
				if t, _ := m["text"].(string); t != "" {
					sysPrefix = truncate(t, 60)
					break
				}
			}
		}
	}
	log.Printf("[详情] %s %s stream=%v tools=%d %v sys=%q 图片=%v 搜索=%v", r.Method, r.URL.Path, stream, tools, toolNames, sysPrefix, hasImage(body), hasWebSearch(body))
}

// locateModel 定位请求体顶层 "model" 字段的字符串值区间。
// 轻量实现：定位 "model" 键后读取紧随的字符串字面量，不整体解析 JSON，避免每个请求都 Unmarshal。
// 参数 body：请求体字节；返回 value=模型名，start/end=值在 body 中的字节区间（不含引号），ok=是否成功定位。
func locateModel(body []byte) (value string, start, end int, ok bool) {
	key := []byte(`"model"`)
	i := bytes.Index(body, key)
	if i < 0 {
		return "", 0, 0, false
	}
	i += len(key)
	// 跳过 : 与空白，定位到值。
	for i < len(body) {
		c := body[i]
		if c == ':' || c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			i++
			continue
		}
		break
	}
	if i >= len(body) || body[i] != '"' {
		return "", 0, 0, false
	}
	i++ // 跳过开头引号
	start = i
	for i < len(body) && body[i] != '"' {
		if body[i] == '\\' && i+1 < len(body) { // 跳过转义字符
			i += 2
			continue
		}
		i++
	}
	return string(body[start:i]), start, i, true
}

// extractModel 提取请求体 "model" 字段值，仅供 [请求] 日志展示用。提取失败返回空串。
func extractModel(body []byte) string {
	v, _, _, ok := locateModel(body)
	if !ok {
		return ""
	}
	return v
}

// replaceModelValue 把请求体 "model" 字段的值替换为 newModel，供路由命中时改写请求体。
// 复用 locateModel 定位值区间后做字节拼接；长度变化由后续 bytes.NewReader 重算 Content-Length。
// 参数 body：原请求体；newModel：目标模型名；返回改写后的新 body。未定位到 model 字段时原样返回。
func replaceModelValue(body []byte, newModel string) []byte {
	_, start, end, ok := locateModel(body)
	if !ok {
		return body
	}
	out := make([]byte, 0, len(body)-end+start+len(newModel))
	out = append(out, body[:start]...)
	out = append(out, []byte(newModel)...)
	out = append(out, body[end:]...)
	return out
}

// hasImage 判断请求体是否含 Anthropic 图片内容块。
// Anthropic 图片块形如 {"type":"image","source":{"type":"base64",...}}，"type":"image" 在请求体里只出现在图片块。
// 用 bytes.Contains 做零分配子串检测，不整体反序列化，遵循流式定位/文本替换的转发安全原则。
// 参数 body：请求体；返回是否含图片块。
func hasImage(body []byte) bool {
	return bytes.Contains(body, []byte(`"type":"image"`))
}

// hasWebSearch 判断请求体是否带 Anthropic server-side web_search 工具（由上游执行搜索）。
// 只识别 "type":"web_search_YYYYMMDD" 这类 server-side 工具；刻意不识别 client-side WebSearch 工具
// （Claude Code 每个请求都带它的定义，无法据此区分是否真要搜索，会误判所有请求为搜索请求）。
// 参数 body：请求体；返回是否含 server-side web_search 工具。
func hasWebSearch(body []byte) bool {
	return bytes.Contains(body, []byte(`"type":"web_search`))
}

// rewriteResponseModel 把 SSE data: 行里的 "model" 字段值改回 origModel。
// 不依赖上游实际返回的 model 名（可能与请求里写的 targetModel 不同），只要 data: 行含 model 字段就改。
// 对非 data: 行原样返回；未定位到 model 字段或值已等于 origModel 也原样返回。
// 返回：(改写后的行, 上游实际返回的 model 值, 是否发生了改写)
func rewriteResponseModel(line []byte, origModel string) ([]byte, string, bool) {
	prefix := []byte("data:")
	if !bytes.HasPrefix(line, prefix) {
		return line, "", false
	}
	jsonStart := len(prefix)
	for jsonStart < len(line) && (line[jsonStart] == ' ' || line[jsonStart] == '\t') {
		jsonStart++
	}
	curModel, _, _, ok := locateModel(line[jsonStart:])
	if !ok || curModel == origModel {
		return line, "", false
	}
	newJSON := replaceModelValue(line[jsonStart:], origModel)
	out := make([]byte, 0, jsonStart+len(newJSON))
	out = append(out, line[:jsonStart]...)
	out = append(out, newJSON...)
	return out, curModel, true
}

// 按 * 分割 pattern：首段必须是 name 前缀、末段必须是后缀、中间段按序在剩余部分中出现。
// 参数 pattern：含 * 的模式；name：实际模型名；返回是否匹配。
func matchModel(pattern, name string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == name // 无 *，精确匹配
	}
	// 首段前缀。
	if !strings.HasPrefix(name, parts[0]) {
		return false
	}
	name = name[len(parts[0]):]
	// 中间段按序出现。
	for i := 1; i < len(parts)-1; i++ {
		idx := strings.Index(name, parts[i])
		if idx < 0 {
			return false
		}
		name = name[idx+len(parts[i]):]
	}
	// 末段后缀。
	return strings.HasSuffix(name, parts[len(parts)-1])
}

// writeSSEPing 向客户端写一个 Anthropic 标准 SSE ping 事件并 flush，用于 429 重试期间保活。
// ping 事件被 Claude Code 忽略（Anthropic 官方流本身也穿插 ping），不产生消息内容。
// 参数 w：响应写入器；flusher：若非 nil 则写后 flush，保证客户端立即收到。
func writeSSEPing(w http.ResponseWriter, flusher http.Flusher) {
	w.Write([]byte("event: ping\ndata: {\"type\":\"ping\"}\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
}

// writeSSEError 在已发 200 头后向上游失败兜底：写一个 Anthropic SSE error 事件并 flush。
// 已发 200 头后无法再改状态码透传 429，只能用 SSE error 让客户端识别错误（重试用尽时走这里）。
// 参数 w：响应写入器；flusher：若非 nil 则写后 flush；msg：错误描述。
func writeSSEError(w http.ResponseWriter, flusher http.Flusher, msg string) {
	w.Write([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":"))
	b, _ := json.Marshal(msg) // json 编码 message，避免特殊字符破坏 SSE
	w.Write(b)
	w.Write([]byte("}}\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
}

// startSSEKeepalive 在首次重试时向客户端发 200 + SSE 头 + 首个 ping，开启保活流。
// 之后重试期间持续发 ping，避免 Claude Code 因长时间无数据超时报 API error。
// 返回 flusher 供后续 ping/转发使用。参数 f：本次 flight（日志用 id）；reason：重试原因（日志）。
func startSSEKeepalive(w http.ResponseWriter, f *flight, reason string) http.Flusher {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	writeSSEPing(w, flusher)
	log.Printf("[保活] #%d 重试中(%s)，开启 SSE ping 保活", f.id, reason)
	return flusher
}

// sleepWithPing 在 backoff 等待期间定期发 ping 保活，且可被客户端断开（ctx 取消）中断。
// 替换原 time.Sleep(wait)：后者不可中断，客户端超时断开后代理仍傻睡+重试、白烧上游配额。
// 参数 ctx：客户端连接 context；w/flusher：发 ping 用；wait：backoff 时长；interval：ping 间隔；f：flight。
// 返回 false 表示 ctx 已取消（客户端断开），应停止重试直接结束。
func sleepWithPing(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, wait, interval time.Duration, f *flight) bool {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("[保活] #%d 客户端已断开，停止重试", f.id)
			return false
		case <-ticker.C:
			writeSSEPing(w, flusher)
		case <-timer.C:
			return true
		}
	}
}

// forward 把响应透传给客户端：先补回已偷看的 head，再流式转发剩余内容。
// 转发同时按行解析 SSE 的 usage 字段，累加到全局 stats 供状态行显示；
// 同步把已转发字节累加到 flight.bytes 供上半屏图标显示，并标记 phase=1。
// 返回本流最终 output_tokens，供 handler 计算 token/s 样本。
func forward(w http.ResponseWriter, resp *http.Response, head []byte, br *bufio.Reader, f *flight, headersSent bool) int64 {
	defer resp.Body.Close()
	if !headersSent {
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
	}

	flusher, canFlush := w.(http.Flusher)

	// 收到响应开始转发：图标从"等首字(耗时)"切到"转发中(字节)"。
	f.phase.Store(1)
	f.status = resp.StatusCode

	// 计入活跃流，defer 退出时减一。
	stats.mu.Lock()
	stats.active++
	stats.mu.Unlock()
	defer func() {
		log.Printf("[完成] #%d response状态 %d 大小 %s", f.id, f.status, humanBytes(f.bytes.Load()))
		stats.mu.Lock()
		stats.active--
		stats.mu.Unlock()
	}()

	var lastOutput int64 // 本流 output_tokens 累积值，用于算增量
	// writeAndCount 转发一段字节并解析其中的 data: 行更新 stats。
	writeAndCount := func(data []byte) {
		if len(data) == 0 {
			return
		}
		if f.targetModel != "" && f.targetModel != f.origModel {
			lines := bytes.Split(data, []byte("\n"))
			modified := false
			var actualModel string
			for i, line := range lines {
				newLine, am, mod := rewriteResponseModel(line, f.origModel)
				if mod {
					modified = true
					if actualModel == "" {
						actualModel = am
					}
				}
				lines[i] = newLine
			}
			if modified {
				data = bytes.Join(lines, []byte("\n"))
				if !f.modelLogged.Swap(true) {
					log.Printf("[改写] #%d 响应流 model 回改 %s → %s", f.id, actualModel, f.origModel)
				}
			}
		}
		w.Write(data)
		if canFlush {
			flusher.Flush()
		}
		n := int64(len(data))
		stats.bytesForward.Add(n) // 实时流量（字节），每转发一段就涨
		f.bytes.Add(n)            // 单流字节，供图标显示
		for _, line := range bytes.Split(data, []byte("\n")) {
			parseSSEStats(line, &lastOutput)
		}
	}

	writeAndCount(head)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			writeAndCount(line)
		}
		if err != nil {
			break
		}
	}
	return lastOutput
}

// handler 是所有请求的入口：缓冲请求体 → 命中分类器则关 thinking →
// 带重试地转发 → 流式透传响应。
// 重试三种情况：
//
//	A. 上游 HTTP 状态码本身就是 429/5xx；
//	B. 状态码 200，但错误藏在 SSE 响应体里（event:error / rate_limit 等）；
//	C. 正常响应，直接透传。
func handler(w http.ResponseWriter, r *http.Request) {
	c := cfg.Load()

	// Claude Code 在 /fast 预检或启动时可能发 HEAD / 探测连通性。
	// 上游（如 Kimi）对 / 返回 404，会被误判为网络不可达。
	// 这里直接返回 200，避免预检失败。
	if r.Method == http.MethodHead && r.URL.Path == "/" {
		w.WriteHeader(http.StatusOK)
		return
	}

	// 为本次请求建一个 flight：网页控制台展示在途流（#id/耗时/字节）。
	// ctx 在客户端断开时自动取消（r.Context()），上游请求与重试等待均基于它，断连即中止重试。
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	f := &flight{
		id:    flights.nextID.Add(1),
		start: time.Now(),
	}
	flights.register(f)
	// 兜底清理：所有 return 路径统一由 defer 注销，避免遗漏导致在途流残留。
	defer flights.unregister(f.id)

	// 1. 缓冲请求体，以便重试时重放。
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("[请求] #%d %s %s 读取body失败: %v", f.id, r.Method, r.URL.Path, err)
		http.Error(w, "read body failed", http.StatusBadGateway)
		return
	}
	r.Body.Close()

	if c.LogRequestDetail {
		logRequestDetail(r, body)
	}
	// 提取 model：[请求] 日志展示 + 路由匹配共用。非 JSON/无 model 时为空。
	origModel := extractModel(body)
	modelPart := ""
	if origModel != "" {
		modelPart = " model=" + origModel
	}
	log.Printf("[请求] #%d %s %s%s (body=%d字节) 来自 %s", f.id, r.Method, r.URL.Path, modelPart, len(body), r.RemoteAddr)

	// 1.5 命中分类器请求就关掉 thinking，让分类快速返回。
	// isClassifier 同时供路由决策：分类器路由优先于 model 路由。
	isClassifier := isClassifierRequest(c, body)
	body = maybeRewriteClassifier(body)

	// 1.6 路由匹配。
	// 优先级：分类器路由 > fast 路由 > model 路由（按 routes 通配）。
	// 未命中任何路由时 upstream=c.Upstream、authToken=""（透传客户端 token，原逻辑）。
	upstream := c.Upstream
	authToken := ""
	var targetModel string
	fastRouteHit := false
	if isClassifier && c.ClassifierRoute != nil {
		// 分类器路由：把安全判断请求统一甩到指定上游，省主模型额度。
		cr := c.ClassifierRoute
		upstream = cr.URL
		authToken = cr.API
		if cr.Model != "" && cr.Model != origModel {
			body = replaceModelValue(body, cr.Model)
			targetModel = cr.Model
		}
		log.Printf("[路由] #%d 分类器 %s → %s (model %s → %s)", f.id, origModel, cr.URL, origModel, cr.Model)
	} else if c.FastRoute != nil && isFastRequest(body, r) {
		// fast 路由：检测到 "speed":"fast" 的非分类器请求，统一甩到指定上游。
		fr := c.FastRoute
		upstream = fr.URL
		authToken = fr.API
		fastRouteHit = true
		if fr.Model != "" && fr.Model != origModel {
			body = replaceModelValue(body, fr.Model)
			targetModel = fr.Model
		}
		body = removeSpeedField(body)
		log.Printf("[路由] #%d fast %s → %s (model %s → %s)", f.id, origModel, fr.URL, origModel, fr.Model)
	} else if len(c.Routes) > 0 && origModel != "" {
		for i := range c.Routes {
			if matchModel(c.Routes[i].Pattern, origModel) {
				rr := &c.Routes[i]
				// 能力兜底：目标上游缺图片/搜索能力但请求需要时，改走对应兜底上游。
				needImage := rr.TextOnly && hasImage(body)
				needSearch := rr.NoSearch && hasWebSearch(body)
				if needImage || needSearch {
					// applyFB 把选中的兜底上游赋到当前请求（改 upstream/API/model）。
					applyFB := func(url, api, model, label string) {
						upstream = url
						authToken = api
						if model != "" && model != origModel {
							body = replaceModelValue(body, model)
							targetModel = model
						}
						log.Printf("[路由] #%d %s %s -> %s (model %s -> %s)", f.id, label, origModel, url, origModel, model)
					}
					sf := c.SearchFallback
					mf := c.MultimodalFallback
					// 1) 需要搜索且 search_fallback 可走（不带图，或带图但兜底支持图）。
					if needSearch && sf != nil && (!needImage || !sf.TextOnly) {
						applyFB(sf.URL, sf.API, sf.Model, "搜索兜底")
						break
					}
					// 2) 需要图片（或上面搜索没走成）且 multimodal_fallback 可走（不带搜索，或带搜索但兜底支持搜索）。
					if (needImage || needSearch) && mf != nil && (!needSearch || !mf.NoSearch) {
						applyFB(mf.URL, mf.API, mf.Model, "图片兜底")
						break
					}
					// 3) 纯图片但 multimodal_fallback 没配，退而走支持图片的 search_fallback。
					if needImage && !needSearch && sf != nil && !sf.TextOnly {
						applyFB(sf.URL, sf.API, sf.Model, "搜索兜底")
						break
					}
					// 4) 都不满足：降级走原 route（由上游处理，可能报错）。
				}
				upstream = rr.URL
				authToken = rr.API
				if rr.Model != "" && rr.Model != origModel {
					body = replaceModelValue(body, rr.Model)
					targetModel = rr.Model
				}
				log.Printf("[路由] #%d %s -> %s (model %s -> %s)", f.id, origModel, rr.URL, origModel, rr.Model)
				break // 有序：第一个命中即止
			}
		}
	}

	f.origModel = origModel
	f.targetModel = targetModel
	deadline := time.Now().Add(time.Duration(c.TotalBudgetSec * float64(time.Second)))
	// headersSent：是否已向客户端发过 200 + SSE 头（首次重试保活时置 true）。
	// 一旦 true，后续成功转发跳过 WriteHeader/copyHeaders（头已发），重试用尽改发 SSE error。
	headersSent := false
	var flusher http.Flusher // 保活开启后由 startSSEKeepalive 赋值
	pingInterval := time.Duration(c.PingIntervalSec * float64(time.Second))

	for attempt := 0; attempt <= c.MaxRetries; attempt++ {
		// 2. 构造发往上游的请求。
		upReq, err := http.NewRequestWithContext(ctx, r.Method, upstream+r.URL.Path, bytes.NewReader(body))
		if err != nil {
			log.Printf("[错误] 构造上游请求失败: %v", err)
			http.Error(w, "build request failed", http.StatusBadGateway)
			return
		}
		upReq.URL.RawQuery = r.URL.RawQuery
		copyHeaders(upReq.Header, r.Header)
		upReq.Header.Set("Accept-Encoding", "identity") // 关闭压缩，方便检查响应体
		upReq.Host = ""                                 // 让 Go 根据 Upstream 自动设置 Host
		// 路由命中时用目标 API key 覆盖鉴权头（删原 Authorization/x-api-key，避免客户端 token 透传到目标上游）。
		if authToken != "" {
			upReq.Header.Del("Authorization")
			upReq.Header.Del("x-api-key")
			upReq.Header.Set("Authorization", "Bearer "+authToken)
		}
		// fast 路由命中时删除 Anthropic-Beta 头（含 fast-mode-2026-02-01），上游不支持。
		if fastRouteHit {
			upReq.Header.Del("Anthropic-Beta")
		}

		// 已发上游、进入"等首字节"阶段：状态灯转黄（waiting++）。
		stats.mu.Lock()
		stats.waiting++
		stats.mu.Unlock()
		tSend := time.Now() // 请求发出时刻，用于算首字延迟
		resp, err := client.Do(upReq)
		// 拿到响应（或超时/出错）即离开等待阶段。
		stats.mu.Lock()
		stats.waiting--
		stats.mu.Unlock()

		// 情况 0：网络层错误。
		if err != nil {
			log.Printf("[尝试 %d] #%d 请求错误: %v", attempt+1, f.id, err)
			// 双击图标中断（ctx 取消）不重试，直接结束。
			if ctx.Err() != nil {
				if headersSent {
					return // 已发保活头，客户端已断开，直接结束
				}
				http.Error(w, "client canceled", http.StatusServiceUnavailable)
				return
			}
			if attempt < c.MaxRetries && time.Now().Before(deadline) {
				wait := computeBackoff(nil, attempt)
				if !time.Now().Add(wait).After(deadline) {
					log.Printf("[重试] 等待 %v 后重试 (剩余预算 %v)", wait, time.Until(deadline).Round(time.Millisecond))
					stats.statusRetries.Add(1) // 实时状态行计数：重试次数（网络错误/超时）
					if !headersSent {
						flusher = startSSEKeepalive(w, f, "网络错误")
						headersSent = true
					}
					if !sleepWithPing(ctx, w, flusher, wait, pingInterval, f) {
						return // 客户端已断开，停止重试
					}
					continue
				}
			}
			// 超时/网络错误重试用尽：透传 503 给 Claude Code 自行重试（SDK 内置 5xx 重试）。
			log.Printf("[透传] 超过超时等待时间或重试用尽，透传 503 给客户端自行处理: %v", err)
			if headersSent {
				writeSSEError(w, flusher, "upstream timeout/error: "+err.Error())
				return
			}
			http.Error(w, "upstream timeout/error: "+err.Error(), http.StatusServiceUnavailable)
			return
		}

		// 无条件打印状态码，方便诊断到底收到了什么。
		log.Printf("[尝试 %d] 上游响应状态码: %d", attempt+1, resp.StatusCode)

		// 情况 A：HTTP 状态码本身就要求重试（真正的 429/5xx）。
		if shouldRetry(resp.StatusCode) {
			if attempt < c.MaxRetries && time.Now().Before(deadline) {
				wait := computeBackoff(resp, attempt)
				if !time.Now().Add(wait).After(deadline) {
					log.Printf("[重试] 状态码 %d，等待 %v 后重试 (剩余预算 %v)", resp.StatusCode, wait, time.Until(deadline).Round(time.Millisecond))
					stats.statusRetries.Add(1) // 实时状态行计数：重试次数（状态码）
					resp.Body.Close()
					if !headersSent {
						flusher = startSSEKeepalive(w, f, fmt.Sprintf("状态码 %d", resp.StatusCode))
						headersSent = true
					}
					if !sleepWithPing(ctx, w, flusher, wait, pingInterval, f) {
						return // 客户端已断开，停止重试
					}
					continue
				}
			}
			log.Printf("[透传] 次数或预算用尽，透传状态码 %d", resp.StatusCode)
			if headersSent {
				resp.Body.Close()
				writeSSEError(w, flusher, fmt.Sprintf("upstream status %d", resp.StatusCode))
				return
			}
			forward(w, resp, nil, bufio.NewReader(resp.Body), f, false)
			return
		}

		// 情况 B：状态码 200，但错误可能藏在响应体（SSE 流）里。
		br := bufio.NewReader(resp.Body)
		head := peekHead(br, 8192)
		tFirstByte := time.Now() // 第一个 body 数据字节到达时刻（"吐第一个字"）

		if headHasError(head) {
			log.Printf("[错误] 状态码 200 但响应体含错误: %s", firstLine(head))
			if attempt < c.MaxRetries && time.Now().Before(deadline) {
				wait := computeBackoff(resp, attempt)
				if !time.Now().Add(wait).After(deadline) {
					log.Printf("[重试] 等待 %v 后重试 (剩余预算 %v)", wait, time.Until(deadline).Round(time.Millisecond))
					stats.statusRetries.Add(1) // 实时状态行计数：重试次数（200 体内错误）
					resp.Body.Close()
					if !headersSent {
						flusher = startSSEKeepalive(w, f, "体内错误")
						headersSent = true
					}
					if !sleepWithPing(ctx, w, flusher, wait, pingInterval, f) {
						return // 客户端已断开，停止重试
					}
					continue
				}
			}
			log.Printf("[透传] 次数或预算用尽，透传体内错误")
			if headersSent {
				resp.Body.Close()
				writeSSEError(w, flusher, "upstream stream error after retries")
				return
			}
			forward(w, resp, head, br, f, false)
			return
		}

		// 情况 C：正常响应，透传。记录首字延迟 / 流式时长 / output_tokens 入滑动窗口。
		log.Printf("[响应] 正常透传 (共尝试 %d 次)", attempt+1)
		// 配了 fast_route 就在所有正常响应里注入假的 fast 限流 headers，
		// 让 Claude Code 的 /fast 预检认为 fast 模式可用。
		if c.FastRoute != nil {
			now := time.Now().Format(time.RFC3339)
			resp.Header.Set("anthropic-fast-output-tokens-remaining", "999999")
			resp.Header.Set("anthropic-fast-input-tokens-remaining", "999999")
			resp.Header.Set("anthropic-fast-output-tokens-reset", now)
			resp.Header.Set("anthropic-fast-input-tokens-reset", now)
		}
		stats.pushFirstByte(int64(tFirstByte.Sub(tSend).Milliseconds()))
		out := forward(w, resp, head, br, f, headersSent)
		tEnd := time.Now()
		stats.pushThroughput(int64(tEnd.Sub(tFirstByte).Milliseconds()), out)
		// 单流统计：本次首字延迟 + 流式 tok/s（区别于状态行的滑动窗口均值）。
		firstByteMs := tFirstByte.Sub(tSend).Milliseconds()
		streamMs := tEnd.Sub(tFirstByte).Milliseconds()
		var tps float64
		if streamMs > 0 {
			tps = float64(out) / (float64(streamMs) / 1000.0)
		}
		log.Printf("[单流] #%d 首字 %.2fs 流式 %.2fs %d tok %.1f tok/s",
			f.id, float64(firstByteMs)/1000, float64(streamMs)/1000, out, tps)
		return
	}
}

func main() {
	configPath := flag.String("config", "", "配置文件路径（留空则按 ./config.json -> 用户配置目录 顺序查找）")
	flag.Parse()
	configFilePath = resolveConfigPath(*configPath)

	c, err := loadConfig(configFilePath)
	if err != nil {
		log.Fatalf("读取 %s 失败: %v", configFilePath, err)
	}
	cfg.Store(c)
	stats.resetSampleCap(c.RecentSampleWindow) // 初始化"最近X次"延迟/吞吐滑动窗口容量
	// flight 注册表与日志缓冲区始终初始化（handler 总会 register flight，map 不能为 nil）。
	flights.m = make(map[uint64]*flight)
	logBuf.lines = make([]string, 0, maxLogBuf)

	// 给上游请求设"首字节超时"：超过则认为请求卡住，内部重发（走情况0 重试）。
	// 用 ResponseHeaderTimeout 而非 client.Timeout，确保只限等首字节、不砍流式 Body。
	// 仅启动时设置一次；reload 不重建 Transport（避免与在跑请求并发改字段）。
	if c.UpstreamHeaderTimeoutSec > 0 {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.ResponseHeaderTimeout = time.Duration(c.UpstreamHeaderTimeoutSec * float64(time.Second))
		client.Transport = tr
	}

	// 日志输出：始终进 logRing（供网页「查看日志」页轮询）+ stderr。
	// 程序无终端 UI：Windows 以 GUI 子系统编译无控制台、macOS .app 无终端，stderr 写入无副作用；
	// 显式配 log_file 时再追加落盘，方便留存排查。
	var logOut io.Writer = io.MultiWriter(logBufAppender{}, os.Stderr)
	if c.LogFile != "" {
		if f, err := os.OpenFile(c.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
			logOut = io.MultiWriter(logOut, f)
			log.Printf("[日志] 同时写入文件 %s", c.LogFile)
		} else {
			log.Printf("[日志] 打开日志文件 %s 失败: %v", c.LogFile, err)
		}
	}
	log.SetOutput(logOut)

	log.Printf("代理启动 v%s: 监听 http://%s -> 转发到 %s (最多重试 %d 次, 分类器关thinking=%v)",
		Version, c.Listen, c.Upstream, c.MaxRetries, c.ClassifierThinkingDisabled)

	// HTTP 服务放 goroutine：托盘事件循环（systray.Run）必须占主线程（macOS 要求 UI 在主线程），
	// 故主线程阻塞点留给托盘，HTTP 在后台跑。
	go runServer(c)
	// Ctrl+C -> 优雅退出托盘：systray.Quit 触发 onExit 停状态轮询，Run 返回后 main 随即退出。
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt)
		<-sigCh
		systray.Quit()
	}()
	// 托盘事件循环，阻塞主线程直到 systray.Quit（菜单「退出代理」或 Ctrl+C 触发）。
	setupTray()
}

// runServer 注册 handler 并监听；监听失败（如端口占用）则 log.Fatal 退出整个进程。
func runServer(c *Config) {
	// 网页控制台挂同端口 /__*（仅本机访问），Claude Code 走 /v1/... 不冲突。
	// 必须在 "/" 之前注册：Go DefaultServeMux 精确匹配优先于 / 通配。
	http.HandleFunc(logViewerPath, logViewerHandler)
	http.HandleFunc(logDataPath, logDataHandler)
	http.HandleFunc(configPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			configPostHandler(w, r)
		} else {
			configGetHandler(w, r)
		}
	})
	http.HandleFunc(reloadPath, reloadHandler)
	http.HandleFunc("/", handler)
	err := http.ListenAndServe(c.Listen, nil)
	log.Fatal(err)
}
