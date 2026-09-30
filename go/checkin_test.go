package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// ---------------------------------------------------------------------------
// 签到（CheckinOnce）
//
// 实测结论（2026-10-01 对真实上游验证）：
//   - 真正发分的是 POST tenant-app/v2/init-app（客户端登录时调的「签到」）
//   - client-popups/pending 里的弹窗和 claim / complete 都是空操作：
//     连早已领过的账号调 claim 也返回 flag:true/code:0 且余额不变
// 所以 CheckinOnce 必须先打 init-app，否则等于什么都没做。
// ---------------------------------------------------------------------------

// signinServer 造一个假的 Studio：init-app 每次 +step 分，并记录调用次数。
func signinServer(t *testing.T, step float64, calls *int64) *httptest.Server {
	t.Helper()
	var balance float64 = 1000
	return newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/tenant-app/v2/init-app"):
			if r.Method != http.MethodPost {
				t.Errorf("init-app 必须是 POST，实得 %s", r.Method)
			}
			atomic.AddInt64(calls, 1)
			balance += step
			_, _ = w.Write([]byte(`{"flag":true,"code":0,"desc":"成功","data":{"banned":false}}`))
		case strings.HasSuffix(r.URL.Path, "/points/balance"):
			_, _ = w.Write([]byte(`{"flag":true,"code":0,"desc":"成功","data":{` +
				`"totalAmount":` + ftoa(balance) + `,"totalBalance":` + ftoa(balance) + `}}`))
		case strings.HasSuffix(r.URL.Path, "/client-popups/pending"):
			_, _ = w.Write([]byte(`{"flag":true,"code":0,"desc":"成功","data":[]}`))
		default:
			t.Errorf("未预期的请求: %s %s", r.Method, r.URL.Path)
			_, _ = w.Write([]byte(`{"flag":false,"code":-1,"desc":"unexpected"}`))
		}
	}))
}

func ftoa(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

func TestCheckinCallsInitAppAndReportsGain(t *testing.T) {
	var calls int64
	srv := signinServer(t, 100, &calls)
	old := StudioBase
	StudioBase = srv.URL + "/"
	t.Cleanup(func() { StudioBase = old })

	a := acct("a", "u1", "13800138000")
	a.Raw["ssoSessionId"] = "sid"
	up := newUpstream(a, srv.Client())

	res, err := up.CheckinOnce(context.Background(), false)
	if err != nil {
		t.Fatalf("签到不该报错: %v", err)
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("必须且只调一次 init-app，实得 %d 次", got)
	}
	if res.Gained != 100 {
		t.Fatalf("应报告 +100，实得 %v", res.Gained)
	}
	// Claimed 里必须留下 SIGN_IN 记录，否则运维看不出到底签没签
	var found bool
	for _, c := range res.Claimed {
		if c["type"] == "SIGN_IN" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Claimed 应含 SIGN_IN 记录，实得 %v", res.Claimed)
	}
}

func TestCheckinDryRunDoesNotCallInitApp(t *testing.T) {
	var calls int64
	srv := signinServer(t, 100, &calls)
	old := StudioBase
	StudioBase = srv.URL + "/"
	t.Cleanup(func() { StudioBase = old })

	a := acct("a", "u1", "13800138000")
	a.Raw["ssoSessionId"] = "sid"
	up := newUpstream(a, srv.Client())

	if _, err := up.CheckinOnce(context.Background(), true); err != nil {
		t.Fatalf("dry-run 不该报错: %v", err)
	}
	if got := atomic.LoadInt64(&calls); got != 0 {
		t.Fatalf("dry-run 绝不能真的签到，实得 %d 次调用", got)
	}
}

// 同一天重复签到：服务端按天幂等只发一次，所以我们第二次应该看到 +0，
// 且不该把失败算进去。
func TestCheckinIsIdempotentPerDayUpstreamSide(t *testing.T) {
	var calls int64
	srv := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/tenant-app/v2/init-app"):
			n := atomic.AddInt64(&calls, 1)
			bal := 1000
			if n == 1 {
				bal = 1100 // 只有第一次发分
			}
			_, _ = w.Write([]byte(`{"flag":true,"code":0,"desc":"成功","data":{"banned":false,"bal":` +
				ftoa(float64(bal)) + `}}`))
		case strings.HasSuffix(r.URL.Path, "/points/balance"):
			bal := 1000
			if atomic.LoadInt64(&calls) > 0 {
				bal = 1100
			}
			_, _ = w.Write([]byte(`{"flag":true,"code":0,"desc":"成功","data":{` +
				`"totalAmount":` + ftoa(float64(bal)) + `,"totalBalance":` + ftoa(float64(bal)) + `}}`))
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

	first, _ := up.CheckinOnce(context.Background(), false)
	if first.Gained != 100 {
		t.Fatalf("首签应 +100，实得 %v", first.Gained)
	}
	second, _ := up.CheckinOnce(context.Background(), false)
	if second.Gained != 0 {
		t.Fatalf("同日重签应 +0，实得 %v", second.Gained)
	}
	if len(second.Errors) != 0 {
		t.Fatalf("同日重签不该报错，实得 %v", second.Errors)
	}
}

// 凭据失效时必须把 ErrAuthExpired 透出来，让池子把账号标成待重登，
// 而不是当成「签到成功 0 分」静默吞掉。
func TestCheckinSurfacesAuthExpired(t *testing.T) {
	srv := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"flag":false,"code":80000,"desc":"登录异常，请重新登录"}`))
	}))
	old := StudioBase
	StudioBase = srv.URL + "/"
	t.Cleanup(func() { StudioBase = old })

	a := acct("a", "u1", "13800138000")
	a.Raw["ssoSessionId"] = "sid"
	up := newUpstream(a, srv.Client())

	if _, err := up.CheckinOnce(context.Background(), false); err == nil {
		t.Fatal("凭据失效应报错，不能静默当成功")
	} else if !strings.Contains(err.Error(), ErrAuthExpired.Error()) {
		t.Fatalf("应返回 ErrAuthExpired，实得 %v", err)
	}
}
