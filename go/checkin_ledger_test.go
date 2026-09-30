package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 台账毒化回归
//
// 真实踩过的坑：旧版 checkin 只调 client-popups/* 那几个空操作接口，
// 一个积分都没拿到，却把当天写进台账标成「已签到」（ok=true, claimed=0）。
// 结果真正的签到接口（tenant-app/v2/init-app）整个当天再也不会被调用
// —— 台账把当日名额毒化了，账号静默漏签。
//
// 判据必须是「签到那一步确实成功了」，不是「跑过了」。
// ---------------------------------------------------------------------------

// poisonServer：init-app 恒成功；用来观察台账怎么记。
func poisonServer(t *testing.T, initAppOK bool) *httptest.Server {
	t.Helper()
	return newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/tenant-app/v2/init-app"):
			if initAppOK {
				_, _ = w.Write([]byte(`{"flag":true,"code":0,"desc":"成功","data":{"banned":false}}`))
			} else {
				_, _ = w.Write([]byte(`{"flag":false,"code":50000,"desc":"服务繁忙"}`))
			}
		case strings.HasSuffix(r.URL.Path, "/points/balance"):
			_, _ = w.Write([]byte(`{"flag":true,"code":0,"desc":"成功","data":{"totalAmount":1000,"totalBalance":1000}}`))
		case strings.HasSuffix(r.URL.Path, "/client-popups/pending"):
			_, _ = w.Write([]byte(`{"flag":true,"code":0,"desc":"成功","data":[]}`))
		}
	}))
}

func newCheckinPool(t *testing.T, srv *httptest.Server) (*Pool, *Ledger, string) {
	t.Helper()
	old := StudioBase
	StudioBase = srv.URL + "/"
	t.Cleanup(func() { StudioBase = old })

	dir := t.TempDir()
	cfg := Config{AccountsDir: filepath.Join(dir, "accounts"), StatePath: filepath.Join(dir, "state.json")}
	l := newLedger(cfg.StatePath)
	p := newPool(cfg, l)

	a := acct("acct-x", "u1", "13800138000")
	a.Path = filepath.Join(dir, "accounts", "acct-x.json")
	a.Raw["ssoSessionId"] = "sid"
	a.Raw["accountId"] = "13800138000"
	p.slots = []*Slot{{Account: a, Up: newUpstream(a, srv.Client())}}
	return p, l, a.Identity()
}

// 签到失败时绝不能占掉当日名额，否则当天再也不会重试。
func TestCheckinFailureDoesNotClaimTheDay(t *testing.T) {
	p, l, id := newCheckinPool(t, poisonServer(t, false))
	// 用实现同源的日期，别硬编码 —— 否则测试结果会随跑在哪台时区的机器上而变。
	day := CheckinDay(time.Now())

	p.CheckinAll(context.Background(), false, false, 1)

	if l.CheckinDone(id, day) {
		t.Fatal("init-app 失败时不能把当天记为已签到——否则整天漏签且不重试")
	}
}

// 签到成功才占名额。
func TestCheckinSuccessClaimsTheDay(t *testing.T) {
	p, l, id := newCheckinPool(t, poisonServer(t, true))
	day := CheckinDay(time.Now())

	p.CheckinAll(context.Background(), false, false, 1)

	if !l.CheckinDone(id, day) {
		t.Fatal("init-app 成功后应占掉当日名额，避免重复签到")
	}
}

// 签到的「一天」必须按上游时区（北京时间）算，不能跟宿主时区走。
// 回归：早先直接用 time.Now().Format，在 UTC 容器里北京时间 00:00–08:00
// 会被算成前一天，与上游按天幂等窗口错位。
func TestCheckinDayUsesUpstreamTimezone(t *testing.T) {
	// 北京时间 2026-10-01 03:00 == UTC 2026-09-30 19:00
	utc := time.Date(2026, 9, 30, 19, 0, 0, 0, time.UTC)
	if got := CheckinDay(utc); got != "2026-10-01" {
		t.Fatalf("北京时间 10-01 03:00 应算作 2026-10-01，实得 %q（时区没钉死）", got)
	}

	// 同一个时刻，在 UTC 与在任意宿主时区下必须得到同一个日期串
	day := CheckinDay(utc)
	for _, tz := range []*time.Location{time.UTC, CheckinTZ} {
		if got := CheckinDay(utc.In(tz)); got != day {
			t.Fatalf("同一时刻换个时区表示就变日期：%q vs %q", got, day)
		}
	}

	// 北京时间跨过午夜即换天
	before := time.Date(2026, 9, 30, 23, 30, 0, 0, CheckinTZ)
	after := time.Date(2026, 10, 1, 0, 30, 0, 0, CheckinTZ)
	if CheckinDay(before) != "2026-09-30" || CheckinDay(after) != "2026-10-01" {
		t.Fatalf("北京时间午夜应换天：%q / %q", CheckinDay(before), CheckinDay(after))
	}
}

// 旧版留下的毒化记录（ok=true/claimed=0）必须被自愈成「未签到」，
// 否则那些账号当天永远不会被重试。
func TestPoisonedLedgerEntrySelfHeals(t *testing.T) {
	l := newLedger(filepath.Join(t.TempDir(), "state.json"))
	id, day := "abc", "2026-10-01"

	// 复现毒化：旧版代码什么都没领到却标成成功
	l.MarkCheckin(id, day, true, 0, "领取 0 项")

	if l.CheckinDone(id, day) {
		t.Fatal("claimed==0 的成功记录必须自愈为未签到，否则当天漏签且不重试")
	}
}

// 真签到成功（claimed>=1）才占名额。
func TestRealCheckinStillClaimsTheDay(t *testing.T) {
	l := newLedger(filepath.Join(t.TempDir(), "state.json"))
	id, day := "abc", "2026-10-01"

	l.MarkCheckin(id, day, true, 1, "领取 1 项")

	if !l.CheckinDone(id, day) {
		t.Fatal("真实签到成功后应占掉当日名额")
	}
}

// SIGN_IN 必须落到 Detail 里，运维才能从输出看出到底签没签。
func TestCheckinDetailCarriesSignInMarker(t *testing.T) {
	p, _, _ := newCheckinPool(t, poisonServer(t, true))
	results := p.CheckinAll(context.Background(), false, false, 1)
	if len(results) != 1 {
		t.Fatalf("应有一个账号结果，实得 %d", len(results))
	}
	raw, _ := json.Marshal(results[0].Detail)
	if !strings.Contains(string(raw), "SIGN_IN") {
		t.Fatalf("Detail 里应含 SIGN_IN，实得 %s", raw)
	}
}
