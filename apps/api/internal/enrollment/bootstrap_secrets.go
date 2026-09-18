package enrollment

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"unicode/utf8"
)

const (
	bootstrapGrantPlaintextVersionV2 uint32 = 2
	bootstrapGrantStringCountV2             = 6
	bootstrapPlaintextFixedBytes            = 4 + 4 + bootstrapCredentialBytes + bootstrapKEKBytes + 8 + 8
	bootstrapPlaintextMinBytes              = bootstrapPlaintextFixedBytes + bootstrapGrantStringCountV2*4
	bootstrapPlaintextMaxBytes              = bootstrapPlaintextFixedBytes + bootstrapGrantStringCountV2*(4+bootstrapMaxBindingStringBytes)
	bootstrapCiphertextMinBytes             = bootstrapPlaintextMinBytes + 16
	bootstrapCiphertextMaxBytes             = bootstrapPlaintextMaxBytes + 16
)

type BootstrapGrantMetadata struct {
	ProtocolVersion             uint32 `json:"protocolVersion"`
	CredentialID                string `json:"credentialId"`
	ClusterKeyID                string `json:"clusterKeyId"`
	SourcePrimaryInstallationID string `json:"sourcePrimaryInstallationId"`
	TargetInstallationID        string `json:"targetInstallationId"`
	TargetNodeID                string `json:"targetNodeId"`
	AttemptID                   string `json:"attemptId"`
	LeadershipGeneration        uint64 `json:"leadershipGeneration"`
	ExpiresAtUnix               int64  `json:"expiresAtUnix"`
}

type BootstrapGrant struct {
	Metadata BootstrapGrantMetadata `json:"metadata"`
	Secrets  BootstrapSecrets       `json:"-"`
}

func NewBootstrapGrant(metadata BootstrapGrantMetadata, credential, clusterKEK []byte) (BootstrapGrant, error) {
	grant := BootstrapGrant{
		Metadata: metadata,
		Secrets: BootstrapSecrets{
			NodeCredential: NodeCredential{bytes: newSecretCopy(credential)},
			ClusterKEK:     ClusterKEK{bytes: newSecretCopy(clusterKEK)},
		},
	}
	if grant.validateShape() != nil {
		grant.Destroy()
		return BootstrapGrant{}, ErrBootstrapDenied
	}
	return grant, nil
}

func (g BootstrapGrant) validateBinding(binding BootstrapBinding) error {
	if err := validateBootstrapBinding(binding); err != nil {
		return ErrBootstrapDenied
	}
	if err := g.validateShape(); err != nil {
		return ErrBootstrapDenied
	}
	if g.Metadata.ProtocolVersion != binding.ProtocolVersion ||
		g.Metadata.SourcePrimaryInstallationID != binding.SourcePrimaryInstallationID ||
		g.Metadata.TargetInstallationID != binding.TargetInstallationID ||
		g.Metadata.TargetNodeID != binding.TargetNodeID ||
		g.Metadata.AttemptID != binding.AttemptID ||
		g.Metadata.LeadershipGeneration != binding.LeadershipGeneration {
		return ErrBootstrapDenied
	}
	return nil
}

func (g BootstrapGrant) validateShape() error {
	if err := g.Metadata.validate(); err != nil {
		return ErrBootstrapDenied
	}
	if len(g.Secrets.NodeCredential.bytes) != bootstrapCredentialBytes || len(g.Secrets.ClusterKEK.bytes) != bootstrapKEKBytes {
		return ErrBootstrapDenied
	}
	return nil
}

func (m BootstrapGrantMetadata) validate() error {
	if m.ProtocolVersion != BootstrapProtocolVersion || m.LeadershipGeneration == 0 || m.ExpiresAtUnix <= 0 {
		return ErrBootstrapDenied
	}
	for _, value := range []string{
		m.CredentialID,
		m.ClusterKeyID,
		m.SourcePrimaryInstallationID,
		m.TargetInstallationID,
		m.TargetNodeID,
		m.AttemptID,
	} {
		if !validBootstrapIdentifier(value) {
			return ErrBootstrapDenied
		}
	}
	return nil
}

func validBootstrapIdentifier(value string) bool {
	return len(value) > 0 && len(value) <= bootstrapMaxBindingStringBytes && utf8.ValidString(value)
}

func encodeBootstrapGrantPlaintext(grant BootstrapGrant) ([]byte, error) {
	if err := grant.validateShape(); err != nil {
		return nil, ErrBootstrapDenied
	}
	result := make([]byte, 0, bootstrapPlaintextMaxBytes)
	var fixed [8]byte
	binary.BigEndian.PutUint32(fixed[:4], bootstrapGrantPlaintextVersionV2)
	result = append(result, fixed[:4]...)
	binary.BigEndian.PutUint32(fixed[:4], grant.Metadata.ProtocolVersion)
	result = append(result, fixed[:4]...)
	result = appendBootstrapLengthPrefixed(result, []byte(grant.Metadata.CredentialID))
	result = append(result, grant.Secrets.NodeCredential.bytes...)
	result = appendBootstrapLengthPrefixed(result, []byte(grant.Metadata.ClusterKeyID))
	result = append(result, grant.Secrets.ClusterKEK.bytes...)
	for _, value := range []string{
		grant.Metadata.SourcePrimaryInstallationID,
		grant.Metadata.TargetInstallationID,
		grant.Metadata.TargetNodeID,
		grant.Metadata.AttemptID,
	} {
		result = appendBootstrapLengthPrefixed(result, []byte(value))
	}
	binary.BigEndian.PutUint64(fixed[:], grant.Metadata.LeadershipGeneration)
	result = append(result, fixed[:]...)
	binary.BigEndian.PutUint64(fixed[:], uint64(grant.Metadata.ExpiresAtUnix))
	result = append(result, fixed[:]...)
	if len(result) < bootstrapPlaintextMinBytes || len(result) > bootstrapPlaintextMaxBytes {
		return nil, ErrBootstrapDenied
	}
	return result, nil
}

func decodeBootstrapGrantPlaintext(plaintext []byte) (BootstrapGrant, error) {
	if len(plaintext) < bootstrapPlaintextMinBytes || len(plaintext) > bootstrapPlaintextMaxBytes {
		return BootstrapGrant{}, ErrBootstrapDenied
	}
	reader := bootstrapPlaintextReader{value: plaintext}
	version, ok := reader.uint32()
	if !ok || version != bootstrapGrantPlaintextVersionV2 {
		return BootstrapGrant{}, ErrBootstrapDenied
	}
	protocol, ok := reader.uint32()
	if !ok {
		return BootstrapGrant{}, ErrBootstrapDenied
	}
	credentialID, ok := reader.identifier()
	if !ok {
		return BootstrapGrant{}, ErrBootstrapDenied
	}
	credential, ok := reader.fixed(bootstrapCredentialBytes)
	if !ok {
		return BootstrapGrant{}, ErrBootstrapDenied
	}
	clusterKeyID, ok := reader.identifier()
	if !ok {
		return BootstrapGrant{}, ErrBootstrapDenied
	}
	clusterKEK, ok := reader.fixed(bootstrapKEKBytes)
	if !ok {
		return BootstrapGrant{}, ErrBootstrapDenied
	}
	values := make([]string, 4)
	for index := range values {
		values[index], ok = reader.identifier()
		if !ok {
			return BootstrapGrant{}, ErrBootstrapDenied
		}
	}
	generation, ok := reader.uint64()
	if !ok {
		return BootstrapGrant{}, ErrBootstrapDenied
	}
	expiresAtRaw, ok := reader.uint64()
	if !ok || expiresAtRaw == 0 || expiresAtRaw > math.MaxInt64 || !reader.done() {
		return BootstrapGrant{}, ErrBootstrapDenied
	}
	grant := BootstrapGrant{
		Metadata: BootstrapGrantMetadata{
			ProtocolVersion:             protocol,
			CredentialID:                credentialID,
			ClusterKeyID:                clusterKeyID,
			SourcePrimaryInstallationID: values[0],
			TargetInstallationID:        values[1],
			TargetNodeID:                values[2],
			AttemptID:                   values[3],
			LeadershipGeneration:        generation,
			ExpiresAtUnix:               int64(expiresAtRaw),
		},
		Secrets: BootstrapSecrets{
			NodeCredential: NodeCredential{bytes: newSecretCopy(credential)},
			ClusterKEK:     ClusterKEK{bytes: newSecretCopy(clusterKEK)},
		},
	}
	if grant.validateShape() != nil {
		grant.Destroy()
		return BootstrapGrant{}, ErrBootstrapDenied
	}
	return grant, nil
}

type bootstrapPlaintextReader struct {
	value  []byte
	offset int
}

func (r *bootstrapPlaintextReader) uint32() (uint32, bool) {
	if r == nil || len(r.value)-r.offset < 4 {
		return 0, false
	}
	value := binary.BigEndian.Uint32(r.value[r.offset : r.offset+4])
	r.offset += 4
	return value, true
}

func (r *bootstrapPlaintextReader) uint64() (uint64, bool) {
	if r == nil || len(r.value)-r.offset < 8 {
		return 0, false
	}
	value := binary.BigEndian.Uint64(r.value[r.offset : r.offset+8])
	r.offset += 8
	return value, true
}

func (r *bootstrapPlaintextReader) fixed(size int) ([]byte, bool) {
	if r == nil || size < 0 || len(r.value)-r.offset < size {
		return nil, false
	}
	value := r.value[r.offset : r.offset+size]
	r.offset += size
	return value, true
}

func (r *bootstrapPlaintextReader) identifier() (string, bool) {
	length, ok := r.uint32()
	if !ok || length == 0 || length > bootstrapMaxBindingStringBytes {
		return "", false
	}
	value, ok := r.fixed(int(length))
	if !ok || !utf8.Valid(value) || !validBootstrapIdentifier(string(value)) {
		return "", false
	}
	return string(value), true
}

func (r *bootstrapPlaintextReader) done() bool {
	return r != nil && r.offset == len(r.value)
}

func (g *BootstrapGrant) Destroy() {
	if g != nil {
		g.Secrets.Destroy()
	}
}

func (g BootstrapGrant) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "[bootstrap grant redacted]")
}
