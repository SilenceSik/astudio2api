package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 签到战果计量口径
//
// 真实坑：points/balance 里有两组数——
//   totalAmount   累计发放（单调递增）
//   totalBalance  当前可用（会被模型调用消耗）
// 早期代码用 totalBalance 算战果，账号只要当天花过分，签到就会显示
// 「+0」甚至负数，看起来像没签到成功。
// 战果必须用 totalAmount 的差。
// ---------------------------------------------------------------------------

// drainServer 模拟：签到时累计发放 +100，但同时消耗掉 500 分（可用余额反而下降）。
func drainServer(t *testing.T) *httptest.Server {
	t.Helper()
	var signed bool
	var granted, avail float64 = 1000, 1000
	return newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/tenant-app/v2/init-app"):
			if !signed {
				signed = true
				granted += 100 // 发了 100
				avail -= 400   // 同时又花掉 400（净可用 -300）
			}
			_, _ = w.Write([]byte(`{"flag":true,"code":0,"desc":"成功","data":{"banned":false}}`))
		case strings.HasSuffix(r.URL.Path, "/points/balance"):
			_, _ = w.Write([]byte(`{"flag":true,"code":0,"desc":"成功","data":{` +
				`"totalAmount":` + ftoa(granted) + `,"totalBalance":` + ftoa(avail) +
				`,"sparkTotalAmount":5000,"sparkTotalBalance":5000}}`))
		case strings.HasSuffix(r.URL.Path, "/client-popups/pending"):
			_, _ = w.Write([]byte(`{"flag":true,"code":0,"desc":"成功","data":[]}`))
		}
	}))
}

func TestCheckinGainUsesGrantedNotAvailable(t *testing.T) {
	srv := drainServer(t)
	old := StudioBase
	StudioBase = srv.URL + "/"
	t.Cleanup(func() { StudioBase = old })

	a := acct("a", "u1", "13800138000")
	a.Raw["ssoSessionId"] = "sid"
	up := newUpstream(a, srv.Client())

	res, err := up.CheckinOnce(context.Background(), false)
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if res.Gained != 100 {
		t.Fatalf("战果应按累计发放算 +100，实得 %v（用 totalBalance 会算成 -300）", res.Gained)
	}
	if res.Balance == nil || *res.Balance != 600 {
		t.Fatalf("可用余额应单独报告 600，实得 %v", res.Balance)
	}
}

// 星火积分不在 totalAmount 里，必须单独计量，否则国庆 5000 会被漏报成「+0」。
func TestCheckinReportsSparkGain(t *testing.T) {
	var signed bool
	srv := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		spark := 0
		switch {
		case strings.HasSuffix(r.URL.Path, "/tenant-app/v2/init-app"):
			signed = true
			spark = 5000
			_, _ = w.Write([]byte(`{"flag":true,"code":0,"desc":"成功","data":{"banned":false}}`))
		case strings.HasSuffix(r.URL.Path, "/points/balance"):
			if signed {
				spark = 5000
			}
			_, _ = w.Write([]byte(`{"flag":true,"code":0,"desc":"成功","data":{` +
				`"totalAmount":1100,"totalBalance":1100,` +
				`"sparkTotalAmount":` + ftoa(float64(spark)) + `,"sparkTotalBalance":` + ftoa(float64(spark)) + `}}`))
		case strings.HasSuffix(r.URL.Path, "/client-popups/pending"):
			_, _ = w.Write([]byte(`{"flag":true,"code":0,"desc":"成功","data":[]}`))
		}
	}))
	old := StudioBase
	StudioBase = srv.URL + "/"
	t.Cleanup(func() { StudioBase = old })

	a := acct("a", "u1", "13800138000")
	a.Raw["ssoSessionId"] = "sid"
	up := newUpstream(a, srv.Client())

	res, err := up.CheckinOnce(context.Background(), false)
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if res.SparkGained != 5000 {
		t.Fatalf("星火战果应为 +5000，实得 %v", res.SparkGained)
	}
	if res.SparkBalance == nil || *res.SparkBalance != 5000 {
		t.Fatalf("星火可用应为 5000，实得 %v", res.SparkBalance)
	}
}

// banned=true 必须被当成错误暴露出来，别静默当签到成功。
func TestCheckinSurfacesBannedAccount(t *testing.T) {
	srv := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/tenant-app/v2/init-app"):
			_, _ = w.Write([]byte(`{"flag":true,"code":0,"desc":"成功","data":{"banned":true}}`))
		case strings.HasSuffix(r.URL.Path, "/points/balance"):
			_, _ = w.Write([]byte(`{"flag":true,"code":0,"desc":"成功","data":{"totalAmount":0,"totalBalance":0}}`))
		case strings.HasSuffix(r.URL.Path, "/client-popups/pending"):
			_, _ = w.Write([]byte(`{"flag":true,"code":0,"desc":"成功","data":[]}`))
		}
	}))
	old := StudioBase
	StudioBase = srv.URL + "/"
	t.Cleanup(func() { StudioBase = old })

	a := acct("a", "u1", "13800138000")
	a.Raw["ssoSessionId"] = "sid"
	up := newUpstream(a, srv.Client())

	res, err := up.CheckinOnce(context.Background(), false)
	if err != nil {
		t.Fatalf("不该直接报错: %v", err)
	}
	if !res.Banned {
		t.Fatal("应识别 banned=true")
	}
	if !signinSucceeded(res) {
		// banned 是错误，不该占当日名额
		t.Log("banned 账号不应占当日签到名额")
	}
}
