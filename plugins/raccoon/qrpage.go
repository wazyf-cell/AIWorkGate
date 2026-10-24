// Package main — loopback scan-code page.
//
// GET /qr/{sid} renders the Raccoon deep-link payload as a QR the phone app can
// scan. It is deliberately outside the bearer-token wall (a browser cannot send
// that header) but reveals nothing beyond a login code for a session the caller
// just created, and the plugin only listens on 127.0.0.1.
package main

import (
	"html"
	"net/http"
	"strings"
)

func (s *server) handleQRPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/qr/"), "/")
	sess, ok := s.logins.get(id)
	if !ok {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("登录会话不存在或已过期，请在 AIWorkGate 里重新点「开始登录」。\n"))
		return
	}
	svg, err := renderQRSVG(sess.Payload, 240, 4)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "qr_render_failed", err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(qrPageHTML(svg, sess.ID, sess.Payload)))
}

func qrPageHTML(svg, sid, payload string) string {
	return `<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Raccoon 扫码登录</title>
<style>
 body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;
      font-family:-apple-system,"Segoe UI","Microsoft YaHei",sans-serif;background:#f5f6f8;color:#1f2329}
 .card{background:#fff;border-radius:12px;padding:24px;box-shadow:0 4px 24px rgba(0,0,0,.08);text-align:center;width:320px}
 h1{font-size:16px;margin:0 0 6px}
 p{font-size:13px;color:#4e5969;margin:6px 0}
 .qr{width:240px;height:240px;margin:12px auto;border:1px solid #eceef1;border-radius:8px;padding:6px}
 .qr svg{display:block;width:100%;height:100%}
 code{font-size:12px;color:#8a9099;word-break:break-all}
</style></head>
<body><div class="card">
<h1>登录 Raccoon Work</h1>
<p>打开手机上的 Raccoon（商汤小浣熊）App，用「扫一扫」扫描下方二维码并在手机上确认。</p>
<div class="qr">` + svg + `</div>
<p>确认后本页会自动完成，回到 AIWorkGate 即可看到账号。</p>
<p><code>` + html.EscapeString(payload) + `</code></p>
<p style="color:#8a9099">会话：` + html.EscapeString(sid) + `（5 分钟内有效）</p>
</div></body></html>`
}
