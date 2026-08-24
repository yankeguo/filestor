package main

import (
	"log"
	"net/http"
	"strings"
	"time"
)

const loginFailDelay = time.Second

type Server struct {
	Config     *Config
	store      ObjectStore
	sessionKey []byte
}

func NewServer(cfg *Config, store ObjectStore) *Server {
	s := &Server{Config: cfg, store: store}
	if cfg != nil {
		s.sessionKey = sessionCookieKey(cfg.Admin.Username, cfg.Admin.Password)
	}
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /login", s.handleLogin)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.handleLogout)
	mux.Handle("GET /{$}", s.requireAuth(http.HandlerFunc(s.handleHome)))
	mux.Handle("GET /download", s.requireAuth(http.HandlerFunc(s.handleDownload)))
	return withSecurityHeaders(mux)
}

func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Security-Policy", strings.Join([]string{
			"default-src 'none'",
			"script-src https://cdn.jsdelivr.net 'unsafe-inline'",
			"style-src https://cdn.jsdelivr.net 'unsafe-inline'",
			"font-src https://cdn.jsdelivr.net",
			"img-src 'self' data:",
			"media-src 'self'",
			"connect-src 'self'",
			"form-action 'self'",
			"base-uri 'none'",
			"frame-ancestors 'none'",
		}, "; "))
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("OK"))
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if s.sessionCookieValid(r) {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		s.render(w, "login.html", map[string]any{"Error": ""})
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if !s.checkLogin(r.Form.Get("username"), r.Form.Get("password")) {
			time.Sleep(loginFailDelay)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			s.render(w, "login.html", map[string]any{"Error": "invalid username or password"})
			return
		}
		s.setSessionCookie(w, r)
		http.Redirect(w, r, "/", http.StatusFound)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.clearSessionCookie(w, r)
	http.Redirect(w, r, "/login", http.StatusFound)
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "object store unavailable", http.StatusServiceUnavailable)
		return
	}
	prefix := normalizePrefix(r.URL.Query().Get("prefix"))
	marker := r.URL.Query().Get("marker")
	page, err := s.store.List(prefix, marker)
	if err != nil {
		log.Println("list objects:", err)
		http.Error(w, "list failed", http.StatusBadGateway)
		return
	}
	s.render(w, "home.html", buildBrowseData(prefix, page))
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "object store unavailable", http.StatusServiceUnavailable)
		return
	}
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" {
		http.Error(w, "missing key", http.StatusBadRequest)
		return
	}
	signed, err := s.store.SignGetURL(key, signURLTTL)
	if err != nil {
		log.Println("sign url:", err)
		http.Error(w, "sign failed", http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, signed, http.StatusFound)
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := webTmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Println("template:", err)
	}
}
