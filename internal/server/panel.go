package server

import (
	_ "embed"
	"net/http"
	"strings"
)

//go:embed panel.html
var panelHTMLRaw string

func (h *Handler) servePanel(w http.ResponseWriter, r *http.Request) {
	// 仅根路径与 /panel，避免吞掉其他路由（Go 1.22 精确匹配已处理，这里再保险）
	if r.URL.Path != "/" && r.URL.Path != "/panel" && r.URL.Path != "/panel/" {
		http.NotFound(w, r)
		return
	}
	html := panelHTMLRaw
	html = strings.ReplaceAll(html, "__SERVICE_NAME__", "workbuddy2api")
	html = strings.ReplaceAll(html, "__SERVICE_TITLE__", "WorkBuddy2API")
	html = strings.ReplaceAll(html, "__LOGO__", "WB")
	html = strings.ReplaceAll(html, "__ACCENT__", "#34d399")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(html))
}
