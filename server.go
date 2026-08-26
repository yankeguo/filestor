package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"
)

const loginFailDelay = time.Second

type Server struct {
	Config     *Config
	store      ObjectStore
	index      *bundleIndex
	sessionKey []byte
	hub        *eventHub
	state      *workspaceStateStore
}

func NewServer(cfg *Config, store ObjectStore) *Server {
	s := &Server{Config: cfg, store: store, index: newBundleIndex(), hub: newEventHub()}
	s.state = newWorkspaceStateStore(s.workspaceDir())
	if store != nil {
		if err := s.index.load(store); err != nil {
			log.Println("load bundle index:", err)
		}
	}
	if cfg != nil {
		s.sessionKey = sessionCookieKey(cfg.Admin.Username, cfg.Admin.Password)
		if err := prepWorkspace(s.workspaceDir()); err != nil {
			log.Println("prepare workspace:", err)
		}
		// The digest embedding pipeline needs both sides; with only one
		// configured it stays off.
		if e, v := cfg.LLM.Embeddings.BailianMultimodalEmbedding, cfg.LLM.Vectors.AliyunOSSVectors; (e.URL != "") != (v.URL != "") {
			log.Println("config: llm.embeddings and llm.vectors take effect only together; digest embedding is off")
		}
	}
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /login", s.handleLogin)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.handleLogout)
	mux.HandleFunc("GET /{$}", s.handleRoot)
	mux.Handle("GET /browse", s.requireAuth(http.HandlerFunc(s.handleBrowse)))
	mux.Handle("GET /bundle/{id}", s.requireAuth(http.HandlerFunc(s.handleBundle)))
	mux.Handle("GET /download", s.requireAuth(http.HandlerFunc(s.handleDownload)))
	mux.Handle("GET /preview", s.requireAuth(http.HandlerFunc(s.handlePreview)))
	mux.Handle("GET /static/", s.requireAuth(staticHandler()))
	mux.Handle("GET /upload", s.requireAuth(http.HandlerFunc(s.handleUploadPage)))
	mux.Handle("GET /upload/files", s.requireAuth(http.HandlerFunc(s.handleUploadList)))
	mux.Handle("POST /upload/files", s.requireAuth(http.HandlerFunc(s.handleUploadAdd)))
	mux.Handle("DELETE /upload/files", s.requireAuth(http.HandlerFunc(s.handleUploadDelete)))
	mux.Handle("PUT /upload/state", s.requireAuth(http.HandlerFunc(s.handleUploadState)))
	mux.Handle("POST /upload/analyze", s.requireAuth(http.HandlerFunc(s.handleUploadAnalyze)))
	mux.Handle("POST /upload/push", s.requireAuth(http.HandlerFunc(s.handleUploadPush)))
	mux.Handle("GET /upload/events", s.requireAuth(http.HandlerFunc(s.handleUploadEvents)))
	return s.withSecurityHeaders(mux)
}

func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	imgSrc := "img-src 'self' data: https:"
	mediaSrc := "media-src 'self' https:"
	if s.Config != nil && strings.HasPrefix(s.Config.S3.Endpoint, "http://") {
		// A plain-http object store (e.g. local MinIO) can only render
		// previews if images and media may load over http too.
		imgSrc += " http:"
		mediaSrc += " http:"
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Security-Policy", strings.Join([]string{
			"default-src 'none'",
			"script-src 'self' https://cdn.jsdelivr.net",
			"style-src https://cdn.jsdelivr.net 'unsafe-inline'",
			"font-src https://cdn.jsdelivr.net",
			imgSrc,
			mediaSrc,
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
			http.Redirect(w, r, "/browse", http.StatusFound)
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
		http.Redirect(w, r, "/browse", http.StatusFound)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.clearSessionCookie(w, r)
	http.Redirect(w, r, "/login", http.StatusFound)
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/browse", http.StatusFound)
}

func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "object store unavailable", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	// Calendar view over the in-memory monthly indexes (loaded at startup,
	// updated by pushes).
	now := time.Now()
	var day time.Time
	if d := q.Get("day"); d != "" {
		if parsed, err := time.ParseInLocation(browseDayLayout, d, time.Local); err == nil {
			day = parsed
		}
	}
	var month time.Time
	if m := q.Get("month"); m != "" {
		if parsed, err := time.ParseInLocation(browseMonthLayout, m, time.Local); err == nil {
			month = parsed
		}
	}
	if month.IsZero() {
		if !day.IsZero() {
			month = day
		} else {
			month = now
		}
	}
	month = time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, time.Local)
	// Landing on /browse selects today; a day outside the displayed month is
	// dropped rather than shown against the wrong calendar.
	if q.Get("month") == "" && q.Get("day") == "" {
		day = now
	} else if !day.IsZero() && (day.Year() != month.Year() || day.Month() != month.Month()) {
		day = time.Time{}
	}
	s.render(w, "home.html", buildBrowseCalendar(month, day, s.index.year(month.Year()), now))
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
	if strings.ContainsRune(key, 0) || strings.ContainsAny(key, "\r\n") {
		http.Error(w, "invalid key", http.StatusBadRequest)
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

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "object store unavailable", http.StatusServiceUnavailable)
		return
	}
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" {
		http.Error(w, "missing key", http.StatusBadRequest)
		return
	}
	if strings.ContainsRune(key, 0) || strings.ContainsAny(key, "\r\n") {
		http.Error(w, "invalid key", http.StatusBadRequest)
		return
	}
	// No attachment disposition: the browser renders the object inline.
	signed, err := s.store.SignPreviewURL(key, signURLTTL)
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Println("encode json:", err)
	}
}
