package httpserver

import (
	"context"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

// ConfigurationStore is the persistence surface the configuration handlers
// (settings, ingress bootstrap, zones, records, streams, apply jobs, status and
// enrollment hostnames) depend on. It is owned by this package so any backend
// can be plugged in without importing a concrete store.
type ConfigurationStore interface {
	GetSettings(ctx context.Context) (domain.Settings, error)
	InitializeIngress(ctx context.Context, ingress domain.Ingress) (bool, error)
	UpdateSettings(ctx context.Context, patch configuration.SettingsPatch) (domain.Settings, error)
	ListZones(ctx context.Context) ([]domain.ZoneState, error)
	GetZone(ctx context.Context, id string) (domain.ZoneState, error)
	CreateZone(ctx context.Context, name, actorUserID string) (configuration.AutoApplyResult[domain.ZoneState], error)
	AddRecord(ctx context.Context, zoneID string, input domain.DNSRecordInput, actorUserID string) (configuration.AutoApplyResult[domain.DNSRecord], error)
	ListStreams(ctx context.Context) ([]domain.StreamRoute, error)
	AddStream(ctx context.Context, input domain.StreamRoute) (domain.StreamRoute, error)
	DeleteStream(ctx context.Context, id string) error
	CreateApplyJob(ctx context.Context, actorUserID string) (configuration.ApplyResult, error)
	Status(ctx context.Context) (configuration.StatusResult, error)
	GetEnrollmentHostnames(ctx context.Context) (configuration.EnrollmentHostnameConfig, error)
	UpdateEnrollmentHostnames(ctx context.Context, hostnames []string) (configuration.EnrollmentHostnameConfig, error)
}

// CertificateStore is the persistence surface of the certificate handlers and
// the enrollment trust endpoints.
type CertificateStore interface {
	ListCertificates(ctx context.Context) ([]domain.CertificateStatus, error)
	IssueCertificate(ctx context.Context, input configuration.CertificateIssueInput, actorUserID string) (domain.CertificateStatus, error)
	DeleteCertificate(ctx context.Context, id string) error
	RenewSelfSignedCertificateByID(ctx context.Context, id string) (configuration.RenewResult, error)
	EnsureInternalCACertificatePEM(ctx context.Context) (string, error)
	LoadEnrollmentTLSTrust(ctx context.Context) (configuration.EnrollmentTLSTrust, error)
}

// ConfigurationBackend is the composition the server holds: a single backend
// that serves both configuration and certificate routes. WithConfigurationStore
// takes the composition, so a backend must implement both halves before it can
// be plugged in. A SQLite backend may be assembled by embedding smaller
// implementations (settings, zones, streams, certificates) into one struct.
type ConfigurationBackend interface {
	ConfigurationStore
	CertificateStore
}

var (
	_ ConfigurationBackend = (*configuration.SQLiteConfigurationBackend)(nil)
)
