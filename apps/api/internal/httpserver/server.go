package httpserver

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/config"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

type Server struct {
	cfg            config.Config
	mux            *http.ServeMux
	log            *log.Logger
	auth           *auth.Service
	config         *configuration.Store
	defaultIngress domain.Ingress
}

type Option func(*Server)

func WithAuthService(service *auth.Service) Option {
	return func(s *Server) {
		s.auth = service
	}
}

func WithConfigurationStore(store *configuration.Store) Option {
	return func(s *Server) {
		s.config = store
	}
}

func WithDefaultIngress(ingress domain.Ingress) Option {
	return func(s *Server) {
		s.defaultIngress = ingress
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

	// Configuration API (ported from the transitional node-api).
	s.mux.HandleFunc("GET /api/status", s.handleStatus)
	s.mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	s.mux.HandleFunc("PUT /api/settings", s.handlePutSettings)
	s.mux.HandleFunc("POST /api/apply", s.handleApply)
	s.mux.HandleFunc("GET /api/users", s.handleListUsers)
	s.mux.HandleFunc("POST /api/users", s.handleCreateUser)
	s.mux.HandleFunc("PATCH /api/users/{userId}", s.handleUpdateUser)
	s.mux.HandleFunc("DELETE /api/users/{userId}", s.handleDeleteUser)
	s.mux.HandleFunc("GET /api/zones", s.handleListZones)
	s.mux.HandleFunc("POST /api/zones", s.handleCreateZone)
	s.mux.HandleFunc("GET /api/zones/{zoneId}/records", s.handleGetZoneRecords)
	s.mux.HandleFunc("POST /api/zones/{zoneId}/records", s.handleAddRecord)
	s.mux.HandleFunc("PATCH /api/zones/{zoneId}/records/{recordId}", s.handlePatchRecord)
	s.mux.HandleFunc("GET /api/streams", s.handleListStreams)
	s.mux.HandleFunc("POST /api/streams", s.handleCreateStream)
	s.mux.HandleFunc("PATCH /api/streams/{streamId}", s.handlePatchStream)
	s.mux.HandleFunc("DELETE /api/streams/{streamId}", s.handleDeleteStream)
	s.mux.HandleFunc("GET /api/certificates", s.handleListCertificates)
	s.mux.HandleFunc("GET /api/certificates/ca", s.handleDownloadInternalCA)
	s.mux.HandleFunc("POST /api/certificates/{certificateId}/renew", s.handleRenewSelfSignedCertificate)
	s.mux.HandleFunc("POST /api/certificates", s.handleIssueCertificate)
	s.mux.HandleFunc("GET /api/acme-challenge/{token}", s.handleAcmeChallenge)

	s.mux.Handle("/", s.spaHandler())
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

// requireUser authenticates the caller, enforces roles, and performs the
// ingress-initialization side effect (matching the Node requireUser).
func (s *Server) requireUser(w http.ResponseWriter, r *http.Request, roles ...auth.Role) (auth.User, bool) {
	if s.auth == nil {
		writeConfigError(w, &httpError{status: http.StatusServiceUnavailable, message: "auth is not configured"})
		return auth.User{}, false
	}
	user, err := s.auth.Authenticate(r.Context(), s.tokenFromRequest(r))
	if err != nil {
		writeConfigError(w, &httpError{status: http.StatusUnauthorized, message: "Authentication required"})
		return auth.User{}, false
	}
	if len(roles) > 0 && !roleAllowed(user.Role, roles) {
		writeConfigError(w, &httpError{status: http.StatusForbidden, message: "Permission denied"})
		return auth.User{}, false
	}
	if s.config != nil {
		if err := s.initializeIngressSideEffect(r, user); err != nil {
			writeConfigError(w, err)
			return auth.User{}, false
		}
	}
	return user, true
}

func (s *Server) initializeIngressSideEffect(r *http.Request, user auth.User) error {
	requestIngress := inferRequestIngress(r)
	ingress := domain.Ingress{
		IPv4: firstNonEmpty(s.defaultIngress.IPv4, requestIngress.IPv4),
		IPv6: firstNonEmpty(s.defaultIngress.IPv6, requestIngress.IPv6),
	}
	initialized, err := s.config.InitializeIngress(r.Context(), ingress)
	if err != nil {
		return err
	}
	if !initialized {
		return nil
	}
	settings, err := s.config.GetSettings(r.Context())
	if err != nil {
		return err
	}
	if settings.DefaultPool == nil {
		return nil
	}
	_, err = s.config.CreateApplyJob(r.Context(), user.ID)
	return err
}

func roleAllowed(role auth.Role, roles []auth.Role) bool {
	for _, allowed := range roles {
		if role == allowed {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
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
