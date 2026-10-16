package enrollment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log"
	"strings"
	"testing"
	"time"
)

func (f *acceptFixture) captureLog() *bytes.Buffer {
	var buf bytes.Buffer
	f.acceptor.logger = log.New(&buf, "", 0)
	return &buf
}

// assertRedacted fails when the log carries secret or payload material in any
// common encoding.
func assertRedacted(t *testing.T, output string, sealed []byte, hash string) {
	t.Helper()
	credential, kek := testBootstrapInputs()
	for _, secret := range [][]byte{credential, kek, sealed} {
		for _, form := range []string{
			string(secret), hex.EncodeToString(secret),
			base64.StdEncoding.EncodeToString(secret), base64.RawURLEncoding.EncodeToString(secret),
		} {
			if len(form) > 0 && strings.Contains(output, form) {
				t.Fatalf("log leaks secret material: %q", output)
			}
		}
	}
	if hash != "" && strings.Contains(output, hash) {
		t.Fatalf("log leaks payload hash: %q", output)
	}
}

func TestBootstrapAcceptorLogsOnlyReasonCodes(t *testing.T) {
	ctx := context.Background()
	type scenario struct {
		name   string
		reason string
		run    func(t *testing.T, f *acceptFixture) (sealed []byte, hash string, err error)
	}
	scenarios := []scenario{
		{"hash mismatch", "hash_mismatch", func(t *testing.T, f *acceptFixture) ([]byte, string, error) {
			sealed, _ := f.seal(t, f.binding, f.now.Add(time.Hour))
			bad := hex.EncodeToString(make([]byte, 32))
			_, err := f.acceptor.Accept(ctx, &f.recipient, f.binding, sealed, bad)
			return sealed, bad, err
		}},
		{"open failed", "open_failed", func(t *testing.T, f *acceptFixture) ([]byte, string, error) {
			sealed, hash := f.seal(t, f.binding, f.now.Add(time.Hour))
			wrong := f.binding
			wrong.TargetNodeID = "another-node"
			_, err := f.acceptor.Accept(ctx, &f.recipient, wrong, sealed, hash)
			return sealed, hash, err
		}},
		{"grant expired", "grant_expired", func(t *testing.T, f *acceptFixture) ([]byte, string, error) {
			sealed, hash := f.seal(t, f.binding, f.now.Add(-time.Second))
			_, err := f.acceptor.Accept(ctx, &f.recipient, f.binding, sealed, hash)
			return sealed, hash, err
		}},
		{"persist failed", "persist_failed", func(t *testing.T, f *acceptFixture) ([]byte, string, error) {
			sealed, hash := f.seal(t, f.binding, f.now.Add(time.Hour))
			f.acceptor.afterKEKWrite = func() error { return errors.New("injected failure carrying db-secret-detail") }
			_, err := f.acceptor.Accept(ctx, &f.recipient, f.binding, sealed, hash)
			return sealed, hash, err
		}},
		{"invalid input", "invalid_input", func(t *testing.T, f *acceptFixture) ([]byte, string, error) {
			_, err := f.acceptor.Accept(ctx, nil, f.binding, []byte("x"), "")
			return nil, "", err
		}},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			f := newAcceptFixture(t)
			buf := f.captureLog()
			sealed, hash, err := sc.run(t, f)
			if err == nil {
				t.Fatal("Accept succeeded")
			}
			if want := "reason=" + sc.reason; !strings.Contains(buf.String(), want) {
				t.Fatalf("log = %q, want %q", buf.String(), want)
			}
			if strings.Contains(buf.String(), "db-secret-detail") {
				t.Fatalf("log leaks raw error text: %q", buf.String())
			}
			assertRedacted(t, buf.String(), sealed, hash)
		})
	}
}

func TestBootstrapAcceptorLogsConflicts(t *testing.T) {
	ctx := context.Background()
	replay := func(t *testing.T, f *acceptFixture, credential, kek []byte) ([]byte, string, error) {
		t.Helper()
		other, err := NewBootstrapGrant(BootstrapGrantMetadata{
			ProtocolVersion: f.binding.ProtocolVersion, CredentialID: f.credID, ClusterKeyID: f.keyID,
			SourcePrimaryInstallationID: f.binding.SourcePrimaryInstallationID, TargetInstallationID: f.binding.TargetInstallationID,
			TargetNodeID: f.binding.TargetNodeID, AttemptID: f.binding.AttemptID,
			LeadershipGeneration: f.binding.LeadershipGeneration, ExpiresAtUnix: f.now.Add(time.Hour).Unix(),
		}, credential, kek)
		if err != nil {
			t.Fatal(err)
		}
		defer other.Destroy()
		public, _ := f.recipient.PublicKey()
		sealed, err := SealBootstrap(public, f.binding, other)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(sealed)
		hash := hex.EncodeToString(sum[:])
		_, err = f.acceptor.Accept(ctx, &f.recipient, f.binding, sealed, hash)
		return sealed, hash, err
	}
	t.Run("key conflict", func(t *testing.T) {
		f := newAcceptFixture(t)
		sealed, hash := f.seal(t, f.binding, f.now.Add(time.Hour))
		if _, err := f.acceptor.Accept(ctx, &f.recipient, f.binding, sealed, hash); err != nil {
			t.Fatal(err)
		}
		buf := f.captureLog()
		credential, _ := testBootstrapInputs()
		sealed2, hash2, err := replay(t, f, credential, bytes.Repeat([]byte{0x44}, 32))
		if !errors.Is(err, ErrBootstrapDenied) || !strings.Contains(buf.String(), "reason=key_conflict") {
			t.Fatalf("err=%v log=%q", err, buf.String())
		}
		assertRedacted(t, buf.String(), sealed2, hash2)
	})
	t.Run("credential conflict", func(t *testing.T) {
		f := newAcceptFixture(t)
		sealed, hash := f.seal(t, f.binding, f.now.Add(time.Hour))
		if _, err := f.acceptor.Accept(ctx, &f.recipient, f.binding, sealed, hash); err != nil {
			t.Fatal(err)
		}
		buf := f.captureLog()
		_, kek := testBootstrapInputs()
		sealed2, hash2, err := replay(t, f, bytes.Repeat([]byte{0x33}, 32), kek)
		if !errors.Is(err, ErrBootstrapDenied) || !strings.Contains(buf.String(), "reason=credential_conflict") {
			t.Fatalf("err=%v log=%q", err, buf.String())
		}
		assertRedacted(t, buf.String(), sealed2, hash2)
	})
}

func TestBootstrapAcceptorNilLoggerIsNoOp(t *testing.T) {
	f := newAcceptFixture(t)
	sealed, _ := f.seal(t, f.binding, f.now.Add(time.Hour))
	if _, err := f.acceptor.Accept(context.Background(), &f.recipient, f.binding, sealed, "bad"); !errors.Is(err, ErrBootstrapDenied) {
		t.Fatalf("err = %v, want ErrBootstrapDenied", err)
	}
}
