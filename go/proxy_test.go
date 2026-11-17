package main

import (
	"net/http"
	"testing"
)

func TestProxySpecOf(t *testing.T) {
	a := &Account{Raw: map[string]any{"proxy": "socks5://1.2.3.4:1080"}}
	if got := proxySpecOf(a); got != "socks5://1.2.3.4:1080" {
		t.Fatalf("proxy field not read: %q", got)
	}
	// 连字符 / 下划线写法都要认（CPA 也做同样的归一）
	if got := proxySpecOf(&Account{Raw: map[string]any{"proxy-url": "http://h:8080"}}); got != "http://h:8080" {
		t.Fatalf("hyphen form not read: %q", got)
	}
	if got := proxySpecOf(&Account{Raw: map[string]any{"proxy_url": "http://h:8081"}}); got != "http://h:8081" {
		t.Fatalf("underscore form not read: %q", got)
	}
	if got := proxySpecOf(&Account{Raw: map[string]any{}}); got != "" {
		t.Fatalf("empty account should have no proxy, got %q", got)
	}
}

func TestNormalizeProxySpec(t *testing.T) {
	if u, ok := normalizeProxySpec("socks5h://1.2.3.4:1080"); !ok || u.Scheme != "socks5" {
		t.Fatalf("socks5h should normalize to socks5, got %v %v", u, ok)
	}
	if u, ok := normalizeProxySpec("http://1.2.3.4:3128"); !ok || u.Scheme != "http" {
		t.Fatalf("http should pass through, got %v %v", u, ok)
	}
	for _, bad := range []string{"", "not-a-url", "ftp://h:1", "1.2.3.4:1080"} {
		if _, ok := normalizeProxySpec(bad); ok {
			t.Fatalf("spec %q should be rejected", bad)
		}
	}
}

func TestClientForAccountIsolation(t *testing.T) {
	base := &http.Client{Transport: &http.Transport{}}
	plain := &Account{Raw: map[string]any{}}
	if clientForAccount(base, plain) != base {
		t.Fatal("account without proxy must reuse the shared client")
	}
	withProxy := &Account{Raw: map[string]any{"proxy": "socks5://127.0.0.1:11081"}}
	got := clientForAccount(base, withProxy)
	if got == base {
		t.Fatal("account with proxy must get its own client")
	}
	tr, ok := got.Transport.(*http.Transport)
	if !ok || tr.Proxy == nil {
		t.Fatal("cloned transport must carry a Proxy func")
	}
	req, _ := http.NewRequest("GET", "https://maas-api.cn-huabei-1.xf-yun.com/v1/models", nil)
	u, err := tr.Proxy(req)
	if err != nil || u == nil || u.Scheme != "socks5" {
		t.Fatalf("proxy resolution wrong: %v %v", u, err)
	}
	// 共享 client 的 Transport 不能被改
	if base.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("shared transport must stay untouched")
	}
}
