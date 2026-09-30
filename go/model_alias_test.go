package main

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"
)

// ---------------------------------------------------------------------------
// 模型名映射
//
// 上游下发的 id 是 `xopglm52`、`xopdeepseekv4pro` 这类内部代号；对外要暴露
// 干净名字（`glm-5.2`、`deepseek-v4-pro`），并且客户端写别名 / 显示名 / 代号
// 三种写法都要命中同一个模型。
// ---------------------------------------------------------------------------

func TestFriendlyModelName(t *testing.T) {
	cases := []struct{ name, id, want string }{
		{"GLM-5.2", "xopglm52", "glm-5.2"},
		{"DeepSeek-V4-Pro", "xopdeepseekv4pro", "deepseek-v4-pro"},
		{"Qwen3.6-35B-A3B", "xopqwen36v35b", "qwen3.6-35b-a3b"},
		{"Kimi-K2.6", "xopkimik26", "kimi-k2.6"},
		{"MiniMax-M2.5", "xminimaxm25", "minimax-m2.5"},
		{"Spark-X2-Agent", "xsparkx2agent", "spark-x2-agent"},
		{"Spark-X2-Flash", "spark-x", "spark-x2-flash"},
		{"DeepSeek-V4-Flash", "xopdsv4flash0731in", "deepseek-v4-flash"},
		// 太泛的名字会跟别的 provider 撞车，加前缀区分
		{"Auto", "astronclaw-auto", "astudio-auto"},
		// 没有显示名时回落到 id
		{"", "xopfoo", "xopfoo"},
	}
	for _, c := range cases {
		if got := friendlyModelName(c.name, c.id); got != c.want {
			t.Errorf("friendlyModelName(%q,%q) = %q, want %q", c.name, c.id, got, c.want)
		}
	}
}

func TestNormalizeModelKeyCollapsesPunctuation(t *testing.T) {
	// 这几串都该归一成同一个键
	for _, s := range []string{"GLM-5.2", "glm-5.2", "glm 5.2", " glm-5.2 "} {
		if got := normalizeModelKey(s); got != "glm-5.2" {
			t.Errorf("normalizeModelKey(%q) = %q, want glm-5.2", s, got)
		}
	}
}

// 下划线/连字符/点号混用是人工手输的常见错法，归一后仍要能命中。
func TestModelKeyMatchesTolerantOfSeparators(t *testing.T) {
	for _, s := range []string{"GLM_5_2", "glm-5.2", "GLM 5 2", "glm.5.2"} {
		if !modelKeyMatches(normalizeModelKey(s), "glm-5.2") {
			t.Errorf("%q 应能命中 glm-5.2（实得键 %q）", s, normalizeModelKey(s))
		}
	}
	// 不该误配：不同模型之间不能互相命中
	if modelKeyMatches(normalizeModelKey("glm-5.1"), "glm-5.2") {
		t.Error("glm-5.1 不该命中 glm-5.2")
	}
}

// newAliasGateway 造一个带单槽位（模型目录=内置兜底表）的网关。
func newAliasGateway(t *testing.T) (*Gateway, string) {
	t.Helper()
	dir := t.TempDir()
	const key = "test-key"
	cfg := Config{APIKey: key, AccountsDir: filepath.Join(dir, "accounts"),
		StatePath: filepath.Join(dir, "state.json")}
	p := newPool(cfg, newLedger(cfg.StatePath))
	a := acct("a", "u1", "13800138000")
	p.slots = []*Slot{{Account: a, Up: newUpstream(a, http.DefaultClient)}}
	return &Gateway{cfg: cfg, pool: p}, key
}

// 别名 / 显示名 / 内部代号三种写法都归一到同一个上游 id。
func TestCanonicalModelAcceptsAllThreeForms(t *testing.T) {
	g, _ := newAliasGateway(t)
	for _, in := range []string{"glm-5.2", "GLM-5.2", "xopglm52", "glm 5.2"} {
		if got := g.canonicalModel(in); got != "xopglm52" {
			t.Errorf("canonicalModel(%q) = %q, want xopglm52", in, got)
		}
	}
	for _, in := range []string{"deepseek-v4-pro", "DeepSeek-V4-Pro", "xopdeepseekv4pro0813"} {
		if got := g.canonicalModel(in); got != "xopdeepseekv4pro0813" {
			t.Errorf("canonicalModel(%q) = %q, want xopdeepseekv4pro0813", in, got)
		}
	}
	// 客户端目录里带日期后缀，旧的裸 slug 也仍要能透传（上游两个都通）
	if got := g.canonicalModel("xopdeepseekv4pro"); got != "xopdeepseekv4pro" {
		t.Errorf("旧 slug 应原样透传，实得 %q", got)
	}
	for _, in := range []string{"spark-x2.5", "Spark-X2.5"} {
		if got := g.canonicalModel(in); got != "spark-x2.5" {
			t.Errorf("canonicalModel(%q) = %q, want spark-x2.5", in, got)
		}
	}
	// Auto 不对外暴露，但解析面仍认得它（客户端会点名），否则本来能调的变成 404。
	if got := g.canonicalModel("Auto"); got != "astronclaw-auto" {
		t.Errorf("canonicalModel(Auto) = %q, want astronclaw-auto", got)
	}
}

// 对外清单必须恰好是标准集 —— 受限模型（只有老号有订购权限）不得出现，
// 否则请求会落到新号上吃 403 并白烧该模型冷却。
func TestModelsEndpointExposesOnlyStandardSet(t *testing.T) {
	g, key := newAliasGateway(t)
	srv := newTestServer(t, g.Handler())
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out modelsResp
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解析失败: %v (%s)", err, raw)
	}

	want := map[string]string{
		"spark-x2.5":        "spark-x2.5",
		"glm-5.2":           "xopglm52",
		"deepseek-v4-pro":   "xopdeepseekv4pro0813",
		"deepseek-v4-flash": "xopdsv4flash0731in",
	}
	got := map[string]string{}
	for _, m := range out.Data {
		got[m.ID] = m.Root
	}
	if len(got) != len(want) {
		t.Errorf("对外模型数应为 %d，实得 %d：%v", len(want), len(got), got)
	}
	for id, root := range want {
		if got[id] != root {
			t.Errorf("%s 应映射到 %s，实得 %q", id, root, got[id])
		}
	}
	// 受限模型一个都不该出现
	for _, banned := range []string{
		"glm-5.1", "kimi-k2.6", "qwen3.6-35b-a3b", "minimax-m2.5",
		"spark-x2-agent", "spark-x2-flash", "astudio-auto",
	} {
		if _, bad := got[banned]; bad {
			t.Errorf("受限/非模型项 %q 不应出现在对外清单", banned)
		}
	}
}

// 解析面和暴露面必须分开：受限模型不对外列出，但友好名仍要能解析出 slug。
// 收窄成只解析标准集会让本来能调的模型变成 404 —— 那是回退。
func TestRestrictedAliasesStillResolve(t *testing.T) {
	g, _ := newAliasGateway(t)
	cases := map[string]string{
		"GLM-5.1":         "xopglm51",
		"glm-5.1":         "xopglm51",
		"Kimi-K2.6":       "xopkimik26",
		"MiniMax-M2.5":    "xminimaxm25",
		"Qwen3.6-35B-A3B": "xopqwen36v35b",
		"Spark-X2-Agent":  "xsparkx2agent",
		"Spark-X2-Flash":  "spark-x",
		"Auto":            "astronclaw-auto",
	}
	for in, want := range cases {
		if got := g.canonicalModel(in); got != want {
			t.Errorf("canonicalModel(%q) = %q, want %q", in, got, want)
		}
	}
	// 空手写 slug 也要认（上游自己也在返回这个 id）
	if got := g.canonicalModel("xopdeepseekv4pro"); got != "xopdeepseekv4pro" {
		t.Errorf("裸 slug 应可解析，实得 %q", got)
	}
}

// 同一轮里出现多个上游 id 映射到同一友好名时，canonicalModel 必须稳定
// 命中其中一个（不能时而 0813 时而裸 slug）。
func TestAliasResolutionIsDeterministic(t *testing.T) {
	g, _ := newAliasGateway(t)
	first := g.canonicalModel("DeepSeek-V4-Pro")
	for i := 0; i < 20; i++ {
		if got := g.canonicalModel("DeepSeek-V4-Pro"); got != first {
			t.Fatalf("解析不稳定：%q 与 %q 交替", first, got)
		}
	}
	if first != "xopdeepseekv4pro0813" {
		t.Errorf("应以标准集那条为准，实得 %q", first)
	}
}

// 认不出来的名字原样透传——让上游回 404 比我们猜一个更准。
func TestCanonicalModelPassesThroughUnknown(t *testing.T) {
	g, _ := newAliasGateway(t)
	if got := g.canonicalModel("some-unknown-model"); got != "some-unknown-model" {
		t.Errorf("未知模型应原样透传，实得 %q", got)
	}
}

type modelsResp struct {
	Data []struct {
		ID         string `json:"id"`
		Root       string `json:"root"`
		Multiplier any    `json:"multiplier"`
	} `json:"data"`
}

// /v1/models 对外暴露干净别名，并带上游代号与倍率。
func TestModelsEndpointExposesFriendlyAliases(t *testing.T) {
	g, key := newAliasGateway(t)
	srv := newTestServer(t, g.Handler())
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("应 200，实得 %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	var out modelsResp
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解析失败: %v (%s)", err, raw)
	}

	seen := map[string]string{}
	count := map[string]int{}
	for _, m := range out.Data {
		seen[m.ID] = m.Root
		count[m.ID]++
	}
	// 同一 id 出现多次说明内置表里有两条映射到同一友好名，客户端看到会混乱
	for id, n := range count {
		if n > 1 {
			t.Errorf("模型 id %q 在 /v1/models 里重复出现 %d 次", id, n)
		}
	}
	if len(out.Data) != len(count) {
		t.Errorf("模型条数 %d 与去重后 %d 不一致", len(out.Data), len(count))
	}
	if seen["glm-5.2"] != "xopglm52" {
		t.Errorf("应暴露 glm-5.2 → xopglm52，实得 %q", seen["glm-5.2"])
	}
	if seen["deepseek-v4-pro"] != "xopdeepseekv4pro0813" {
		t.Errorf("应暴露 deepseek-v4-pro → xopdeepseekv4pro0813，实得 %q", seen["deepseek-v4-pro"])
	}
	if seen["spark-x2.5"] != "spark-x2.5" {
		t.Errorf("应暴露 spark-x2.5，实得 %q", seen["spark-x2.5"])
	}
	if _, bad := seen["xopglm52"]; bad {
		t.Error("对外 id 不该再用内部代号")
	}
}
