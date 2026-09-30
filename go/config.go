package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// 常量（协议来自对 AStudio 3.4.4 客户端的静态分析）
const (
	DefaultMaasBase = "https://maas-api.cn-huabei-1.xf-yun.com/v1"
	// 客户端身份头 —— 缺了会被上游拒绝 (code 11001001)
	ClientTypeWin = "21"
	StudioVersion = "3.4.4"

	// 服务端下发的模型清单接口（cookie 认证，用它换 api_key）
	ModelsConfigPath = "bot/models/configs"

	// 调度参数
	StickyTTL = 30 * time.Minute
	StickyMax = 4096

	CooldownAuth  = 600 * time.Second // 401：凭据失效
	CooldownRate  = 60 * time.Second  // 429
	CooldownError = 30 * time.Second  // 5xx / 网络错误

	// CooldownEntitlement：403 表示「该账号对这个模型没有订购/额度」
	// ——是账号×模型的权限属性，不是凭据失效。只冷却这一个模型，
	// 否则一个未订购的模型会把账号上其它能用的模型一起拖下水。
	CooldownEntitlement = 1800 * time.Second

	ModelsTTL = 3600 * time.Second

	CheckinStartupDelay = 30 * time.Second
	CheckinInterval     = 3600 * time.Second

	MaxAttempts = 3
)

// StudioBase AStudio 账号站基址。是变量而非常量：测试要指向 mock 服务器。
var StudioBase = "https://agent.xfyun.cn/xingchen-studio/"

// Model 模型条目。
type Model struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Multiplier any    `json:"multiplier"`
	BaseURL    string `json:"base_url"`
	Provider   any    `json:"provider,omitempty"`
	APIKey     string `json:"api_key,omitempty"`
	IsDefault  bool   `json:"is_default,omitempty"`
	IsCurrent  bool   `json:"is_current,omitempty"`
}

// 上游下发的模型 id 是 `xopglm52`、`xopdeepseekv4pro` 这类内部代号，
// 但配置里同时带了一个干净的人类名字（`GLM-5.2`、`DeepSeek-V4-Pro`）。
// 对外要用干净名字，所以把 Name 归一成 OpenAI 风格的小写别名。
//
// 归一规则：转小写 → 非字母数字的连续字符折成一个 `-` → 去掉首尾 `-`。
// 例：`GLM-5.2` → `glm-5.2`，`Qwen3.6-35B-A3B` → `qwen3.6-35b-a3b`。
var genericModelNames = map[string]bool{
	// 太泛的名字会被别的 provider 撞车，加前缀区分
	"auto": true, "chat": true, "default": true, "model": true, "gpt": true,
}

func friendlyModelName(name, id string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	if s == "" {
		s = strings.ToLower(strings.TrimSpace(id))
	}
	var b strings.Builder
	dash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' {
			if dash {
				b.WriteByte('-')
				dash = false
			}
			b.WriteRune(r)
		} else {
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return strings.ToLower(id)
	}
	if genericModelNames[out] {
		return "astudio-" + out
	}
	return out
}

// Alias 对外暴露的友好模型名。
func (m Model) Alias() string {
	return friendlyModelName(m.Name, m.ID)
}

// builtinModels 标准模型集：对外暴露的唯一清单，同时也是服务端拉不到
// 模型表时的兜底。
//
// 这里**只放三个账号都能用的通用模型**。受限模型（GLM-5.1、Kimi-K2.6、
// Qwen3.6-35B-A3B、MiniMax-M2.5、Spark-X2-Agent、Spark-X2-Flash）只有老号
// 有订购权限，暴露出去会让请求落到新号上吃 403、白烧一次冷却，所以不对外列出。
// 它们仍可用原始 slug 直接调用（canonicalModel 找不到就原样透传上游）。
func builtinModels() []Model {
	mk := func(id, name string, mult any, base string) Model {
		return Model{ID: id, Name: name, Multiplier: mult, BaseURL: base}
	}
	return []Model{
		mk("spark-x2.5", "Spark-X2.5", nil, DefaultMaasBase),
		mk("xopglm52", "GLM-5.2", "x2.0", DefaultMaasBase),
		// 客户端真实目录里的 slug 是 xopdeepseekv4pro0813（带日期后缀）。
		// 不要再把裸 slug xopdeepseekv4pro 也放进表里：它和这条映射到同一个
		// 友好名，会在 /v1/models 里产出两个同 id 的条目。
		mk("xopdeepseekv4pro0813", "DeepSeek-V4-Pro", "x2.0", DefaultMaasBase),
		mk("xopdsv4flash0731in", "DeepSeek-V4-Flash", "x1.0", DefaultMaasBase),
	}
}

// standardModelIDs 标准集快照，供对外清单过滤用。
func standardModelIDs() map[string]bool {
	out := map[string]bool{}
	for _, m := range builtinModels() {
		out[m.ID] = true
	}
	return out
}

// aliasModels 全量已知目录，**只用于「友好名 → 上游 slug」解析**。
//
// 它比对外标准集大：受限模型虽然不对外列出，但调用方仍可能直接点名
// （`GLM-5.1`、`Kimi-K2.6`…）。**解析与暴露必须分开**：
// 解析要全（否则本来能调的变成调不了，是回退），暴露要窄（否则请求落到
// 没订购的账号上吃 403）。标准集排在最前，保证同名 slug 优先命中标准集那条。
func aliasModels() []Model {
	mk := func(id, name string, mult any, base string) Model {
		return Model{ID: id, Name: name, Multiplier: mult, BaseURL: base}
	}
	out := builtinModels()
	return append(out,
		mk("astronclaw-auto", "Auto", nil, DefaultMaasBase),
		mk("xsparkx2agent", "Spark-X2-Agent", "x2.0", DefaultMaasBase),
		mk("spark-x", "Spark-X2-Flash", "x0.5",
			"https://spark-api-open.xf-yun.com/agent/v1"),
		mk("xopglm51", "GLM-5.1", "x2.0", DefaultMaasBase),
		mk("xminimaxm25", "MiniMax-M2.5", "x1.0", DefaultMaasBase),
		mk("xopkimik26", "Kimi-K2.6", "x2.0", DefaultMaasBase),
		mk("xopqwen36v35b", "Qwen3.6-35B-A3B", "x1.0", DefaultMaasBase),
		// 裸 slug 也收进来，让 `xopdeepseekv4pro` 能解析到自身；
		// 友好名匹配时标准集那条（0813）在前，会先命中。
		mk("xopdeepseekv4pro", "DeepSeek-V4-Pro", "x2.0", DefaultMaasBase),
	)
}

// ---------------------------------------------------------------------------
// 配置（环境变量）
// ---------------------------------------------------------------------------

type Config struct {
	APIKey      string
	Port        int
	AccountsDir string
	StatePath   string
	SessionPath string // 客户端活会话文件（空则自动探测）
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func loadConfig() Config {
	port := 8788
	if v := strings.TrimSpace(os.Getenv("ASTUDIO_PORT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			port = n
		}
	}
	base := exeDir()
	return Config{
		APIKey:      strings.TrimSpace(os.Getenv("ASTUDIO_API_KEY")),
		Port:        port,
		AccountsDir: envOr("ASTUDIO_ACCOUNTS_DIR", base+"/accounts"),
		StatePath:   envOr("ASTUDIO_STATE_PATH", base+"/state.json"),
		SessionPath: strings.TrimSpace(os.Getenv("ASTUDIO_SESSION_PATH")),
	}
}

// exeDir 运行根目录：可执行文件所在目录；若二进制放在 bin/ 这类子目录下，
// 则上溯一级，避免账号目录默认落到 bin/accounts。
func exeDir() string {
	if p, err := os.Executable(); err == nil {
		if d := dirOf(p); d != "" && d != "." {
			if base := filepath.Base(d); base == "bin" || base == "sbin" {
				if parent := dirOf(d); parent != "" && parent != "." {
					return parent
				}
			}
			return d
		}
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

func dirOf(p string) string {
	i := strings.LastIndexAny(p, `/\`)
	if i <= 0 {
		return "."
	}
	return p[:i]
}

// liveSessionCandidates 客户端活会话文件候选位置。
func liveSessionCandidates(cfg Config) []string {
	if cfg.SessionPath != "" {
		return []string{cfg.SessionPath}
	}
	home, _ := os.UserHomeDir()
	var out []string
	if v := strings.TrimSpace(os.Getenv("ASTUDIO_SESSION_PATH")); v != "" {
		out = append(out, v)
	}
	out = append(out,
		`D:\AStudio Data\userdata\astron-session.json`,
		home+`/AppData/Local/AStudio/userdata/astron-session.json`,
		home+`/AppData/Roaming/AStudio/userdata/astron-session.json`,
		home+`/Library/Application Support/AStudio/userdata/astron-session.json`,
	)
	return out
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// CheckinTZ 是签到「一天」的判定时区。
//
// 必须与上游对齐，不能用宿主本地时区：上游按北京时间算日界，若宿主是 UTC
// （容器镜像默认就是 UTC），北京时间 00:00–08:00 会被算成前一天，
// 与上游的按天幂等窗口错位 —— 表现为该区间内签到被判成「昨日已领」而跳过，
// 或同一天被上游认作两天。本机/国内服务器是 CST 时看不出问题，
// 一旦跑在 UTC 容器里就出错，所以这里显式钉死。
var CheckinTZ = time.FixedZone("CST", 8*3600)

// CheckinDay 把时刻折算成上游口径的日期串（YYYY-MM-DD）。
func CheckinDay(t time.Time) string {
	return t.In(CheckinTZ).Format("2006-01-02")
}

func logf(format string, args ...any) {
	fmt.Printf("[%s] %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}

// multiplierIsFree 倍率是否为零计费（x0 / x0.00 / 0）。未知倍率不算免费。
func multiplierIsFree(m any) bool {
	if m == nil {
		return false
	}
	var s string
	switch v := m.(type) {
	case string:
		s = v
	case float64:
		return v == 0
	case int:
		return v == 0
	case json.Number:
		f, err := v.Float64()
		return err == nil && f == 0
	default:
		s = fmt.Sprint(v)
	}
	s = strings.TrimSpace(strings.TrimLeft(s, "xX"))
	f, err := strconv.ParseFloat(s, 64)
	return err == nil && f == 0
}

func mask(v string) string {
	if len(v) > 8 {
		return v[:4] + "…" + v[len(v)-2:]
	}
	if v == "" {
		return ""
	}
	return "***"
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f, err == nil
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}
