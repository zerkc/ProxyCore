package configuration

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// sqliteImportReplicatedSecrets unwraps every replicated secret with the
// cluster KEK the node received at enrollment (env.NodeLocal.ClusterKeyRef) and
// re-wraps it with the node's own master key, exactly as locally created
// secrets are stored. It runs inside the import's writer transaction, so any
// failure rolls the whole import back, and before the certificate rows are
// written so key_secret_id references resolve.
//
// Semantics: upsert by id. Secrets the snapshot does not carry are left alone
// because they may be node-local (the node's own credential secret) or be
// referenced by node-local rows; the primary's secret set is therefore a
// replace for ids it carries and never a delete.
func sqliteImportReplicatedSecrets(v sqlitedb.WriteValue, env replicationsnapshot.Envelope, keys *cluster.SQLiteKeyStore, master string) error {
	if len(env.Replicated.Secrets) == 0 {
		return nil
	}
	if keys == nil || master == "" || env.NodeLocal.ClusterKeyRef == nil {
		return errors.New("import secrets: cluster key is unavailable")
	}
	material, err := keys.LoadActiveInWrite(v, *env.NodeLocal.ClusterKeyRef)
	if err != nil {
		return errors.New("import secrets: cluster key is unavailable")
	}
	defer material.Destroy()
	raw := material.CopyBytes()
	defer zeroSecretBytes(raw)
	kek, err := cluster.NewKEK(raw)
	if err != nil {
		return errors.New("import secrets: cluster key is invalid")
	}
	defer kek.Destroy()

	for _, secret := range env.Replicated.Secrets {
		if secret.ID == uuid.Nil || strings.TrimSpace(secret.Purpose) == "" {
			return errors.New("import secrets: secret is incomplete")
		}
		plain, err := kek.Unwrap(secret.Envelope)
		if err != nil {
			return fmt.Errorf("import secrets: secret %s cannot be unwrapped", secret.ID)
		}
		ciphertext, err := secrets.EncryptSecret(string(plain), master)
		zeroSecretBytes(plain)
		if err != nil {
			return fmt.Errorf("import secrets: secret %s cannot be re-wrapped", secret.ID)
		}
		if _, err := v.Conn().ExecContext(v.Context(), `insert into secrets (id, purpose, ciphertext, created_at, updated_at)
			values (?,?,?,?,?)
			on conflict(id) do update set purpose = excluded.purpose, ciphertext = excluded.ciphertext, updated_at = excluded.updated_at`,
			secret.ID.String(), secret.Purpose, ciphertext, v.Timestamp(), v.Timestamp()); err != nil {
			return err
		}
	}
	return nil
}

// sqliteImportReplicatedOwners makes the node's login match the primary's.
//
//   - An envelope without owners leaves local users untouched, so a malformed or
//     legacy snapshot can never lock the node out. An envelope with owners must
//     carry at least one with role "owner", otherwise the import fails closed.
//   - A replicated owner is matched by id, then by username. A match keeps the
//     local row id (audit, enrollment and job history reference it with
//     NO ACTION foreign keys) and takes the primary's username, password hash
//     and role, and becomes active. No match inserts the row with the primary's
//     id. password_change_required is kept for matches and 0 for new rows.
//   - Local users the primary does not carry are deactivated, not deleted:
//     history references them and the node-local session table cascades from
//     them. Deactivation makes the node's accounts equal the primary's (login
//     and session resolution both require active). A later import that carries
//     the user again reactivates it.
//   - Sessions are node-local state and are never imported or rewritten.
func sqliteImportReplicatedOwners(v sqlitedb.WriteValue, owners []replicationsnapshot.ReplicatedOwner) error {
	if len(owners) == 0 {
		return nil
	}
	hasOwner := false
	for _, owner := range owners {
		if owner.UserID == uuid.Nil || owner.Username == "" || owner.PasswordHash == "" ||
			(owner.Role != "owner" && owner.Role != "operator") {
			return errors.New("import owners: owner is incomplete")
		}
		hasOwner = hasOwner || owner.Role == "owner"
	}
	if !hasOwner {
		return errors.New("import owners: snapshot carries no owner")
	}
	kept := make([]any, 0, len(owners))
	for _, owner := range owners {
		localID, err := sqliteFindImportedOwner(v, owner)
		if err != nil {
			return err
		}
		if localID == "" {
			localID = owner.UserID.String()
			if _, err := v.Conn().ExecContext(v.Context(), `insert into users
				(id, username, password_hash, role, active, password_change_required, created_at, updated_at)
				values (?,?,?,?,1,0,?,?)`,
				localID, owner.Username, owner.PasswordHash, owner.Role, v.Timestamp(), v.Timestamp()); err != nil {
				return err
			}
		} else {
			if _, err := v.Conn().ExecContext(v.Context(), `update users set username=?, password_hash=?, role=?, active=1, updated_at=? where id=?`,
				owner.Username, owner.PasswordHash, owner.Role, v.Timestamp(), localID); err != nil {
				return err
			}
		}
		kept = append(kept, localID)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(kept)), ",")
	args := append([]any{v.Timestamp()}, kept...)
	_, err := v.Conn().ExecContext(v.Context(),
		`update users set active=0, updated_at=? where active=1 and id not in (`+placeholders+`)`, args...)
	return err
}

// sqliteFindImportedOwner returns the local id of the row that represents the
// replicated owner (by id, then by username), or "" when there is none. A
// username held by a different row than the id match fails closed.
func sqliteFindImportedOwner(v sqlitedb.WriteValue, owner replicationsnapshot.ReplicatedOwner) (string, error) {
	var byID, byName string
	err := v.Conn().QueryRowContext(v.Context(), `select id from users where id = ?`, owner.UserID.String()).Scan(&byID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	err = v.Conn().QueryRowContext(v.Context(), `select id from users where username = ?`, owner.Username).Scan(&byName)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if byID != "" && byName != "" && byID != byName {
		return "", errors.New("import owners: username is held by another local user")
	}
	if byID != "" {
		return byID, nil
	}
	return byName, nil
}

func zeroSecretBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
