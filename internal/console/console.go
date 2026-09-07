// Package console serves the compiled, same-origin administrative console.
package console

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// Assets are produced by `bun --cwd web run build` into this directory.
// Keeping the embed in this package means the production binary needs no JS runtime.
//
//go:embed assets/*
var assets embed.FS

// New returns the console handler. Unknown extensionless routes receive the SPA shell;
// missing static assets remain 404 and are never replaced with HTML.
func New() http.Handler {
	subtree, err := fs.Sub(assets, "assets")
	if err != nil {
		return http.NotFoundHandler()
	}
	files := http.FileServer(http.FS(subtree))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" || name == "." {
			serveIndex(w, subtree)
			return
		}
		if info, statErr := fs.Stat(subtree, name); statErr == nil && !info.IsDir() {
			setAssetHeaders(w, name)
			files.ServeHTTP(w, r)
			return
		}
		if path.Ext(name) == "" {
			serveIndex(w, subtree)
			return
		}
		http.NotFound(w, r)
	})
}

func serveIndex(w http.ResponseWriter, files fs.FS) {
	data, err := fs.ReadFile(files, "index.html")
	if err != nil {
		http.Error(w, "console assets unavailable", http.StatusServiceUnavailable)
		return
	}
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}
func setAssetHeaders(w http.ResponseWriter, name string) {
	setSecurityHeaders(w)
	if strings.HasSuffix(name, ".html") {
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
}
func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; img-src 'self' data: blob:; media-src 'self' blob:; style-src 'self' 'unsafe-inline'; script-src 'self'; base-uri 'self'; frame-ancestors 'none'; form-action 'self'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Frame-Options", "DENY")
}
