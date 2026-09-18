package enrollment_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
)

func TestNewEnrollmentTLSServerValidatesInputsAndUsesExplicitSafeSettings(t *testing.T) {
	fixture := newProviderFixture(t)
	provider, err := enrollment.NewTLSCertificateProvider(fixture.material(fixture.first, fixture.ca.CertificatePEM))
	if err != nil {
		t.Fatal(err)
	}
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	cases := []struct {
		name string
		ln   net.Listener
		h    http.Handler
		p    *enrollment.TLSCertificateProvider
		want error
	}{
		{"listener", nil, handler, provider, enrollment.ErrEnrollmentTLSListenerRequired},
		{"handler", mustListen(t), nil, provider, enrollment.ErrEnrollmentTLSHandlerRequired},
		{"provider", mustListen(t), handler, nil, enrollment.ErrEnrollmentTLSCertificateProviderRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.ln != nil {
				t.Cleanup(func() { tc.ln.Close() })
			}
			if _, err := enrollment.NewEnrollmentTLSServer(tc.ln, tc.h, tc.p); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}

	ln := mustListen(t)
	defer ln.Close()
	server, err := enrollment.NewEnrollmentTLSServer(ln, handler, provider)
	if err != nil || server == nil {
		t.Fatalf("valid constructor = %v, server=%v", err, server)
	}
	if enrollment.EnrollmentTLSReadHeaderTimeout != 5*time.Second ||
		enrollment.EnrollmentTLSReadTimeout != 10*time.Second ||
		enrollment.EnrollmentTLSWriteTimeout != 10*time.Second ||
		enrollment.EnrollmentTLSIdleTimeout != 30*time.Second ||
		enrollment.EnrollmentTLSMaxHeaderBytes != 16<<10 {
		t.Fatal("enrollment TLS server settings changed from the explicit safe values")
	}
}

func TestEnrollmentTLSServerServeReturnsDeterministicClosedListenerError(t *testing.T) {
	fixture := newProviderFixture(t)
	provider, err := enrollment.NewTLSCertificateProvider(fixture.material(fixture.first, fixture.ca.CertificatePEM))
	if err != nil {
		t.Fatal(err)
	}
	ln := mustListen(t)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	server, err := enrollment.NewEnrollmentTLSServer(ln, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), provider)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Serve(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed listener error = %v", err)
	}
}

func TestEnrollmentTLSServerShutdownIsIdempotentAndOwnsNoCallerGoroutine(t *testing.T) {
	fixture := newProviderFixture(t)
	provider, err := enrollment.NewTLSCertificateProvider(fixture.material(fixture.first, fixture.ca.CertificatePEM))
	if err != nil {
		t.Fatal(err)
	}
	server, err := enrollment.NewEnrollmentTLSServer(mustListen(t), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), provider)
	if err != nil {
		t.Fatal(err)
	}
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Serve()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown = %v", err)
	}
	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("repeated shutdown = %v", err)
	}
	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("serve error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve goroutine did not stop after Shutdown")
	}
}

func TestEnrollmentTLSServerRejectsNilShutdownContext(t *testing.T) {
	fixture := newProviderFixture(t)
	provider, err := enrollment.NewTLSCertificateProvider(fixture.material(fixture.first, fixture.ca.CertificatePEM))
	if err != nil {
		t.Fatal(err)
	}
	server, err := enrollment.NewEnrollmentTLSServer(mustListen(t), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), provider)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Shutdown(nil); !errors.Is(err, enrollment.ErrEnrollmentTLSContextRequired) {
		t.Fatalf("nil context error = %v", err)
	}
}

func mustListen(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return listener
}
