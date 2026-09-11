package snapshot

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
)

// Validator applies every structural and secret-related check that the
// Phase 0 schema defines, returning a typed ValidationResult that the
// importer can use to decide whether to proceed with apply.
type Validator struct {
	kek *cluster.KEK
}

// NewValidator constructs a Validator that decrypts replicated secrets
// with the supplied cluster KEK. The KEK MUST be the one the snapshot was
// produced with; otherwise the secret-availability check will fail.
func NewValidator(kek *cluster.KEK) *Validator {
	return &Validator{kek: kek}
}

// ValidationIssue is one reason a snapshot was rejected. The Phase 1
// importer treats any non-empty Issues slice as a hard failure.
type ValidationIssue struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}

// ValidationResult is the outcome of a validation pass.
type ValidationResult struct {
	OK    bool              `json:"ok"`
	Issues []ValidationIssue `json:"issues,omitempty"`
}

// HasIssues reports whether the result carries any rejection reasons.
func (r ValidationResult) HasIssues() bool {
	return len(r.Issues) > 0
}

// add appends an issue with the given code, field, and message.
func (r *ValidationResult) add(code, field, format string, args ...any) {
	r.Issues = append(r.Issues, ValidationIssue{
		Code:    code,
		Field:   field,
		Message: fmt.Sprintf(format, args...),
	})
	r.OK = false
}

// Validate runs every check and returns the aggregated result. The
// function never returns an error: validation issues are reported through
// ValidationResult.Issues so callers can decide between hard-fail and
// soft-fail policies.
func (v *Validator) Validate(ctx context.Context, env Envelope) ValidationResult {
	result := ValidationResult{OK: true}
	if err := env.Validate(); err != nil {
		result.add("STRUCTURE", "", "structural validation failed: %v", err)
		// Structural failures short-circuit the rest because every other
		// check assumes a well-shaped envelope.
		return result
	}
	if err := CheckCompatibility(env); err != nil {
		var compatErr *CompatibilityError
		if errors.As(err, &compatErr) {
			result.add("COMPATIBILITY", compatErr.Field, "%s", err.Error())
		} else {
			result.add("COMPATIBILITY", "", "%s", err.Error())
		}
		// Compatibility is a hard failure: the rest of the checks assume
		// the local node understands the envelope version.
		return result
	}
	v.validateSecrets(env, &result)
	v.validateOwners(env, &result)
	return result
}

func (v *Validator) validateSecrets(env Envelope, result *ValidationResult) {
	if len(env.Replicated.Secrets) == 0 {
		// No secrets to validate; a nil KEK is acceptable.
		return
	}
	if v.kek == nil {
		result.add("NO_KEY", "kek", "validator has no cluster KEK")
		return
	}
	for _, secret := range env.Replicated.Secrets {
		if secret.ID.String() == "" {
			result.add("SECRET_ID", "replicated.secrets[].id", "secret id is empty")
			continue
		}
		if strings.TrimSpace(secret.Purpose) == "" {
			result.add("SECRET_PURPOSE", "replicated.secrets[].purpose", "purpose is empty")
		}
		if !cluster.IsKEKEnvelope(secret.Envelope) {
			result.add("SECRET_ENVELOPE", "replicated.secrets[].envelope",
				"secret %s: envelope is not a cluster-KEK envelope", secret.ID)
			continue
		}
		if _, err := v.kek.Unwrap(secret.Envelope); err != nil {
			result.add("SECRET_DECRYPT", "replicated.secrets[].envelope",
				"secret %s: cannot decrypt with local KEK: %v", secret.ID, err)
		}
	}
}

func (v *Validator) validateOwners(env Envelope, result *ValidationResult) {
	for _, owner := range env.Replicated.Owners {
		if owner.UserID.String() == "" {
			result.add("OWNER_ID", "replicated.owners[].userId", "owner id is empty")
			continue
		}
		if strings.TrimSpace(owner.Username) == "" {
			result.add("OWNER_USERNAME", "replicated.owners[].username",
				"owner %s: username is empty", owner.UserID)
		}
		if !strings.HasPrefix(owner.PasswordHash, "scrypt$") {
			result.add("OWNER_HASH", "replicated.owners[].passwordHash",
				"owner %s: password hash is not a scrypt-encoded value", owner.UserID)
		}
		if owner.Role != "owner" && owner.Role != "operator" {
			result.add("OWNER_ROLE", "replicated.owners[].role",
				"owner %s: role %q is not owner or operator", owner.UserID, owner.Role)
		}
	}
}
