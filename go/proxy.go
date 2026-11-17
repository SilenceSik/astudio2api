package main

// 账号级出口代理。
//
// 设计参考 CLIProxyAPI（CPA）：凭据文件最外层的 proxy 字段优先于全局配置，
// 这样一个进程里不同账号可以各自从不同 IP 出网，不必为每个号起一个容器。
//
// 支持的 scheme：http / https / socks5 / socks5h（socks5h 归一成 socks5，
// Go 标准库的 http.Transport 在 socks5 下同样用对端解析 DNS）。

import (
	"net/http"
	"net/url"
	"strings"
)

// proxySpecOf 取账号自己的出口；空表示沿用全局（环境变量）设置。
func proxySpecOf(a *Account) string {
	for _, k := range []string{"proxy", "proxy_url", "proxy-url"} {
		if v := a.str(k); v != "" {
			return v
		}
	}
	return ""
}

// normalizeProxySpec 把 socks5h 归一成 socks5，并校验可解析。
func normalizeProxySpec(spec string) (*url.URL, bool) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, false
	}
	u, err := url.Parse(spec)
	if err != nil || u.Host == "" {
		return nil, false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	case "socks5", "socks5h":
		u.Scheme = "socks5"
	default:
		return nil, false
	}
	return u, true
}

// clientForAccount 为带 proxy 的账号克隆一份 Transport（独立连接池 + 独立出口）。
// 未配置 proxy 时返回共享 client，行为与之前完全一致。
func clientForAccount(base *http.Client, a *Account) *http.Client {
	u, ok := normalizeProxySpec(proxySpecOf(a))
	if !ok {
		return base
	}
	var tr *http.Transport
	if base != nil {
		if t, ok := base.Transport.(*http.Transport); ok && t != nil {
			tr = t.Clone()
		}
	}
	if tr == nil {
		tr = http.DefaultTransport.(*http.Transport).Clone()
	}
	tr.Proxy = http.ProxyURL(u)
	c := &http.Client{Transport: tr}
	if base != nil {
		c.Timeout = base.Timeout
	}
	return c
}
