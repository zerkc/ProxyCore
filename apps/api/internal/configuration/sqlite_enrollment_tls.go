package configuration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/acme"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// sqliteEnrollmentHostnames is pure: unlike SQLiteSettingsStore.GetEnrollmentHostnames,
// it never initializes settings or acquires a writer. It also accepts the caller's
// connection for transactional rechecks.
func sqliteEnrollmentHostnames(ctx context.Context, q sqliteCertificateQuerier) (EnrollmentHostnameConfig, error) {
	var raw sql.NullString
	err := q.QueryRowContext(ctx, `select enrollment_hostnames from installation_settings where id = ?`, installationID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return EnrollmentHostnameConfig{}, nil
	}
	if err != nil {
		return EnrollmentHostnameConfig{}, err
	}
	if !raw.Valid || raw.String == "" {
		return EnrollmentHostnameConfig{}, nil
	}
	var names []string
	if err := json.Unmarshal([]byte(raw.String), &names); err != nil {
		return EnrollmentHostnameConfig{}, fmt.Errorf("read enrollment hostnames: %w", err)
	}
	names, err = NormalizeEnrollmentHostnames(names)
	if err != nil {
		return EnrollmentHostnameConfig{}, fmt.Errorf("read enrollment hostnames: %w", err)
	}
	return EnrollmentHostnameConfig{Configured: len(names) != 0, Hostnames: names}, nil
}

// This comparable snapshot includes all issuance inputs, but not incidental
// timestamps. Changed inputs require signing again outside the writer.
type sqliteEnrollmentState struct {
	names                        string
	established, caPresent       bool
	caPEM, caID, leafPEM, leafID string
}

func sqliteReadEnrollmentState(ctx context.Context, q sqliteCertificateQuerier) (sqliteEnrollmentState, error) {
	var state sqliteEnrollmentState
	names, err := sqliteEnrollmentHostnames(ctx, q)
	if err != nil {
		return state, err
	}
	state.names = strings.Join(names.Hostnames, "\x00")
	if !names.Configured {
		return state, nil
	}
	var stamp string
	err = q.QueryRowContext(ctx, `select established_at from internal_ca_enrollment_state where id = ?`, enrollmentStateID).Scan(&stamp)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return state, err
	}
	state.established = err == nil
	var leaf, leafID sql.NullString
	err = q.QueryRowContext(ctx, `select certificate_pem, key_secret_id, enrollment_certificate_pem, enrollment_key_secret_id from internal_ca where id = ?`, internalCAID).Scan(&state.caPEM, &state.caID, &leaf, &leafID)
	if errors.Is(err, sql.ErrNoRows) {
		if state.established {
			return state, errors.New("established internal CA row is missing")
		}
		return state, nil
	}
	if err != nil {
		return state, err
	}
	state.caPresent = true
	state.leafPEM, state.leafID = leaf.String, leafID.String
	if !state.established && (strings.TrimSpace(state.leafPEM) != "" || strings.TrimSpace(state.leafID) != "") {
		return state, errors.New("enrollment TLS material exists without its establishment marker")
	}
	if state.established && (strings.TrimSpace(state.caPEM) == "" || strings.TrimSpace(state.caID) == "" || strings.TrimSpace(state.leafPEM) == "" || strings.TrimSpace(state.leafID) == "") {
		return state, errors.New("established enrollment TLS material is incomplete")
	}
	return state, nil
}

// LoadEnrollmentTLSTrust never enters Writer.Write, including on missing or
// tampered settings/material. Only validated, redacted trust is returned.
func (s *SQLiteInternalCAStore) LoadEnrollmentTLSTrust(ctx context.Context) (EnrollmentTLSTrust, error) {
	state, err := sqliteReadEnrollmentState(ctx, s.db)
	if err != nil {
		return EnrollmentTLSTrust{}, err
	}
	trust := EnrollmentTLSTrust{Configured: state.names != ""}
	if !trust.Configured || !state.established {
		return trust, nil
	}
	if s.secrets == nil {
		return EnrollmentTLSTrust{}, errors.New("established enrollment TLS material is unavailable")
	}
	material, err := state.material(func(id string) (string, error) { return s.secrets.Get(ctx, id) })
	if err != nil {
		return EnrollmentTLSTrust{}, err
	}
	cert, err := parseSingleCertificatePEM(material.CertificatePEM)
	if err != nil {
		return EnrollmentTLSTrust{}, err
	}
	if !sameEnrollmentHostnames(cert, strings.Split(state.names, "\x00")) {
		return EnrollmentTLSTrust{}, errors.New("established enrollment leaf certificate SANs do not cover configured hostnames")
	}
	trust.Ready, trust.Material = true, material.Public()
	return trust, nil
}

// material validates established keys and certificates without repairing them.
func (state sqliteEnrollmentState) material(get func(string) (string, error)) (EnrollmentTLSMaterial, error) {
	caKey, err := state.caKey(get)
	if err != nil {
		return EnrollmentTLSMaterial{}, err
	}
	key, err := get(state.leafID)
	if err != nil {
		return EnrollmentTLSMaterial{}, err
	}
	if strings.TrimSpace(key) == "" {
		return EnrollmentTLSMaterial{}, errors.New("enrollment leaf private key secret is missing")
	}
	validated, err := validateEnrollmentLeaf(state.leafPEM, key, state.caPEM, caKey)
	if err != nil {
		return EnrollmentTLSMaterial{}, fmt.Errorf("validate established enrollment TLS material: %w", err)
	}
	return newEnrollmentTLSMaterial(state.leafPEM, key, state.caPEM, validated), nil
}

func (state sqliteEnrollmentState) caKey(get func(string) (string, error)) (string, error) {
	if strings.TrimSpace(state.caPEM) == "" || strings.TrimSpace(state.caID) == "" {
		return "", errors.New("established internal CA material is incomplete")
	}
	key, err := get(state.caID)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(key) == "" {
		return "", errors.New("internal CA private key secret is missing")
	}
	if err := acme.ValidateInternalCAMaterial(state.caPEM, key); err != nil {
		return "", fmt.Errorf("validate established internal CA: %w", err)
	}
	return key, nil
}

func sqliteEnrollmentReusable(state sqliteEnrollmentState, material EnrollmentTLSMaterial) bool {
	cert, err := parseSingleCertificatePEM(material.CertificatePEM)
	return err == nil && sameEnrollmentHostnames(cert, strings.Split(state.names, "\x00")) && cert.NotAfter.After(time.Now().UTC().Add(enrollmentLeafRenewalWindow))
}

// EnsureEnrollmentTLSMaterial signs outside Write. ONE atomic write rechecks
// configuration/identity, persists encrypted keys, leaf and establishment marker.
// A concurrent winner is returned; changed signing inputs retry outside Write.
func (s *SQLiteInternalCAStore) EnsureEnrollmentTLSMaterial(ctx context.Context) (EnrollmentTLSMaterial, error) {
	if s.secrets == nil {
		return EnrollmentTLSMaterial{}, errors.New("enrollment TLS requires a master key")
	}
	for {
		if err := ctx.Err(); err != nil {
			return EnrollmentTLSMaterial{}, err
		}
		before, err := sqliteReadEnrollmentState(ctx, s.db)
		if err != nil {
			return EnrollmentTLSMaterial{}, err
		}
		if before.names == "" {
			return EnrollmentTLSMaterial{}, errors.New("enrollment hostnames must be configured before issuing TLS material")
		}
		if !before.established && (before.leafPEM != "" || before.leafID != "") {
			return EnrollmentTLSMaterial{}, errors.New("enrollment TLS material exists without its establishment marker")
		}
		get := func(id string) (string, error) { return s.secrets.Get(ctx, id) }
		caPEM, caKey, key := before.caPEM, "", ""
		if before.caPresent {
			caKey, err = before.caKey(get)
		} else {
			var ca acme.Material
			ca, err = acme.CreateInternalCA(0)
			caPEM, caKey = ca.CertificatePEM, ca.PrivateKeyPEM
		}
		if err != nil {
			return EnrollmentTLSMaterial{}, err
		}
		if before.established {
			existing, err := before.material(get)
			if err != nil {
				return EnrollmentTLSMaterial{}, err
			}
			if sqliteEnrollmentReusable(before, existing) {
				return existing, nil
			}
			key = existing.PrivateKeyPEM
		}
		issued, err := acme.IssueSignedByCAWithKey(strings.Split(before.names, "\x00"), enrollmentLeafValidityDays, caPEM, caKey, key)
		if err != nil {
			return EnrollmentTLSMaterial{}, fmt.Errorf("renew enrollment TLS material: %w", err)
		}
		validated, err := validateEnrollmentLeaf(issued.CertificatePEM, issued.PrivateKeyPEM, caPEM, caKey)
		if err != nil {
			return EnrollmentTLSMaterial{}, fmt.Errorf("validate renewed enrollment TLS material: %w", err)
		}
		if !sameEnrollmentHostnames(validated.certificate, strings.Split(before.names, "\x00")) {
			return EnrollmentTLSMaterial{}, errors.New("renewed enrollment leaf SANs do not cover configured hostnames")
		}
		candidate := newEnrollmentTLSMaterial(issued.CertificatePEM, issued.PrivateKeyPEM, caPEM, validated)
		var result EnrollmentTLSMaterial
		retry := false
		err = s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
			current, err := sqliteReadEnrollmentState(v.Context(), v.Conn())
			if err != nil {
				return err
			}
			if !current.established && (current.leafPEM != "" || current.leafID != "") {
				return errors.New("enrollment TLS material exists without its establishment marker")
			}
			get := func(id string) (string, error) { return s.secrets.GetInWrite(v, id) }
			if current.established {
				existing, err := current.material(get)
				if err != nil {
					return err
				}
				if sqliteEnrollmentReusable(current, existing) {
					result = existing
					return nil
				}
			} else if current.caPresent {
				if _, err := current.caKey(get); err != nil {
					return err
				}
			}
			if current != before {
				retry = true
				return nil
			}
			if !current.caPresent {
				caID, err := s.secrets.PutInWrite(v, "internal-ca-private-key", caKey)
				if err != nil {
					return err
				}
				if _, err := v.Conn().ExecContext(v.Context(), `insert into internal_ca (id, certificate_pem, key_secret_id, created_at, updated_at) values (?, ?, ?, ?, ?)`, internalCAID, caPEM, caID, v.Timestamp(), v.Timestamp()); err != nil {
					return fmt.Errorf("persist internal CA: %w", err)
				}
			}
			leafID := current.leafID
			if !current.established {
				leafID, err = s.secrets.PutInWrite(v, EnrollmentLeafKeyPurpose, candidate.PrivateKeyPEM)
				if err != nil || strings.TrimSpace(leafID) == "" {
					if err == nil {
						err = errors.New("secret store returned an empty id")
					}
					return fmt.Errorf("store enrollment leaf private key: %w", err)
				}
			}
			update, err := v.Conn().ExecContext(v.Context(), `update internal_ca set enrollment_certificate_pem = ?, enrollment_key_secret_id = ?, updated_at = ? where id = ?`, candidate.CertificatePEM, leafID, v.Timestamp(), internalCAID)
			if err != nil {
				return fmt.Errorf("persist enrollment TLS material: %w", err)
			}
			n, err := update.RowsAffected()
			if err != nil {
				return err
			}
			if n != 1 {
				return errors.New("persist enrollment TLS material: internal CA row is missing")
			}
			if !current.established {
				if _, err := v.Conn().ExecContext(v.Context(), `insert into internal_ca_enrollment_state (id, established_at) values (?, ?)`, enrollmentStateID, v.Timestamp()); err != nil {
					return fmt.Errorf("mark enrollment TLS identity established: %w", err)
				}
			}
			result = candidate
			return nil
		})
		if err != nil {
			return EnrollmentTLSMaterial{}, err
		}
		if retry {
			continue
		}
		return result, nil
	}
}
