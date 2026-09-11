// Package snapshot defines the versioned PRIMARY/NODE replication envelope
// used by the primary-node continuity change.
//
// The envelope separates three field classes:
//
//   - Replicated: settings, zones, streams, certificates, encrypted
//     secrets, and administrator identity hashes that all nodes must hold
//     identically.
//   - NodeLocal: the node's own ingress address, node id, role, leadership
//     generation, and cluster-key reference. These fields are
//     authoritative locally and MUST be replaced before a snapshot is
//     applied to a different node.
//   - Transient: envelope metadata (snapshot version, replication version,
//     content hash, source primary id, captured-at timestamp). These are
//     authored by the producing primary and never accepted as-is on a
//     different installation.
package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

// ReplicatedSecrets are the secret-references that the snapshot carries for
// the active data plane. The values themselves are stored encrypted with the
// cluster KEK (see the cluster.KEK envelope) so a node can decrypt them
// offline after promotion without contacting the primary.
type ReplicatedSecret struct {
	ID       uuid.UUID `json:"id"`
	Purpose  string    `json:"purpose"`
	Envelope string    `json:"envelope"`
}

// ReplicatedOwner carries the durable admin identity that all nodes must
// hold identically. The plaintext password is never replicated; only the
// scrypt-encoded hash.
type ReplicatedOwner struct {
	UserID       uuid.UUID `json:"userId"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"passwordHash"`
	Role         string    `json:"role"`
}

// ReplicatedFields are the values the snapshot carries for every node. The
// inner ConfigurationSnapshot mirrors the existing ConfigurationSnapshot
// shape from apps/api/internal/domain/snapshot.go.
type ReplicatedFields struct {
	Configuration any                  `json:"configuration"`
	Secrets       []ReplicatedSecret   `json:"secrets"`
	Owners        []ReplicatedOwner    `json:"owners"`
}

// NodeLocalFields are authoritative on the receiving node. They MUST be
// replaced before a snapshot is applied locally; the validator and the
// importer both enforce this.
type NodeLocalFields struct {
	Ingress             domain.Ingress          `json:"ingress"`
	NodeID              domain.NodeID           `json:"nodeId"`
	Role                domain.TopologyRole     `json:"role"`
	LeadershipGeneration domain.LeadershipGeneration `json:"leadershipGeneration"`
	ClusterKeyRef       *uuid.UUID              `json:"clusterKeyRef,omitempty"`
}

// TransientFields are authored by the producing primary and describe the
// envelope itself.
type TransientFields struct {
	SnapshotVersion      domain.SnapshotVersion     `json:"snapshotVersion"`
	ReplicationVersion   domain.ReplicationVersion  `json:"replicationVersion"`
	SourcePrimaryID      uuid.UUID                  `json:"sourcePrimaryId"`
	LeadershipGeneration domain.LeadershipGeneration `json:"leadershipGeneration"`
	CapturedAt           time.Time                  `json:"capturedAt"`
}

// Envelope is the outer shape of a replication snapshot. The ContentHash
// covers the canonical serialization of ReplicatedFlattened (see the
// snapshot.Hash function).
type Envelope struct {
	Transient   TransientFields `json:"transient"`
	NodeLocal   NodeLocalFields `json:"nodeLocal"`
	Replicated  ReplicatedFields `json:"replicated"`
	ContentHash string         `json:"contentHash"`
}

// ClassificationError is returned when a snapshot places an
// authoritative-local field inside the replicated payload or vice versa.
type ClassificationError struct {
	Field string
}

func (e *ClassificationError) Error() string {
	return fmt.Sprintf("snapshot classification error: %s placed in wrong field class", e.Field)
}

// validateClassification rejects obvious shape mistakes that the structural
// validation cannot catch.
func (e *Envelope) validateClassification() error {
	if e.Transient.SourcePrimaryID == uuid.Nil {
		return &ClassificationError{Field: "transient.sourcePrimaryId"}
	}
	if e.NodeLocal.NodeID == "" {
		return &ClassificationError{Field: "nodeLocal.nodeId"}
	}
	if !e.NodeLocal.Role.IsValid() {
		return &ClassificationError{Field: "nodeLocal.role"}
	}
	if e.Replicated.Configuration == nil {
		return &ClassificationError{Field: "replicated.configuration"}
	}
	return nil
}

// ExpectedHash returns the canonical content hash that this envelope should
// carry once the producer signs it. The hash covers a canonical serialization
// of the ReplicatedFields only: the snapshot hash is the contract that lets
// a node verify the payload came from the claimed primary and was not
// tampered with in transit. The NodeLocal and Transient metadata are not
// included in the content hash so a node can re-anchor them at apply time
// without invalidating the snapshot.
func (e *Envelope) ExpectedHash() (string, error) {
	if err := e.validateClassification(); err != nil {
		return "", err
	}
	canonical := canonicalizeReplicated(e.Replicated)
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// VerifyHash checks that the envelope's declared ContentHash matches the
// canonical content hash. Returns nil when the hash matches; an error
// otherwise.
func (e *Envelope) VerifyHash() error {
	got, err := e.ExpectedHash()
	if err != nil {
		return err
	}
	if got != e.ContentHash {
		return fmt.Errorf("snapshot content hash mismatch: declared %s, computed %s", e.ContentHash, got)
	}
	return nil
}

// Validate accepts or rejects an envelope purely on its structural shape and
// declared hash. Field-level validation against the desired state (DNS
// labels, certificate hostnames, secret availability) lives in the
// validator (Phase 1).
func (e *Envelope) Validate() error {
	if err := e.validateClassification(); err != nil {
		return err
	}
	if !e.Transient.SnapshotVersion.IsValid() {
		return errors.New("invalid transient.snapshotVersion")
	}
	if !e.Transient.ReplicationVersion.IsValid() {
		return errors.New("invalid transient.replicationVersion")
	}
	if e.Transient.CapturedAt.IsZero() {
		return errors.New("missing transient.capturedAt")
	}
	return e.VerifyHash()
}

// canonicalizeReplicated returns the canonical byte representation of the
// ReplicatedFields. The legacy domain package exposes StableStringify over
// arbitrary values, which produces a deterministic JSON serialization. The
// bytes are then ready to be hashed.
func canonicalizeReplicated(r ReplicatedFields) []byte {
	return []byte(domain.StableStringify(r))
}
