// Package webui serves the embedded Mail Relay browser interface.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed assets
var content embed.FS

// Handler serves only the shipped application files.
func Handler() http.Handler {
	assets, err := fs.Sub(content, "assets")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(assets))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/", "/app.js", "/app.css", "/NanumGothic-Regular.woff2", "/NanumGothic-OFL.txt":
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Referrer-Policy", "no-referrer")
			w.Header().Set("Content-Security-Policy", "default-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; font-src 'self'")
			files.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}
