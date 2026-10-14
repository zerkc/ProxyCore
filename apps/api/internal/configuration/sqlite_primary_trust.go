package configuration

import (
	"bytes"
	"context"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/acme"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

const (
	primaryTrustMaxPEMBytes = 64 << 10
	primaryTrustMaxCerts    = 4
)

// ErrPrimaryTrustInvalid is the only validation outcome; it carries no detail
// from the submitted material.
var ErrPrimaryTrustInvalid = errors.New("primary trust anchor invalid")

// PrimaryTrustAnchor is the CA a NODE trusts for one PRIMARY origin.
type PrimaryTrustAnchor struct {
	Origin      string
	CAPEM       string
	Fingerprint string
	UpdatedAt   time.Time
}

// NormalizePrimaryTrustCA validates owner-supplied CA material and returns its
// canonical PEM plus the DER SHA-256 fingerprint the primary displays. Only
// unexpired CA certificates are accepted; private keys and trailing data are
// rejected.
func NormalizePrimaryTrustCA(input string) (string, string, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" || len(trimmed) > primaryTrustMaxPEMBytes {
		return "", "", ErrPrimaryTrustInvalid
	}
	rest := []byte(trimmed)
	var canonical bytes.Buffer
	var firstDER []byte
	count := 0
	for len(bytes.TrimSpace(rest)) > 0 {
		block, remaining := pem.Decode(rest)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 || count == primaryTrustMaxCerts {
			return "", "", ErrPrimaryTrustInvalid
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		now := time.Now()
		if err != nil || !certificate.BasicConstraintsValid || !certificate.IsCA ||
			now.Before(certificate.NotBefore) || !now.Before(certificate.NotAfter) {
			return "", "", ErrPrimaryTrustInvalid
		}
		if count == 0 {
			firstDER = block.Bytes
		}
		canonical.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes}))
		count++
		rest = remaining
	}
	if count == 0 {
		return "", "", ErrPrimaryTrustInvalid
	}
	return canonical.String(), acme.DERHashSHA256(firstDER), nil
}

func canonicalPrimaryTrustOrigin(origin string) (string, bool) {
	parsed, err := url.Parse(origin)
	if err != nil || parsed == nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Hostname() == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		(parsed.Path != "" && parsed.Path != "/") {
		return "", false
	}
	return "https://" + strings.ToLower(parsed.Host), true
}

// SQLitePrimaryTrustStore persists one CA per primary origin.
type SQLitePrimaryTrustStore struct {
	writer *sqlitedb.Writer
}

func NewSQLitePrimaryTrustStore(writer *sqlitedb.Writer) *SQLitePrimaryTrustStore {
	return &SQLitePrimaryTrustStore{writer: writer}
}

// Put validates and stores the CA for the origin, replacing any previous one.
func (s *SQLitePrimaryTrustStore) Put(ctx context.Context, origin, caPEM string) (PrimaryTrustAnchor, error) {
	canonicalOrigin, ok := canonicalPrimaryTrustOrigin(origin)
	if !ok {
		return PrimaryTrustAnchor{}, ErrPrimaryTrustInvalid
	}
	normalized, fingerprint, err := NormalizePrimaryTrustCA(caPEM)
	if err != nil {
		return PrimaryTrustAnchor{}, err
	}
	if ctx == nil || s == nil || s.writer == nil {
		return PrimaryTrustAnchor{}, errors.New("primary trust store unavailable")
	}
	err = s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		_, err := v.Conn().ExecContext(v.Context(), `insert into primary_trust_anchors (origin, ca_pem, ca_der_sha256, updated_at)
			values (?, ?, ?, ?)
			on conflict(origin) do update set ca_pem = excluded.ca_pem,
				ca_der_sha256 = excluded.ca_der_sha256, updated_at = excluded.updated_at`,
			canonicalOrigin, normalized, fingerprint, v.Timestamp())
		return err
	})
	if err != nil {
		return PrimaryTrustAnchor{}, err
	}
	return PrimaryTrustAnchor{Origin: canonicalOrigin, CAPEM: normalized, Fingerprint: fingerprint}, nil
}

// Get returns the CA trusted for the origin, if any.
func (s *SQLitePrimaryTrustStore) Get(ctx context.Context, origin string) (PrimaryTrustAnchor, bool, error) {
	canonicalOrigin, ok := canonicalPrimaryTrustOrigin(origin)
	if !ok || ctx == nil || s == nil || s.writer == nil {
		return PrimaryTrustAnchor{}, false, nil
	}
	var anchor PrimaryTrustAnchor
	err := s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		return v.Conn().QueryRowContext(v.Context(),
			`select origin, ca_pem, ca_der_sha256 from primary_trust_anchors where origin = ?`, canonicalOrigin).
			Scan(&anchor.Origin, &anchor.CAPEM, &anchor.Fingerprint)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return PrimaryTrustAnchor{}, false, nil
	}
	if err != nil {
		return PrimaryTrustAnchor{}, false, err
	}
	return anchor, true, nil
}
