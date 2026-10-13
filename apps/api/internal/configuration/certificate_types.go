package configuration

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

// ErrCertificateNotFound is returned when a certificate id does not exist.
var ErrCertificateNotFound = errors.New("Certificate not found")

// ErrCertificateInUse is returned when a certificate is still referenced by DNS proxy settings.
var ErrCertificateInUse = errors.New("Certificate is still assigned to one or more proxied DNS records")

func scanCertificate(row scanner) (domain.CertificateStatus, error) {
	var (
		cert          domain.CertificateStatus
		hostnamesRaw  []byte
		secretID      *string
		certificate   *string
		failureReason *string
	)
	if err := row.Scan(
		&cert.ID, &hostnamesRaw, &cert.Issuer, &cert.Challenge, &cert.Environment, &cert.Status,
		&cert.ExpiresAt, &cert.RenewAfter, &secretID, &certificate, &failureReason,
	); err != nil {
		return domain.CertificateStatus{}, err
	}
	if len(hostnamesRaw) > 0 {
		if err := json.Unmarshal(hostnamesRaw, &cert.Hostnames); err != nil {
			return domain.CertificateStatus{}, err
		}
	}
	cert.SecretID = deref(secretID)
	cert.CertificatePEM = deref(certificate)
	cert.FailureReason = deref(failureReason)
	return cert, nil
}

func renewalDate(expiresAt time.Time) time.Time {
	return expiresAt.Add(-30 * 24 * time.Hour)
}
