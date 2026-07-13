package nineinbox

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

// WebFS contains the dependency-free Nine Inbox PWA.
//
//go:embed web/*
var WebFS embed.FS

type siteHandler struct {
	api http.Handler
	web fs.FS
}

// NewSiteHandler serves the PWA and delegates relay routes to api.
func NewSiteHandler(api http.Handler) http.Handler {
	web, _ := fs.Sub(WebFS, "web")
	return &siteHandler{api: api, web: web}
}

func (h *siteHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/nine/") || r.URL.Path == "/healthz" {
		h.api.ServeHTTP(w, r)
		return
	}
	setSiteHeaders(w)
	if r.URL.Path == "/inbox" && r.Method == http.MethodGet {
		http.Redirect(w, r, "/inbox/", http.StatusPermanentRedirect)
		return
	}
	if r.URL.Path == "/inbox/share" && r.Method == http.MethodPost {
		http.Redirect(w, r, "/inbox/?share=1", http.StatusSeeOther)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodHead)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/inbox/")
	if r.URL.Path == "/inbox/" {
		name = "index.html"
	}
	if name == "" || name != path.Base(name) || strings.ContainsAny(name, `\`) {
		http.NotFound(w, r)
		return
	}
	body, err := fs.ReadFile(h.web, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	contentType := mime.TypeByExtension(path.Ext(name))
	if name == "manifest.webmanifest" {
		contentType = "application/manifest+json"
	}
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	if name == "index.html" || name == "service-worker.js" || name == "manifest.webmanifest" {
		w.Header().Set("Cache-Control", "no-store")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(body)
	}
}

func setSiteHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; manifest-src 'self'; worker-src 'self'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
}
