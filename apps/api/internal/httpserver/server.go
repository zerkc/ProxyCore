package httpserver

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/config"
)

type Server struct {
	cfg  config.Config
	mux  *http.ServeMux
	log  *log.Logger
	auth *auth.Service
}

type Option func(*Server)

func WithAuthService(service *auth.Service) Option {
	return func(s *Server) {
		s.auth = service
	}
}

func WithDBPool(pool *pgxpool.Pool) Option {
	return func(s *Server) {
		if pool != nil {
			s.auth = auth.NewService(auth.NewPostgresStore(pool), auth.ServiceOptions{SessionTTL: s.sessionTTL()})
		}
	}
}

func New(cfg config.Config, logger *log.Logger, opts ...Option) *Server {
	if logger == nil {
		logger = log.Default()
	}
	s := &Server{cfg: cfg, mux: http.NewServeMux(), log: logger}
	for _, opt := range opts {
		opt(s)
	}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler {
	return s.mux
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/health", s.handleHealth)
	s.mux.HandleFunc("GET /api/ready", s.handleReady)
	s.mux.HandleFunc("POST /api/auth/bootstrap", s.handleAuthBootstrap)
	s.mux.HandleFunc("POST /api/auth/login", s.handleAuthLogin)
	s.mux.HandleFunc("POST /api/auth/logout", s.handleAuthLogout)
	if proxy := s.nodeAPIProxy(); proxy != nil {
		// Transitional: configuration routes still served by Node (no Next.js).
		s.mux.Handle("/api/", proxy)
	}
	s.mux.Handle("/", s.spaHandler())
}

func (s *Server) nodeAPIProxy() http.Handler {
	if strings.TrimSpace(s.cfg.NodeAPIURL) == "" {
		return nil
	}
	target, err := url.Parse(s.cfg.NodeAPIURL)
	if err != nil {
		s.log.Printf("invalid PROXYCORE_NODE_API_URL: %v", err)
		return nil
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	original := proxy.Director
	proxy.Director = func(req *http.Request) {
		original(req)
		req.Host = target.Host
		req.Header.Set("X-Forwarded-Host", req.Header.Get("Host"))
		req.Header.Set("X-Forwarded-Proto", "http")
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		s.log.Printf("node-api proxy %s: %v", r.URL.Path, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"ok":    false,
			"error": "configuration API unavailable",
		})
	}
	return proxy
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"service":   "proxycore-api",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	// Postgres readiness lands with the Go persistence port.
	// For now the API process itself being up is enough to serve the SPA.
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"service": "proxycore-api",
	})
}

func (s *Server) handleAuthBootstrap(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "auth is not configured"})
		return
	}
	input, ok := decodeCredentials(w, r)
	if !ok {
		return
	}
	user, err := s.auth.Bootstrap(r.Context(), input.Username, input.Password)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrBootstrapComplete):
			writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": err.Error()})
		case errors.Is(err, auth.ErrUsernameInvalid), strings.Contains(err.Error(), "password"):
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		default:
			s.log.Printf("auth bootstrap: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "bootstrap failed"})
		}
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"user": user})
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "auth is not configured"})
		return
	}
	input, ok := decodeCredentials(w, r)
	if !ok {
		return
	}
	session, err := s.auth.Login(r.Context(), input.Username, input.Password)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredential) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		s.log.Printf("auth login: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "login failed"})
		return
	}
	http.SetCookie(w, s.sessionCookie(session.Token, session.ExpiresAt))
	writeJSON(w, http.StatusOK, map[string]any{
		"user":      session.User,
		"expiresAt": session.ExpiresAt,
	})
}

func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "auth is not configured"})
		return
	}
	token := s.tokenFromRequest(r)
	if err := s.auth.Logout(r.Context(), token); err != nil {
		s.log.Printf("auth logout: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "logout failed"})
		return
	}
	http.SetCookie(w, s.clearSessionCookie())
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) requireUser(w http.ResponseWriter, r *http.Request) (auth.User, bool) {
	if s.auth == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "auth is not configured"})
		return auth.User{}, false
	}
	user, err := s.auth.Authenticate(r.Context(), s.tokenFromRequest(r))
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "authentication required"})
		return auth.User{}, false
	}
	return user, true
}

type credentialsRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func decodeCredentials(w http.ResponseWriter, r *http.Request) (credentialsRequest, bool) {
	var input credentialsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON body"})
		return input, false
	}
	return input, true
}

func (s *Server) tokenFromRequest(r *http.Request) string {
	if cookie, err := r.Cookie(s.cookieName()); err == nil {
		return cookie.Value
	}
	const prefix = "Bearer "
	if header := r.Header.Get("Authorization"); strings.HasPrefix(header, prefix) {
		return strings.TrimSpace(strings.TrimPrefix(header, prefix))
	}
	return ""
}

func (s *Server) sessionCookie(token string, expiresAt time.Time) *http.Cookie {
	ttl := s.sessionTTL()
	return &http.Cookie{
		Name:     s.cookieName(),
		Value:    token,
		Path:     "/",
		Expires:  expiresAt,
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.cfg.SecureCookies,
	}
}

func (s *Server) clearSessionCookie() *http.Cookie {
	return &http.Cookie{
		Name:     s.cookieName(),
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.cfg.SecureCookies,
	}
}

func (s *Server) cookieName() string {
	if strings.TrimSpace(s.cfg.SessionCookieName) == "" {
		return "proxycore_session"
	}
	return s.cfg.SessionCookieName
}

func (s *Server) sessionTTL() time.Duration {
	if s.cfg.SessionTTL <= 0 {
		return 8 * time.Hour
	}
	return s.cfg.SessionTTL
}

func (s *Server) spaHandler() http.Handler {
	root := s.cfg.UIDist
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.NotFound(w, r)
				return
			}
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"ok":      false,
				"error":   "ui dist not found",
				"uiDist":  root,
				"hint":    "build apps/ui (pnpm --dir apps/ui build) or set PROXYCORE_UI_DIST",
				"service": "proxycore-api",
			})
		})
	}

	fileServer := http.FileServer(http.Dir(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}

		clean := path.Clean("/" + r.URL.Path)
		full := filepath.Join(root, filepath.FromSlash(clean))
		if !strings.HasPrefix(full, filepath.Clean(root)+string(os.PathSeparator)) &&
			filepath.Clean(full) != filepath.Clean(root) {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}

		st, err := os.Stat(full)
		if err == nil && !st.IsDir() {
			fileServer.ServeHTTP(w, r)
			return
		}
		// SPA fallback
		http.ServeFile(w, r, filepath.Join(root, "index.html"))
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
