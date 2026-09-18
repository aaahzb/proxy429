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
	"regexp"
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
	SearchDebugDir             string           `json:"search_debug_dir,omitempty"`    // 搜索调试目录；非空时把搜索摘要各步请求/响应 raw 写入该目录，便于排查
	ConvertAllToStream         bool             `json:"convertAlltoStream"`            // 全局流式化：开启后所有非流式请求改为流式发上游，收集完整流后重建非流式 JSON 一次性返回（客户端无感知，网页可监控吐字/首字/tok/s）
	ResponsesListen            string           `json:"responses_listen"`              // OpenAI Responses API 监听口（如 127.0.0.1:8081）；空不启用。把 Responses 协议请求翻译成 Anthropic 走主管线，供 Codex CLI 等工具接入。保存/重载即动态启停
}

// RouteRule 定义一条模型路由：命中的请求改走指定上游，并替换 model 名与 API key。
// Pattern 用 * 通配模型名；命中后 URL 覆盖默认 upstream，API 覆盖客户端 token，Model 替换请求体 model 字段。
type RouteRule struct {
	Pattern        string               `json:"pattern"`                    // 模型名通配符，仅支持 *（匹配任意长度任意字符），如 "claude-opus*"
	URL            string               `json:"url"`                        // 目标上游 Base URL，如 https://api.deepseek.com
	API            string               `json:"api"`                        // 目标 API key，设为 Authorization: Bearer；空则透传客户端原 token
	Model          string               `json:"model"`                      // 替换成的目标模型名；空则不改 model 字段
	TextOnly       bool                 `json:"text_only"`                  // 目标模型仅支持纯文本；请求含图片时改走 multimodal_fallback 兜底
	NoSearch       bool                 `json:"no_search"`                  // 目标上游不支持搜索；请求带搜索工具时改走 search_fallback 兜底
	EnhanceSearch  *EnhanceSearchConfig `json:"enhance_search,omitempty"`   // 增强搜索：非 nil 启用。请求带搜索工具时不调主力，改用本 route 上游走 kimi 摘要模式
	URLResponseAPI string               `json:"url_response_api,omitempty"` // 原生 Responses API 上游 Base URL：非空时 Responses 监听口命中本路由的请求不翻译，原样透传到此（仅影响 Responses 口；Anthropic 口流量不受影响仍走 url）
	Thinking       string               `json:"thinking,omitempty"`         // 目标模型的思考形态（仅 Responses 翻译流生效）：""/"auto"=按客户端 model 名查表；"adaptive"=强制 adaptive+effort；"budget"=强制 enabled+budget_tokens
}

// EnhanceSearchConfig 是 routes 条目内可选的增强搜索参数。route 命中且请求带搜索工具时，
// 若 EnhanceSearch 非 nil，则不调主力，改用该 route 自己的 url/api/model 走 kimi 摘要模式
// （step1 搜索 + step2 摘要 + 自构响应）。等价于 no_search 走 search_fallback.summary_mode，
// 只是搜索上游换成 route 自己的。SummaryMode 字段仅用于内部转成 SearchRoute 时标记。
type EnhanceSearchConfig struct {
	SummaryThinking bool   `json:"summary_thinking"` // step2 摘要是否开 thinking（默认关，加快摘要）
	SummaryLevel    string `json:"summary_level"`    // 摘要详细程度：low（默认）/ mid / high / max
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
// 自动改走此处指定的多模态上游。
type MultimodalRoute struct {
	URL   string `json:"url"`   // 目标上游 Base URL
	API   string `json:"api"`   // 目标 API key；空则透传客户端原 token
	Model string `json:"model"` // 替换成的目标模型名；空则不改 model 字段
}

// SearchRoute 定义搜索兜底路由：当请求带搜索工具却命中 no_search 的不支持搜索上游时，
// 自动改走此处指定的支持搜索的上游。
type SearchRoute struct {
	URL             string `json:"url"`              // 目标上游 Base URL
	API             string `json:"api"`              // 目标 API key；空则透传客户端原 token
	Model           string `json:"model"`            // 替换成的目标模型名；空则不改 model 字段
	SummaryMode     bool   `json:"summary_mode"`     // 搜索子代理请求：step1搜索+step2生成详细摘要，构造标准 web_search 响应返回客户端（不整请求转、不调主力）
	SummaryThinking bool   `json:"summary_thinking"` // summary_mode 下 step2 摘要请求是否开启 thinking（默认关，加快摘要）
	SummaryLevel    string `json:"summary_level"`    // summary_mode 下摘要详细程度：low（默认，简短）/ mid（中等）/ high（详尽）
}

// Version 是代理版本号，编译时用 -ldflags "-X main.Version=<git-short>" 注入；默认 dev。
var Version = "dev"

// debugSearchDirOverride 由 ldflags 注入（debug 版）：强制把搜索摘要各步 raw 写入此目录，忽略 config.search_debug_dir。
var debugSearchDirOverride string

var cfg atomic.Pointer[Config]

// configFilePath 是配置文件路径，main 启动时设置，托盘 reload 复用。
// 切换配置时会被 switchConfig 改写，故用 configMu 保护并发读写。
var configFilePath string

// configMu 保护 configFilePath 的并发读写：
// 切换配置(写)与重载/网页读写配置(读)可能并发，加锁避免读到半改的路径。
var configMu sync.RWMutex

// currentConfigPath 加锁返回当前配置文件路径。
func currentConfigPath() string {
	configMu.RLock()
	defer configMu.RUnlock()
	return configFilePath
}

// listConfigFiles 扫描当前配置所在目录的所有 .json 文件（排除 config.example.json），
// 返回排序后的文件名列表（不含目录）。供托盘「切换配置」子菜单与网页下拉列出。
func listConfigFilesIn(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || name == "config.example.json" {
			continue
		}
		files = append(files, name)
	}
	sort.Strings(files)
	return files
}

// listConfigFiles 列出当前配置目录下所有 .json 配置（供网页「切换配置」菜单）。
func listConfigFiles() []string {
	return listConfigFilesIn(filepath.Dir(currentConfigPath()))
}

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
	if c.UpstreamHeaderTimeoutSec <= 0 {
		c.UpstreamHeaderTimeoutSec = 70 // 默认 70s：等上游首字节超时则内部重发
	}
	if c.PingIntervalSec <= 0 {
		c.PingIntervalSec = 5 // 默认 5s：429 重试时向客户端发 SSE ping 保活的间隔
	}
	if c.RecentSampleWindow <= 0 {
		c.RecentSampleWindow = 20 // 默认统计最近 20 次请求的首字延迟与 token/s
	}
	if c.Upstream == "" && !hasCatchAllRoute(c.Routes) {
		log.Printf("[配置] 警告：upstream 为空且 routes 无 pattern:\"*\" 兜底，未命中路由的请求将直接 502")
	}
	for i := range c.Routes {
		if isReservedRoutePattern(c.Routes[i].Pattern) {
			log.Printf("[配置] 警告：第 %d 条路由 pattern 全字撞保留名 %q（Codex 菜单保留名：* 兜底=Fallback、fast 通道=fast_route），该路由不生效，请改名", i+1, c.Routes[i].Pattern)
		}
		switch c.Routes[i].Thinking {
		case "", "auto", "adaptive", "budget":
		default:
			return nil, fmt.Errorf("第 %d 条路由（pattern %q）的 thinking 值 %q 非法：只支持 auto / adaptive / budget", i+1, c.Routes[i].Pattern, c.Routes[i].Thinking)
		}
	}
	return &c, nil
}

// defaultCacheTTL 是完成流裁剪保护的固定窗口：每锚定键最新一行在流开始后 5 分钟内不被
// finishedCap 挤掉。上游缓存存活期实测是动态的（见缓存观测），不存在可配置的"有效期"，
// 故不再有 cache_time 配置项，这里只剩一个够覆盖常见会话节奏的固定保护窗口。
const defaultCacheTTL = 5 * time.Minute

// configExampleBytes 是内嵌的默认配置模板，首次运行时写入用户配置目录。
//
//go:embed config.example.json
var configExampleBytes []byte

// codexSetupPS1 / codexSetupSH 是内嵌的 Codex 一键配置脚本模板（Windows / macOS·Linux），
// 经 /__codexsetup.ps1 与 /__codexsetup.sh 提供：服务端按 query 参数（model/base/catalog）
// 烤制其中的 BAKED 锚点后下发，网页控制台给用户的只是一行拉取命令（DeepSeek 文档同款格式）。
//
//go:embed codex-setup.ps1
var codexSetupPS1 []byte

//go:embed codex-setup.sh
var codexSetupSH []byte

// activeConfigStateFile 记录上次选中的路由配置文件名（basename），放在路由配置同目录。
// 用 .txt 扩展名而非 .json：既不会被 listConfigFiles 当作路由配置列出，也避免与用户创建的 .json 重名。
const activeConfigStateFile = "active-config.txt"

// classifierSystemPrefix 是 Claude Code 分类器（安全判断）请求 system 字段的固定前缀。
// 代理据此识别分类器请求并路由到便宜模型、关闭 thinking。硬编码：客户端实现细节，不应由用户配置。
const classifierSystemPrefix = "You are a security monitor"

// readActiveConfigState 读取配置目录下的状态文件，返回它记录的上次选中配置的完整路径。
// 状态文件不存在、为空、或指向的文件已不存在时返回空串，调用方回退到默认 config.json。
func readActiveConfigState(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, activeConfigStateFile))
	if err != nil {
		return ""
	}
	name := strings.TrimSpace(string(data))
	if name == "" {
		return ""
	}
	full := filepath.Join(dir, name)
	if _, err := os.Stat(full); err != nil {
		return ""
	}
	return full
}

// writeActiveConfigState 把给定配置的文件名（basename）写入其所在目录的状态文件，供下次启动恢复。
// 写失败仅记日志，不影响切换本身。
func writeActiveConfigState(path string) {
	statePath := filepath.Join(filepath.Dir(path), activeConfigStateFile)
	if err := os.WriteFile(statePath, []byte(filepath.Base(path)), 0644); err != nil {
		log.Printf("[配置] 写状态文件失败: %v", err)
	}
}

// resolveConfigPath 决定配置文件路径，优先级：
//  1. -config 显式指定（最高，直接用）
//  2. 当前目录 config.json 存在（终端在项目目录运行）
//  3. 用户配置目录 proxy429/（.app 双击、安装后运行）
//
// 第 3 条让 macOS .app 双击启动也能找到配置：.app 的 cwd 是 /，没有 ./config.json，
// 故落到 ~/Library/Application Support/proxy429/（macOS）、
// ~/.config/proxy429/（Linux）、%AppData%/proxy429/（Windows）。
//
// 确定目录后，恢复顺序：
//   - active-config.txt 记录的上次选中配置仍存在 → 恢复它；
//   - 否则目录里已有任意 .json 配置 → 用第一个（按字母序），不自动生成 config.json；
//   - 否则首次运行：目录无任何配置，才写入内嵌默认模板 config.json。
//
// 这样用户即使从未用过 config.json 这个名字（只用自己的配置文件名），重启也不会凭空多出标准模板。
func resolveConfigPath(flagPath string) string {
	if flagPath != "" {
		return flagPath
	}
	var dir, defaultPath string
	if _, err := os.Stat("config.json"); err == nil {
		dir = "."
		defaultPath = "config.json"
	} else {
		ud, err := os.UserConfigDir()
		if err != nil || ud == "" {
			return "config.json"
		}
		dir = filepath.Join(ud, "proxy429")
		defaultPath = filepath.Join(dir, "config.json")
	}
	// 恢复上次选中的配置（若文件仍存在），使重启回到关闭前在用的配置。
	if active := readActiveConfigState(dir); active != "" {
		return active
	}
	// 目录里已有其他配置文件（用户在用非 config.json 的名字），不自动生成 config.json，
	// 回到目录里第一个配置（按字母序）。
	if files := listConfigFilesIn(dir); len(files) > 0 {
		return filepath.Join(dir, files[0])
	}
	// 首次运行：目录无任何配置，建目录并写入内嵌的默认模板，避免双击 .app 后无配置启动失败。
	// 用户随后在网页「配置」标签里改成自己的上游/key 即可。
	if dir != "." {
		if mkErr := os.MkdirAll(dir, 0755); mkErr == nil {
			if wErr := os.WriteFile(defaultPath, configExampleBytes, 0644); wErr == nil {
				log.Printf("[配置] 首次运行，已生成默认配置: %s", defaultPath)
			}
		}
	}
	return defaultPath
}

// clearStats 清空累计统计、延迟/吞吐样本与「最近完成的流」列表（含各自存档的透传内容），
// 按给定配置的 RecentSampleWindow 重建样本容量。在途流与流编号不清（避免与在途流撞号）。
// 仅网页「清空统计」按钮调用（切换/重载均不清统计，统计跨配置延续）。
func clearStats(c *Config) {
	stats.mu.Lock()
	stats.cacheRead = 0
	stats.cacheCreation = 0
	stats.inputTokens = 0
	stats.outputTokens = 0
	stats.modelStats = nil
	stats.mu.Unlock()
	stats.bytesForward.Store(0)
	stats.statusRetries.Store(0)
	stats.classifierRewrites.Store(0)
	stats.classifierHits.Store(0)
	stats.resetSampleCap(c.RecentSampleWindow)
	finishedMu.Lock()
	finished = nil
	cacheObsMap = map[string]*cacheObsEntry{} // 实测缓存存活观测一并清零
	finishedMu.Unlock()
}

// reloadConfig 重新读取当前配置文件并原子替换全局 cfg，不清统计（统计仅「清空统计」按钮清）。
// 失败时全局 cfg 保持旧配置不变。
func reloadConfig() error {
	c, err := loadConfig(currentConfigPath())
	if err != nil {
		log.Printf("[重载] 失败: %v", err)
		return err
	}
	cfg.Store(c)
	reconcileResponsesServer(c.ResponsesListen) // Responses 口随配置动态启停
	log.Printf("[重载] 配置已重新加载: http://%s -> %s (最多重试 %d 次, 分类器关thinking=%v)",
		c.Listen, c.Upstream, c.MaxRetries, c.ClassifierThinkingDisabled)
	return nil
}

// switchConfig 切换到指定配置文件并即时生效：加载新配置成功后改 configFilePath + cfg.Store。
// 不清空统计（统计跨配置延续，用「清空统计」按钮独立清）。失败则 configFilePath 不变、旧配置继续跑。
func switchConfig(newPath string) error {
	c, err := loadConfig(newPath)
	if err != nil {
		log.Printf("[切换] 加载 %s 失败: %v", newPath, err)
		return err
	}
	configMu.Lock()
	configFilePath = newPath
	configMu.Unlock()
	writeActiveConfigState(newPath)
	cfg.Store(c)
	reconcileResponsesServer(c.ResponsesListen) // Responses 口随配置动态启停
	// 通知托盘重建「切换配置」子菜单刷新勾选（网页端发起的切换不走托盘点击路径）
	notifyTrayCfgChanged()
	log.Printf("[切换] 已切换到 %s：http://%s -> %s (最多重试 %d 次)",
		filepath.Base(newPath), c.Listen, c.Upstream, c.MaxRetries)
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

// modelUsage 是单个真实上游模型的累计 token 用量，按模型名聚合供状态页明细展示。
type modelUsage struct {
	cacheRead     int64 // 命中 token（cache_read_input_tokens）
	cacheCreation int64 // 缓存写入 token（cache_creation_input_tokens）
	input         int64 // 未命中 token（input_tokens）
	output        int64 // 输出 token
	retries       int64 // 该模型触发的重试次数（网络错误/状态码/体内错误）
}

// liveStats 是所有流的聚合计数器，网页控制台与托盘状态灯用它展示实时状态。
type liveStats struct {
	mu                 sync.Mutex
	active             int                    // 当前透传中的流数量
	waiting            int                    // 已发上游、等首字节的请求数（状态灯黄）
	cacheRead          int64                  // 累计缓存命中 token（cache_read_input_tokens）
	cacheCreation      int64                  // 累计缓存写入 token（cache_creation_input_tokens）
	inputTokens        int64                  // 累计输入 token
	outputTokens       int64                  // 累计输出 token（各流当前累积值之和，随流增长）
	modelStats         map[string]*modelUsage // 按真实上游模型名聚合的 token 用量
	bytesForward       atomic.Int64           // 累计已转发字节，流过程中实时增长（ARK 不在流中发 token，用它体现实时迸出）
	statusRetries      atomic.Int64           // 启动至今的重试次数（含状态码/超时/网络错误/体内错误，每重试一次 +1）
	classifierRewrites atomic.Int64           // 启动至今命中分类器请求并关 thinking 的次数
	classifierHits     atomic.Int64           // 启动至今命中分类器（安全判断）特征的请求数：无论是否分流/关思考都计

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

// addModelUsage 按真实上游模型名累加一次流的 token 用量（流结束时调用一次）。
func (s *liveStats) addModelUsage(model string, in, cr, cc, out int64) {
	if model == "" || (in == 0 && cr == 0 && cc == 0 && out == 0) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.modelStats == nil {
		s.modelStats = make(map[string]*modelUsage)
	}
	m := s.modelStats[model]
	if m == nil {
		m = &modelUsage{}
		s.modelStats[model] = m
	}
	m.input += in
	m.cacheRead += cr
	m.cacheCreation += cc
	m.output += out
}

// addModelRetry 按真实上游模型名累加一次重试（网络错误/状态码/体内错误触发时调用）。
func (s *liveStats) addModelRetry(model string) {
	if model == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.modelStats == nil {
		s.modelStats = make(map[string]*modelUsage)
	}
	m := s.modelStats[model]
	if m == nil {
		m = &modelUsage{}
		s.modelStats[model] = m
	}
	m.retries++
}

// modelUsageEntry 是 modelStats 的 JSON 快照条目，供网页按模型展示缓存命中明细。
type modelUsageEntry struct {
	Model         string `json:"model"`
	CacheRead     int64  `json:"cacheRead"`     // 命中 token
	CacheCreation int64  `json:"cacheCreation"` // 缓存写入 token（命中率分母的一部分）
	Input         int64  `json:"input"`         // 未命中 token
	Output        int64  `json:"output"`        // 输出 token
	Retries       int64  `json:"retries"`       // 重试次数
}

// snapshotModelStats 返回按模型聚合的用量快照，按总 token（input+cacheRead+cacheCreation+output）降序。
func (s *liveStats) snapshotModelStats() []modelUsageEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.modelStats) == 0 {
		return nil
	}
	out := make([]modelUsageEntry, 0, len(s.modelStats))
	for name, m := range s.modelStats {
		out = append(out, modelUsageEntry{Model: name, CacheRead: m.cacheRead, CacheCreation: m.cacheCreation, Input: m.input, Output: m.output, Retries: m.retries})
	}
	sort.Slice(out, func(i, j int) bool {
		ti := out[i].Input + out[i].CacheRead + out[i].CacheCreation + out[i].Output
		tj := out[j].Input + out[j].CacheRead + out[j].CacheCreation + out[j].Output
		return ti > tj
	})
	return out
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

// cacheHitRate 返回缓存命中率，与 Claude Code 的 cache hit 算法一致：
// cache_read / (input + cache_read + cache_creation)。
// 分母是总输入（新鲜 + 命中 + 写入）：缓存写入不算命中但占输入量，漏掉它会虚高命中率。
// 分母为 0（无 usage 数据，如非流式响应或未解析到 usage）时返回 "-"。
func cacheHitRate(cacheRead, input, cacheCreation int64) string {
	denom := input + cacheRead + cacheCreation
	if denom == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", float64(cacheRead)*100/float64(denom))
}

// fmtMs 把毫秒格式化为秒字符串；≤0（未记录）返回 "-"。
func fmtMs(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.2fs", float64(ms)/1000)
}

// fmtFirstByte 把首字延迟毫秒格式化为秒字符串；≤0（未记录，如错误兜底流）返回 "-"。
func fmtFirstByte(ms int64) string {
	return fmtMs(ms)
}

// fmtTps 把 tok/s 格式化为字符串；≤0（无 output 或未记录）返回 "-"。
func fmtTps(tps float64) string {
	if tps <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f", tps)
}

// maxLogBuf 是日志环形缓冲区最大行数，也是网页「查看日志」页可翻看的历史上限。
const maxLogBuf = 500

// flight 跟踪单个进行中的请求，供网页控制台展示在途流（#id + 耗时/字节）。
type flight struct {
	id             uint64       // 递增编号
	start          time.Time    // 收到下游请求（flight 建立）时间
	phase          atomic.Int32 // 0=等待响应, 1=已收到响应（开始转发）
	status         int          // HTTP 状态码（phase=1 时有效）
	gaveUp         bool         // 重试/预算用尽后已向下游透传兜底错误事件（writeSSEError 置位，status 保持 0 不伪造）；完成流状态码列显 [重试尽]
	bytes          atomic.Int64 // 已转发字节数
	origModel      string       // 客户端原始 model；路由改写后用于响应流回改
	targetModel    string       // 路由改写后的目标 model；非空且≠origModel 时 forward 会回改
	upstreamModel  string       // 上游响应里实际返回的 model（由 rewriteResponseModel 捕获）；空则未知
	modelLogged    atomic.Bool  // 是否已打 [改写] 日志，只打一次
	stage          atomic.Int32 // 当前阶段（stage*），供网页在途流「状态」列展示
	stageStart     atomic.Int64 // 当前灯色开始时刻（unixnano；灯色由 stage 经 stageColor 映射），网页在状态灯旁显示该灯色已持续的秒数
	attempt        atomic.Int32 // 当前尝试序号（1 起），stage=stageAttempt 时显示「尝试N」
	attemptStart   atomic.Int64 // 当前尝试上游请求发出时刻（unixnano）：网页黄灯旁 [尝试N:Xs] 计时起点；0=重试退避中（下一次尝试未发出）
	routeReason    atomic.Int32 // 本次路由原因（route*），供网页「状态」列附加显示
	delivered      atomic.Bool  // 响应已完整送达下游（透传读到上游干净 EOF / 组装 JSON 一次性写完 / 合成流写到 message_stop）；499 改记只针对没送完的流——客户端常在收完整流后立刻断连（Codex 尤甚），不算中断
	contentMu      sync.Mutex
	content        []byte           // 最近 flightContentCap 字节透传内容（SSE 原文），供网页点击在途流查看
	reqBody        []byte           // 下游请求体原文（contentMu 同护；「储存完整结构体」关闭时 ≤ flightContentCap 只留前段，开启时不截断），供网页查看"什么请求导致这个流"；翻译口的流存的是翻译成 Anthropic 后的请求体
	fullContent    []byte           // 完整透传内容（contentMu 同护，仅「储存完整结构体」开启时记录，不设上限），供下载输出原文
	reqTruncated   bool             // 请求体是否被截断只剩前段（contentMu 同护）：记录时超长且未开完整储存，或关开关时被 purge 截断；供网页置灰下载按钮
	searchDebug    bool             // 搜索摘要模式：forward 时把主力响应 SSE 追加写入 cfg.SearchDebugDir
	inTokens       int64            // 本流 input_tokens 累积值（forward 结束时由 lastInput 存入）
	cacheRead      int64            // 本流 cache_read 累积值
	cacheCreation  int64            // 本流 cache_creation 累积值（缓存写入，命中率分母的一部分）
	outTokens      int64            // 本流 output_tokens 累积值
	firstByteMs    int64            // 本流首字延迟（毫秒），仅正常响应路径记录
	tps            float64          // 本流流式 tok/s，仅正常响应路径记录
	searchPrompt   string           // step2 摘要指令文本（仅搜索摘要子流非空），供状态页在途流/完成流最前端显示
	translated     string           // 翻译口来源标记（"responses"=翻译进来的流，"responses-raw"=route 配 url_response_api 的原生透传流），网页 API 列显示 [translate]/[Response]
	countTokens    bool             // count_tokens 探针流（countTokensPath），响应只有 {"input_tokens":N}，网页 model 列显示 [count_tokens] 前缀
	searchStripped atomic.Int32     // 剥掉的回放搜索块总数（对话水位剥+400 兜底剥；网页红标 [剥N]，拆分只写日志）
	searchReplay   *searchReplayCtx // Responses 翻译口的搜索还原上下文（ctx 带入，仅 handler goroutine 读写）；400 兜底剥块时取还原时刻学对话水位

	// 会话缓存跟踪（状态页"缓存年龄"列）：路由阶段一次性写入，addFinished 同 goroutine 读取。
	convID      string // 会话标识（Anthropic 口取 metadata.user_id 内 session_id，Responses 口取 prompt_cache_key）；空则不参与
	convAnchor  string // 锚定键后缀（"route:<pattern>"/"classifier"/"fast"，空=默认上游）：同会话同锚才互为同一条缓存 lineage
	upstreamKey string // 上游归类键（路由后最终 base URL|实际发送模型，其他参数不看）：实测缓存存活观测的归类维度

	// think 是实际发给上游的请求体里的思考配置最短形态（"关"/"开 <budget>"/"adaptive"/档位词），
	// 状态页「API」列思考值用——哪套 API 的词汇由列颜色承担，不在文字里。空=请求体未带思考字段（列显 -）。
	think string

	toolMu    sync.Mutex
	toolNames []string       // 响应流里工具调用的名字（按首次出现顺序；toolMu 保护）
	toolCalls map[string]int // 各工具调用次数（toolMu 保护）
	toolEmpty map[string]int // 各工具「参数结构体为空」的调用次数（toolMu 保护）
}

// realModel 返回本流的真实上游模型名：优先响应实际返回的，退路由目标，退原始 model。
// 供按模型统计 token/重试用，与 forward 里 addModelUsage 的取值逻辑一致。
func (f *flight) realModel() string {
	if f.upstreamModel != "" {
		return f.upstreamModel
	}
	if f.targetModel != "" {
		return f.targetModel
	}
	return f.origModel
}

// responsesRaw 报告本流是否为 Responses 原生透传流（命中的 route 配了 url_response_api）：
// 透传流的响应是 Responses 协议 SSE——usage/工具计数/终局标记都换成 Responses 口径解析，
// 网页 API 列显示 [Response] 而非 [translate]。
func (f *flight) responsesRaw() bool { return f.translated == translatedResponsesRaw }

// noteToolCall 记录一次工具调用（响应流 content_block_start 里 tool_use/server_tool_use 的
// 名字）。网页「最近完成的流」据此在 model 列后追加 [Read*1][Edit*3] 式标签。
func (f *flight) noteToolCall(name string) {
	if name == "" {
		return
	}
	f.toolMu.Lock()
	if f.toolCalls == nil {
		f.toolCalls = make(map[string]int)
	}
	if f.toolCalls[name] == 0 {
		f.toolNames = append(f.toolNames, name) // 只记首次出现，保序
	}
	f.toolCalls[name]++
	f.toolMu.Unlock()
}

// noteToolCallEmpty 标记该工具的一次调用参数结构体为空（流式在块结束/项完成时判定，
// 如 Kimi 空搜索的无参 server_tool_use）。只影响单次调用的 *0 标签；同名多次调用
// 仍显原始次数 *N。
func (f *flight) noteToolCallEmpty(name string) {
	f.toolMu.Lock()
	if f.toolCalls[name] > 0 {
		if f.toolEmpty == nil {
			f.toolEmpty = make(map[string]int)
		}
		f.toolEmpty[name]++
	}
	f.toolMu.Unlock()
}

// toolCallsTag 把记录的工具调用格式化成 "[Read*1][Edit*3][web_search*0]"：按首次出现
// 顺序；同名 N>1 次显 *N（原始次数）；单次调用显 *1，其参数结构体为空显 *0——
// 一眼区分空搜索与真搜索。无工具调用返回空串。
func (f *flight) toolCallsTag() string {
	f.toolMu.Lock()
	defer f.toolMu.Unlock()
	var b strings.Builder
	for _, n := range f.toolNames {
		suffix := 1
		if c := f.toolCalls[n]; c > 1 {
			suffix = c // 多次调用显原始次数（*0 空参判定只服务单次调用）
		} else if f.toolEmpty[n] > 0 {
			suffix = 0
		}
		b.WriteByte('[')
		b.WriteString(n)
		b.WriteByte('*')
		b.WriteString(strconv.Itoa(suffix))
		b.WriteByte(']')
	}
	return b.String()
}

// flight 阶段枚举：对应网页在途流「状态」列展示的进度。
// stageForward 时显示 HTTP 状态码（正在透传响应）；其余阶段显示对应中文标签。
const (
	stageRequest int32 = iota // 请求：读 body / 准备
	stageRoute                // 路由：匹配路由规则
	stageAttempt              // 尝试N：已发上游等首字节（含重试等待下一次尝试）
	stageForward              // 响应：收到响应正在透传，显示状态码
)

// stageColor 把阶段映射为网页状态灯颜色编号：0=白（请求）1=黄（路由/等首字节）2=绿（转发中），
// 与网页 flightDot 的配色一一对应。状态灯旁显示的持续时长按灯色计，不是按阶段计。
func stageColor(s int32) int32 {
	if s >= stageForward {
		return 2
	}
	if s >= stageRoute {
		return 1
	}
	return 0
}

// setStage 推进当前阶段；仅灯色变化时重置 stageStart（白→黄→绿各计各的时长）。
// 同色内的阶段推进（路由→尝试、重试再进尝试）不打断计时，"黄灯亮了多久"才是连续真实的。
func (f *flight) setStage(s int32) {
	if stageColor(s) != stageColor(f.stage.Load()) {
		f.stageStart.Store(time.Now().UnixNano())
	}
	f.stage.Store(s)
}

// stageMs 返回当前灯色已持续的毫秒数，供网页在状态灯旁显示。
func (f *flight) stageMs() int64 {
	return time.Since(time.Unix(0, f.stageStart.Load())).Milliseconds()
}

// attemptMs 返回当前尝试已等首字节的毫秒数（网页黄灯旁 [尝试N:Xs]）；
// attemptStart=0（重试退避中，下一次尝试尚未发出）返回 -1。
func (f *flight) attemptMs() int64 {
	t := f.attemptStart.Load()
	if t == 0 {
		return -1
	}
	return time.Since(time.Unix(0, t)).Milliseconds()
}

// flight 路由原因枚举：供网页「状态」列在阶段后附加显示（如「尝试1·搜索」）。
// routePassthrough 为默认零值：未命中任何路由，走默认 upstream 透传。
const (
	routePassthrough int32 = iota // 透传：未命中路由
	routePattern                  // pattern：命中 routes[] 通配
	routeClassifier               // 分类器：classifier_route
	routeFast                     // fast：fast_route
	routeMultimodal               // 多模态：multimodal_fallback 兜底
	routeSearch                   // 搜索：search_fallback 兜底
)

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

// flightContentCap 限制单个在途流内容缓冲上限：只保留最近这么多字节供网页查看，
// 避免长流式响应内存膨胀。256KB 足以看到绝大多数流的完整输出，超长流只留尾部。
const flightContentCap = 256 * 1024

// appendContent 把一段透传字节追加到 flight 的内容缓冲（超限只留尾部）。
// 在 forward 的 writeAndCount 里 w.Write 之后调用--tee 一份供网页查看，不影响透传。
func (f *flight) appendContent(data []byte) {
	f.contentMu.Lock()
	defer f.contentMu.Unlock()
	if fullStore.Load() {
		f.fullContent = append(f.fullContent, data...) // 完整副本：不设上限，供下载
	}
	f.content = append(f.content, data...)
	if len(f.content) > flightContentCap {
		f.content = f.content[len(f.content)-flightContentCap:]
		// 截断点可能落在 SSE 行中间，导致开头是半截 JSON（如 `ext"}}`）。
		// 对齐到下一个 event 边界（空行），丢弃开头不完整的 event，使原始视图开头是完整 SSE 行。
		if i := bytes.Index(f.content, []byte("\n\n")); i >= 0 {
			f.content = f.content[i+2:]
		}
	}
}

// snapshotContent 返回当前内容缓冲的副本，供网页端点读取。
func (f *flight) snapshotContent() []byte {
	f.contentMu.Lock()
	defer f.contentMu.Unlock()
	out := make([]byte, len(f.content))
	copy(out, f.content)
	return out
}

// setReqBody 记录下游请求体原文。「储存完整结构体」关闭时超长截断只留前段
// （请求开头的 model/system/tools 比尾部更有定位价值）；开启时不截断，供完整下载。
// handler 读完 body 时调用一次，之后不再变。
func (f *flight) setReqBody(b []byte) {
	truncated := false
	if !fullStore.Load() && len(b) > flightContentCap {
		b = b[:flightContentCap]
		truncated = true
	}
	f.contentMu.Lock()
	f.reqBody = b
	f.reqTruncated = truncated
	f.contentMu.Unlock()
}

// snapshotReqBody 返回请求体原文的副本（未记录返回 nil），供网页端点读取。
func (f *flight) snapshotReqBody() []byte {
	f.contentMu.Lock()
	defer f.contentMu.Unlock()
	if f.reqBody == nil {
		return nil
	}
	out := make([]byte, len(f.reqBody))
	copy(out, f.reqBody)
	return out
}

// snapshotFullContent 返回完整透传内容的副本（未记录返回 nil），供下载端点读取。
func (f *flight) snapshotFullContent() []byte {
	f.contentMu.Lock()
	defer f.contentMu.Unlock()
	if f.fullContent == nil {
		return nil
	}
	out := make([]byte, len(f.fullContent))
	copy(out, f.fullContent)
	return out
}

// purgeFull 关闭「储存完整结构体」时释放完整副本：fullContent 置 nil，reqBody 截回 cap
// （截断置 reqTruncated 标记，网页据此置灰下载按钮——截断版不能当完整版下载）。
func (f *flight) purgeFull() {
	f.contentMu.Lock()
	f.fullContent = nil
	if len(f.reqBody) > flightContentCap {
		f.reqBody = f.reqBody[:flightContentCap]
		f.reqTruncated = true
	}
	f.contentMu.Unlock()
}

// reqTrunc 返回请求体是否被截断只剩前段，供网页端点下发。
func (f *flight) reqTrunc() bool {
	f.contentMu.Lock()
	defer f.contentMu.Unlock()
	return f.reqTruncated
}

// hasFullContent 返回是否仍持有完整输出副本，供网页判定下载/交互按钮可用性。
func (f *flight) hasFullContent() bool {
	f.contentMu.Lock()
	defer f.contentMu.Unlock()
	return f.fullContent != nil
}

// finishedFlight 是一个已结束 flight 的存档快照，供网页回看最近完成的流式输出。
// 只在 flight 结束且 content 非空（有流式推送）时存档；content 是 snapshotContent 的副本。
type finishedFlight struct {
	id             uint64
	model          string // origModel -> targetModel（-> upstreamModel 若上游返回且不同）
	upstreamModel  string // 上游响应实际返回的 model 名（空则未返回/同 targetModel）
	routeReason    int32  // 路由原因（route*），网页 model 列显示 [标签] 前缀用
	status         int    // HTTP 状态码
	gaveUp         bool   // 重试/预算用尽已透传兜底错误事件（此时 status 为 0），网页状态码列显 [重试尽]
	attempts       int32  // 尝试次数（=重试次数+1；1 = 一把过无重试），网页状态码列 >1 时追加 [重试N次]
	bytes          int64
	stage          int32 // 结束时阶段
	ended          time.Time
	totalMs        int64   // 总耗时：收到下游请求到响应全部发回下游（= 翻译/路由/缓冲等内部处理 + 首字等待 + 吐字 + 收尾内部处理）
	content        []byte  // ≤ flightContentCap 的透传内容副本
	reqBody        []byte  // ≤ flightContentCap 的下游请求体原文副本（「储存完整结构体」开启时不截断；未记录为 nil）
	fullContent    []byte  // 完整透传内容副本（仅「储存完整结构体」开启时记录，未记录为 nil）
	reqTruncated   bool    // 请求体是否被截断只剩前段（供网页置灰下载按钮）
	inTokens       int64   // 本流 input_tokens 累积值
	cacheRead      int64   // 本流 cache_read 累积值
	cacheCreation  int64   // 本流 cache_creation 累积值
	outTokens      int64   // 本流 output_tokens 累积值
	firstByteMs    int64   // 本流首字延迟（毫秒）
	tps            float64 // 本流流式 tok/s
	searchPrompt   string  // step2 摘要指令文本（仅搜索摘要子流非空）
	translated     string  // 翻译口来源标记（"responses"=翻译，"responses-raw"=原生透传），网页 API 列 [translate]/[Response]
	countTokens    bool    // count_tokens 探针流，网页 model 列 [count_tokens] 前缀
	tools          string  // 工具调用标签（"[Read*1][Edit*3]"，无工具为空），网页 model 列追加显示
	searchStripped int     // 剥掉的回放搜索块总数（对话水位剥+400 兜底剥；0=未剥），网页缓存命中列红标 [剥N]
	convKey        string  // 会话缓存锚定键（convID|convAnchor）；空则该流不参与"缓存年龄"显示与裁剪保护
	// start 锚流开始时刻：缓存写入/刷新发生在上游处理输入（≈流开始）时，"缓存年龄"与裁剪保护窗口都锚它。
	start       time.Time
	obsKey      string // 实测缓存存活配对键（convID|upstreamKey）；空则不参与观测（黄灯 499/无会话/无归类键）
	upstreamKey string // 上游归类键（路由后 url|模型）：「缓存命中」弹窗实测表按此键归组
	think       string // 实际发给上游的思考配置最短形态（状态页「API」列思考值）；空=请求体未带思考字段
}

var (
	finishedMu  sync.Mutex
	finished    []finishedFlight
	finishedCap atomic.Int32 // 保留最近多少个完成流，状态页可改；init 置 10
)

// fullStore 是「储存完整结构体」开关（状态页可切，默认关，重启即复位）：
// 开 = flight 额外记录完整请求体/输出（不设 256KB 上限）供下载；关 = 立即清空完整副本。
var fullStore atomic.Bool

func init() {
	finishedCap.Store(10)
}

// markClientGone 在 flight 归档时按上游口径校正状态码（目的：显示与上游提供商后台一致）：
//   - 上游已完整发完（delivered）：无论下游是否还在都保留 200（上游后台也是 200）；
//   - 上游流开了头但没发完（status==200 未送达）：记 499——下游取消会经 ctx 传导成上游
//     断连，上游后台同样记 499（nginx 惯例 client closed request）；
//   - 还没收到上游响应就结束且因下游取消（status==0 且 ctx 已取消）：请求已发到上游但
//     被中止，上游后台同样是 499。本地错误（读 body 失败等，ctx 未取消）不受影响。
//
// 调用点在 handler 收尾 defer——此刻 ctx 只会因下游断开而取消（自身 cancel 按 defer LIFO
// 在此之后才执行）。
func markClientGone(f *flight, ctx context.Context) {
	if f.delivered.Load() {
		return
	}
	if f.status == 200 || (f.status == 0 && ctx.Err() != nil) {
		f.status = 499
	}
}

// addFinished 在 flight 结束后存档其透传内容，供网页回看最近完成的流。
// 不跳过空内容：失败/重试用尽/非流式请求也进列表，便于排查为何某个流没输出。
// 超 finishedCap 时丢弃最旧的。调用方为 handler defer，单 goroutine。
func addFinished(f *flight) {
	content := f.snapshotContent()
	model := f.origModel
	if f.targetModel != "" && f.targetModel != f.origModel {
		model = f.origModel + " -> " + f.targetModel
	}
	// 上游响应里实际返回的 model 名与路由目标不同时，追加第三段，让用户看到真实落点。
	if f.upstreamModel != "" && f.upstreamModel != f.targetModel && f.upstreamModel != f.origModel {
		model = model + " -> " + f.upstreamModel
	}
	now := time.Now()
	convKey := convKeyOf(f)
	obsKey := obsKeyOf(f)
	// 黄灯（等首字节，stage<3）阶段被下游断开的 499：上游未必已处理输入写缓存，保守不刷新锚——
	// 视同无锚（本行显 "-"，该键锚停留再上一次同键流）。绿灯（stage=3 转发中）断开的 499：
	// 上游已在吐字说明输入已处理、缓存已写，照常刷新锚。
	if f.status == 499 && f.stage.Load() < 3 {
		convKey = ""
		obsKey = "" // 缓存写没写都不确定，存活实测同样不算数（本流不当 cur，归档后也不当别人的 prev）
	}
	ff := finishedFlight{
		id:             f.id,
		model:          model,
		upstreamModel:  f.upstreamModel,
		routeReason:    f.routeReason.Load(),
		status:         f.status,
		gaveUp:         f.gaveUp,
		attempts:       f.attempt.Load(),
		bytes:          f.bytes.Load(),
		stage:          f.stage.Load(),
		ended:          now,
		totalMs:        now.Sub(f.start).Milliseconds(),
		content:        content,
		reqBody:        f.snapshotReqBody(),
		fullContent:    f.snapshotFullContent(),
		reqTruncated:   f.reqTrunc(),
		inTokens:       f.inTokens,
		cacheRead:      f.cacheRead,
		cacheCreation:  f.cacheCreation,
		outTokens:      f.outTokens,
		firstByteMs:    f.firstByteMs,
		tps:            f.tps,
		searchPrompt:   f.searchPrompt,
		translated:     f.translated,
		countTokens:    f.countTokens,
		tools:          f.toolCallsTag(),
		searchStripped: int(f.searchStripped.Load()),
		convKey:        convKey,
		start:          f.start,
		obsKey:         obsKey,
		upstreamKey:    f.upstreamKey,
		think:          f.think,
	}
	finishedMu.Lock()
	// 实测缓存存活观测：与同会话同上游的上一条完成流配对（finished 升序，倒扫取最近一条同键）。
	// 间隔锚两条流的开始时刻（缓存写/读都发生在流开始附近）。
	if ff.obsKey != "" {
		for i := len(finished) - 1; i >= 0; i-- {
			if finished[i].obsKey == ff.obsKey {
				if kind, iv := classifyCacheObservation(finished[i].cacheRead, ff.cacheRead, ff.inTokens, ff.cacheCreation, f.start.Sub(finished[i].start)); kind != obsNone {
					recordCacheObsLocked(ff.upstreamKey, kind, iv)
				}
				break
			}
		}
	}
	finished = append(finished, ff)
	trimFinishedLocked()
	finishedMu.Unlock()
	log.Printf("[完成流] #%d 存档 %d 字节 stage=%d", f.id, len(content), ff.stage)
}

// convKeyOf 返回该流的会话缓存锚定键；无会话标识（count_tokens 探针、裸 API 无 metadata）返回空。
func convKeyOf(f *flight) string {
	if f.convID == "" {
		return ""
	}
	return f.convID + "|" + f.convAnchor
}

// ---- 上游缓存存活实测（状态页「缓存命中」弹窗实测表，纯展示、内存态）----
//
// 同会话同上游（obsKey = convID|upstreamKey）相邻两条完成流给一次观测：
//   - 后条命中率 ≥95% → 存活观测：缓存至少活了「两条流开始时刻的间隔」那么久，取 max 作实测下界；
//   - 前条命中过 + 后条命中率 <50% → 死亡观测：缓存没活过那个间隔，取 min 作实测上界
//     （不看严格归零：上游会缓存系统提示词等公共前缀，会话缓存死后仍可能有零星命中）；
//   - 中间地带（50%~95%）不观测：说不清是缓存过期还是输入漂移；
//   - 输入体量 <1024 token 不观测：小请求命中率噪声大；
//   - 间隔 ≤0（时钟回拨等）不观测；
//   - 两侧矛盾（上游缓存时间中途变化，或缓存被提前驱逐）时以较新的观测为准，
//     被否的一侧作废重测——保证显示永不倒挂（如 "≥18分钟、<16分钟"）。
// 测的是下限不是真 TTL：用户不再追问的会话永远不提供死亡信号。
// cacheObsMap 与 finished 同受 finishedMu 保护，clearStats 时一并清零（重启自然清零）。

type cacheObsKind int

const (
	obsNone  cacheObsKind = iota // 不构成观测
	obsAlive                     // 存活观测：缓存至少活了间隔那么久
	obsDead                      // 死亡观测：缓存没活过间隔
)

const cacheAliveHitRate = 0.95 // 后条命中率 ≥95% 视为"仍在缓存内"
const cacheDeadHitRate = 0.50  // 前条命中过、后条命中率 <50% 视为缓存已失（系统提示词等公共前缀的残留命中不算活着）
const cacheObsMinTokens = 1024 // 输入体量（input+cacheRead+cacheCreation）低于此不观测
const cacheObsMaxKeyLen = 512  // obsKey 异常超长（恶意 session_id）时截断，防内存膨胀

type cacheObsEntry struct {
	aliveMax time.Duration // 存活观测的最大间隔（实测下限：缓存至少活过这么久）
	deadMin  time.Duration // 死亡观测的最小间隔（实测上界：缓存没活过这么久）；0 = 尚无死亡观测
	samples  int           // 观测次数（存活+死亡合计）

	// aliveAt/deadAt 是各自界数值最后变化的时刻：该界数值变化（含矛盾作废）即重置，
	// 作废重测侧清零——界不存在时对应形成时间也不存在。
	aliveAt time.Time
	deadAt  time.Time
}

var cacheObsMap = map[string]*cacheObsEntry{} // 键 = upstreamKey（url|模型）；finishedMu 同护

// classifyCacheObservation 判定一条完成流相对其同会话同上游上一条构成什么观测。
// 死亡判定不看严格归零（上游会缓存系统提示词等公共前缀，会话缓存过期后仍可能有零星
// cache_read），用命中率 <50% 作"缓存已失"。返回 obsNone 时第二返回值无意义。
func classifyCacheObservation(prevCacheRead, curCacheRead, curInput, curCacheCreation int64, interval time.Duration) (cacheObsKind, time.Duration) {
	if interval <= 0 || curInput+curCacheRead+curCacheCreation < cacheObsMinTokens {
		return obsNone, 0
	}
	hitRate := float64(curCacheRead) / float64(curInput+curCacheRead+curCacheCreation)
	// 先查死亡：前条命中过说明缓存写过，后条命中率 <50% 说明会话缓存没活到这次
	// （两分支互斥：命中率不可能同时 <50% 与 ≥95%，顺序只为可读）。
	if prevCacheRead > 0 && hitRate < cacheDeadHitRate {
		return obsDead, interval
	}
	if hitRate >= cacheAliveHitRate {
		return obsAlive, interval
	}
	return obsNone, 0
}

// recordCacheObsLocked 把一次观测累积进该上游条目：存活取 max（下界只升），死亡取 min（上界只降）。
// 两侧矛盾时以较新的观测为准、被否的一侧作废重测：上游缓存时间中途变长（新存活观测越过旧上界）
// 则上界作废；中途变短或被提前驱逐（新死亡观测跌破旧下界）则下界作废。
// 每界的形成时间各自记账：该界数值变化（含被作废清零）才动，仅新增支撑观测不动。
// 不变式：两侧都非零时 aliveMax < deadMin（显示永不倒挂）。调用方须持 finishedMu。
func recordCacheObsLocked(key string, kind cacheObsKind, interval time.Duration) {
	e := cacheObsMap[key]
	if e == nil {
		e = &cacheObsEntry{}
		cacheObsMap[key] = e
	}
	now := time.Now()
	if kind == obsAlive {
		if interval > e.aliveMax {
			e.aliveMax = interval
			e.aliveAt = now // 下界数值变化才重起算
		}
		if e.deadMin > 0 && e.deadMin <= e.aliveMax {
			e.deadMin = 0 // 旧上界被新存活证据否定（TTL 变长/旧上界是噪声），作废重测
			e.deadAt = time.Time{}
			e.samples = 0 // 观测次数同步归零重计：只计支撑当前上下界的观测
		}
	}
	if kind == obsDead {
		if e.deadMin == 0 || interval < e.deadMin {
			e.deadMin = interval
			e.deadAt = now // 上界数值变化才重起算
		}
		if e.aliveMax >= e.deadMin {
			e.aliveMax = 0 // 旧下界被新死亡证据否定（TTL 变短/提前驱逐），作废重测
			e.aliveAt = time.Time{}
			e.samples = 0 // 观测次数同步归零重计：只计支撑当前上下界的观测
		}
	}
	e.samples++
}

// obsKeyOf 返回实测缓存存活的配对键（convID|upstreamKey）；任一侧为空（count_tokens 探针、
// 裸 API 无 metadata、上游归类键缺失）返回空串表示不参与观测。
func obsKeyOf(f *flight) string {
	if f.convID == "" || f.upstreamKey == "" {
		return ""
	}
	k := f.convID + "|" + f.upstreamKey
	if len(k) > cacheObsMaxKeyLen {
		k = k[:cacheObsMaxKeyLen]
	}
	return k
}

// cacheObsRow 是实测缓存存活的 JSON 快照条目（缓存命中弹窗「实测缓存时间」表用）。
type cacheObsRow struct {
	URL     string `json:"url"`     // 上游 base URL（剥掉 scheme 缩短显示）
	Model   string `json:"model"`   // 实际发送模型
	Alive   string `json:"alive"`   // 下界读法（"23分钟"），无存活观测为 ""
	Dead    string `json:"dead"`    // 上界读法，无死亡观测为 ""
	Samples int    `json:"samples"` // 观测次数（存活+死亡合计）

	// AliveAge/DeadAge 是各自界数值形成至今的时长（该界数值变化即重新起算），m:ss 递增；
	// 对应界无观测（Alive/Dead 为 ""）时同样为 ""。
	AliveAge string `json:"aliveAge"`
	DeadAge  string `json:"deadAge"`
}

// snapshotCacheObs 返回全部上游的实测缓存存活快照，按 URL+模型排序（显示稳定）。
func snapshotCacheObs() []cacheObsRow {
	finishedMu.Lock()
	defer finishedMu.Unlock()
	if len(cacheObsMap) == 0 {
		return nil
	}
	now := time.Now()
	out := make([]cacheObsRow, 0, len(cacheObsMap))
	for key, e := range cacheObsMap {
		url, model, _ := strings.Cut(key, "|")
		url = strings.TrimPrefix(url, "https://")
		url = strings.TrimPrefix(url, "http://")
		row := cacheObsRow{URL: url, Model: model, Samples: e.samples}
		if e.aliveMax > 0 {
			row.Alive = fmtObsDur(e.aliveMax)
			if !e.aliveAt.IsZero() {
				row.AliveAge = fmtCacheAge(now.Sub(e.aliveAt))
			}
		}
		if e.deadMin > 0 {
			row.Dead = fmtObsDur(e.deadMin)
			if !e.deadAt.IsZero() {
				row.DeadAge = fmtCacheAge(now.Sub(e.deadAt))
			}
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].URL != out[j].URL {
			return out[i].URL < out[j].URL
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// trimFinishedLocked 把 finished 裁到 finishedCap 条：裁员窗口 = 最旧的 len-cap 行，
// 窗口内受保护行跳过不丢（也不顺延补偿——14 行留 10 时窗口 4 行里有 1 行受保护，则只丢 3 行留 11 行）。
// 受保护 = 每条锚定键（会话+路由）的最新一行且仍在保护窗口内（now < start + defaultCacheTTL），
// 它是状态页"缓存年龄"的载体，挤掉就看不到了（此时行数允许超 cap）。
// 出保护窗口与被同键更新行刷新（显示 -）的行不保护，照常 FIFO。调用方须持 finishedMu。
func trimFinishedLocked() {
	capN := int(finishedCap.Load())
	if len(finished) <= capN {
		return
	}
	// 每键最新行下标（finished 按时间升序，遍历时后者覆盖前者即最新）。
	latest := make(map[string]int)
	for i := range finished {
		if finished[i].convKey != "" {
			latest[finished[i].convKey] = i
		}
	}
	now := time.Now()
	protected := make(map[int]bool, len(latest))
	for _, i := range latest {
		if now.Before(finished[i].start.Add(defaultCacheTTL)) {
			protected[i] = true
		}
	}
	window := len(finished) - capN // 裁员窗口：只有最旧的 window 行参与淘汰
	out := finished[:0]            // 原地过滤：写指针永远 <= 读指针，安全
	for i, ff := range finished {
		if i < window && !protected[i] {
			continue
		}
		out = append(out, ff)
	}
	finished = out
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

// clear 清空所有已存日志（保留底层数组容量）。
func (r *logRing) clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = r.lines[:0]
	r.head = 0
	r.len = 0
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
// 见到带 usage 的 message_delta 时置 *sawDeltaUsage=true：真实用量拆分在该事件里
// （ARK 的 start 全 0；Kimi 的 start.input 含 cache_read 且 start.cr=0），
// 流中断没等到它时调用方应回滚本流已计入的增量（见 rollbackUsageStats）。可传 nil 表示不用标记。
func parseSSEStats(line []byte, lastOutput, lastInput, lastCacheRead, lastCacheCreation *int64, sawDeltaUsage *bool) {
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
		if usage != nil && sawDeltaUsage != nil {
			*sawDeltaUsage = true
		}
	}
	if usage == nil {
		return
	}
	stats.mu.Lock()
	defer stats.mu.Unlock()
	// usage 每个字段取本流最后出现的值（后值覆盖前值），全局只加 new-last 差值。
	// 这样一个 response 走完后，单流/全局都是各字段最后一次出现时的值。
	// 不同上游语义不一：ARK 的 message_start 全 0、真实值在 message_delta；
	// Kimi 的 message_start.input 含 cache_read、message_delta.input 为纯 input（更小），
	// 取最后值才能拿到标准语义的 input_tokens。字段缺失时保持原值（不覆盖为 0）。
	// 注：ARK 的 usage 不含 cache_creation_input_tokens，该上游缓存写入按 0 计。
	if v, ok := usage["input_tokens"]; ok {
		in := toInt64(v)
		if in != *lastInput {
			stats.inputTokens += in - *lastInput
			*lastInput = in
		}
	}
	if v, ok := usage["cache_read_input_tokens"]; ok {
		cr := toInt64(v)
		if cr != *lastCacheRead {
			stats.cacheRead += cr - *lastCacheRead
			*lastCacheRead = cr
		}
	}
	if v, ok := usage["cache_creation_input_tokens"]; ok {
		cc := toInt64(v)
		if cc != *lastCacheCreation {
			stats.cacheCreation += cc - *lastCacheCreation
			*lastCacheCreation = cc
		}
	}
	if v, ok := usage["output_tokens"]; ok {
		out := toInt64(v)
		if out != *lastOutput {
			stats.outputTokens += out - *lastOutput
			*lastOutput = out
		}
	}
}

// toolCallStart 是 content_block_start 里 tool_use/server_tool_use 的解析结果：
// index 用于跨行追踪同一调用（input_json_delta/content_block_stop 按它对齐）；
// input 是 start 事件自带的参数（tool_use 恒 {}、真参数走后续 delta；server_tool_use
// 可能已完整，Kimi 空搜索则无 input）。
type toolCallStart struct {
	index int
	name  string
	input json.RawMessage
}

// parseToolCallStart 从 SSE data 行解析工具调用的块开始事件：仅 content_block_start
// 且块类型为 tool_use/server_tool_use 时返回结果，否则 ok=false。先用子串粗筛再
// JSON 解析，避免对每行都做完整反序列化。
func parseToolCallStart(line []byte) (ts toolCallStart, ok bool) {
	s := bytes.TrimSpace(line)
	if !bytes.HasPrefix(s, []byte("data:")) {
		return ts, false
	}
	payload := bytes.TrimSpace(s[len("data:"):])
	if !bytes.Contains(payload, []byte("content_block_start")) || !bytes.Contains(payload, []byte("tool_use")) {
		return ts, false
	}
	var ev struct {
		Type         string `json:"type"`
		Index        int    `json:"index"`
		ContentBlock struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content_block"`
	}
	if json.Unmarshal(payload, &ev) != nil || ev.Type != "content_block_start" {
		return ts, false
	}
	switch ev.ContentBlock.Type {
	case "tool_use", "server_tool_use":
		return toolCallStart{index: ev.Index, name: ev.ContentBlock.Name, input: ev.ContentBlock.Input}, true
	}
	return ts, false
}

// parseToolCallName 从 SSE data 行提取工具调用名（parseToolCallStart 的名字部分）。
func parseToolCallName(line []byte) string {
	if ts, ok := parseToolCallStart(line); ok {
		return ts.name
	}
	return ""
}

// parseToolArgsDelta 从 SSE data 行解析 input_json_delta，返回所属块 index 与参数碎片。
func parseToolArgsDelta(line []byte) (index int, partial string, ok bool) {
	s := bytes.TrimSpace(line)
	if !bytes.HasPrefix(s, []byte("data:")) {
		return 0, "", false
	}
	payload := bytes.TrimSpace(s[len("data:"):])
	if !bytes.Contains(payload, []byte("input_json_delta")) {
		return 0, "", false
	}
	var ev struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
		Delta struct {
			Type    string `json:"type"`
			Partial string `json:"partial_json"`
		} `json:"delta"`
	}
	if json.Unmarshal(payload, &ev) != nil || ev.Type != "content_block_delta" || ev.Delta.Type != "input_json_delta" {
		return 0, "", false
	}
	return ev.Index, ev.Delta.Partial, true
}

// parseToolBlockStop 从 SSE data 行解析 content_block_stop，返回结束块的 index。
func parseToolBlockStop(line []byte) (index int, ok bool) {
	s := bytes.TrimSpace(line)
	if !bytes.HasPrefix(s, []byte("data:")) {
		return 0, false
	}
	payload := bytes.TrimSpace(s[len("data:"):])
	if !bytes.Contains(payload, []byte("content_block_stop")) {
		return 0, false
	}
	var ev struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
	}
	if json.Unmarshal(payload, &ev) != nil || ev.Type != "content_block_stop" {
		return 0, false
	}
	return ev.Index, true
}

// isEmptyArgsJSON 报告一段 JSON 文本是否等价于"空参数结构体"：空串、{}、null
// （忽略所有空白）。*0 标签判定专用。
func isEmptyArgsJSON(s string) bool {
	s = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r':
			return -1
		}
		return r
	}, s)
	return s == "" || s == "{}" || s == "null"
}

// openToolCall 追踪一次进行中的流式工具调用，供块结束时判定参数结构体是否为空
// （*0 标签）。参数碎片只留前 64 字节：超出必然不是空对象，不为大参数（Edit 全量
// diff）留全量副本。
type openToolCall struct {
	name    string
	hasArgs bool   // start 事件已带非空 input（server_tool_use 完整块）
	buf     []byte // input_json_delta 碎片（≤64 字节，stop 时经 isEmptyArgsJSON 判定）
}

// notePartial 累积参数碎片，封顶 64 字节。
func (t *openToolCall) notePartial(p string) {
	if len(t.buf) >= 64 {
		return
	}
	n := 64 - len(t.buf)
	if n > len(p) {
		n = len(p)
	}
	t.buf = append(t.buf, p[:n]...)
}

// argsEmpty 报告块结束时参数结构体是否为空。
func (t *openToolCall) argsEmpty() bool {
	return !t.hasArgs && isEmptyArgsJSON(string(t.buf))
}

// parseResponsesStreamStats 是 parseSSEStats 的 Responses 协议版（url_response_api 透传流用）。
// Responses SSE 里 usage 只在终局事件（response.completed/response.incomplete）出现一次；
// 且 OpenAI 语义 input_tokens 含缓存总量、cached_tokens 是其中命中部分——拆成
// fresh = input - cached（下限 0）+ cr = cached，与 Anthropic 口径对齐后再进全局聚合。
// 见到终局 usage 时置 *sawDeltaUsage=true（语义同 Anthropic 的 message_delta usage：
// 真实用量已到达；流中断没等到它时调用方回滚，此时各追踪器本就是 0，回滚为无操作）。
func parseResponsesStreamStats(line []byte, lastOutput, lastInput, lastCacheRead, lastCacheCreation *int64, sawDeltaUsage *bool) {
	s := bytes.TrimSpace(line)
	if !bytes.HasPrefix(s, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(s[len("data:"):])
	// 粗筛：只对终局事件做完整反序列化（response.created 等事件无 usage 或全 0，直接跳过）。
	if !bytes.Contains(payload, []byte("response.completed")) && !bytes.Contains(payload, []byte("response.incomplete")) {
		return
	}
	var ev struct {
		Type     string `json:"type"`
		Response struct {
			Usage *struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
				// cache_creation_input_tokens 非 OpenAI 标准字段，部分 Anthropic 兼容端点会带；缺失即 0。
				CacheCreation     int64 `json:"cache_creation_input_tokens"`
				InputTokenDetails struct {
					CachedTokens int64 `json:"cached_tokens"`
				} `json:"input_tokens_details"`
			} `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(payload, &ev) != nil {
		return
	}
	if (ev.Type != "response.completed" && ev.Type != "response.incomplete") || ev.Response.Usage == nil {
		return
	}
	u := ev.Response.Usage
	in := u.InputTokens - u.InputTokenDetails.CachedTokens
	if in < 0 {
		in = 0 // 防御：上游口径异常时别把全局 input 加成负的
	}
	cr := u.InputTokenDetails.CachedTokens
	cc := u.CacheCreation
	out := u.OutputTokens
	if sawDeltaUsage != nil {
		*sawDeltaUsage = true
	}
	stats.mu.Lock()
	defer stats.mu.Unlock()
	// 与 parseSSEStats 同模式：后值覆盖前值，全局只加差值（usage 虽只来一次，保持同构便于复用追踪器）。
	if in != *lastInput {
		stats.inputTokens += in - *lastInput
		*lastInput = in
	}
	if cr != *lastCacheRead {
		stats.cacheRead += cr - *lastCacheRead
		*lastCacheRead = cr
	}
	if cc != *lastCacheCreation {
		stats.cacheCreation += cc - *lastCacheCreation
		*lastCacheCreation = cc
	}
	if out != *lastOutput {
		stats.outputTokens += out - *lastOutput
		*lastOutput = out
	}
}

// parseResponsesToolStart 是 parseToolCallStart 的 Responses 协议版：从
// response.output_item.added 事件提取 (itemID, 工具名)——function_call/custom_tool_call
// 取 item.name，web_search_call（服务端工具，无 name）固定返回 "web_search"。
// itemID 用于与 output_item.done 对齐（*0 空参判定）。粗筛后再解析，避免每行反序列化。
func parseResponsesToolStart(line []byte) (itemID, name string, ok bool) {
	s := bytes.TrimSpace(line)
	if !bytes.HasPrefix(s, []byte("data:")) {
		return "", "", false
	}
	payload := bytes.TrimSpace(s[len("data:"):])
	if !bytes.Contains(payload, []byte("output_item.added")) {
		return "", "", false
	}
	var ev struct {
		Type string `json:"type"`
		Item struct {
			ID   string `json:"id"`
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"item"`
	}
	if json.Unmarshal(payload, &ev) != nil || ev.Type != "response.output_item.added" {
		return "", "", false
	}
	switch ev.Item.Type {
	case "function_call", "custom_tool_call":
		return ev.Item.ID, ev.Item.Name, true
	case "web_search_call":
		return ev.Item.ID, "web_search", true
	}
	return "", "", false
}

// parseResponsesToolName 从 SSE data 行提取工具调用名（parseResponsesToolStart 的名字部分）。
func parseResponsesToolName(line []byte) string {
	if _, name, ok := parseResponsesToolStart(line); ok {
		return name
	}
	return ""
}

// parseResponsesToolDone 从 response.output_item.done 事件提取 (itemID, 参数结构体为空)：
// function_call 看最终 arguments、custom_tool_call 看 input、web_search_call 看 action
// （无 query 且无 sources = 空）。done 事件带完整参数，无需跨行累积。其他项类型 ok=false。
func parseResponsesToolDone(line []byte) (itemID string, argsEmpty bool, ok bool) {
	s := bytes.TrimSpace(line)
	if !bytes.HasPrefix(s, []byte("data:")) {
		return "", false, false
	}
	payload := bytes.TrimSpace(s[len("data:"):])
	if !bytes.Contains(payload, []byte("output_item.done")) {
		return "", false, false
	}
	var ev struct {
		Type string `json:"type"`
		Item struct {
			ID        string          `json:"id"`
			Type      string          `json:"type"`
			Arguments string          `json:"arguments"`
			Input     json.RawMessage `json:"input"`
			Action    json.RawMessage `json:"action"`
		} `json:"item"`
	}
	if json.Unmarshal(payload, &ev) != nil || ev.Type != "response.output_item.done" {
		return "", false, false
	}
	switch ev.Item.Type {
	case "function_call":
		return ev.Item.ID, isEmptyArgsJSON(ev.Item.Arguments), true
	case "custom_tool_call":
		// custom 工具输入是裸值：空 = 缺席/null/空字符串。
		return ev.Item.ID, len(ev.Item.Input) == 0 || isEmptyArgsJSON(string(ev.Item.Input)) || string(ev.Item.Input) == `""`, true
	case "web_search_call":
		var action struct {
			Query   string        `json:"query"`
			Sources []interface{} `json:"sources"`
		}
		json.Unmarshal(ev.Item.Action, &action)
		return ev.Item.ID, action.Query == "" && len(action.Sources) == 0, true
	}
	return "", false, false
}

// rollbackUsageStats 整体回滚本流已计入全局 stats 的 usage 增量。
// parseSSEStats 的流式增量之和恰好等于各追踪器当前值，整体减掉即精确撤销。
// 用于中断流（客户端断开/上游掉线，没等到带真实拆分的 message_delta）：
// 已计入的只有 message_start 的预估 usage——Kimi 实测 start.input 含 cache_read
// 且 start.cr=0，留着会把整个上下文算成未命中输入，严重拉低聚合命中率。
func rollbackUsageStats(in, cr, cc, out int64) {
	stats.mu.Lock()
	stats.inputTokens -= in
	stats.cacheRead -= cr
	stats.cacheCreation -= cc
	stats.outputTokens -= out
	stats.mu.Unlock()
}

// parseNonStreamUsage 从非流式 JSON 响应体提取 usage 字段（分类器等请求返回非流式 JSON）。
// 支持 Anthropic 非流式（usage.input_tokens/cache_read_input_tokens/cache_creation_input_tokens/output_tokens）、
// OpenAI 非流式（usage.prompt_tokens/completion_tokens，无 cache_*，cache 两项返回 0）
// 与 Responses 非流式（透传流客户端 stream:false 时上游回整个 response 对象：
// usage.input_tokens 含缓存总量、input_tokens_details.cached_tokens 为命中部分，拆分同流式口径）。
// 提取不到返回 ok=false。
func parseNonStreamUsage(content []byte) (input, cacheRead, cacheCreation, output int64, ok bool) {
	var obj map[string]interface{}
	if json.Unmarshal(content, &obj) != nil {
		return 0, 0, 0, 0, false
	}
	u, _ := obj["usage"].(map[string]interface{})
	if u == nil {
		return 0, 0, 0, 0, false
	}
	// Responses 形状必须先判：它也有顶层 input_tokens/output_tokens，
	// 判据是 input_tokens_details 存在（Anthropic/OpenAI 的 usage 都没这个子对象）。
	if det, _ := u["input_tokens_details"].(map[string]interface{}); det != nil {
		cached := toInt64(det["cached_tokens"])
		input = toInt64(u["input_tokens"]) - cached
		if input < 0 {
			input = 0
		}
		cacheRead = cached
		cacheCreation = toInt64(u["cache_creation_input_tokens"])
		output = toInt64(u["output_tokens"])
		if input == 0 && cacheRead == 0 && cacheCreation == 0 && output == 0 {
			return 0, 0, 0, 0, false
		}
		return input, cacheRead, cacheCreation, output, true
	}
	input = toInt64(u["input_tokens"])
	cacheRead = toInt64(u["cache_read_input_tokens"])
	cacheCreation = toInt64(u["cache_creation_input_tokens"])
	output = toInt64(u["output_tokens"])
	if input == 0 && cacheRead == 0 && cacheCreation == 0 && output == 0 {
		// OpenAI 风格：prompt_tokens / completion_tokens（无 cache_*）
		input = toInt64(u["prompt_tokens"])
		output = toInt64(u["completion_tokens"])
	}
	if input == 0 && cacheRead == 0 && cacheCreation == 0 && output == 0 {
		return 0, 0, 0, 0, false
	}
	return input, cacheRead, cacheCreation, output, true
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
// Responses 透传流的错误事件是 response.failed（SSE data 里的 type 字段），一并认。
func headHasError(head []byte) bool {
	s := strings.ToLower(string(head))
	return strings.Contains(s, "event: error") ||
		strings.Contains(s, "event:error") ||
		strings.Contains(s, `"type":"error"`) ||
		strings.Contains(s, `"type": "error"`) ||
		strings.Contains(s, "response.failed") ||
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

// getBoolField 从 body 的顶层 JSON object 中读取指定字段的布尔值；未找到或非布尔返回 ok=false。
func getBoolField(body []byte, key string) (val bool, ok bool) {
	spans, ok2 := locateTopFields(body)
	if !ok2 {
		return false, false
	}
	for _, s := range spans {
		if s.name != key {
			continue
		}
		var b bool
		if err := json.Unmarshal(body[s.valStart:s.valEnd], &b); err != nil {
			return false, false
		}
		return b, true
	}
	return false, false
}

// forceStreamTrue 把请求体顶层 "stream" 字段改写为 true；字段缺失则在对象末尾追加。
// 采用流式字段定位 + 文本替换，不整体重序列化，未改字段原样保留字节（含 key 顺序与格式），
// 避免影响上游缓存命中。参数 body：原请求体；返回改写后的新 body。
func forceStreamTrue(body []byte) []byte {
	spans, ok := locateTopFields(body)
	if !ok {
		return body
	}
	for _, s := range spans {
		if s.name != "stream" {
			continue
		}
		// 已是 true 直接返回原 body，避免无谓改写。
		if bytes.Equal(bytes.TrimSpace(body[s.valStart:s.valEnd]), []byte("true")) {
			return body
		}
		out := make([]byte, 0, len(body))
		out = append(out, body[:s.valStart]...)
		out = append(out, "true"...)
		out = append(out, body[s.valEnd:]...)
		return out
	}
	// stream 字段不存在：在对象末尾 } 前追加 ,"stream":true（空对象不加逗号）。
	if len(body) == 0 || body[len(body)-1] != '}' {
		return body
	}
	sep := ","
	if len(spans) == 0 {
		sep = ""
	}
	out := make([]byte, 0, len(body)+len(sep)+len(`"stream":true}`))
	out = append(out, body[:len(body)-1]...)
	out = append(out, sep...)
	out = append(out, `"stream":true}`...)
	return out
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
	if !bytes.Contains(body, []byte(classifierSystemPrefix)) {
		return false
	}
	spans, ok := locateTopFields(body)
	if !ok {
		return false
	}
	return classifierSystemMatches(body, spans, classifierSystemPrefix)
}

// maybeRewriteClassifier 命中分类器请求时关掉 thinking，让分类快速返回。
// 用 bytes.Contains 预筛，正常请求（不含前缀子串）不做 JSON 解析，开销极低。
// 命中后用 json.Decoder 流式定位目标字段的字节位置，再做文本替换--不整体重序列化，
// 未改字段原样保留字节（含 key 顺序与格式），避免影响上游缓存命中。
func maybeRewriteClassifier(body []byte) []byte {
	c := cfg.Load()
	if !c.ClassifierThinkingDisabled {
		return body
	}
	if !bytes.Contains(body, []byte(classifierSystemPrefix)) {
		return body
	}
	spans, ok := locateTopFields(body)
	if !ok {
		return body
	}
	if !classifierSystemMatches(body, spans, classifierSystemPrefix) {
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

// extractConvID 提取 Anthropic 口请求体的会话标识，供状态页"缓存年龄"列按会话锚定。
// Claude Code 恒带顶层 metadata.user_id，且值是 JSON 字符串
// （{"device_id":"...","account_uuid":"...","session_id":"<uuid>"}），取其中 session_id；
// 裸 API 调用方塞的自由文本则原样用作标识（稳定即可关联）。无 metadata 返回空。
// 复用 locateTopFields 定位顶层 metadata 后只 unmarshal 该小对象，不整体解析 body；只读不改。
func extractConvID(body []byte) string {
	spans, ok := locateTopFields(body)
	if !ok {
		return ""
	}
	for _, s := range spans {
		if s.name != "metadata" {
			continue
		}
		var md struct {
			UserID string `json:"user_id"`
		}
		if err := json.Unmarshal(body[s.valStart:s.valEnd], &md); err != nil || md.UserID == "" {
			return ""
		}
		if strings.HasPrefix(md.UserID, "{") {
			var inner struct {
				SessionID string `json:"session_id"`
			}
			if err := json.Unmarshal([]byte(md.UserID), &inner); err == nil && inner.SessionID != "" {
				return inner.SessionID
			}
		}
		return md.UserID
	}
	return ""
}

// extractThinkMode 提取 Anthropic 格式请求体里的思考配置，供状态页「API」列思考值显示
// 实际发给上游的形态（调用点在分类器关思考改写之后；翻译口传进来的是映射后的 body）。
// 值取最短形态（词汇口径由列颜色承担，不靠前缀文字）：
// thinking.type=disabled → "关"；enabled → "开 <budget_tokens>"（无预算只显 "开"）；
// adaptive 无档 → "adaptive"，带 output_config.effort → 只显档位词（"high"/"max"…）；
// 只有 output_config.effort 没有 thinking → 同样只显档位词；未知 type 原样显示。
// 无 thinking/output_config 字段返回空（列显 -）。
// 复用 locateTopFields 只 unmarshal 相关小对象，不整体解析 body；只读不改。
func extractThinkMode(body []byte) string {
	spans, ok := locateTopFields(body)
	if !ok {
		return ""
	}
	mode := ""
	effort := ""
	for _, s := range spans {
		switch s.name {
		case "thinking":
			var t struct {
				Type   string `json:"type"`
				Budget int64  `json:"budget_tokens"`
			}
			if err := json.Unmarshal(body[s.valStart:s.valEnd], &t); err != nil {
				continue
			}
			switch t.Type {
			case "disabled":
				mode = "关"
			case "enabled":
				if t.Budget > 0 {
					mode = fmt.Sprintf("开 %d", t.Budget)
				} else {
					mode = "开"
				}
			default:
				mode = t.Type // adaptive 及未知类型原样显示
			}
		case "output_config":
			var oc struct {
				Effort string `json:"effort"`
			}
			if err := json.Unmarshal(body[s.valStart:s.valEnd], &oc); err == nil {
				effort = oc.Effort
			}
		}
	}
	// 档位词独显：adaptive 带档与仅 effort 都归到档位本身（"adaptive·high" 这类前缀是冗余的，
	// 网页用颜色区分这是哪套 API 的词汇）。
	if effort != "" && (mode == "" || mode == "adaptive") {
		return effort
	}
	return mode
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

// searchStep1Body 构造第1步搜索请求：复制原 body，model 改 sf.Model，stream 强制 false。
// 参数 body：原请求体；sf：搜索路由配置；返回新请求体或错误。
func searchStep1Body(body []byte, sf *SearchRoute) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	if sf.Model != "" {
		m["model"] = sf.Model
	}
	m["stream"] = false
	return json.Marshal(m)
}

// searchStep2Body 构造第2步摘要请求：messages=[user(原最后user), assistant(r1.content), user(摘要指令)]。
// summaryLevelConfig 按详细程度返回 step2 摘要指令与 max_tokens。
// low=简短摘要（默认），mid=中等详细（含关键事实），high=详尽（含全部数据点/引文/上下文）。
// 参数 level：配置里的 summary_level 值；query：用户原始问题（搜索意图），带进指令让总结重点关注相关内容；返回 (指令文本, max_tokens)。
func summaryLevelConfig(level, query string) (string, int) {
	// 指令带上用户原始问题（搜索意图）：总结时重点关照与问题相关的内容，但仍覆盖每条结果的关键信息，
	// 避免无脑通用摘要。query 为空时退化为通用总结。
	q := strings.TrimSpace(query)
	if q == "" {
		q = "search"
	}
	// 搜索结果常含多个日期/时间（如"今天/最新/近期/现在"），带上当前日期时间让模型能正确侧重时间相关内容。
	now := time.Now().In(time.FixedZone("UTC+8", 8*60*60)).Format("2006-01-02 15:04")
	base := "Current date and time is " + now + " (UTC+8, Beijing time).\n" +
		"The user's original question (the search intent) is: \"%s\".\n" +
		"Based on the web_search_tool_result above, %s IN ORDER. " +
		"While summarizing each result, pay special attention to and explicitly call out information that addresses or relates to the user's question above, but still cover the other key content of each result. " +
		"Format each as 'Result N: <summary>' where N starts at 1 and goes in order across all web_search_tool_result blocks. " +
		"Do not answer the original question, just summarize each result."
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "mid", "medium":
		return fmt.Sprintf(base, q, "write a medium-detail plaintext summary for EACH search result, including key facts and relevant data points"), 4096
	case "max", "full":
		// max：在 high 基础上，遇到步骤/方法/代码/公式必须完完整整逐字复述，max_tokens 加大以容纳完整代码与公式。
		return fmt.Sprintf(base, q, "write a detailed comprehensive plaintext summary for EACH search result, including all key facts, data points, direct quotes, dates, numbers, and relevant context; be thorough. When you encounter any steps, methods, code, or formulas, you MUST reproduce them completely and verbatim in full detail"), 16384
	case "high":
		return fmt.Sprintf(base, q, "write a detailed comprehensive plaintext summary for EACH search result, including all key facts, data points, direct quotes, dates, numbers, and relevant context; be thorough"), 8192
	default: // low / 空 / 未知
		return fmt.Sprintf(base, q, "write a short plaintext summary for EACH search result"), 2048
	}
}

// searchStep2Body 构造第2步摘要请求：messages=[user(原最后user), assistant(r1.content), user(摘要指令)]。
// 参数 r1：第1步响应 map；origBody：原请求体（取最后一条 user 文本）；sf：搜索路由配置；返回新请求体或错误。
func searchStep2Body(r1 map[string]any, origBody []byte, sf *SearchRoute, level string, thinking bool) ([]byte, string, error) {
	lastUser, ok := searchLastUserText(origBody)
	if !ok {
		lastUser = "search"
	}
	content, _ := r1["content"].([]any)
	instr, maxTokens := summaryLevelConfig(level, lastUser)
	m := map[string]any{
		"model":      sf.Model,
		"max_tokens": maxTokens,
		"stream":     true,
		"messages": []map[string]any{
			{"role": "user", "content": lastUser},
			{"role": "assistant", "content": content},
			{"role": "user", "content": instr},
		},
	}
	if thinking {
		m["thinking"] = map[string]any{"type": "enabled", "budget_tokens": 2048}
	}
	body, err := json.Marshal(m)
	return body, instr, err
}

// searchLastUserText 从请求体取最后一条 user 消息的文本内容。
// 参数 body：请求体；返回文本与是否找到。
func searchLastUserText(body []byte) (string, bool) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return "", false
	}
	msgs, _ := m["messages"].([]any)
	for i := len(msgs) - 1; i >= 0; i-- {
		mm, _ := msgs[i].(map[string]any)
		if mm == nil || mm["role"] != "user" {
			continue
		}
		switch c := mm["content"].(type) {
		case string:
			return c, true
		case []any:
			var sb strings.Builder
			for _, b := range c {
				bb, _ := b.(map[string]any)
				if bb != nil && bb["type"] == "text" {
					if t, ok := bb["text"].(string); ok {
						sb.WriteString(t)
					}
				}
			}
			if sb.Len() > 0 {
				return sb.String(), true
			}
		}
	}
	return "", false
}

// searchPostFlight 发送搜索/摘要内部请求，并把进度登记到 flight（状态页可见）。
// stream=false 一次性读完整响应（step1 搜索）；stream=true 流式读 SSE 并 tee 到 flight（step2 摘要）。
// 流式模式累积最小 response map（model/stop_reason/content[text]）。
// 参数 url/api：上游地址与密钥；reqBody：请求体；f：关联 flight；stream：是否流式；
// 返回：(最小 response map, 原始字节, error)。
func searchPostFlight(url, api string, reqBody []byte, f *flight, stream bool, modelHint string) (map[string]any, []byte, error) {
	req, err := http.NewRequest("POST", url+"/v1/messages", bytes.NewReader(reqBody))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Authorization", "Bearer "+api)
	tSend := time.Now()
	// 计入托盘状态灯：等上游首字节期间黄灯（waiting++），与主转发路径一致。
	stats.mu.Lock()
	stats.waiting++
	stats.mu.Unlock()
	resp, err := client.Do(req)
	stats.mu.Lock()
	stats.waiting--
	stats.mu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if f != nil {
		f.status = resp.StatusCode
		f.phase.Store(1)
		f.setStage(stageForward)
	}
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(resp.Body)
		if f != nil {
			f.appendContent(data)
			f.bytes.Add(int64(len(data)))
		}
		return nil, data, fmt.Errorf("upstream %d: %s", resp.StatusCode, string(data))
	}
	// 开始读响应（step1 全量 / step2 流式）：状态灯转绿，读完/出错恢复空闲。
	// 修复搜索摘要模式全程灰灯（此前未计 active/waiting，即便摘要模型在吐字也显示空闲）。
	stats.mu.Lock()
	stats.active++
	stats.mu.Unlock()
	defer func() {
		stats.mu.Lock()
		stats.active--
		stats.mu.Unlock()
	}()
	if !stream {
		tFirstByte := time.Now()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, nil, err
		}
		if f != nil {
			f.appendContent(data)
			f.bytes.Add(int64(len(data)))
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, data, fmt.Errorf("parse response: %w (body=%s)", err, string(data))
		}
		if f != nil {
			if in, cr, cc, out, ok := parseNonStreamUsage(data); ok {
				f.inTokens = in
				f.cacheRead = cr
				f.cacheCreation = cc
				f.outTokens = out
				stats.mu.Lock()
				stats.inputTokens += in
				stats.cacheRead += cr
				stats.cacheCreation += cc
				stats.outputTokens += out
				stats.mu.Unlock()
			}
			realModel := modelHint
			if mm, ok := m["model"].(string); ok && mm != "" {
				realModel = mm
			}
			stats.addModelUsage(realModel, f.inTokens, f.cacheRead, f.cacheCreation, f.outTokens)
			f.firstByteMs = tFirstByte.Sub(tSend).Milliseconds()
			log.Printf("[单流] #%d 搜索step1 首字 %.2fs %d+%d/%d tok",
				f.id, float64(f.firstByteMs)/1000, f.inTokens, f.cacheRead, f.outTokens)
		}
		return m, data, nil
	}
	// 流式：边读 SSE 边 tee 到 flight，同时累积最小 response map。
	var raw bytes.Buffer
	var model, stopReason string
	var textBuf strings.Builder
	var lastOutput, lastInput, lastCacheRead, lastCacheCreation int64
	var sawDeltaUsage bool // 见到过带 usage 的 message_delta（真实用量拆分已到达）
	var tFirstByte time.Time
	br := bufio.NewReader(resp.Body)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if tFirstByte.IsZero() {
				tFirstByte = time.Now()
			}
			raw.Write(line)
			if f != nil {
				f.appendContent(line)
				f.bytes.Add(int64(len(line)))
				parseSSEStats(line, &lastOutput, &lastInput, &lastCacheRead, &lastCacheCreation, &sawDeltaUsage)
			}
			t := bytes.TrimSpace(line)
			if bytes.HasPrefix(t, []byte("data:")) {
				payload := bytes.TrimSpace(t[5:])
				if len(payload) > 0 {
					var ev map[string]any
					if json.Unmarshal(payload, &ev) == nil {
						switch ev["type"] {
						case "message_start":
							if msg, ok := ev["message"].(map[string]any); ok {
								if mm, ok := msg["model"].(string); ok && model == "" {
									model = mm
								}
							}
						case "content_block_delta":
							if d, ok := ev["delta"].(map[string]any); ok {
								if d["type"] == "text_delta" {
									if s, ok := d["text"].(string); ok {
										textBuf.WriteString(s)
									}
								}
							}
						case "message_delta":
							if d, ok := ev["delta"].(map[string]any); ok {
								if s, ok := d["stop_reason"].(string); ok {
									stopReason = s
								}
							}
						}
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	if f != nil {
		if sawDeltaUsage {
			f.inTokens = lastInput
			f.cacheRead = lastCacheRead
			f.cacheCreation = lastCacheCreation
			f.outTokens = lastOutput
		} else {
			// 中断流：回滚已计入的 message_start 预估 usage，聚合只统计完整响应
			rollbackUsageStats(lastInput, lastCacheRead, lastCacheCreation, lastOutput)
		}
		tEnd := time.Now()
		if !tFirstByte.IsZero() {
			f.firstByteMs = tFirstByte.Sub(tSend).Milliseconds()
			streamMs := tEnd.Sub(tFirstByte).Milliseconds()
			if streamMs > 0 {
				f.tps = float64(lastOutput) / (float64(streamMs) / 1000.0)
			}
			log.Printf("[单流] #%d 搜索step2 首字 %.2fs 流式 %.2fs %d tok %.1f tok/s",
				f.id, float64(f.firstByteMs)/1000, float64(streamMs)/1000, lastOutput, f.tps)
		}
		realModel := model
		if realModel == "" {
			realModel = modelHint
		}
		stats.addModelUsage(realModel, f.inTokens, f.cacheRead, f.cacheCreation, f.outTokens)
	}
	m := map[string]any{
		"model":       model,
		"stop_reason": stopReason,
		"content":     []any{map[string]any{"type": "text", "text": textBuf.String()}},
	}
	return m, raw.Bytes(), nil
}

// searchExtractResults 从第1步响应提取 server_tool_use id 和所有 web_search_tool_result 的 result 条目。
// 参数 r1：第1步响应 map；返回 (toolUseID, results, error)。
func searchExtractResults(r1 map[string]any) (string, []map[string]any, error) {
	content, _ := r1["content"].([]any)
	toolUseID := ""
	var results []map[string]any
	for _, b := range content {
		bb, _ := b.(map[string]any)
		if bb == nil {
			continue
		}
		t, _ := bb["type"].(string)
		if t == "server_tool_use" && toolUseID == "" {
			if id, ok := bb["id"].(string); ok {
				toolUseID = id
			}
		}
		if t == "web_search_tool_result" {
			items, _ := bb["content"].([]any)
			for _, it := range items {
				im, _ := it.(map[string]any)
				if im != nil {
					results = append(results, im)
				}
			}
		}
	}
	if toolUseID == "" {
		toolUseID = "srvtoolu_proxy_search"
	}
	return toolUseID, results, nil
}

// searchParseSummaries 从第2步响应文本按 "Result N:" 拆分摘要，返回 n 条。
// 参数 r2：第2步响应 map；n：期望条数；返回按序的摘要切片（未匹配位置为空串）。
func searchParseSummaries(r2 map[string]any, n int) []string {
	out := make([]string, n)
	text := ""
	if content, ok := r2["content"].([]any); ok {
		for _, b := range content {
			bb, _ := b.(map[string]any)
			if bb != nil && bb["type"] == "text" {
				if t, ok := bb["text"].(string); ok {
					text += t
				}
			}
		}
	}
	re := regexp.MustCompile(`(?im)^\s*\**\s*Result\s*(\d+)\s*[:\.\)]\s*\**\s*`)
	idx := re.FindAllStringSubmatchIndex(text, -1)
	if len(idx) == 0 {
		if n > 0 {
			out[0] = strings.TrimSpace(text)
		}
		return out
	}
	for i, m := range idx {
		numStart, numEnd := m[2], m[3]
		num, _ := strconv.Atoi(text[numStart:numEnd])
		start := m[1]
		var end int
		if i+1 < len(idx) {
			end = idx[i+1][0]
		} else {
			end = len(text)
		}
		s := strings.TrimSpace(text[start:end])
		if num >= 1 && num <= n {
			out[num-1] = s
		}
	}
	return out
}

// logSearchResponse 打印搜索响应的 model/stop_reason/block 类型/文本预览。
// 参数 fid：流 ID；tag：阶段标签；r：响应 map。
func logSearchResponse(fid uint64, tag string, r map[string]any) {
	if e, ok := r["error"]; ok {
		log.Printf("[搜索摘要debug] #%d %s 响应含 error=%v", fid, tag, e)
	}
	stop, _ := r["stop_reason"].(string)
	model, _ := r["model"].(string)
	var blockTypes []string
	var text string
	if content, ok := r["content"].([]any); ok {
		for _, b := range content {
			bb, _ := b.(map[string]any)
			if bb == nil {
				continue
			}
			t, _ := bb["type"].(string)
			blockTypes = append(blockTypes, t)
			if t == "text" {
				if s, ok := bb["text"].(string); ok {
					text += s
				}
			}
		}
	}
	log.Printf("[搜索摘要debug] #%d %s model=%s stop=%s blocks=%v", fid, tag, model, stop, blockTypes)
	if text != "" {
		preview := text
		if len(preview) > 500 {
			preview = preview[:500] + "...(截断)"
		}
		log.Printf("[搜索摘要debug] #%d %s 文本:\n%s", fid, tag, preview)
	}
}

// searchDebugWrite 把一段 raw 写入搜索调试目录/#<fid>_<tag>，目录为空则跳过。
// 参数 fid：流 ID；tag：文件标签；data：原始字节。
func searchDebugWrite(fid uint64, tag string, data []byte) {
	dir := debugSearchDirOverride
	if dir == "" {
		if c := cfg.Load(); c != nil {
			dir = c.SearchDebugDir
		}
	}
	if dir == "" || len(data) == 0 {
		return
	}
	os.MkdirAll(dir, 0755)
	fname := fmt.Sprintf("%s%c#%d_%s", dir, os.PathSeparator, fid, tag)
	if err := os.WriteFile(fname, data, 0644); err != nil {
		log.Printf("[搜索摘要debug] #%d 写 %s 失败: %v", fid, tag, err)
	}
}

// searchDebugAppend 把一段 raw 追加写入搜索调试目录/#<fid>_<tag>。
// 参数 fid：流 ID；tag：文件标签；data：原始字节。
func searchDebugAppend(fid uint64, tag string, data []byte) {
	dir := debugSearchDirOverride
	if dir == "" {
		if c := cfg.Load(); c != nil {
			dir = c.SearchDebugDir
		}
	}
	if dir == "" || len(data) == 0 {
		return
	}
	os.MkdirAll(dir, 0755)
	fname := fmt.Sprintf("%s%c#%d_%s", dir, os.PathSeparator, fid, tag)
	f, err := os.OpenFile(fname, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(data)
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

// isReservedRoutePattern 报告 pattern 是否全字撞保留名（撞名的路由不生效）："Fallback" 是
// * 兜底路由、"fast_route" 是 fast 通道在 Codex 菜单里的条目名——Codex 脚本会把这些名字当
// model 发来（fast_route 由翻译层注入 speed:"fast" 走 fast 分支），撞名路由会截走对应流量。
func isReservedRoutePattern(p string) bool {
	return p == "Fallback" || p == "fast_route"
}

// matchPassthroughResponsesRoute 预检 Responses 口请求是否该走原生透传：model 按路由表
// 有序首个非保留名匹配（与 handler 路由循环同规则），且命中路由配了 url_response_api。
// 只供 responsesHandler 决定"翻译还是透传"；真正的上游切换在 handler 路由循环里再做一次。
func matchPassthroughResponsesRoute(c *Config, model string) *RouteRule {
	if model == "" {
		return nil
	}
	for i := range c.Routes {
		if isReservedRoutePattern(c.Routes[i].Pattern) {
			continue
		}
		if matchModel(c.Routes[i].Pattern, model) && c.Routes[i].URLResponseAPI != "" {
			return &c.Routes[i]
		}
	}
	return nil
}

// responsesAPIPath 计算原生透传的拼接路径：url_response_api 允许填 base（…/coding）、
// 带 /v1 或完整 …/v1/responses，只补差额不双拼。配合 handler 里 upstream+upPath 使用。
func responsesAPIPath(base string) string {
	b := strings.TrimRight(base, "/")
	if strings.HasSuffix(b, "/v1/responses") {
		return ""
	}
	if strings.HasSuffix(b, "/v1") {
		return "/responses"
	}
	return "/v1/responses"
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

// hasCatchAllRoute 报告 routes 里是否有 pattern:"*" 兜底规则（命中一切模型）。
// 有它时顶层 upstream 允许留空——所有带 model 的请求都会被兜底路由接管，默认 upstream 用不到。
func hasCatchAllRoute(routes []RouteRule) bool {
	for i := range routes {
		if routes[i].Pattern == "*" {
			return true
		}
	}
	return false
}

// writeSSEPing 向客户端写一个 Anthropic 标准 SSE ping 事件并 flush，用于 429 重试期间保活。
// ping 事件被 Claude Code 忽略（Anthropic 官方流本身也穿插 ping），不产生消息内容。
// Responses 透传流改写 SSE 注释行（": ping"）——注释行所有 SSE 客户端都忽略，
// 而 Anthropic 形状的 ping JSON 塞给原生 Responses 客户端可能解析告警。
// 参数 w：响应写入器；flusher：若非 nil 则写后 flush；f：本次 flight（判协议形状，可为 nil）。
func writeSSEPing(w http.ResponseWriter, flusher http.Flusher, f *flight) {
	if f != nil && f.responsesRaw() {
		w.Write([]byte(": ping\n\n"))
	} else {
		w.Write([]byte("event: ping\ndata: {\"type\":\"ping\"}\n\n"))
	}
	if flusher != nil {
		flusher.Flush()
	}
}

// writeSSEError 在已发 200 头后向上游失败兜底：写一个 Anthropic SSE error 事件并 flush。
// 已发 200 头后无法再改状态码透传 429，只能用 SSE error 让客户端识别错误（重试用尽时走这里）。
// Responses 透传流改写 response.failed 事件（原生 Responses 客户端认这个形状）。
// 参数 w：响应写入器；flusher：若非 nil 则写后 flush；msg：错误描述；f：本次 flight（tee 一份供网页回看）。
func writeSSEError(w http.ResponseWriter, flusher http.Flusher, msg string, f *flight) {
	var buf bytes.Buffer
	b, _ := json.Marshal(msg) // json 编码 message，避免特殊字符破坏 SSE
	if f != nil && f.responsesRaw() {
		buf.WriteString("event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_p429\",\"object\":\"response\",\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":")
		buf.Write(b)
		buf.WriteString("}}}\n\n")
	} else {
		buf.WriteString("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":")
		buf.Write(b)
		buf.WriteString("}}\n\n")
	}
	data := buf.Bytes()
	w.Write(data)
	if flusher != nil {
		flusher.Flush()
	}
	if f != nil {
		f.gaveUp = true       // 完成流状态码列据此显 [重试尽]（区别于 status=0 的其他形态）
		f.appendContent(data) // tee 一份，供网页回看（重试用尽时的 error 也记录）
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
	writeSSEPing(w, flusher, f)
	log.Printf("[保活] #%d 重试中(%s)，开启 SSE ping 保活", f.id, reason)
	return flusher
}

// sleepWithPing 在 backoff 等待期间定期发 ping 保活，且可被客户端断开（ctx 取消）中断。
// 替换原 time.Sleep(wait)：后者不可中断，客户端超时断开后代理仍傻睡+重试、白烧上游配额。
// 参数 ctx：客户端连接 context；w/flusher：发 ping 用；wait：backoff 时长；interval：ping 间隔；f：flight。
// flusher 为 nil 时静默等待不发 ping：convertAlltoStream 下客户端期待非流式响应，
// 提前写任何 SSE 字节都会污染响应，只能干等（客户端连接保持，只是无数据）。
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
			if flusher != nil {
				writeSSEPing(w, flusher, f)
			}
		case <-timer.C:
			return true
		}
	}
}

// newSearchSubFlight 为搜索摘要的 step1/step2 建子 flight 并注册到状态页（在途流可见）。
// 参数 label：显示在 model 列的标签；返回新建并已注册的 flight（调用方负责 unregister+addFinished）。
func newSearchSubFlight(label string) *flight {
	f := &flight{
		id:        flights.nextID.Add(1),
		start:     time.Now(),
		origModel: label,
	}
	f.routeReason.Store(routeSearch)
	f.stageStart.Store(f.start.UnixNano())   // 灯色计时起点=流建立
	f.attemptStart.Store(f.start.UnixNano()) // 子 flight 建立即待发上游，[尝试N] 计时同起点
	f.setStage(stageAttempt)
	flights.register(f)
	return f
}

// searchAndRespond 执行摘要模式：step1 搜索（非流式，拿 web_search 结果）+ step2 摘要（流式，拿详细摘要），
// 构建 Kimi 格式 SSE 响应（server_tool_use + web_search_tool_result + 摘要文本）流式写回客户端。
// 不调用主 ark。成功返回 true（handler 应直接 return）；失败返回 false（handler 降级走原转发）。
// 参数 w：客户端响应；body：原请求体；sf：搜索路由配置；f：当前 flight；origModel：原模型名（回填 model 字段）。
func searchAndRespond(w http.ResponseWriter, body []byte, sf *SearchRoute, f *flight, origModel string) bool {
	fid := f.id
	f.routeReason.Store(routeSearch)
	f.setStage(stageForward)
	f.phase.Store(1)

	// step1：搜索（非流式），拿 server_tool_use + web_search_tool_result。
	step1Body, err := searchStep1Body(body, sf)
	if err != nil {
		log.Printf("[搜索摘要] #%d step1 构造失败: %v", fid, err)
		return false
	}
	searchDebugWrite(fid, "step1_req.json", step1Body)
	step1F := newSearchSubFlight("搜索step1·" + sf.Model)
	step1F.think = extractThinkMode(step1Body) // 状态页「API」列思考值 = 实际发给上游的配置
	r1, r1Raw, err := searchPostFlight(sf.URL, sf.API, step1Body, step1F, false, sf.Model)
	flights.unregister(step1F.id)
	addFinished(step1F)
	if err != nil {
		log.Printf("[搜索摘要] #%d step1 失败: %v", fid, err)
		return false
	}
	searchDebugWrite(fid, "step1_resp.json", r1Raw)
	logSearchResponse(fid, "step1", r1)
	toolUseID, results, err := searchExtractResults(r1)
	if err != nil || len(results) == 0 {
		log.Printf("[搜索摘要] #%d step1 无搜索结果 (results=%d err=%v)", fid, len(results), err)
		return false
	}

	// step1 成功：立即发 200 + SSE 头并起 ping 保活，避免 step2 摘要慢（尤其 max+thinking）时
	// 客户端长时间无数据超时。step1 失败仍 return false 降级走兜底；发头后失败只能发 SSE error。
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	f.status = http.StatusOK
	flusher, canFlush := w.(http.Flusher)
	pingInterval := 5 * time.Second
	if cc := cfg.Load(); cc != nil && cc.PingIntervalSec > 0 {
		pingInterval = time.Duration(cc.PingIntervalSec * float64(time.Second))
	}
	pingStop := make(chan struct{})
	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		// 合成流是 Anthropic 形状（本函数 f.translated 顶多是 "responses" 翻译，不会是透传），
		// 传 f 后 writeSSEPing 自动走 Anthropic ping 分支。
		writeSSEPing(w, flusher, f) // 立即发首个 ping，让客户端尽早确认连接活跃
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-pingStop:
				return
			case <-ticker.C:
				writeSSEPing(w, flusher, f)
			}
		}
	}()
	pingStopped := false
	stopPing := func() {
		if pingStopped {
			return
		}
		pingStopped = true
		close(pingStop)
		<-pingDone // 等 goroutine 退出，避免与后续写 w 并发
	}
	defer stopPing() // 兜底：任何 return 前确保 ping 已停

	// step2：分级摘要尝试。step1 结果已缓存，即使摘要全失败也可直接返回搜索结果（仅少描述）。
	// 尝试1：配置 level+thinking；失败则尝试2：mid+关thinking；再失败则 summaryText 留空（返回 step1 结果）。
	summaryText := ""
	for _, att := range []struct {
		level    string
		thinking bool
		label    string
	}{
		{sf.SummaryLevel, sf.SummaryThinking, "总结step2"},
		{"mid", false, "总结step2降级mid"},
	} {
		step2Body, instr, err := searchStep2Body(r1, body, sf, att.level, att.thinking)
		if err != nil {
			log.Printf("[搜索摘要] #%d %s 构造失败: %v", fid, att.label, err)
			continue
		}
		searchDebugWrite(fid, att.label+"_req.json", step2Body)
		step2F := newSearchSubFlight(att.label + "·" + sf.Model)
		step2F.searchPrompt = instr
		step2F.think = extractThinkMode(step2Body) // 状态页「API」列思考值 = 实际发给上游的配置
		r2, r2Raw, err := searchPostFlight(sf.URL, sf.API, step2Body, step2F, true, sf.Model)
		flights.unregister(step2F.id)
		addFinished(step2F)
		if err != nil {
			log.Printf("[搜索摘要] #%d %s 失败: %v", fid, att.label, err)
			continue
		}
		searchDebugWrite(fid, att.label+"_resp.sse", r2Raw)
		logSearchResponse(fid, att.label, r2)
		if content, ok := r2["content"].([]any); ok {
			for _, b := range content {
				bb, _ := b.(map[string]any)
				if bb != nil && bb["type"] == "text" {
					if t, ok := bb["text"].(string); ok {
						summaryText += t
					}
				}
			}
		}
		if summaryText != "" {
			break
		}
		log.Printf("[搜索摘要] #%d %s 摘要为空，尝试下一档", fid, att.label)
	}

	// 发正式内容：停 ping，独占写 w。摘要为空则只返回 step1 搜索结果（无 text 块）。
	stopPing()
	if summaryText == "" {
		log.Printf("[搜索摘要] #%d 摘要全失败，返回 step1 搜索结果（无摘要文本）", fid)
	}
	msgID := fmt.Sprintf("msg_proxy_%d", fid)
	// writeSSE 写一个 SSE 事件并 flush，同时 tee 到 flight + 全局流量统计。
	writeSSE := func(event string, data map[string]any) {
		payload, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload)
		line := make([]byte, 0, len(event)+len(payload)+12)
		line = append(line, []byte("event: ")...)
		line = append(line, event...)
		line = append(line, []byte("\ndata: ")...)
		line = append(line, payload...)
		line = append(line, '\n', '\n')
		f.appendContent(line)
		f.bytes.Add(int64(len(line)))
		stats.bytesForward.Add(int64(len(line)))
		if canFlush {
			flusher.Flush()
		}
	}
	// 1) message_start
	writeSSE("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         origModel,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 1, "output_tokens": 1},
		},
	})
	// 2) server_tool_use（index 0）
	f.noteToolCall("web_search") // 合成流由我们自构、不经 writeAndCount 解析，直接计数
	writeSSE("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": 0,
		"content_block": map[string]any{
			"type":  "server_tool_use",
			"id":    toolUseID,
			"name":  "web_search",
			"input": map[string]any{},
		},
	})
	writeSSE("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	// 3) web_search_tool_result（index 1，原始 results 回填，下拉菜单显示 title/url）
	writeSSE("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": 1,
		"content_block": map[string]any{
			"type":        "web_search_tool_result",
			"tool_use_id": toolUseID,
			"content":     results,
		},
	})
	writeSSE("content_block_stop", map[string]any{"type": "content_block_stop", "index": 1})
	// 4) 摘要文本（index 2）：仅当摘要成功才发；摘要全失败时跳过，客户端只收到搜索结果。
	if summaryText != "" {
		writeSSE("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         2,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
		for _, ch := range splitSummaryChunks(summaryText, 200) {
			writeSSE("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": 2,
				"delta": map[string]any{"type": "text_delta", "text": ch},
			})
		}
		writeSSE("content_block_stop", map[string]any{"type": "content_block_stop", "index": 2})
	}
	// 5) message_delta + message_stop 收尾
	writeSSE("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": estimateTokens(summaryText)},
	})
	writeSSE("message_stop", map[string]any{"type": "message_stop"})
	f.delivered.Store(true) // 合成流已完整写完
	log.Printf("[搜索摘要] #%d 完成：results=%d summary=%d字节", fid, len(results), len(summaryText))
	return true
}

// splitSummaryChunks 把文本按 maxLen 切成多段（尽量在换行/句号边界），用于流式输出 text_delta。
// 参数 text：原文；maxLen：每段上限字节；返回切段切片。
func splitSummaryChunks(text string, maxLen int) []string {
	if len(text) <= maxLen {
		return []string{text}
	}
	var out []string
	for len(text) > maxLen {
		cut := maxLen
		for i := maxLen; i > maxLen/2 && i < len(text); i-- {
			if text[i] == '\n' {
				cut = i + 1
				break
			}
		}
		out = append(out, text[:cut])
		text = text[cut:]
	}
	if text != "" {
		out = append(out, text)
	}
	return out
}

// estimateTokens 粗估 token 数（中英折中按 3 字符/token）。
// 参数 text：原文；返回估算 token 数。
func estimateTokens(text string) int {
	return len(text)/3 + 1
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
	f.setStage(stageForward) // 网页「状态」列改为显示状态码

	// 计入活跃流，defer 退出时减一。
	stats.mu.Lock()
	stats.active++
	stats.mu.Unlock()

	var lastOutput int64                 // 本流 output_tokens 累积值，用于算增量
	var lastInput int64                  // 本流 input_tokens 累积值
	var lastCacheRead int64              // 本流 cache_read 累积值
	var lastCacheCreation int64          // 本流 cache_creation 累积值
	var sawDeltaUsage bool               // 见到过带 usage 的 message_delta（真实用量拆分已到达）
	interrupted := false                 // 本流是否中途断开（usage 已回滚，不计入聚合）
	openTools := map[int]*openToolCall{} // Anthropic：块 index → 进行中的工具调用（*0 空参判定，跨 chunk 存活）
	respTools := map[string]string{}     // Responses 透传：item_id → 工具名（与 output_item.done 对齐空参判定）
	defer func() {
		counted := false
		if lastInput == 0 && lastCacheRead == 0 && lastCacheCreation == 0 && lastOutput == 0 {
			// 非流式 JSON 响应：SSE 解析不到 usage，从 body 提取（分类器等请求）
			if in, cr, cc, out, ok := parseNonStreamUsage(f.snapshotContent()); ok {
				f.inTokens = in
				f.cacheRead = cr
				f.cacheCreation = cc
				f.outTokens = out
				stats.mu.Lock()
				stats.inputTokens += in
				stats.cacheRead += cr
				stats.cacheCreation += cc
				stats.outputTokens += out
				stats.mu.Unlock()
				counted = true
			}
		} else if sawDeltaUsage {
			// 流正常走完（拿到真实拆分）：计入聚合。
			f.inTokens = lastInput
			f.cacheRead = lastCacheRead
			f.cacheCreation = lastCacheCreation
			f.outTokens = lastOutput
			counted = true
		} else {
			// 流中断：已计入全局的只是 message_start 的预估 usage，整体回滚，
			// 聚合只统计完整响应（与 Claude Code 一致）。f.* 清零，单流命中率显示 "-"。
			rollbackUsageStats(lastInput, lastCacheRead, lastCacheCreation, lastOutput)
			f.inTokens, f.cacheRead, f.cacheCreation, f.outTokens = 0, 0, 0, 0
			interrupted = true
		}
		// 真实上游模型：优先用上游响应实际返回的 model 名（上游可能再把请求路由到别的模型）；
		// 上游未返回 model 时退回路由目标 targetModel，透传则用 origModel。
		realModel := f.upstreamModel
		if realModel == "" {
			realModel = f.targetModel
		}
		if realModel == "" {
			realModel = f.origModel
		}
		if counted {
			stats.addModelUsage(realModel, f.inTokens, f.cacheRead, f.cacheCreation, f.outTokens)
		}
		if interrupted {
			log.Printf("[中断] #%d response状态 %d 大小 %s（流未完成，usage 不计入聚合）", f.id, f.status, humanBytes(f.bytes.Load()))
		} else {
			log.Printf("[完成] #%d response状态 %d 大小 %s 缓存命中 %s", f.id, f.status, humanBytes(f.bytes.Load()), cacheHitRate(f.cacheRead, f.inTokens, f.cacheCreation))
		}
		stats.mu.Lock()
		stats.active--
		stats.mu.Unlock()
	}()
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
				if f.upstreamModel == "" && actualModel != "" {
					f.upstreamModel = actualModel
				}
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
		f.appendContent(data)     // tee 一份供网页在途流查看（w.Write 之后，不影响透传）
		if f.searchDebug {
			searchDebugAppend(f.id, "main_resp.sse", data) // 搜索摘要降级时记录主力响应 raw
		}
		responsesRaw := f.responsesRaw() // 循环外算一次：透传流按 Responses 口径解析
		for _, line := range bytes.Split(data, []byte("\n")) {
			if responsesRaw {
				parseResponsesStreamStats(line, &lastOutput, &lastInput, &lastCacheRead, &lastCacheCreation, &sawDeltaUsage)
				if itemID, name, ok := parseResponsesToolStart(line); ok {
					f.noteToolCall(name) // 工具调用计数：完成流列表显示 [exec_command*1] 等
					respTools[itemID] = name
				}
				if itemID, empty, ok := parseResponsesToolDone(line); ok {
					if name := respTools[itemID]; name != "" {
						if empty {
							f.noteToolCallEmpty(name) // 参数结构体为空：单次调用标签显 *0
						}
						delete(respTools, itemID)
					}
				}
				// response.completed/failed/incomplete = Responses 协议终局标记（对应 Anthropic 的
				// message_stop，语义同下：收完即标记 delivered，不等干净 EOF，防 Codex 快断连误记 499）。
				if bytes.HasPrefix(bytes.TrimSpace(line), []byte("data:")) &&
					(bytes.Contains(line, []byte("response.completed")) || bytes.Contains(line, []byte("response.failed")) || bytes.Contains(line, []byte("response.incomplete"))) {
					f.delivered.Store(true)
				}
				continue
			}
			parseSSEStats(line, &lastOutput, &lastInput, &lastCacheRead, &lastCacheCreation, &sawDeltaUsage)
			if ts, ok := parseToolCallStart(line); ok {
				f.noteToolCall(ts.name) // 工具调用计数：完成流列表显示 [Read*1][Edit*3]
				tr := &openToolCall{name: ts.name}
				if !isEmptyArgsJSON(string(ts.input)) {
					tr.hasArgs = true // server_tool_use 的 start 即完整块：input 非空直接定论
				}
				openTools[ts.index] = tr
			}
			if idx, partial, ok := parseToolArgsDelta(line); ok {
				if tr := openTools[idx]; tr != nil {
					tr.notePartial(partial) // 累积参数碎片（≤64B），stop 时判定空参
				}
			}
			if idx, ok := parseToolBlockStop(line); ok {
				if tr := openTools[idx]; tr != nil {
					if tr.argsEmpty() {
						f.noteToolCallEmpty(tr.name)
					}
					delete(openTools, idx)
				}
			}
			// message_stop = 语义上流已送完，立刻标记 delivered：客户端常在收完后马上断连
			// （Codex 收完 response.completed 即关连接），ctx 一取消上游 EOF 就读不到，
			// 等干净 EOF 再标记会来不及（翻译口正常流曾被误记 499）。
			if bytes.HasPrefix(bytes.TrimSpace(line), []byte("data:")) && bytes.Contains(line, []byte("message_stop")) {
				f.delivered.Store(true)
			}
		}
	}

	writeAndCount(head)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			writeAndCount(line)
		}
		if err != nil {
			if err == io.EOF {
				// 上游流干净读完=最后一段也已写给下游，标记完整送达
				// （收尾 defer 据此区分"收完后断连"与"中途断连"，前者不改记 499）。
				f.delivered.Store(true)
			}
			break
		}
	}
	// 流结束仍未关闭（缺 content_block_stop，如流被截断）的工具调用按已收内容定论空参，
	// best-effort 补 *0；正常结束的流此处 openTools 已空。
	for _, tr := range openTools {
		if tr.argsEmpty() {
			f.noteToolCallEmpty(tr.name)
		}
	}
	return lastOutput
}

// collectStreamToJSON 把上游返回的 SSE 流完整收完，原样重建 Anthropic 非流式 message JSON，
// 收完后一次性写给客户端。convertAlltoStream 下请求已被改为流式发上游，但客户端仍期待
// 非流式 JSON，故必须先把流缓存完整（网页在途流仍实时可见：每段字节 tee 到 flight）。
// 重建规则（流式是事件流、非流式是完整块，结构不同需重拼）：
//   - message_start 的 message 对象作为骨架原样保留（id/type/role/model 等）；
//   - 每个 content_block_start 的块骨架按 index 保留--web_search_tool_result（含
//     encrypted_content）、server_tool_use 等整块内联的字段原样不动；
//   - delta 事件只往已知字段追加：text_delta->text、thinking_delta->thinking、
//     signature_delta->signature、input_json_delta->input（tool_use 参数，收完解析成对象）；
//   - usage 合并：message_start 打底、message_delta 覆盖（后值语义，与统计逻辑一致）。
// 参数 w：客户端响应写入器；resp/head/br：上游响应（head 为 peekHead 已读的字节，需先处理）；
// f：本次 flight。返回 (是否完整收到 message_stop, 输出 token 数)。
// 流不完整时未向客户端写任何字节，调用方可安全整体重试（重发流式请求、重收一次流）。
func collectStreamToJSON(w http.ResponseWriter, resp *http.Response, head []byte, br *bufio.Reader, f *flight) (bool, int64) {
	defer resp.Body.Close()
	f.phase.Store(1)
	f.status = resp.StatusCode
	f.setStage(stageForward)
	stats.mu.Lock()
	stats.active++
	stats.mu.Unlock()
	defer func() {
		stats.mu.Lock()
		stats.active--
		stats.mu.Unlock()
	}()

	var msgObj map[string]interface{}          // message_start 的 message 骨架
	var usageObj map[string]interface{}        // usage 合并结果：start 打底，delta 覆盖
	blocks := map[int]map[string]interface{}{} // index -> 内容块（start 骨架 + delta 累积）
	var lastOutput, lastInput, lastCacheRead, lastCacheCreation int64
	complete := false

	// strVal 取 map 里的字符串字段（不存在或非字符串返回空串）。
	strVal := func(m map[string]interface{}, k string) string {
		if m == nil {
			return ""
		}
		if v, ok := m[k].(string); ok {
			return v
		}
		return ""
	}

	// handleChunk 处理一段原始 SSE 字节（可能含多行）：逐行 tee 到网页与统计，并解析事件结构。
	handleChunk := func(chunk []byte) {
		for _, line := range bytes.SplitAfter(chunk, []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			stats.bytesForward.Add(int64(len(line)))
			f.bytes.Add(int64(len(line)))
			f.appendContent(line)
			if f.searchDebug {
				searchDebugAppend(f.id, "main_resp.sse", line)
			}
			parseSSEStats(line, &lastOutput, &lastInput, &lastCacheRead, &lastCacheCreation, nil)

			s := bytes.TrimRight(line, "\r\n")
			if !bytes.HasPrefix(s, []byte("data:")) {
				continue
			}
			payload := bytes.TrimSpace(bytes.TrimPrefix(s, []byte("data:")))
			if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
				continue
			}
			var ev map[string]interface{}
			if json.Unmarshal(payload, &ev) != nil {
				continue
			}
			switch ev["type"] {
			case "message_start":
				if msg, ok := ev["message"].(map[string]interface{}); ok {
					msgObj = msg
					if u, ok := msg["usage"].(map[string]interface{}); ok {
						usageObj = make(map[string]interface{}, len(u))
						for k, v := range u {
							usageObj[k] = v
						}
					}
					if m, ok := msg["model"].(string); ok && m != "" && f.upstreamModel == "" {
						f.upstreamModel = m
					}
				}
			case "content_block_start":
				if cb, ok := ev["content_block"].(map[string]interface{}); ok {
					blocks[int(toInt64(ev["index"]))] = cb
					if t, _ := cb["type"].(string); t == "tool_use" || t == "server_tool_use" {
						f.noteToolCall(strVal(cb, "name")) // 工具调用计数（组装 JSON 路径不经 writeAndCount）
					}
				}
			case "content_block_delta":
				b := blocks[int(toInt64(ev["index"]))]
				d, _ := ev["delta"].(map[string]interface{})
				if b == nil || d == nil {
					continue
				}
				switch d["type"] {
				case "text_delta":
					b["text"] = strVal(b, "text") + strVal(d, "text")
				case "thinking_delta":
					b["thinking"] = strVal(b, "thinking") + strVal(d, "thinking")
				case "signature_delta":
					b["signature"] = strVal(b, "signature") + strVal(d, "signature")
				case "input_json_delta":
					b["input"] = strVal(b, "input") + strVal(d, "partial_json")
				}
			case "message_delta":
				if d, ok := ev["delta"].(map[string]interface{}); ok && msgObj != nil {
					if sr, ok := d["stop_reason"].(string); ok {
						msgObj["stop_reason"] = sr
					}
					if ss, ok := d["stop_sequence"]; ok {
						msgObj["stop_sequence"] = ss
					}
				}
				if u, ok := ev["usage"].(map[string]interface{}); ok {
					if usageObj == nil {
						usageObj = make(map[string]interface{}, len(u))
					}
					for k, v := range u {
						usageObj[k] = v
					}
				}
			case "message_stop":
				complete = true
			}
		}
	}

	handleChunk(head)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			handleChunk(line)
		}
		if err != nil {
			break
		}
	}
	if !complete {
		// 未完整收完的流不计入聚合（只有 message_start 的预估 usage），回滚后供整体重试
		rollbackUsageStats(lastInput, lastCacheRead, lastCacheCreation, lastOutput)
		log.Printf("[流式化] #%d 上游流未完整收完（未见 message_stop），不写客户端，供整体重试", f.id)
		return false, lastOutput
	}

	// 重建非流式 message JSON。model 回写客户端原始 model（路由改写对客户端无感）。
	if msgObj == nil {
		// 缺 message_start 的异常流：搭最小骨架，保证回传结构完整。
		msgObj = map[string]interface{}{"type": "message", "role": "assistant"}
	}

	// 块按 index 排序回填 content；input 为字符串（input_json_delta 拼出的）尝试解析成对象。
	indexes := make([]int, 0, len(blocks))
	for i := range blocks {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	content := make([]interface{}, 0, len(indexes))
	for _, i := range indexes {
		b := blocks[i]
		if s, ok := b["input"].(string); ok && s != "" {
			var obj interface{}
			if json.Unmarshal([]byte(s), &obj) == nil {
				b["input"] = obj
			}
		}
		// 工具块在此刻 input 已终局：判空补 *0 标签（start 时只计数，空参看终态）。
		if t, _ := b["type"].(string); t == "tool_use" || t == "server_tool_use" {
			empty := false
			switch inp := b["input"].(type) {
			case nil:
				empty = true
			case string:
				empty = isEmptyArgsJSON(inp)
			case map[string]interface{}:
				empty = len(inp) == 0
			}
			if empty {
				f.noteToolCallEmpty(strVal(b, "name"))
			}
		}
		content = append(content, b)
	}
	msgObj["content"] = content
	if usageObj != nil {
		msgObj["usage"] = usageObj
	}
	// model 回写客户端原始 model（路由改写对客户端无感）。
	if f.targetModel != "" && f.targetModel != f.origModel {
		msgObj["model"] = f.origModel
		if f.upstreamModel != "" && !f.modelLogged.Swap(true) {
			log.Printf("[改写] #%d 响应流 model 回改 %s -> %s", f.id, f.upstreamModel, f.origModel)
		}
	}
	out, err := json.Marshal(msgObj)
	if err != nil {
		return false, lastOutput
	}

	// 一次性写完整响应（客户端期待非流式 JSON，收完前绝不能写任何字节）。
	f.inTokens = lastInput
	f.cacheRead = lastCacheRead
	f.cacheCreation = lastCacheCreation
	f.outTokens = lastOutput
	stats.addModelUsage(f.realModel(), f.inTokens, f.cacheRead, f.cacheCreation, f.outTokens)
	log.Printf("[完成] #%d response状态 %d 大小 %s 缓存命中 %s", f.id, f.status, humanBytes(f.bytes.Load()), cacheHitRate(f.cacheRead, f.inTokens, f.cacheCreation))

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(out)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	stats.bytesForward.Add(int64(len(out)))
	f.bytes.Add(int64(len(out)))
	f.appendContent(out) // tee 重建后的非流式 JSON，供网页对照回传形态
	// 组装 JSON 已一次性写完=完整送达（499 改记只看没送完的流）
	f.delivered.Store(true)
	log.Printf("[流式化] #%d 流已收完，非流式 JSON 一次性返回客户端 (%d 输出 tokens)", f.id, lastOutput)
	return true, lastOutput
}

// handler 是所有请求的入口：缓冲请求体 → 命中分类器则关 thinking →
// 带重试地转发 → 流式透传响应。
// 重试三种情况：
//
//	A. 上游 HTTP 状态码本身就是 429/5xx；
//	B. 状态码 200，但错误藏在 SSE 响应体里（event:error / rate_limit 等）；
//	C. 正常响应，直接透传。
// countTokensPath 是 Anthropic 的 token 计数端点：Claude Code 定期调它做上下文长度估算，
// 响应只有 {"input_tokens":N}，不生成内容。这类流照转上游，仅打标记便于网页区分。
const countTokensPath = "/v1/messages/count_tokens"

func handler(w http.ResponseWriter, r *http.Request) {
	c := cfg.Load()

	// 访问控制：转发通道永远只允许本机——误把 listen 设成 0.0.0.0 也不会
	// 让内网设备白嫖路由里配的 API key。管理端点（/__logs、/__config、/__reload）另有 isLocalRequest 守卫。
	if !isLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}

	// Claude Code 启动时会发 HEAD /、HEAD /api/hello 等探测连通性。
	// 上游对这类路径返回 404/401，会被误判为网络不可达/认证失败，还污染在途流/完成流列表。
	// Anthropic API 正常请求都是 POST，HEAD 一律是探测，直接返回 200，不建 flight 不转上游。
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	// 浏览器刷新控制台页面会自动请求 /favicon.ico（非 API 请求），直接返回 204，
	// 避免被当转发请求建 flight、转上游返回认证错误污染在途流/完成流列表。
	if r.URL.Path == "/favicon.ico" {
		w.WriteHeader(http.StatusNoContent)
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
	f.stageStart.Store(f.start.UnixNano()) // 灯色计时起点=流建立（白灯从收到请求起计时）
	flights.register(f)
	// Responses 翻译口进来的内部请求带 context 标记，给 flight 打上来源，
	// 网页 API 列显示 [translate]/[Response]（与路由原因标签并列）。
	if v, ok := r.Context().Value(ctxKeyTranslated).(string); ok {
		f.translated = v
	}
	// Responses 口进来的内部请求经 context 带会话标识（prompt_cache_key），供"缓存年龄"列按会话锚定。
	if v, ok := r.Context().Value(ctxKeyConvID).(string); ok {
		f.convID = v
	}
	// responses-raw 透传口经 context 带思考模式（reasoning.effort 原样——透传零修改，
	// 上游收到的就是它；翻译口/直连口的思考值在下面改写完成后从 Anthropic body 提取）。
	if v, ok := r.Context().Value(ctxKeyThink).(string); ok {
		f.think = v
	}
	// Responses 翻译口带来的搜索还原上下文：水位主动剥块计数进 flight（[剥N] 显示）；
	// replay 指针留在 flight 上，400 兜底剥块时取还原时刻学对话水位
	// （见转发循环的 tool_call_id 分支）。
	if v, ok := r.Context().Value(ctxKeySearchReplay).(*searchReplayCtx); ok && v != nil {
		f.searchReplay = v
		if v.proactiveCutoff > 0 {
			f.searchStripped.Store(int32(v.proactiveCutoff))
			log.Printf("[剥块] #%d 对话水位剥 %d 个回放搜索块", f.id, v.proactiveCutoff)
		}
	}
	// count_tokens 探针打标记：完成流的原始内容只有 {"input_tokens":N}，
	// 没有标记的话在网页上看着像"空响应"，难以分辨是探针还是异常。
	if r.URL.Path == countTokensPath {
		f.countTokens = true
	}
	// 兜底清理：所有 return 路径统一由 defer 注销，避免遗漏导致在途流残留。
	// 注销后存档其透传内容（若有），供网页回看最近完成的流。
	defer func() {
		markClientGone(f, ctx)
		flights.unregister(f.id)
		addFinished(f)
	}()

	// 1. 缓冲请求体，以便重试时重放。
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("[请求] #%d %s %s 读取body失败: %v", f.id, r.Method, r.URL.Path, err)
		http.Error(w, "read body failed", http.StatusBadGateway)
		return
	}
	r.Body.Close()
	// 留一份请求体原文供网页查看（什么请求导致这个流）；翻译口的流存的是翻译后 body。
	f.setReqBody(body)
	// Anthropic 口：从请求体 metadata.user_id 提会话标识（Responses 口已由 context 带上，不重复解析）。
	if f.convID == "" {
		f.convID = extractConvID(body)
	}

	if c.LogRequestDetail {
		logRequestDetail(r, body)
	}
	// 提取 model：[请求] 日志展示 + 路由匹配共用。非 JSON/无 model 时为空。
	origModel := extractModel(body)
	modelPart := ""
	if origModel != "" {
		modelPart = " model=" + origModel
	}
	if f.translated == translatedResponsesRaw {
		modelPart += " [透传:responses]"
	} else if f.translated != "" {
		modelPart += " [翻译:" + f.translated + "]"
	}
	log.Printf("[请求] #%d %s %s%s (body=%d字节) 来自 %s", f.id, r.Method, r.URL.Path, modelPart, len(body), r.RemoteAddr)

	// 1.5 命中分类器请求就关掉 thinking，让分类快速返回。
	// isClassifier 同时供路由决策：分类器路由优先于 model 路由。
	isClassifier := isClassifierRequest(c, body)
	if isClassifier {
		stats.classifierHits.Add(1) // 命中即计：分流/关思考与否都统计（观测 Claude Code 安全判断请求量）
	}
	body = maybeRewriteClassifier(body)

	// 状态页「API」列思考值 = 实际发给上游的思考配置：在分类器改写之后提取，翻译口看到的也是
	// 映射后的 thinking（如 Codex effort high → 开 16384），不是客户端发下来的原样。
	// responses-raw 透传 body 是 Responses 格式（无 thinking 字段），其 reasoning.effort
	// 已在上面由 ctx 带上，这里不覆盖。
	if f.think == "" {
		f.think = extractThinkMode(body)
	}

	// 1.6 路由匹配。
	// 优先级：分类器路由 > fast 路由 > model 路由（按 routes 通配）。
	// 未命中任何路由时 upstream=c.Upstream、authToken=""（透传客户端 token，原逻辑）。
	f.setStage(stageRoute)
	upstream := c.Upstream
	authToken := ""
	var targetModel string
	fastRouteHit := false
	searchSummaryMode := false // 搜索摘要模式：step1+step2 自构响应，不走主 ark。
	var summarySF *SearchRoute // 增强搜索触发时由命中的 RouteRule 构造；空则用 c.SearchFallback
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
		f.routeReason.Store(routeClassifier)
		f.convAnchor = "classifier"
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
		f.routeReason.Store(routeFast)
		f.convAnchor = "fast"
	} else if len(c.Routes) > 0 && origModel != "" {
		for i := range c.Routes {
			// "Fallback"/"fast_route" 是保留名（* 兜底路由、fast 通道在 Codex 菜单里的名字）：
			// 不允许 pattern 全字撞名，撞名的路由不生效（loadConfig 有警告），防止截走 Codex 流量。
			if isReservedRoutePattern(c.Routes[i].Pattern) {
				continue
			}
			if matchModel(c.Routes[i].Pattern, origModel) {
				rr := &c.Routes[i]
				// 缓存锚定键随路由命中一次性记下（透传分支同样经此处，天然覆盖）：
				// 同会话同路由才互为同一条缓存 lineage，工具跳（标题生成等）不会抢走主对话的锚。
				f.convAnchor = "route:" + rr.Pattern
				// Responses 原生透传：本请求来自 Responses 口且预检已知路由带 url_response_api。
				// 上游切成该字段（Anthropic 专用的图片/搜索兜底、增强搜索对它无意义，直接跳过）。
				if f.responsesRaw() {
					if rr.URLResponseAPI == "" {
						// 预检时有、现在没了——只可能是配置热重载把字段删了；明说 502 不静默错路。
						log.Printf("[错误] #%d Responses 透传请求命中路由 %q 但其 url_response_api 已消失（配置热重载？），无法转发", f.id, rr.Pattern)
						http.Error(w, "route lost url_response_api mid-request", http.StatusBadGateway)
						return
					}
					upstream = rr.URLResponseAPI
					authToken = rr.API
					if rr.Model != "" && rr.Model != origModel {
						body = replaceModelValue(body, rr.Model)
						targetModel = rr.Model
					}
					log.Printf("[路由] #%d %s -> %s (Responses 原生透传, model %s -> %s)", f.id, origModel, rr.URLResponseAPI, origModel, rr.Model)
					f.routeReason.Store(routePattern)
					break
				}
				// 能力兜底：目标上游缺图片/搜索能力但请求需要时，改走对应兜底上游。
				needImage := rr.TextOnly && hasImage(body)
				needSearch := rr.NoSearch && hasWebSearch(body)
				if needImage || needSearch {
					// applyFB 把选中的兜底上游赋到当前请求（改 upstream/API/model）。
					applyFB := func(url, api, model, label string, reason int32) {
						upstream = url
						authToken = api
						if model != "" && model != origModel {
							body = replaceModelValue(body, model)
							targetModel = model
						}
						f.routeReason.Store(reason)
						log.Printf("[路由] #%d %s %s -> %s (model %s -> %s)", f.id, label, origModel, url, origModel, model)
					}
					sf := c.SearchFallback
					mf := c.MultimodalFallback
					// 搜索：整个请求交给 search_fallback，不管是否含图片（无证据表明会同时多模态+搜索）。
					//    summary_mode 优先（step1 搜索 + step2 摘要）；否则整请求转发。
					if needSearch && sf != nil {
						if sf.SummaryMode {
							searchSummaryMode = true
							f.routeReason.Store(routeSearch)
							log.Printf("[路由] #%d 搜索摘要模式 %s -> %s (model %s -> %s)", f.id, origModel, sf.URL, origModel, sf.Model)
							break
						}
						applyFB(sf.URL, sf.API, sf.Model, "搜索兜底", routeSearch)
						break
					}
					// 纯图片（无搜索）：有 multimodal_fallback 走 mf；搜索请求不落 mf（没 sf 则降级透传）。
					if needImage && mf != nil && !needSearch {
						applyFB(mf.URL, mf.API, mf.Model, "图片兜底", routeMultimodal)
						break
					}
					// 降级：搜索无 sf、或纯图片无 mf，透传原 route（由上游处理，可能报错）。
				}
				upstream = rr.URL
				authToken = rr.API
				if rr.Model != "" && rr.Model != origModel {
					body = replaceModelValue(body, rr.Model)
					targetModel = rr.Model
				}
				log.Printf("[路由] #%d %s -> %s (model %s -> %s)", f.id, origModel, rr.URL, origModel, rr.Model)
				f.routeReason.Store(routePattern)
				// 增强搜索：route 支持搜索（no_search:false）且配了 enhance_search，请求带搜索工具时，
				// 不调主力，改用本 route 上游走 kimi 摘要模式（等价于 no_search 走 search_fallback.summary_mode，
				// 只是搜索上游用 route 自己的 url/api/model，摘要参数用 route 的 enhance_search）。
				if rr.EnhanceSearch != nil && hasWebSearch(body) && !rr.NoSearch && !(rr.TextOnly && hasImage(body)) {
					searchSummaryMode = true
					summarySF = &SearchRoute{
						URL:             rr.URL,
						API:             rr.API,
						Model:           rr.Model,
						SummaryMode:     true,
						SummaryLevel:    rr.EnhanceSearch.SummaryLevel,
						SummaryThinking: rr.EnhanceSearch.SummaryThinking,
					}
					log.Printf("[路由] #%d 增强搜索 %s -> %s (model %s)", f.id, origModel, rr.URL, rr.Model)
				}
				break // 有序：第一个命中即止
			}
		}
	}

	f.origModel = origModel
	f.targetModel = targetModel

	// 搜索摘要模式：step1 搜索 + step2 摘要 + 自构 Kimi 格式响应，不走主 ark。
	// 成功则直接 return；失败降级走 search_fallback 整请求转走（Kimi 模式）。
	if searchSummaryMode {
		sf := summarySF
		if sf == nil {
			sf = c.SearchFallback
		}
		if sf != nil && searchAndRespond(w, body, sf, f, origModel) {
			return
		}
		log.Printf("[搜索摘要] #%d 失败，降级整请求转走 %s", f.id, func() string {
			if sf != nil {
				return sf.URL
			}
			return upstream
		}())
		if sf != nil {
			upstream = sf.URL
			authToken = sf.API
			if sf.Model != "" && sf.Model != origModel {
				body = replaceModelValue(body, sf.Model)
				targetModel = sf.Model
			}
			f.searchDebug = true // 降级走 Kimi 整请求转走时，记录主力响应 raw 便于对照
		}
	}

	// upstream 允许留空（有 pattern:"*" 兜底时默认 upstream 用不到）；走到这里仍为空
	// 说明既没命中路由也没配默认 upstream，重试无意义，直接 502 明说原因。
	if upstream == "" {
		log.Printf("[错误] #%d 未命中任何路由且默认 upstream 为空，无法转发", f.id)
		http.Error(w, "no route matched and upstream is empty", http.StatusBadGateway)
		return
	}

	// 上游归类键随路由最终值一次性记下（实测缓存存活的归类维度：url+模型，其他参数不看）。
	// 搜索摘要模式成功时已提前 return 不走到这里；降级转走分支已把 upstream/targetModel 改写成最终值。
	{
		m := targetModel
		if m == "" {
			m = origModel
		}
		f.upstreamKey = upstream + "|" + m
		// 搜索信封归属三元组随路由最终值注入（Responses 翻译口的 translatingWriter
		// 才有此方法；Anthropic 口类型断言失败自然跳过）。key 用实际生效的 token：
		// 路由 key 空时透传的是客户端 Authorization——与回放侧 effectiveKey 同口径。
		if t, ok := w.(interface{ setSearchTriple(string, string, string) }); ok {
			effKey := authToken
			if effKey == "" {
				effKey = r.Header.Get("Authorization")
			}
			t.setSearchTriple(upstream, effKey, m)
		}
	}

	// 1.7 全局流式化（convertAlltoStream）：开启时把所有非流式请求改为流式发上游，
	// 收集完整 SSE 流后重建非流式 JSON 一次性返回客户端（客户端无感知，网页可监控吐字/首字/tok/s）。
	// 只改 Anthropic Messages API（/v1/messages）：OpenAI 兼容路径的 SSE 格式不同，无法重建。
	// 搜索摘要模式自构响应不走主上游；已是流式的请求本就流式返回，均不受影响。
	convertToStream := false
	if c.ConvertAllToStream && !searchSummaryMode && r.URL.Path == "/v1/messages" {
		if stream, ok := getBoolField(body, "stream"); !ok || !stream {
			body = forceStreamTrue(body)
			convertToStream = true
			log.Printf("[流式化] #%d 非流式请求已改为流式发上游 (stream:%v → true)", f.id, stream)
		}
	}

	deadline := time.Now().Add(time.Duration(c.TotalBudgetSec * float64(time.Second)))
	// headersSent：是否已向客户端发过 200 + SSE 头（首次重试保活时置 true）。
	// 一旦 true，后续成功转发跳过 WriteHeader/copyHeaders（头已发），重试用尽改发 SSE error。
	headersSent := false
	var flusher http.Flusher // 保活开启后由 startSSEKeepalive 赋值
	pingInterval := time.Duration(c.PingIntervalSec * float64(time.Second))

	searchStripped := false // 搜索回放 id 被上游拒后是否已剥块重试过（fail-soft 只一次机会）
	for attempt := 0; attempt <= c.MaxRetries; attempt++ {
		// 进入「尝试N」阶段：网页「状态」列显示尝试序号（等上游首字节）。
		f.attempt.Store(int32(attempt + 1))
		f.setStage(stageAttempt)
		// 2. 构造发往上游的请求。
		// 透传流的上游是 url_response_api 填的 base（…/coding、…/v1 或完整 …/v1/responses 都行），
		// 不能照抄客户端路径（那是 /v1/responses，拼在 base 后面会双拼或错拼）——按 base 形态补差额。
		upPath := r.URL.Path
		if f.responsesRaw() {
			upPath = responsesAPIPath(upstream)
		}
		upReq, err := http.NewRequestWithContext(ctx, r.Method, upstream+upPath, bytes.NewReader(body))
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
		tSend := time.Now()                    // 请求发出时刻，用于算首字延迟
		f.attemptStart.Store(tSend.UnixNano()) // [尝试N:Xs] 从本次发出时刻起计（退避清零后在此重新打点）
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
					stats.addModelRetry(f.realModel())
					f.attempt.Store(int32(attempt + 2)) // 退避期间把尝试序号推进到下一次
					f.attemptStart.Store(0)             // 退避中：尝试未发出，黄灯旁显示 [退避中]
					if !headersSent && !convertToStream {
						// convertToStream 的客户端期待非流式响应，不能发 SSE 保活（会污染），静默等待。
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
				writeSSEError(w, flusher, "upstream timeout/error: "+err.Error(), f)
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
					stats.addModelRetry(f.realModel())
					f.attempt.Store(int32(attempt + 2)) // 退避期间把尝试序号推进到下一次
					f.attemptStart.Store(0)             // 退避中：尝试未发出，黄灯旁显示 [退避中]
					resp.Body.Close()
					if !headersSent && !convertToStream {
						// convertToStream 的客户端期待非流式响应，不能发 SSE 保活（会污染），静默等待。
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
				writeSSEError(w, flusher, fmt.Sprintf("upstream status %d", resp.StatusCode), f)
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
			// 搜索信封还原被上游拒（Kimi 的 tool_call_id 注册表已不认这个旧 id）：
			// 剥掉请求里回放的搜索块立即重试一次——还原失败自动降级为"没还原"，
			// 不烧退避预算（attempt-- 抵消循环自增，不占重试次数，MaxRetries=0
			// 时也能重试），也不能把这个 400 透给客户端。
			if !searchStripped && bytes.Contains(head, []byte("tool_call_id")) {
				if nb, n, ok := stripSearchBlocksInBody(body); ok {
					f.searchStripped.Add(int32(n))
					// 学对话水位：本次还原上行的信封里取最老封入时刻——注册表按龄
					// 淘汰，最老的被拒说明比它更老的也都死了，之后默认全剥不撞 400。
					water := ""
					if f.searchReplay != nil && len(f.searchReplay.restored) > 0 {
						oldest := f.searchReplay.restored[0]
						for _, ts := range f.searchReplay.restored[1:] {
							if ts.Before(oldest) {
								oldest = ts
							}
						}
						learnSearchCutoff(f.searchReplay.convID, oldest)
						water = fmt.Sprintf("，对话水位记为 %s", oldest.Format("01-02 15:04:05"))
					}
					log.Printf("[兜底] #%d 上游不认搜索回放 id (HTTP %d)，剥掉 %d 个搜索块重试%s", f.id, resp.StatusCode, n, water)
					stats.statusRetries.Add(1)
					stats.addModelRetry(f.realModel())
					body = nb
					searchStripped = true
					attempt--
					resp.Body.Close()
					continue
				}
			}
			log.Printf("[错误] 状态码 200 但响应体含错误: %s", firstLine(head))
			if attempt < c.MaxRetries && time.Now().Before(deadline) {
				wait := computeBackoff(resp, attempt)
				if !time.Now().Add(wait).After(deadline) {
					log.Printf("[重试] 等待 %v 后重试 (剩余预算 %v)", wait, time.Until(deadline).Round(time.Millisecond))
					stats.statusRetries.Add(1) // 实时状态行计数：重试次数（200 体内错误）
					stats.addModelRetry(f.realModel())
					f.attempt.Store(int32(attempt + 2)) // 退避期间把尝试序号推进到下一次
					f.attemptStart.Store(0)             // 退避中：尝试未发出，黄灯旁显示 [退避中]
					resp.Body.Close()
					if !headersSent && !convertToStream {
						// convertToStream 的客户端期待非流式响应，不能发 SSE 保活（会污染），静默等待。
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
				writeSSEError(w, flusher, "upstream stream error after retries", f)
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
		// convertToStream 的请求：上游按流式回 SSE，收集完整流重建非流式 JSON 后一次性返回
		// （网页在途流仍实时可见，统计与普通流式请求一致）。上游没按流式回（JSON）则走下面透传。
		if convertToStream && strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
			ok, out := collectStreamToJSON(w, resp, head, br, f)
			if ok {
				// 统计与普通流式透传一致：首字延迟 / 流式时长 / tok/s。
				tEnd := time.Now()
				stats.pushFirstByte(int64(tFirstByte.Sub(tSend).Milliseconds()))
				stats.pushThroughput(int64(tEnd.Sub(tFirstByte).Milliseconds()), out)
				firstByteMs := tFirstByte.Sub(tSend).Milliseconds()
				streamMs := tEnd.Sub(tFirstByte).Milliseconds()
				var tps float64
				if streamMs > 0 {
					tps = float64(out) / (float64(streamMs) / 1000.0)
				}
				f.firstByteMs = firstByteMs
				f.tps = tps
				log.Printf("[单流] #%d 首字 %.2fs 流式 %.2fs %d tok %.1f tok/s",
					f.id, float64(firstByteMs)/1000, float64(streamMs)/1000, out, tps)
				return
			}
			// 流中途断开：客户端尚未收到任何内容，退避后整体重试（重发流式请求、重收一次流）。
			log.Printf("[流式化] #%d 上游流未收完，重试", f.id)
			stats.statusRetries.Add(1)
			stats.addModelRetry(f.realModel())
			if attempt < c.MaxRetries && time.Now().Before(deadline) {
				wait := computeBackoff(resp, attempt)
				if !time.Now().Add(wait).After(deadline) {
					f.attempt.Store(int32(attempt + 2))
					f.attemptStart.Store(0) // 退避中：尝试未发出，黄灯旁显示 [退避中]
					if !sleepWithPing(ctx, w, flusher, wait, pingInterval, f) {
						return // 客户端已断开，停止重试
					}
					continue
				}
			}
			// 重试用尽：客户端期待非流式 JSON，透传 502 让其自行处理。
			log.Printf("[透传] 次数或预算用尽，非流式化流未收完，透传 502")
			http.Error(w, "upstream stream interrupted after retries", http.StatusBadGateway)
			return
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
		f.firstByteMs = firstByteMs
		f.tps = tps
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
	// 可选：OpenAI Responses API 监听口（翻译成 Anthropic 走主管线），独立于主端口。
	// 重载/切换配置时 reconcileResponsesServer 会按新配置动态启停，不必重启进程。
	reconcileResponsesServer(c.ResponsesListen)
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
	http.HandleFunc(configsPath, configsHandler)
	http.HandleFunc(switchPath, switchHandler)
	http.HandleFunc(newConfigPath, newConfigHandler)
	http.HandleFunc(codexSetupPS1Path, codexScriptHandler(codexSetupPS1, codexPS1Anchors, false))
	http.HandleFunc(codexSetupSHPath, codexScriptHandler(codexSetupSH, codexSHAnchors, true))
	http.HandleFunc(renameConfigPath, renameConfigHandler)
	http.HandleFunc(delConfigPath, delConfigHandler)
	http.HandleFunc(resetStatsPath, resetStatsHandler)
	http.HandleFunc(clearLogsPath, clearLogsHandler)
	http.HandleFunc(flightPath, flightHandler)
	http.HandleFunc(flightReqPath, flightReqHandler)
	http.HandleFunc(recentFlightsPath, recentFlightsHandler)
	http.HandleFunc(finishedCapPath, finishedCapHandler)
	http.HandleFunc(fullStorePath, fullStoreHandler)
	http.HandleFunc("/", handler)
	err := http.ListenAndServe(c.Listen, nil)
	log.Fatal(err)
}
