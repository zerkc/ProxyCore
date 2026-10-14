package configuration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

const (
	nodeSyncStateID         = "default"
	nodeSyncHealthComponent = "node-sync"
)

// ErrNodeSyncUnbound means the installation has no node sync binding.
var ErrNodeSyncUnbound = errors.New("node sync is not bound")

// SQLiteNodeSyncStore keeps the binding in the existing node_state row (the
// columns the continuity schema reserves for it, cleared by promote/demote)
// and the loop's progress as a single node-sync health observation, so no
// schema change is needed.
type SQLiteNodeSyncStore struct {
	writer *sqlitedb.Writer
}

func NewSQLiteNodeSyncStore(writer *sqlitedb.Writer) *SQLiteNodeSyncStore {
	return &SQLiteNodeSyncStore{writer: writer}
}

type nodeSyncProgressWire struct {
	AppliedHash         string `json:"appliedHash"`
	PrimaryHash         string `json:"primaryHash"`
	AckedHash           string `json:"ackedHash"`
	PrimaryAppliedAt    string `json:"primaryAppliedAt,omitempty"`
	LastAttemptAt       string `json:"lastAttemptAt,omitempty"`
	LastSuccessAt       string `json:"lastSuccessAt,omitempty"`
	NextAttemptAt       string `json:"nextAttemptAt,omitempty"`
	ConsecutiveFailures int    `json:"consecutiveFailures"`
	LastError           string `json:"lastError,omitempty"`
	Unlinked            bool   `json:"unlinked,omitempty"`
	UnlinkedAt          string `json:"unlinkedAt,omitempty"`
}

func formatSyncTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseSyncTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

func (p NodeSyncProgress) wire() nodeSyncProgressWire {
	return nodeSyncProgressWire{
		AppliedHash: p.AppliedHash, PrimaryHash: p.PrimaryHash, AckedHash: p.AckedHash,
		PrimaryAppliedAt: formatSyncTime(p.PrimaryAppliedAt), LastAttemptAt: formatSyncTime(p.LastAttemptAt),
		LastSuccessAt: formatSyncTime(p.LastSuccessAt), NextAttemptAt: formatSyncTime(p.NextAttemptAt),
		ConsecutiveFailures: p.ConsecutiveFailures, LastError: p.LastError,
		Unlinked: p.Unlinked, UnlinkedAt: formatSyncTime(p.UnlinkedAt),
	}
}

func (w nodeSyncProgressWire) progress() NodeSyncProgress {
	return NodeSyncProgress{
		AppliedHash: w.AppliedHash, PrimaryHash: w.PrimaryHash, AckedHash: w.AckedHash,
		PrimaryAppliedAt: parseSyncTime(w.PrimaryAppliedAt), LastAttemptAt: parseSyncTime(w.LastAttemptAt),
		LastSuccessAt: parseSyncTime(w.LastSuccessAt), NextAttemptAt: parseSyncTime(w.NextAttemptAt),
		ConsecutiveFailures: w.ConsecutiveFailures, LastError: w.LastError,
		Unlinked: w.Unlinked, UnlinkedAt: parseSyncTime(w.UnlinkedAt),
	}
}

func (p NodeSyncProgress) healthStatus() string {
	switch {
	case p.Unlinked:
		return "unlinked"
	case p.LastError != "":
		return "error"
	case p.Outdated():
		return "outdated"
	default:
		return "ok"
	}
}

func writeNodeSyncProgress(v sqlitedb.WriteValue, progress NodeSyncProgress) error {
	payload, err := json.Marshal(progress.wire())
	if err != nil {
		return err
	}
	if _, err := v.Conn().ExecContext(v.Context(), `delete from health_observations where component = ?`, nodeSyncHealthComponent); err != nil {
		return err
	}
	_, err = v.Conn().ExecContext(v.Context(),
		`insert into health_observations (id, component, status, details, created_at) values (?, ?, ?, ?, ?)`,
		uuid.NewString(), nodeSyncHealthComponent, progress.healthStatus(), string(payload), v.Timestamp())
	return err
}

func (s *SQLiteNodeSyncStore) BindNodeSync(ctx context.Context, binding NodeSyncBinding, appliedHash string, now time.Time) error {
	if ctx == nil || s == nil || s.writer == nil || binding.PrimaryURL == "" || binding.CredentialID == "" {
		return errors.New("node sync binding is incomplete")
	}
	if _, err := uuid.Parse(binding.CredentialID); err != nil {
		return errors.New("node sync credential id is invalid")
	}
	var primaryInstallation any
	if binding.PrimaryInstallationID != "" {
		if _, err := uuid.Parse(binding.PrimaryInstallationID); err != nil {
			return errors.New("node sync primary installation id is invalid")
		}
		primaryInstallation = binding.PrimaryInstallationID
	}
	return s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		if _, err := v.Conn().ExecContext(v.Context(), `insert into node_state
			(id, primary_url, primary_installation_id, credential_id, sync_enabled, consecutive_failures, updated_at)
			values (?, ?, ?, ?, 1, 0, ?)
			on conflict(id) do update set primary_url = excluded.primary_url,
				primary_installation_id = excluded.primary_installation_id,
				credential_id = excluded.credential_id, sync_enabled = 1,
				consecutive_failures = 0, next_attempt_at = null, last_error_code = null,
				updated_at = excluded.updated_at`,
			nodeSyncStateID, binding.PrimaryURL, primaryInstallation, binding.CredentialID, v.Timestamp()); err != nil {
			return err
		}
		return writeNodeSyncProgress(v, NodeSyncProgress{
			AppliedHash: appliedHash, PrimaryHash: appliedHash, LastSuccessAt: now.UTC(),
		})
	})
}

func (s *SQLiteNodeSyncStore) LoadNodeSyncState(ctx context.Context) (NodeSyncState, bool, error) {
	if ctx == nil || s == nil || s.writer == nil {
		return NodeSyncState{}, false, errors.New("node sync store is unavailable")
	}
	var state NodeSyncState
	bound := false
	err := s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		var url, credential, primary sql.NullString
		var enabled int
		err := v.Conn().QueryRowContext(v.Context(),
			`select primary_url, credential_id, primary_installation_id, sync_enabled from node_state where id = ?`,
			nodeSyncStateID).Scan(&url, &credential, &primary, &enabled)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if enabled != 1 || !url.Valid || url.String == "" || !credential.Valid || credential.String == "" {
			return nil
		}
		bound = true
		state.PrimaryURL, state.CredentialID = url.String, credential.String
		state.PrimaryInstallationID = primary.String

		var details sql.NullString
		err = v.Conn().QueryRowContext(v.Context(),
			`select details from health_observations where component = ? order by created_at desc, id desc limit 1`,
			nodeSyncHealthComponent).Scan(&details)
		switch {
		case err == nil && details.Valid:
			var wire nodeSyncProgressWire
			if json.Unmarshal([]byte(details.String), &wire) == nil {
				state.NodeSyncProgress = wire.progress()
			}
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			return err
		}
		if state.AppliedHash == "" {
			// Progress was lost (retention, restore): the imported revision is
			// the durable proof of what was applied.
			var applied sql.NullString
			err := v.Conn().QueryRowContext(v.Context(), `select r.snapshot_content_hash
				from config_revisions r join apply_jobs j on j.revision_id = r.id
				where r.source = 'import' and r.snapshot_content_hash is not null and j.status = 'applied'
				order by r.revision_number desc limit 1`).Scan(&applied)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			state.AppliedHash = applied.String
		}
		return nil
	})
	if err != nil {
		return NodeSyncState{}, false, err
	}
	return state, bound, nil
}

func (s *SQLiteNodeSyncStore) SaveNodeSyncProgress(ctx context.Context, progress NodeSyncProgress) error {
	if ctx == nil || s == nil || s.writer == nil {
		return errors.New("node sync store is unavailable")
	}
	return s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		// A promote/demote that cleared the binding while a cycle was in flight
		// must not resurrect progress for a relationship that no longer exists.
		var enabled int
		err := v.Conn().QueryRowContext(v.Context(),
			`select sync_enabled from node_state where id = ?`, nodeSyncStateID).Scan(&enabled)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && enabled != 1) {
			return ErrNodeSyncUnbound
		}
		if err != nil {
			return err
		}
		return writeNodeSyncProgress(v, progress)
	})
}

var _ NodeSyncStore = (*SQLiteNodeSyncStore)(nil)
