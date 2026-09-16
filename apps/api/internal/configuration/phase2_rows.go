package configuration

import "context"

const phase2NodeStateSelect = `
	select id, enrolled_at, enrollment_token_hash, enrollment_primary_id::text,
		last_seen_at, last_applied_snapshot_id::text, enrollment_attempt_id::text,
		primary_url, primary_installation_id::text, primary_tls_spki_sha256,
		credential_id::text, sync_enabled, last_attempt_at, last_success_at,
		consecutive_failures, next_attempt_at, last_error_code, updated_at
	from node_state where id = $1
`

func scanNodeState(row scanner) (NodeStateRecord, error) {
	var state NodeStateRecord
	if err := row.Scan(
		&state.ID, &state.EnrolledAt, &state.EnrollmentTokenHash, &state.EnrollmentPrimaryID,
		&state.LastSeenAt, &state.LastAppliedSnapshotID, &state.EnrollmentAttemptID,
		&state.PrimaryURL, &state.PrimaryInstallationID, &state.PrimaryTLSSPKISHA256,
		&state.CredentialID, &state.SyncEnabled, &state.LastAttemptAt, &state.LastSuccessAt,
		&state.ConsecutiveFailures, &state.NextAttemptAt, &state.LastErrorCode, &state.UpdatedAt,
	); err != nil {
		return NodeStateRecord{}, err
	}
	return state, nil
}

func loadNodeState(ctx context.Context, q querier) (NodeStateRecord, error) {
	row := q.QueryRow(ctx, phase2NodeStateSelect, installationID)
	return scanNodeState(row)
}

func scanEnrollmentAttempt(row scanner) (EnrollmentAttemptRecord, error) {
	var (
		attempt EnrollmentAttemptRecord
		state   string
	)
	if err := row.Scan(
		&attempt.ID, &state, &attempt.PrimaryURL, &attempt.ExpectedPrimaryID,
		&attempt.VerifiedPrimaryID, &attempt.VerifiedPrimaryNodeID,
		&attempt.VerifiedLeadershipGeneration, &attempt.VerifiedPrimaryTLSSPKISHA256,
		&attempt.VerifiedPrimaryCAFingerprint, &attempt.PreviewDigest, &attempt.LocalNodeIP,
		&attempt.ArchiveID, &attempt.EphemeralPrivateKeyWrapped, &attempt.BootstrapPayload,
		&attempt.NodeCredentialSecretID, &attempt.ClusterKeyID, &attempt.InitialSnapshotHash,
		&attempt.InitialSnapshotRevisionID, &attempt.InitialApplyJobID, &attempt.FailureCode,
		&attempt.ConfirmedAt, &attempt.CreatedAt, &attempt.UpdatedAt,
	); err != nil {
		return EnrollmentAttemptRecord{}, err
	}
	attempt.State = EnrollmentAttemptState(state)
	return attempt, nil
}

func scanSyncAttempt(row scanner) (SyncAttemptRecord, error) {
	var (
		attempt SyncAttemptRecord
		trigger string
		status  string
	)
	if err := row.Scan(
		&attempt.ID, &attempt.NodeID, &trigger, &status, &attempt.SourcePrimaryID,
		&attempt.LeadershipGeneration, &attempt.SnapshotVersion, &attempt.ReplicationVersion,
		&attempt.ContentHash, &attempt.RevisionID, &attempt.ApplyJobID, &attempt.ResultCode,
		&attempt.StartedAt, &attempt.FinishedAt, &attempt.CreatedAt, &attempt.UpdatedAt,
	); err != nil {
		return SyncAttemptRecord{}, err
	}
	attempt.Trigger = SyncTrigger(trigger)
	attempt.Status = SyncAttemptStatus(status)
	return attempt, nil
}

func scanAppliedSnapshot(row scanner) (AppliedSnapshotRecord, error) {
	var (
		snapshot AppliedSnapshotRecord
		status   string
	)
	if err := row.Scan(
		&snapshot.ID, &snapshot.SourcePrimaryID, &snapshot.LeadershipGeneration,
		&snapshot.SnapshotVersion, &snapshot.ReplicationVersion, &snapshot.ContentHash,
		&snapshot.RevisionID, &status, &snapshot.ApplyJobID, &snapshot.FailureCode,
		&snapshot.AppliedAt, &snapshot.DiscardedAt,
	); err != nil {
		return AppliedSnapshotRecord{}, err
	}
	snapshot.Status = AppliedSnapshotStatus(status)
	return snapshot, nil
}

func scanSnapshotAcknowledgement(row scanner) (SnapshotAcknowledgement, error) {
	var ack SnapshotAcknowledgement
	if err := row.Scan(
		&ack.NodeID, &ack.ContentHash, &ack.SnapshotVersion, &ack.ReplicationVersion,
		&ack.RevisionID, &ack.LeadershipGeneration, &ack.AppliedAt, &ack.ReceivedAt,
	); err != nil {
		return SnapshotAcknowledgement{}, err
	}
	return ack, nil
}
