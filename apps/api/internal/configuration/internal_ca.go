package configuration

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/zerkc/ProxyCore/apps/api/internal/acme"
)

const internalCAID = "default"

// EnsureInternalCACertificatePEM returns the ProxyCore internal CA certificate,
// creating the CA on first use.
func (s *Store) EnsureInternalCACertificatePEM(ctx context.Context) (string, error) {
	certPEM, _, err := s.ensureInternalCA(ctx)
	return certPEM, err
}

func (s *Store) ensureInternalCA(ctx context.Context) (certPEM, keyPEM string, err error) {
	if s.secrets == nil {
		return "", "", errors.New("internal CA requires a master key")
	}
	certPEM, keyPEM, err = s.loadInternalCA(ctx)
	if err != nil {
		return "", "", err
	}
	if certPEM != "" && keyPEM != "" {
		return certPEM, keyPEM, nil
	}

	material, err := acme.CreateInternalCA(0)
	if err != nil {
		return "", "", err
	}
	secretID, err := s.secrets.Put(ctx, "internal-ca-private-key", material.PrivateKeyPEM)
	if err != nil {
		return "", "", err
	}
	_, err = s.pool.Exec(ctx, `
		insert into internal_ca (id, certificate_pem, key_secret_id)
		values ($1, $2, $3)
		on conflict (id) do nothing
	`, internalCAID, material.CertificatePEM, secretID)
	if err != nil {
		return "", "", fmt.Errorf("persist internal CA: %w", err)
	}
	return s.loadInternalCA(ctx)
}

func (s *Store) loadInternalCA(ctx context.Context) (certPEM, keyPEM string, err error) {
	var secretID string
	err = s.pool.QueryRow(ctx, `
		select certificate_pem, key_secret_id::text
		from internal_ca
		where id = $1
	`, internalCAID).Scan(&certPEM, &secretID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	keyPEM, err = s.secrets.Get(ctx, secretID)
	if err != nil {
		return "", "", err
	}
	if keyPEM == "" {
		return "", "", errors.New("internal CA private key secret is missing")
	}
	return certPEM, keyPEM, nil
}

func (s *Store) issueInternalLeaf(ctx context.Context, hostnames []string, validityDays int) (acme.Material, error) {
	caCertPEM, caKeyPEM, err := s.ensureInternalCA(ctx)
	if err != nil {
		return acme.Material{}, err
	}
	return acme.IssueSignedByCA(hostnames, validityDays, caCertPEM, caKeyPEM)
}
