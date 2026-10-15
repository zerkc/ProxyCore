package configuration

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

var (
	// ErrEnrollmentTokenNotFound reports an unknown token or one owned by
	// another user; the two are indistinguishable by design.
	ErrEnrollmentTokenNotFound = errors.New("enrollment token not found")
	// ErrEnrollmentTokenActive reports a token that is still redeemable and
	// must be revoked before it can be deleted.
	ErrEnrollmentTokenActive = errors.New("enrollment token is active")
	// ErrEnrollmentTokenInUse reports a token that was consumed or that minted
	// a node grant; its grant still authenticates the node.
	ErrEnrollmentTokenInUse = errors.New("enrollment token is in use")
)

// SQLiteEnrollmentTokenStore is an unwired, writer-only token lifecycle store.
// Even non-consuming checks take the writer, matching PG token serialization.
type SQLiteEnrollmentTokenStore struct {
	writer *sqlitedb.Writer
}

func NewSQLiteEnrollmentTokenStore(writer *sqlitedb.Writer) *SQLiteEnrollmentTokenStore {
	return &SQLiteEnrollmentTokenStore{writer: writer}
}

func sqliteEnrollmentTokenWriter(s *SQLiteEnrollmentTokenStore, ctx context.Context) error {
	if ctx == nil {
		return errors.New("enrollment token context is required")
	}
	if s == nil || s.writer == nil {
		return errors.New("enrollment token writer is required")
	}
	return nil
}

func sqliteEnrollmentTokenTime(at time.Time) (string, error) {
	key, err := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: at.Unix(), Nanos: int32(at.Nanosecond())})
	if err != nil {
		return "", err
	}
	return *key, nil
}

// CreateEnrollmentToken promotes an eligible identity and inserts its token in
// one atomic Write. Any insertion failure rolls back the identity timestamp too.
func (s *SQLiteEnrollmentTokenStore) CreateEnrollmentToken(ctx context.Context, id, selector, hash, version, ownerID string, createdAt, expiresAt time.Time) (domain.TopologyRole, error) {
	if err := sqliteEnrollmentTokenWriter(s, ctx); err != nil {
		return "", err
	}
	var role domain.TopologyRole
	err := s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		if version != EnrollmentTokenHashVersion {
			return errEnrollmentTokenDenied
		}
		canonicalID, err := sqliteCertificateLookupID(id)
		if err != nil {
			return err
		}
		canonicalOwner, err := sqliteCertificateLookupID(ownerID)
		if err != nil {
			return err
		}
		created, err := sqliteEnrollmentTokenTime(createdAt)
		if err != nil {
			return err
		}
		expires, err := sqliteEnrollmentTokenTime(expiresAt)
		if err != nil {
			return err
		}
		var eligible string
		err = v.Conn().QueryRowContext(v.Context(), `update installation_identity
   set role = case when role = 'standalone-primary' then 'primary' else role end, updated_at = ?
   where id = ? and role in ('standalone-primary','primary','primary-with-nodes')
   and leadership_generation >= latest_known_generation returning role`, created, installationID).Scan(&eligible)
		if errors.Is(err, sql.ErrNoRows) {
			return errEnrollmentTokenDenied
		}
		if err != nil {
			return err
		}
		_, err = v.Conn().ExecContext(v.Context(), `insert into enrollment_tokens
   (id,token_selector,token_hash,hash_version,created_by_user_id,created_at,expires_at)
   values (?,?,?,?,?,?,?)`, canonicalID, selector, hash, version, canonicalOwner, created, expires)
		if err == nil {
			role = domain.TopologyRole(eligible)
		}
		return err
	})
	if err != nil {
		return "", err
	}
	return role, nil
}

func (s *SQLiteEnrollmentTokenStore) CheckEnrollmentToken(ctx context.Context, selector, hash string, now time.Time, consume bool) error {
	if err := sqliteEnrollmentTokenWriter(s, ctx); err != nil {
		return err
	}
	return s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		var id, storedHash, version, expires string
		var consumed, revoked *string
		err := v.Conn().QueryRowContext(v.Context(), `select id,token_hash,hash_version,expires_at,consumed_at,revoked_at
   from enrollment_tokens where token_selector = ?`, selector).Scan(&id, &storedHash, &version, &expires, &consumed, &revoked)
		if errors.Is(err, sql.ErrNoRows) {
			return errEnrollmentTokenDenied
		}
		if err != nil {
			return err
		}
		// Validate the persisted canonical key, retaining both infinity tags. Binary
		// key ordering spans the epoch and nanoseconds without calendar conversion.
		if _, err = sqlitedb.DecodeTimestamp(&expires); err != nil {
			return err
		}
		stamp, err := sqliteEnrollmentTokenTime(now)
		if err != nil {
			return err
		}
		hashMatches := subtle.ConstantTimeCompare([]byte(storedHash), []byte(hash)) == 1
		if version != EnrollmentTokenHashVersion || !hashMatches || expires <= stamp || consumed != nil || revoked != nil {
			return errEnrollmentTokenDenied
		}
		if !consume {
			return nil
		}
		result, err := v.Conn().ExecContext(v.Context(), `update enrollment_tokens set consumed_at = ? where id = ? and consumed_at is null`, stamp, id)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return errEnrollmentTokenDenied
		}
		return nil
	})
}

func (s *SQLiteEnrollmentTokenStore) RevokeEnrollmentToken(ctx context.Context, id, ownerID string, now time.Time) error {
	if err := sqliteEnrollmentTokenWriter(s, ctx); err != nil {
		return err
	}
	return s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		canonicalID, err := sqliteCertificateLookupID(id)
		if err != nil {
			return err
		}
		canonicalOwner, err := sqliteCertificateLookupID(ownerID)
		if err != nil {
			return err
		}
		stamp, err := sqliteEnrollmentTokenTime(now)
		if err != nil {
			return err
		}
		_, err = v.Conn().ExecContext(v.Context(), `update enrollment_tokens set revoked_at = coalesce(revoked_at,?) where id = ? and created_by_user_id = ?`, stamp, canonicalID, canonicalOwner)
		return err
	})
}

// DeleteEnrollmentToken removes an inactive token that has no grant. Lookup,
// eligibility checks and the delete share one writer transaction.
func (s *SQLiteEnrollmentTokenStore) DeleteEnrollmentToken(ctx context.Context, id, ownerID string, now time.Time) error {
	if err := sqliteEnrollmentTokenWriter(s, ctx); err != nil {
		return err
	}
	return s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		canonicalID, err := sqliteCertificateLookupID(id)
		if err != nil {
			return err
		}
		canonicalOwner, err := sqliteCertificateLookupID(ownerID)
		if err != nil {
			return err
		}
		stamp, err := sqliteEnrollmentTokenTime(now)
		if err != nil {
			return err
		}
		var expires string
		var consumed, revoked *string
		var granted int
		err = v.Conn().QueryRowContext(v.Context(), `select t.expires_at,t.consumed_at,t.revoked_at,
   exists(select 1 from enrollment_grants g where g.token_id = t.id)
   from enrollment_tokens t where t.id = ? and t.created_by_user_id = ?`, canonicalID, canonicalOwner).Scan(&expires, &consumed, &revoked, &granted)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrEnrollmentTokenNotFound
		}
		if err != nil {
			return err
		}
		if _, err = sqlitedb.DecodeTimestamp(&expires); err != nil {
			return err
		}
		switch {
		case granted != 0:
			return ErrEnrollmentTokenInUse
		case revoked != nil:
		case consumed != nil:
			return ErrEnrollmentTokenInUse
		case expires > stamp:
			return ErrEnrollmentTokenActive
		}
		result, err := v.Conn().ExecContext(v.Context(), `delete from enrollment_tokens where id = ? and created_by_user_id = ?
   and not exists(select 1 from enrollment_grants g where g.token_id = enrollment_tokens.id)`, canonicalID, canonicalOwner)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrEnrollmentTokenNotFound
		}
		return nil
	})
}
