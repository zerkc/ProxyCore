package configuration

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/acme"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

var _ RenewalStore = (*SQLiteCertificateIssueStore)(nil)

// RenewLetsEncryptCertificate mirrors Store.RenewLetsEncryptCertificate: it
// re-issues one active Let's Encrypt certificate in place. It is a low level
// primitive; automatic workers go through RenewDueCertificates so the identity
// lease guards the issuance. ACME and the credential lookup run outside any
// Write; persistRenewedCertificate commits the new key secret and the row
// update in ONE Write. On failure only failure_reason is touched.
func (s *SQLiteCertificateIssueStore) RenewLetsEncryptCertificate(ctx context.Context, cert domain.CertificateStatus, opts RenewalOptions) (RenewResult, error) {
	if cert.Issuer != "letsencrypt" {
		return RenewResult{}, errors.New("automatic renewal is only supported for Let's Encrypt certificates")
	}
	if cert.Status != "active" {
		return RenewResult{}, fmt.Errorf("certificate %s is not active", cert.ID)
	}
	if cert.Challenge != "http-01" && cert.Challenge != "dns-01" {
		return RenewResult{}, fmt.Errorf("certificate %s has unsupported challenge %q", cert.ID, cert.Challenge)
	}
	if s.secrets == nil {
		return RenewResult{}, errors.New("Let's Encrypt renewal requires a master key")
	}

	directoryURL := directoryURLForEnvironment(cert.Environment, opts.StagingDirectoryURL, opts.ProductionDirectoryURL)
	if directoryURL == "" {
		return RenewResult{}, errors.New("ACME directory URL is required for renewal")
	}

	options := acme.LetsEncryptOptions{
		Hostnames:    cert.Hostnames,
		DirectoryURL: directoryURL,
		Email:        opts.Email,
		Challenge:    cert.Challenge,
		KeyType:      "rsa",
	}
	if cert.Challenge == "http-01" {
		options.Http01 = acme.GlobalHttp01Store()
	}
	if cert.Challenge == "dns-01" {
		options.PropagationSeconds = 60
		apiToken, zoneID, zoneName, err := s.resolveCloudflareCredentials(ctx, nil)
		if err != nil {
			_ = s.markRenewalFailure(ctx, cert.ID, err.Error())
			return RenewResult{}, err
		}
		options.Dns01 = acme.NewCloudflareDns01Adapter(acme.CloudflareOptions{
			APIToken: apiToken,
			ZoneID:   zoneID,
			ZoneName: zoneName,
		})
	}

	material, err := s.acmeIssue(ctx, options)
	if err != nil {
		_ = s.markRenewalFailure(ctx, cert.ID, err.Error())
		return RenewResult{}, err
	}
	return s.persistRenewedCertificate(ctx, cert, material)
}

// RenewDueCertificates mirrors Store.RenewDueCertificates over the same
// RenewalOptions: without an identity lease or while the live policy forbids
// it, nothing happens; otherwise every due active Let's Encrypt/self-signed
// certificate is renewed under the lease, with the policy rechecked before each
// item (renewDueCertificateItems is shared with PG). Item failures are only
// counted and logged: the sole returned error is the due-list failure. As in
// PG, a renewal that committed but could not enqueue its apply job counts as
// failed. The lease and identity provider are injected (the SQLite identity
// store is T57).
func (s *SQLiteCertificateIssueStore) RenewDueCertificates(ctx context.Context, opts RenewalOptions) (renewed int, failed int, err error) {
	if opts.IdentityLease == nil {
		return 0, 0, nil
	}
	if status := CheckRenewalPolicy(ctx, opts.Identity); !status.Allowed {
		return 0, 0, nil
	}
	now := time.Now().UTC()
	if opts.Now != nil {
		now = opts.Now().UTC()
	}

	due, err := s.ListCertificatesDueForRenewal(ctx, now)
	if err != nil {
		return 0, 0, err
	}
	renewed, failed = renewDueCertificateItems(ctx, opts, due, func(workCtx context.Context, cert domain.CertificateStatus) (RenewResult, error) {
		switch cert.Issuer {
		case "letsencrypt":
			return s.RenewLetsEncryptCertificate(workCtx, cert, opts)
		case "self-signed":
			return s.RenewSelfSignedCertificate(workCtx, cert)
		default:
			return RenewResult{}, nil
		}
	})
	return renewed, failed, nil
}
