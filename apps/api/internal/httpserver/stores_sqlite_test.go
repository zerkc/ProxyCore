package httpserver

import (
	"context"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

// certificateStoreWithoutEnrollmentTrust is CertificateStore minus
// LoadEnrollmentTLSTrust, which belongs to the SQLite identity work (T57). The
// assertion below fails to compile if the unwired SQLite certificate store
// drifts from the rest of the certificate routes contract; keep this interface
// in sync with CertificateStore until T57 lets the SQLite store satisfy
// CertificateStore itself.
type certificateStoreWithoutEnrollmentTrust interface {
	ListCertificates(ctx context.Context) ([]domain.CertificateStatus, error)
	IssueCertificate(ctx context.Context, input configuration.CertificateIssueInput, actorUserID string) (domain.CertificateStatus, error)
	DeleteCertificate(ctx context.Context, id string) error
	RenewSelfSignedCertificateByID(ctx context.Context, id string) (configuration.RenewResult, error)
	EnsureInternalCACertificatePEM(ctx context.Context) (string, error)
}

var _ certificateStoreWithoutEnrollmentTrust = (*configuration.SQLiteCertificateIssueStore)(nil)

// CertificateStore must keep satisfying the subset, so the subset cannot rot.
var _ certificateStoreWithoutEnrollmentTrust = CertificateStore(nil)
