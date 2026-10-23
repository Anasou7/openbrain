// Package web serves OpenBrain's HTTP interface beneath a configurable base
// path: the dashboard (browser sessions for people), the JSON API (bearer
// tokens) and the MCP endpoint (actor tokens). All three are thin layers over
// package store.
package web

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"openbrain/internal/mcpserver"
	"openbrain/internal/store"
)

//go:embed templates/*.html static
var assets embed.FS

const maxBody = 1 << 20 // request body limit, bytes

type Config struct {
	// BasePath is the URL prefix OpenBrain is served under, e.g. "/openbrain";
	// "" or "/" serves at the root. Normalise it with NormalizeBasePath.
	BasePath string
	// AdminToken, if set, grants administrative API access (OPENBRAIN_TOKEN).
	// Browsers sign in with user accounts instead.
	AdminToken string
	// TrustedProxies are the addresses whose X-Forwarded-For and
	// X-Forwarded-Proto headers are believed.
	TrustedProxies []netip.Prefix
	Version        string // reported to MCP clients and on the System page
	Log            *slog.Logger
}

type Server struct {
	st      *store.Store
	cfg     Config
	log     *slog.Logger
	base    string // "" or "/prefix"
	pages   map[string]*template.Template
	assetV  map[string]string // static file -> content hash, for cache busting
	limiter *limiter
}

// NormalizeBasePath turns "", "/", "openbrain", "/openbrain/" and the like
// into "" (root) or "/openbrain". Segments may use letters, digits and -._~.
func NormalizeBasePath(p string) (string, error) {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" {
		return "", nil
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.Trim(seg, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~") != "" {
			return "", fmt.Errorf("invalid base path %q: use something like /openbrain", "/"+p)
		}
	}
	return "/" + p, nil
}

func New(st *store.Store, cfg Config) http.Handler {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	base, err := NormalizeBasePath(cfg.BasePath)
	if err != nil {
		panic(err) // callers validate configuration first
	}
	s := &Server{st: st, cfg: cfg, log: cfg.Log, base: base, assetV: hashAssets(), limiter: newLimiter()}
	s.pages = s.parsePages()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /ready", s.ready)
	mux.Handle("/mcp", mcpserver.New(st, cfg.Version, cfg.Log))
	s.apiRoutes(mux)
	s.dashboardRoutes(mux)

	var h http.Handler = mux
	if base != "" {
		h = s.underBase(mux)
	}
	return s.middleware(h)
}

// underBase serves the application only beneath the base path: the bare
// prefix redirects to prefix/, and anything outside it is not ours.
func (s *Server) underBase(next http.Handler) http.Handler {
	strip := http.StripPrefix(s.base, next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == s.base:
			http.Redirect(w, r, s.base+"/", http.StatusMovedPermanently)
		case strings.HasPrefix(r.URL.Path, s.base+"/"):
			strip.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

// url maps an application path ("/login") to its external path
// ("/openbrain/login"). Every link, form action, redirect and cookie path
// goes through it.
func (s *Server) url(path string) string { return s.base + path }

// asset is the cache-busting URL of a static file.
func (s *Server) asset(name string) string {
	return s.url("/static/" + name + "?v=" + s.assetV[name])
}

func hashAssets() map[string]string {
	m := map[string]string{}
	fs.WalkDir(assets, "static", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := assets.ReadFile(path)
			sum := sha256.Sum256(b)
			m[strings.TrimPrefix(path, "static/")] = hex.EncodeToString(sum[:5])
		}
		return nil
	})
	return m
}

// static serves embedded assets. Versioned URLs (see asset) are immutable.
func (s *Server) static() http.Handler {
	sub, _ := fs.Sub(assets, "static")
	files := http.StripPrefix("/static/", http.FileServerFS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("v") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=86400")
		}
		files.ServeHTTP(w, r)
	})
}

// ready reports whether the database is reachable and migrated.
func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if err := s.st.Ready(r.Context()); err != nil {
		s.log.Error("not ready", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; script-src 'self'; img-src 'self'; font-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)

		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)

		// Log writes, failures and auth rejections; successful reads are noise.
		// MCP requests are all POSTs, so the MCP layer logs its own writes.
		level := slog.LevelDebug
		switch {
		case sw.status >= 500:
			level = slog.LevelError
		case sw.status == http.StatusUnauthorized || sw.status == http.StatusForbidden || sw.status == http.StatusTooManyRequests:
			level = slog.LevelWarn
		case r.Method != http.MethodGet && r.Method != http.MethodHead && !strings.HasSuffix(r.URL.Path, "/mcp"):
			level = slog.LevelInfo
		}
		s.log.Log(r.Context(), level, "http", "method", r.Method, "path", r.URL.Path, "status", sw.status,
			"dur", time.Since(start).Round(time.Microsecond))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap and Flush keep streaming responses (MCP over SSE) working through
// the wrapper.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) // a failure here means the client went away
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

// render executes a page into a buffer first, so a template error becomes a
// clean 500 instead of a half-written page.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page string, v *viewer, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	data["V"] = v
	data["Rail"] = railState(r)
	if _, set := data["Err"]; !set {
		data["Err"] = r.URL.Query().Get("err")
	}
	if _, set := data["Msg"]; !set {
		data["Msg"] = r.URL.Query().Get("msg")
	}
	var buf bytes.Buffer
	if err := s.pages[page].ExecuteTemplate(&buf, "page", data); err != nil {
		s.log.Error("render failed", "page", page, "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

// railState is the navigation rail preference, remembered in a plain cookie
// by app.js so the page renders collapsed without a flash.
func railState(r *http.Request) string {
	if c, err := r.Cookie("ob_rail"); err == nil && c.Value == "collapsed" {
		return "collapsed"
	}
	return ""
}
