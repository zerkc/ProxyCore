package sync

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestGenerateNodeCredentialReturnsOwnedBearerMaterial(t *testing.T) {
	credential, err := GenerateNodeCredential()
	if err != nil {
		t.Fatalf("GenerateNodeCredential: %v", err)
	}
	defer credential.Destroy()

	if credential.ID() == "" {
		t.Fatal("generated credential has no ID")
	}
	bearer := credential.BearerCopy()
	if len(bearer) != NodeCredentialMaxBytes {
		t.Fatalf("bearer length = %d, want %d", len(bearer), NodeCredentialMaxBytes)
	}
	parsed, err := ParseNodeCredential(bearer)
	if err != nil {
		t.Fatalf("ParseNodeCredential: %v", err)
	}
	defer parsed.Destroy()
	if parsed.ID() != credential.ID() {
		t.Fatalf("parsed ID = %q, want %q", parsed.ID(), credential.ID())
	}
	if got := parsed.CopyBytes(); len(got) != NodeCredentialSecretBytes {
		t.Fatalf("parsed secret length = %d, want %d", len(got), NodeCredentialSecretBytes)
	}
	if got := parsed.Hash(); got == "" {
		t.Fatal("parsed credential has no hash")
	}
}

func TestGenerateNodeCredentialEntropyFailureIsGeneric(t *testing.T) {
	errEntropy := errors.New("entropy unavailable")
	credential, err := generateNodeCredentialWithEntropy(errorReader{err: errEntropy})
	if err != ErrNodeCredentialDenied {
		t.Fatalf("entropy error = %v, want %v", err, ErrNodeCredentialDenied)
	}
	if got := credential.CopyBytes(); len(got) != 0 {
		t.Fatal("failed generation retained secret bytes")
	}
	if credential.BearerCopy() != "" {
		t.Fatal("failed generation returned bearer material")
	}
}

func TestNewNodeCredentialRejectsInvalidParts(t *testing.T) {
	validID := "00112233-4455-4677-8899-aabbccddeeff"
	validSecret := bytes.Repeat([]byte{0x5a}, NodeCredentialSecretBytes)
	cases := []struct {
		name   string
		id     string
		secret []byte
	}{
		{"empty-id", "", validSecret},
		{"uppercase-id", "00112233-4455-4677-8899-AABBCCDDEEFF", validSecret},
		{"nil-id", "00000000-0000-0000-0000-000000000000", validSecret},
		{"short-secret", validID, validSecret[:NodeCredentialSecretBytes-1]},
		{"long-secret", validID, append(validSecret, 0x01)},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			credential, err := NewNodeCredential(test.id, test.secret)
			if err != ErrNodeCredentialDenied {
				t.Fatalf("constructor error = %v, want %v", err, ErrNodeCredentialDenied)
			}
			credential.Destroy()
		})
	}
}

func TestParseNodeCredentialStrictness(t *testing.T) {
	secret := bytes.Repeat([]byte{0x5a}, NodeCredentialSecretBytes)
	credential, err := NewNodeCredential("00112233-4455-4677-8899-aabbccddeeff", secret)
	if err != nil {
		t.Fatalf("NewNodeCredential: %v", err)
	}
	defer credential.Destroy()
	valid := credential.BearerCopy()

	cases := []string{
		"",
		valid + "x",
		"pcnode0_" + valid[len("pcnode1_"):],
		strings.Replace(valid, "pcnode1_", "pcnode1", 1),
		strings.Replace(valid, "_", "-", 1),
		strings.Replace(valid, "00112233-4455-4677-8899-aabbccddeeff", "00112233-4455-4677-8899-AABBCCDDEEFF", 1),
		strings.Replace(valid, "00112233-4455-4677-8899-aabbccddeeff", "00112233445546778899aabbccddeeff", 1),
		strings.Replace(valid, "00112233-4455-4677-8899-aabbccddeeff", "00000000-0000-0000-0000-000000000000", 1),
		valid[:len(valid)-1] + "=",
		valid[:len(valid)-1] + "B",
		valid + "\n",
		strings.Replace(valid, "pcnode1_", "pcnode1_\t", 1),
	}
	for _, candidate := range cases {
		t.Run(fmt.Sprintf("%q", candidate), func(t *testing.T) {
			parsed, err := ParseNodeCredential(candidate)
			if err != ErrNodeCredentialDenied {
				t.Fatalf("parse error = %v, want %v", err, ErrNodeCredentialDenied)
			}
			parsed.Destroy()
		})
	}
}

func TestAuthenticateNodeCredentialReturnsOnlyPrincipalIDs(t *testing.T) {
	secret := bytes.Repeat([]byte{0x31}, NodeCredentialSecretBytes)
	credential, err := NewNodeCredential("00112233-4455-4677-8899-aabbccddeeff", secret)
	if err != nil {
		t.Fatalf("NewNodeCredential: %v", err)
	}
	defer credential.Destroy()
	record := validCredentialRecord(credential, "ffeeddcc-bbaa-4999-8877-665544332211")

	principal, err := AuthenticateNodeCredential(credential.BearerCopy(), &record)
	if err != nil {
		t.Fatalf("AuthenticateNodeCredential: %v", err)
	}
	if principal.CredentialID != record.ID || principal.NodeID != record.NodeID {
		t.Fatalf("principal = %#v, want credential/node IDs", principal)
	}
	if fmt.Sprintf("%v", principal) == credential.BearerCopy() {
		t.Fatal("principal contains bearer material")
	}
}

func TestAuthenticateNodeCredentialFailuresAreIndistinguishable(t *testing.T) {
	secret := bytes.Repeat([]byte{0x42}, NodeCredentialSecretBytes)
	credential, err := NewNodeCredential("00112233-4455-4677-8899-aabbccddeeff", secret)
	if err != nil {
		t.Fatalf("NewNodeCredential: %v", err)
	}
	defer credential.Destroy()
	valid := validCredentialRecord(credential, "ffeeddcc-bbaa-4999-8877-665544332211")
	other, err := NewNodeCredential("11112222-3333-4444-8888-9999aaaabbbb", bytes.Repeat([]byte{0x43}, NodeCredentialSecretBytes))
	if err != nil {
		t.Fatalf("NewNodeCredential(other): %v", err)
	}
	defer other.Destroy()

	cases := []struct {
		name   string
		bearer string
		record *NodeCredentialRecord
	}{
		{"missing-record", credential.BearerCopy(), nil},
		{"revoked", credential.BearerCopy(), withRevocation(valid)},
		{"wrong-secret", other.BearerCopy(), &valid},
		{"wrong-id", replaceCredentialID(credential.BearerCopy(), "11112222-3333-4444-8888-9999aaaabbbb"), &valid},
		{"wrong-version", credential.BearerCopy(), withVersion(valid, "sha256-v1")},
		{"wrong-hash", credential.BearerCopy(), withHash(valid, strings.Repeat("0", 64))},
		{"malformed-hash", credential.BearerCopy(), withHash(valid, "not-a-hash")},
		{"uppercase-hash", credential.BearerCopy(), withHash(valid, strings.ToUpper(valid.Hash))},
		{"empty-node-id", credential.BearerCopy(), withNodeID(valid, "")},
		{"uppercase-node-id", credential.BearerCopy(), withNodeID(valid, "FFEEDDCC-BBAA-4999-8877-665544332211")},
		{"braced-node-id", credential.BearerCopy(), withNodeID(valid, "{ffeeddcc-bbaa-4999-8877-665544332211}")},
		{"malformed-node-id", credential.BearerCopy(), withNodeID(valid, "not-a-uuid")},
		{"nil-node-id", credential.BearerCopy(), withNodeID(valid, "00000000-0000-0000-0000-000000000000")},
		{"malformed-id", credential.BearerCopy(), withID(valid, "not-a-uuid")},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, got := AuthenticateNodeCredential(test.bearer, test.record)
			if got != ErrNodeCredentialDenied {
				t.Fatalf("error = %v, want %v", got, ErrNodeCredentialDenied)
			}
		})
	}
}

func TestNodeCredentialRedactsEveryDefaultRepresentation(t *testing.T) {
	secret := bytes.Repeat([]byte{0xde}, NodeCredentialSecretBytes)
	credential, err := NewNodeCredential("00112233-4455-4677-8899-aabbccddeeff", secret)
	if err != nil {
		t.Fatalf("NewNodeCredential: %v", err)
	}
	defer credential.Destroy()
	bearer := credential.BearerCopy()
	redacted := "[node credential redacted]"

	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%b", "%f", "%U"} {
		output := fmt.Sprintf(verb, credential)
		if strings.Contains(output, bearer) || strings.Contains(output, "3q") {
			t.Fatalf("fmt %s leaked credential: %q", verb, output)
		}
		if !strings.Contains(output, redacted) {
			t.Fatalf("fmt %s did not redact credential: %q", verb, output)
		}
	}
	jsonBytes, err := json.Marshal(struct {
		Credential NodeCredential `json:"credential"`
	}{credential})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(string(jsonBytes), bearer) || !strings.Contains(string(jsonBytes), redacted) {
		t.Fatalf("JSON representation leaked credential: %s", jsonBytes)
	}
	text, err := credential.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText: %v", err)
	}
	if string(text) != redacted {
		t.Fatalf("text representation = %q, want %q", text, redacted)
	}
	if _, err := ParseNodeCredential("pcnode1_bad"); err != ErrNodeCredentialDenied || strings.Contains(err.Error(), bearer) {
		t.Fatal("generic parse error leaked credential material")
	}
}

func TestNodeCredentialCopyAndDestroyOwnership(t *testing.T) {
	secret := bytes.Repeat([]byte{0xa5}, NodeCredentialSecretBytes)
	credential, err := NewNodeCredential("00112233-4455-4677-8899-aabbccddeeff", secret)
	if err != nil {
		t.Fatalf("NewNodeCredential: %v", err)
	}
	secret[0] ^= 0xff
	if bytes.Equal(secret, credential.CopyBytes()) {
		t.Fatal("constructor retained caller-owned secret storage")
	}
	copyOfSecret := credential.CopyBytes()
	copyOfSecret[0] ^= 0xff
	if bytes.Equal(copyOfSecret, credential.CopyBytes()) {
		t.Fatal("CopyBytes did not return independent ownership")
	}
	credential.Destroy()
	if got := credential.CopyBytes(); len(got) != 0 {
		t.Fatal("Destroy retained secret bytes")
	}
	if credential.Hash() != "" || credential.BearerCopy() != "" {
		t.Fatal("destroyed credential still exposed secret-derived material")
	}
	credential.Destroy()
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

func validCredentialRecord(credential NodeCredential, nodeID string) NodeCredentialRecord {
	return NodeCredentialRecord{
		ID:          credential.ID(),
		NodeID:      nodeID,
		Hash:        credential.Hash(),
		HashVersion: NodeCredentialHashVersion,
	}
}

func withRevocation(record NodeCredentialRecord) *NodeCredentialRecord {
	record.RevokedAt = timePtr(time.Unix(1, 0))
	return &record
}

func withVersion(record NodeCredentialRecord, version string) *NodeCredentialRecord {
	record.HashVersion = version
	return &record
}

func withHash(record NodeCredentialRecord, hash string) *NodeCredentialRecord {
	record.Hash = hash
	return &record
}

func withID(record NodeCredentialRecord, id string) *NodeCredentialRecord {
	record.ID = id
	return &record
}

func withNodeID(record NodeCredentialRecord, nodeID string) *NodeCredentialRecord {
	record.NodeID = nodeID
	return &record
}

func timePtr(value time.Time) *time.Time { return &value }

func replaceCredentialID(bearer, id string) string {
	parts := strings.SplitN(bearer, "_", 3)
	return parts[0] + "_" + id + "_" + parts[2]
}
