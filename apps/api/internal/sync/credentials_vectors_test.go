package sync

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
)

func TestNodeCredentialDeterministicVector(t *testing.T) {
	entropy := append([]byte{
		0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77,
		0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff,
	}, byteRange(32)...)
	credential, err := generateNodeCredentialWithEntropy(bytes.NewReader(entropy))
	if err != nil {
		t.Fatalf("generate vector credential: %v", err)
	}
	defer credential.Destroy()

	const wantID = "00112233-4455-4677-8899-aabbccddeeff"
	const wantSecret = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"
	const wantHash = "8052a08dc23853a56291e54988d481576639cdf5ac4166c5c880fb1b0f5a54c6"
	wantBearer := NodeCredentialPrefix + "_" + wantID + "_" + wantSecret
	if credential.ID() != wantID {
		t.Fatalf("vector ID = %q, want %q", credential.ID(), wantID)
	}
	if got := credential.BearerCopy(); got != wantBearer {
		t.Fatalf("vector bearer length = %d, want %d; suffix length = %d, want %d", len(got), len(wantBearer), len(got)-len(NodeCredentialPrefix)-1-NodeCredentialIDLen-1, len(wantSecret))
	}
	if got := credential.Hash(); got != wantHash {
		t.Fatalf("vector hash = %q, want %q", got, wantHash)
	}

	parsed, err := ParseNodeCredential(wantBearer)
	if err != nil {
		t.Fatalf("parse vector bearer: %v", err)
	}
	defer parsed.Destroy()
	if !bytes.Equal(parsed.CopyBytes(), byteRange(32)) {
		t.Fatal("vector secret bytes changed during parse")
	}
}

func TestNodeCredentialHashVectorIsIndependentlyDomainSeparated(t *testing.T) {
	secret := byteRange(NodeCredentialSecretBytes)
	credential, err := NewNodeCredential("00112233-4455-4677-8899-aabbccddeeff", secret)
	if err != nil {
		t.Fatalf("NewNodeCredential: %v", err)
	}
	defer credential.Destroy()

	hasher := sha256.New()
	_, _ = hasher.Write([]byte("proxycore/node-credential/"))
	_, _ = hasher.Write(secret)
	want := hex.EncodeToString(hasher.Sum(nil))
	if got := credential.Hash(); got != want {
		t.Fatalf("independent hash = %q, want %q", got, want)
	}
	if got := credential.CredentialHash(); got != want {
		t.Fatalf("credential hash = %q, want %q", got, want)
	}
	if NodeCredentialHashVersion == "sha256-v1" {
		t.Fatal("node credential hash version is not distinct from enrollment token version")
	}
}

func TestNodeCredentialHashExcludesCredentialID(t *testing.T) {
	secret := bytes.Repeat([]byte{0x7c}, NodeCredentialSecretBytes)
	first, err := NewNodeCredential("00112233-4455-4677-8899-aabbccddeeff", secret)
	if err != nil {
		t.Fatalf("first credential: %v", err)
	}
	defer first.Destroy()
	second, err := NewNodeCredential("11112222-3333-4444-8888-9999aaaabbbb", secret)
	if err != nil {
		t.Fatalf("second credential: %v", err)
	}
	defer second.Destroy()
	if first.Hash() != second.Hash() {
		t.Fatal("credential ID was included in secret hash")
	}
}

func TestNodeCredentialRawSecretFitsBootstrapGrant(t *testing.T) {
	secret := byteRange(NodeCredentialSecretBytes)
	credential, err := NewNodeCredential("00112233-4455-4677-8899-aabbccddeeff", secret)
	if err != nil {
		t.Fatalf("NewNodeCredential: %v", err)
	}
	defer credential.Destroy()

	raw := credential.CopyBytes()
	grant, err := enrollment.NewBootstrapGrant(enrollment.BootstrapGrantMetadata{
		ProtocolVersion:             enrollment.BootstrapProtocolVersion,
		CredentialID:                credential.ID(),
		ClusterKeyID:                "cluster-key-1",
		SourcePrimaryInstallationID: "primary-installation-1",
		TargetInstallationID:        "target-installation-1",
		TargetNodeID:                "target-node-1",
		AttemptID:                   "attempt-1",
		LeadershipGeneration:        1,
		ExpiresAtUnix:               1,
	}, raw, bytes.Repeat([]byte{0xa5}, NodeCredentialSecretBytes))
	if err != nil {
		t.Fatalf("NewBootstrapGrant: %v", err)
	}
	defer grant.Destroy()
	if !bytes.Equal(grant.Secrets.NodeCredential.CopyBytes(), raw) {
		t.Fatal("raw node credential was not directly usable by BootstrapGrant")
	}
	raw[0] ^= 0xff
	if bytes.Equal(grant.Secrets.NodeCredential.CopyBytes(), raw) {
		t.Fatal("BootstrapGrant did not take ownership of a copy")
	}
}

func TestNodeCredentialRandomGenerationIsUnique(t *testing.T) {
	seen := make(map[string]struct{}, 32)
	for index := 0; index < 32; index++ {
		credential, err := GenerateNodeCredential()
		if err != nil {
			t.Fatalf("GenerateNodeCredential(%d): %v", index, err)
		}
		bearer := credential.BearerCopy()
		credential.Destroy()
		if _, exists := seen[bearer]; exists {
			t.Fatalf("duplicate generated bearer at index %d", index)
		}
		seen[bearer] = struct{}{}
	}
}

func byteRange(length int) []byte {
	value := make([]byte, length)
	for index := range value {
		value[index] = byte(index)
	}
	return value
}
