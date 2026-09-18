package enrollment

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	EnrollmentTLSReadHeaderTimeout = 5 * time.Second
	EnrollmentTLSReadTimeout       = 10 * time.Second
	EnrollmentTLSWriteTimeout      = 10 * time.Second
	EnrollmentTLSIdleTimeout       = 30 * time.Second
	EnrollmentTLSMaxHeaderBytes    = 16 << 10
)

var (
	ErrEnrollmentTLSListenerRequired            = errors.New("enrollment TLS listener is required")
	ErrEnrollmentTLSHandlerRequired             = errors.New("enrollment TLS handler is required")
	ErrEnrollmentTLSCertificateProviderRequired = errors.New("enrollment TLS certificate provider is required")
	ErrEnrollmentTLSServerUnavailable           = errors.New("enrollment TLS server is unavailable")
	ErrEnrollmentTLSServerAlreadyServing        = errors.New("enrollment TLS server is already serving")
	ErrEnrollmentTLSContextRequired             = errors.New("enrollment TLS shutdown context is required")
)

// EnrollmentTLSServer serves only the handler supplied at construction over a
// TLS 1.3 listener. Serve is blocking and does not start a hidden goroutine;
// callers that want asynchronous serving own the goroutine they start.
type EnrollmentTLSServer struct {
	server   *http.Server
	listener net.Listener
	config   *tls.Config

	mu      sync.Mutex
	started bool
}

func NewEnrollmentTLSServer(listener net.Listener, handler http.Handler, provider *TLSCertificateProvider) (*EnrollmentTLSServer, error) {
	if listener == nil {
		return nil, ErrEnrollmentTLSListenerRequired
	}
	if handler == nil {
		return nil, ErrEnrollmentTLSHandlerRequired
	}
	if provider == nil {
		return nil, ErrEnrollmentTLSCertificateProviderRequired
	}

	return &EnrollmentTLSServer{
		server: &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: EnrollmentTLSReadHeaderTimeout,
			ReadTimeout:       EnrollmentTLSReadTimeout,
			WriteTimeout:      EnrollmentTLSWriteTimeout,
			IdleTimeout:       EnrollmentTLSIdleTimeout,
			MaxHeaderBytes:    EnrollmentTLSMaxHeaderBytes,
		},
		listener: listener,
		config: &tls.Config{
			MinVersion:     tls.VersionTLS13,
			GetCertificate: provider.GetCertificate,
			ClientAuth:     tls.NoClientCert,
		},
	}, nil
}

// Serve wraps the supplied listener in tls.NewListener and blocks until the
// listener stops, Shutdown is called, or an accept/serve error occurs.
func (s *EnrollmentTLSServer) Serve() error {
	if s == nil || s.server == nil || s.listener == nil || s.config == nil {
		return ErrEnrollmentTLSServerUnavailable
	}
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return ErrEnrollmentTLSServerAlreadyServing
	}
	s.started = true
	s.mu.Unlock()

	return s.server.Serve(tls.NewListener(s.listener, s.config))
}

// Shutdown gracefully stops accepting connections and waits for active
// requests to finish until ctx expires. Repeated calls are safe.
func (s *EnrollmentTLSServer) Shutdown(ctx context.Context) error {
	if s == nil || s.server == nil {
		return ErrEnrollmentTLSServerUnavailable
	}
	if ctx == nil {
		return ErrEnrollmentTLSContextRequired
	}
	return s.server.Shutdown(ctx)
}
