package configuration

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/acme"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
)

// issueLetsEncryptFn is overridable in tests.
var issueLetsEncryptFn = acme.IssueLetsEncrypt

// RenewalIdentityProvider returns the current durable identity snapshot. The
// loaded result is explicit so an unloaded identity fails closed.
type RenewalIdentityProvider func(context.Context) (identity.Identity, bool, error)

// RenewalIdentityLease executes one renewal work item under the identity
// service's read lock. The lease, rather than a prior policy snapshot, is the
// authoritative issuance boundary for the automatic worker. Implementations
// require callbacks to avoid identity mutation methods such as TransitionTo
// or ActivateEnrollment; reentrant write acquisition is unsupported.
type RenewalIdentityLease interface {
	WithAutomaticRenewalLease(context.Context, func(context.Context) error) error
}

// RenewalSkipReason is a non-secret explanation for a policy-gated skip.
type RenewalSkipReason string

const RenewalSkipReasonContextCanceled RenewalSkipReason = "context-canceled"

const RenewalSkipReasonIdentityUnavailable RenewalSkipReason = "identity-unavailable"

const RenewalSkipReasonIdentityUnloaded RenewalSkipReason = "identity-unloaded"

const RenewalSkipReasonRoleNotWritable RenewalSkipReason = "role-not-writable"

const RenewalSkipReasonStalePrimary RenewalSkipReason = "stale-primary"

// RenewalPolicyStatus is the observable result of one live identity check.
type RenewalPolicyStatus struct {
	Allowed bool
	Reason  RenewalSkipReason
	Role    domain.TopologyRole
}

// CheckRenewalPolicy permits ordinary certificate renewal only for a loaded,
// current, writable standalone or PRIMARY identity. The check is intentionally
// pure apart from the provider call so callers can repeat it before each
// cycle and work item.
func CheckRenewalPolicy(ctx context.Context, provider RenewalIdentityProvider) RenewalPolicyStatus {
	if ctx == nil || ctx.Err() != nil {
		return RenewalPolicyStatus{Reason: RenewalSkipReasonContextCanceled}
	}
	if provider == nil {
		return RenewalPolicyStatus{Reason: RenewalSkipReasonIdentityUnavailable}
	}
	current, loaded, err := provider(ctx)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			return RenewalPolicyStatus{Reason: RenewalSkipReasonContextCanceled}
		}
		return RenewalPolicyStatus{Reason: RenewalSkipReasonIdentityUnavailable}
	}
	if !loaded {
		return RenewalPolicyStatus{Reason: RenewalSkipReasonIdentityUnloaded}
	}
	status := RenewalPolicyStatus{Role: current.Role}
	if current.IsStalePrimary() {
		status.Reason = RenewalSkipReasonStalePrimary
		return status
	}
	if !current.IsWritable() {
		status.Reason = RenewalSkipReasonRoleNotWritable
		return status
	}
	switch current.Role {
	case domain.TopologyRoleStandalone, domain.TopologyRolePrimary, domain.TopologyRolePrimaryWithNodes:
		status.Allowed = true
		return status
	default:
		status.Reason = RenewalSkipReasonRoleNotWritable
		return status
	}
}

// RenewalOptions configures automatic certificate renewal.
type RenewalOptions struct {
	StagingDirectoryURL    string
	ProductionDirectoryURL string
	Email                  string
	Now                    func() time.Time
	Log                    *log.Logger
	Identity               RenewalIdentityProvider
	// IdentityLease is required by the automatic worker. Direct Store renewal
	// methods remain low-level primitives and do not acquire this lease.
	IdentityLease RenewalIdentityLease
}

// RenewResult is the outcome of one certificate renewal attempt.
type RenewResult struct {
	Certificate domain.CertificateStatus
	Applied     bool
	JobID       string
}

type renewCertificateFunc func(context.Context, domain.CertificateStatus) (RenewResult, error)

// renewDueCertificateItems applies the live policy before every work item so
// a role transition stops future ACME/internal leaf issuance in the same sweep.
func renewDueCertificateItems(ctx context.Context, opts RenewalOptions, due []domain.CertificateStatus, renew renewCertificateFunc) (renewed int, failed int) {
	logger := opts.Log
	if logger == nil {
		logger = log.Default()
	}
	for _, cert := range due {
		if ctx == nil || ctx.Err() != nil {
			return renewed, failed
		}
		if status := CheckRenewalPolicy(ctx, opts.Identity); !status.Allowed {
			return renewed, failed
		}
		if opts.IdentityLease == nil || renew == nil {
			return renewed, failed
		}
		var result RenewResult
		var renewErr error
		leaseErr := opts.IdentityLease.WithAutomaticRenewalLease(ctx, func(workCtx context.Context) error {
			result, renewErr = renew(workCtx, cert)
			return renewErr
		})
		if errors.Is(leaseErr, identity.ErrAutomaticRenewalNotPermitted) || errors.Is(leaseErr, identity.ErrIdentityNotLoaded) {
			return renewed, failed
		}
		if leaseErr != nil {
			if ctx.Err() != nil {
				return renewed, failed
			}
			if renewErr == nil {
				renewErr = leaseErr
			}
		}
		if renewErr != nil {
			failed++
			logger.Printf("certificate renew %s (%s %v): %v", cert.ID, cert.Issuer, cert.Hostnames, renewErr)
			continue
		}
		renewed++
		if result.Applied {
			logger.Printf("certificate renew %s (%s %v): renewed and apply queued (%s)", cert.ID, cert.Issuer, cert.Hostnames, result.JobID)
		} else {
			logger.Printf("certificate renew %s (%s %v): renewed (apply not queued)", cert.ID, cert.Issuer, cert.Hostnames)
		}
	}
	return renewed, failed
}

func directoryURLForEnvironment(environment, stagingURL, productionURL string) string {
	switch strings.ToLower(strings.TrimSpace(environment)) {
	case "production":
		return productionURL
	default:
		return stagingURL
	}
}
