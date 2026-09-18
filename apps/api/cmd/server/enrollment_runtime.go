package main

import (
	"context"
	"crypto/x509"
	"errors"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/config"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
)

var errEnrollmentTLSRuntimeUnavailable = errors.New("enrollment TLS runtime unavailable")

type enrollmentTLSListenerFactory struct {
	identity  enrollment.EnrollmentTLSIdentityCurrent
	logger    *log.Logger
	listen    func(string, string) (net.Listener, error)
	newServer func(net.Listener, http.Handler, *enrollment.TLSCertificateProvider) (enrollment.EnrollmentTLSListener, error)
	now       func() time.Time
}

func newEnrollmentTLSListenerFactory(identityCurrent enrollment.EnrollmentTLSIdentityCurrent, logger *log.Logger) *enrollmentTLSListenerFactory {
	return &enrollmentTLSListenerFactory{
		identity: identityCurrent,
		logger:   logger,
		listen:   net.Listen,
		now:      time.Now,
		newServer: func(listener net.Listener, handler http.Handler, provider *enrollment.TLSCertificateProvider) (enrollment.EnrollmentTLSListener, error) {
			return enrollment.NewEnrollmentTLSServer(listener, handler, provider)
		},
	}
}

func (f *enrollmentTLSListenerFactory) Listen(ctx context.Context, address string, provider *enrollment.TLSCertificateProvider) (enrollment.EnrollmentTLSListener, error) {
	if f == nil || f.identity == nil || f.listen == nil || f.newServer == nil || provider == nil || strings.TrimSpace(address) == "" {
		return nil, enrollment.ErrEnrollmentTLSBind
	}
	if ctx == nil {
		return nil, errEnrollmentTLSRuntimeUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	listener, err := f.listen("tcp", address)
	if err != nil {
		logEnrollmentTLSFailure(f.logger, "listener bind failed")
		return nil, enrollment.ErrEnrollmentTLSBind
	}
	if err := ctx.Err(); err != nil {
		_ = listener.Close()
		return nil, err
	}

	now := f.now
	if now == nil {
		now = time.Now
	}
	signer, err := enrollment.NewIdentityProofSigner(enrollment.IdentityProofSignerOptions{
		Identity: f.identity.Current,
		Material: provider.Material,
		Now:      now,
	})
	if err != nil {
		_ = listener.Close()
		logEnrollmentTLSFailure(f.logger, "identity proof signer unavailable")
		return nil, enrollment.ErrEnrollmentTLSBind
	}
	handler := httpserver.NewEnrollmentIdentityMux(httpserver.EnrollmentIdentityHandlerOptions{
		Signer: signer,
		Certificate: func(ctx context.Context) (*x509.Certificate, error) {
			return provider.Certificate(ctx)
		},
	})
	server, err := f.newServer(listener, handler, provider)
	if err != nil || server == nil {
		_ = listener.Close()
		logEnrollmentTLSFailure(f.logger, "TLS server construction failed")
		return nil, enrollment.ErrEnrollmentTLSBind
	}
	return server, nil
}

type enrollmentTLSIdentityAdapter struct {
	service *identity.Service
	logger  *log.Logger
}

func (a *enrollmentTLSIdentityAdapter) Current(ctx context.Context) (current identity.Identity, loaded bool, err error) {
	if a == nil || a.service == nil {
		logEnrollmentTLSFailure(identityAdapterLogger(a), "identity unavailable")
		return identity.Identity{}, false, enrollment.ErrEnrollmentTLSIdentityUnavailable
	}
	if ctx == nil {
		return identity.Identity{}, false, errEnrollmentTLSRuntimeUnavailable
	}
	if err := ctx.Err(); err != nil {
		return identity.Identity{}, false, err
	}
	defer func() {
		if recover() != nil {
			current, loaded, err = identity.Identity{}, false, enrollment.ErrEnrollmentTLSIdentityUnavailable
			logEnrollmentTLSFailure(identityAdapterLogger(a), "identity unavailable")
		}
	}()
	current = a.service.Current()
	loaded = true
	return current, loaded, nil
}

type enrollmentTLSConfigurationStore interface {
	GetEnrollmentHostnames(context.Context) (configuration.EnrollmentHostnameConfig, error)
	EnsureEnrollmentTLSMaterial(context.Context) (configuration.EnrollmentTLSMaterial, error)
}

type enrollmentTLSConfigurationAdapter struct {
	store  enrollmentTLSConfigurationStore
	logger *log.Logger
}

func (a *enrollmentTLSConfigurationAdapter) GetEnrollmentHostnames(ctx context.Context) (configuration.EnrollmentHostnameConfig, error) {
	if a == nil || a.store == nil {
		logEnrollmentTLSFailure(configurationAdapterLogger(a), "configuration unavailable")
		return configuration.EnrollmentHostnameConfig{}, enrollment.ErrEnrollmentTLSConfigurationUnavailable
	}
	if ctx == nil {
		return configuration.EnrollmentHostnameConfig{}, errEnrollmentTLSRuntimeUnavailable
	}
	result, err := a.store.GetEnrollmentHostnames(ctx)
	if err != nil {
		logEnrollmentTLSFailure(configurationAdapterLogger(a), "configuration unavailable")
		return configuration.EnrollmentHostnameConfig{}, enrollment.ErrEnrollmentTLSConfigurationUnavailable
	}
	return result, nil
}

func (a *enrollmentTLSConfigurationAdapter) EnsureEnrollmentTLSMaterial(ctx context.Context) (configuration.EnrollmentTLSMaterial, error) {
	if a == nil || a.store == nil {
		logEnrollmentTLSFailure(configurationAdapterLogger(a), "TLS material unavailable")
		return configuration.EnrollmentTLSMaterial{}, enrollment.ErrEnrollmentTLSMaterialUnavailable
	}
	if ctx == nil {
		return configuration.EnrollmentTLSMaterial{}, errEnrollmentTLSRuntimeUnavailable
	}
	material, err := a.store.EnsureEnrollmentTLSMaterial(ctx)
	if err != nil {
		logEnrollmentTLSFailure(configurationAdapterLogger(a), "TLS material unavailable")
		return configuration.EnrollmentTLSMaterial{}, enrollment.ErrEnrollmentTLSMaterialUnavailable
	}
	return material, nil
}

func identityAdapterLogger(adapter *enrollmentTLSIdentityAdapter) *log.Logger {
	if adapter == nil {
		return nil
	}
	return adapter.logger
}

func configurationAdapterLogger(adapter *enrollmentTLSConfigurationAdapter) *log.Logger {
	if adapter == nil {
		return nil
	}
	return adapter.logger
}

func logEnrollmentTLSFailure(logger *log.Logger, message string) {
	if logger != nil {
		logger.Printf("enrollment TLS %s", message)
	}
}

type enrollmentTLSRuntimeDependencies struct {
	identity      enrollment.EnrollmentTLSIdentityCurrent
	configuration enrollment.EnrollmentTLSConfiguration
	listen        func(string, string) (net.Listener, error)
	newServer     func(net.Listener, http.Handler, *enrollment.TLSCertificateProvider) (enrollment.EnrollmentTLSListener, error)
	now           func() time.Time
}

func newEnrollmentTLSSupervisor(cfg config.Config, deps enrollmentTLSRuntimeDependencies, logger *log.Logger) (*enrollment.EnrollmentTLSSupervisor, error) {
	factory := newEnrollmentTLSListenerFactory(deps.identity, logger)
	if deps.listen != nil {
		factory.listen = deps.listen
	}
	if deps.newServer != nil {
		factory.newServer = deps.newServer
	}
	if deps.now != nil {
		factory.now = deps.now
	}
	return enrollment.NewEnrollmentTLSSupervisor(enrollment.EnrollmentTLSSupervisorOptions{
		Identity:        deps.identity,
		Configuration:   deps.configuration,
		ListenerFactory: factory,
		Address:         cfg.EnrollmentTLSAddr,
	})
}

func buildEnrollmentTLSSupervisor(cfg config.Config, identityService *identity.Service, configStore *configuration.Store, logger *log.Logger) (*enrollment.EnrollmentTLSSupervisor, error) {
	identityCurrent := &enrollmentTLSIdentityAdapter{service: identityService, logger: logger}
	configurationStore := &enrollmentTLSConfigurationAdapter{store: configStore, logger: logger}
	return newEnrollmentTLSSupervisor(cfg, enrollmentTLSRuntimeDependencies{
		identity:      identityCurrent,
		configuration: configurationStore,
	}, logger)
}
