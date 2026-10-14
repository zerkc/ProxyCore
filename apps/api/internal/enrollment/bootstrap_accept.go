package enrollment

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// NodeCredentialSecretPurpose labels the secrets row holding a node's raw
// bearer secret (base64url, master-key encrypted). The row id equals the
// primary-assigned credential id so it can be located without a link table.
const NodeCredentialSecretPurpose = "node-credential"

// ErrBootstrapPersistence means the grant was authentic but could not be
// stored (database failure). Nothing was committed; the caller may retry the
// same payload. Every authenticity, binding, hash and expiry failure is
// ErrBootstrapDenied instead.
var ErrBootstrapPersistence = errors.New("bootstrap persistence failed")

// AcceptedBootstrap is the non-secret outcome of a successful acceptance. The
// converter reads the key with cluster.SQLiteKeyStore.LoadActive(ClusterKeyID)
// and the credential with BootstrapAcceptor.LoadNodeCredentialSecret(CredentialID).
type AcceptedBootstrap struct {
	CredentialID                string
	ClusterKeyID                string
	SourcePrimaryInstallationID string
	LeadershipGeneration        uint64
	ExpiresAt                   time.Time
}

type BootstrapAcceptorOptions struct {
	DB              *sql.DB
	Writer          *sqlitedb.Writer
	Keys            *cluster.SQLiteKeyStore
	MasterKeyBase64 string
	Now             func() time.Time
}

// BootstrapAcceptor opens a sealed bootstrap payload and persists the cluster
// KEK and node credential in a single SQLite transaction.
type BootstrapAcceptor struct {
	db     *sql.DB
	writer *sqlitedb.Writer
	keys   *cluster.SQLiteKeyStore
	master string
	now    func() time.Time

	// Test seams; nil in production.
	afterKEKWrite  func() error
	onGrant        func(*BootstrapGrant)
	observeSecrets func(credential, kek []byte)
}

func NewBootstrapAcceptor(opts BootstrapAcceptorOptions) *BootstrapAcceptor {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &BootstrapAcceptor{db: opts.DB, writer: opts.Writer, keys: opts.Keys, master: opts.MasterKeyBase64, now: now}
}

// Accept verifies payloadHash (hex SHA-256 of sealed), opens the envelope with
// the recipient key under binding, rejects expired grants and persists the
// KEK and credential atomically. Replaying the same payload is a no-op success;
// conflicting material under the same ids is denied. All plaintext buffers are
// zeroed before return, including on error paths.
func (a *BootstrapAcceptor) Accept(ctx context.Context, recipient *BootstrapRecipient, binding BootstrapBinding, sealed []byte, payloadHash string) (AcceptedBootstrap, error) {
	if a == nil || a.db == nil || a.writer == nil || a.keys == nil || a.master == "" || ctx == nil || recipient == nil || len(sealed) == 0 {
		return AcceptedBootstrap{}, ErrBootstrapDenied
	}
	sum := sha256.Sum256(sealed)
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(payloadHash)) != 1 {
		return AcceptedBootstrap{}, ErrBootstrapDenied
	}
	grant, err := recipient.OpenBootstrap(binding, sealed)
	if err != nil {
		return AcceptedBootstrap{}, ErrBootstrapDenied
	}
	defer grant.Destroy()
	if a.onGrant != nil {
		a.onGrant(&grant)
	}
	expiresAt := time.Unix(grant.Metadata.ExpiresAtUnix, 0).UTC()
	if !expiresAt.After(a.now()) {
		return AcceptedBootstrap{}, ErrBootstrapDenied
	}
	credentialID, err := uuid.Parse(grant.Metadata.CredentialID)
	if err != nil {
		return AcceptedBootstrap{}, ErrBootstrapDenied
	}
	keyID, err := uuid.Parse(grant.Metadata.ClusterKeyID)
	if err != nil || keyID == uuid.Nil || credentialID == uuid.Nil {
		return AcceptedBootstrap{}, ErrBootstrapDenied
	}

	credential := grant.Secrets.NodeCredential.CopyBytes()
	defer zeroBytes(credential)
	kek := grant.Secrets.ClusterKEK.CopyBytes()
	defer zeroBytes(kek)
	if a.observeSecrets != nil {
		a.observeSecrets(credential, kek)
	}

	err = a.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		if err := a.keys.ImportInWrite(v, keyID, kek); err != nil {
			return err
		}
		if a.afterKEKWrite != nil {
			if err := a.afterKEKWrite(); err != nil {
				return err
			}
		}
		return a.putCredentialInWrite(v, credentialID.String(), credential)
	})
	if err != nil {
		if errors.Is(err, cluster.ErrClusterKeyDenied) || errors.Is(err, ErrBootstrapDenied) {
			return AcceptedBootstrap{}, ErrBootstrapDenied
		}
		return AcceptedBootstrap{}, ErrBootstrapPersistence
	}
	return AcceptedBootstrap{
		CredentialID:                credentialID.String(),
		ClusterKeyID:                keyID.String(),
		SourcePrimaryInstallationID: grant.Metadata.SourcePrimaryInstallationID,
		LeadershipGeneration:        grant.Metadata.LeadershipGeneration,
		ExpiresAt:                   expiresAt,
	}, nil
}

func (a *BootstrapAcceptor) putCredentialInWrite(v sqlitedb.WriteValue, id string, secret []byte) error {
	encoded := make([]byte, base64.RawURLEncoding.EncodedLen(len(secret)))
	base64.RawURLEncoding.Encode(encoded, secret)
	defer zeroBytes(encoded)

	var existing string
	err := v.Conn().QueryRowContext(v.Context(), `select ciphertext from secrets where id = ?`, id).Scan(&existing)
	switch {
	case err == nil:
		plaintext, derr := secrets.DecryptSecret(existing, a.master)
		if derr != nil || subtle.ConstantTimeCompare([]byte(plaintext), encoded) != 1 {
			return ErrBootstrapDenied
		}
		return nil
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	ciphertext, err := secrets.EncryptSecret(string(encoded), a.master)
	if err != nil {
		return err
	}
	_, err = v.Conn().ExecContext(v.Context(),
		`insert into secrets (id, purpose, ciphertext, created_at, updated_at) values (?,?,?,?,?)`,
		id, NodeCredentialSecretPurpose, ciphertext, v.Timestamp(), v.Timestamp())
	return err
}

// LoadNodeCredentialSecret returns the raw 32-byte bearer secret stored for the
// credential id. The caller owns the slice and must zero it.
func (a *BootstrapAcceptor) LoadNodeCredentialSecret(ctx context.Context, credentialID string) ([]byte, error) {
	parsed, err := uuid.Parse(credentialID)
	if a == nil || a.db == nil || ctx == nil || err != nil || parsed == uuid.Nil {
		return nil, ErrBootstrapDenied
	}
	var ciphertext string
	if err := a.db.QueryRowContext(ctx,
		`select ciphertext from secrets where id = ? and purpose = ?`, parsed.String(), NodeCredentialSecretPurpose).Scan(&ciphertext); err != nil {
		return nil, ErrBootstrapDenied
	}
	plaintext, err := secrets.DecryptSecret(ciphertext, a.master)
	if err != nil {
		return nil, ErrBootstrapDenied
	}
	secret, err := base64.RawURLEncoding.DecodeString(plaintext)
	if err != nil || len(secret) != bootstrapCredentialBytes {
		zeroBytes(secret)
		return nil, ErrBootstrapDenied
	}
	return secret, nil
}
