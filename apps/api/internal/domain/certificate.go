package domain

import "time"

// CertificateStatus is the stored/reported state of a certificate.
type CertificateStatus struct {
	ID             string     `json:"id"`
	Hostnames      []string   `json:"hostnames"`
	Issuer         string     `json:"issuer"`
	Challenge      string     `json:"challenge"`
	Environment    string     `json:"environment"`
	Status         string     `json:"status"`
	ExpiresAt      *time.Time `json:"expiresAt,omitempty"`
	RenewAfter     *time.Time `json:"renewAfter,omitempty"`
	SecretID       string     `json:"secretId,omitempty"`
	CertificatePEM string     `json:"certificatePem,omitempty"`
	FailureReason  string     `json:"failureReason,omitempty"`
}

// PublicCertificate strips the secret material for API responses (matches the
// Node routes that omit secretId and certificatePem).
func (c CertificateStatus) PublicCertificate() map[string]any {
	view := map[string]any{
		"id":          c.ID,
		"hostnames":   c.Hostnames,
		"issuer":      c.Issuer,
		"challenge":   c.Challenge,
		"environment": c.Environment,
		"status":      c.Status,
	}
	if c.ExpiresAt != nil {
		view["expiresAt"] = c.ExpiresAt
	}
	if c.RenewAfter != nil {
		view["renewAfter"] = c.RenewAfter
	}
	if c.FailureReason != "" {
		view["failureReason"] = c.FailureReason
	}
	return view
}
