// Package webui embeds the compiled frontend assets for single-binary deployment.
package webui

import (
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
)

//go:embed all:dist
var webFS embed.FS

// NewHandler returns an http.Handler that serves the embedded SPA.
// If staticDir is non-empty, files are served from the local filesystem (dev mode).
// All non-file, non-dot paths are served as index.html (Vue Router history mode).
func NewHandler(staticDir string) http.Handler {
	var fileServer http.Handler
	if staticDir != "" {
		log.Printf("[webui] dev mode: serving from %s", staticDir)
		fileServer = http.FileServer(http.Dir(staticDir))
	} else {
		sub, err := fs.Sub(webFS, "dist")
		if err != nil {
			log.Fatalf("[webui] embed.FS sub: %v", err)
		}
		fileServer = http.FileServer(http.FS(sub))
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		// Non-file paths (no extension, not root) → serve index.html for SPA routing.
		if p != "/" && !strings.Contains(p, ".") {
			serveIndex(w, r, staticDir)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

func serveIndex(w http.ResponseWriter, r *http.Request, staticDir string) {
	if staticDir != "" {
		http.ServeFile(w, r, staticDir+"/index.html")
		return
	}
	data, err := fs.ReadFile(webFS, "dist/index.html")
	if err != nil {
		// If no frontend is built yet, show a helpful message.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<!doctype html><html><body>
<p>Agent Queue API is running. Run <code>make build-web</code> to build the frontend.</p>
</body></html>`))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

// ServeSPA serves index.html for SPA routing fallback (e.g. from Go API handlers
// that need to yield to Vue Router for browser direct-URL access).
func ServeSPA(w http.ResponseWriter, r *http.Request) {
	serveIndex(w, r, StaticDir())
}

// Stat checks if the embedded dist contains a real build (not just a placeholder).
func Stat() (bool, error) {
	entries, err := fs.ReadDir(webFS, "dist")
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.Name() == "assets" || strings.HasSuffix(e.Name(), ".js") {
			return true, nil
		}
	}
	return false, nil
}

// StaticDir is the resolved directory for dev mode (reads AGENT_QUEUE_STATIC_DIR env).
func StaticDir() string {
	return os.Getenv("AGENT_QUEUE_STATIC_DIR")
}
