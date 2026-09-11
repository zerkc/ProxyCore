package snapshot

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

// Marshal produces a deterministic JSON byte slice for the envelope. The
// output is byte-stable across processes and restarts so two installations
// that hold the same payload produce the same bytes. The content hash is
// NOT re-computed by this function; callers MUST re-anchor Envelope.ContentHash
// before publishing.
func Marshal(e Envelope) ([]byte, error) {
	value, err := domain.NormalizeSnapshot(e)
	if err != nil {
		return nil, fmt.Errorf("normalize envelope: %w", err)
	}
	canonical := domain.StableStringify(value)
	return []byte(canonical), nil
}

// Unmarshal parses a JSON payload into the Envelope type. It does NOT
// validate the envelope; callers MUST call Envelope.Validate after parsing
// to confirm structural correctness and the declared content hash.
func Unmarshal(data []byte) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return Envelope{}, fmt.Errorf("unmarshal envelope: %w", err)
	}
	if env.Transient.SnapshotVersion == 0 && env.Replicated.Configuration == nil {
		return Envelope{}, errors.New("envelope shape does not match the snapshot schema")
	}
	return env, nil
}
