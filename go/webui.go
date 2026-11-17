package main

// 内置 Web 控制台。
//
// 页面本身不含任何账号数据（只是一个静态 HTML），数据全部由浏览器带 Bearer
// 调 /admin/* 取。所以 /ui 不需要鉴权，而 /admin/* 仍然受 auth 保护——
// 这样既能给不熟命令行的人一个图形入口，也不扩大暴露面。

import (
	_ "embed"
	"net/http"
)

//go:embed webui.html
var webuiHTML []byte

func (g *Gateway) handleWebUI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeErr(w, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(webuiHTML)
}
