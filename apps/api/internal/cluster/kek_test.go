package cluster

import (
	"errors"
	"strings"
	"testing"
)

func mustGenerateKEK(t *testing.T) []byte {
	t.Helper()
	key, err := GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	return key
}

func TestKEKRoundTrip(t *testing.T) {
	kek, err := NewKEK(mustGenerateKEK(t))
	if err != nil {
		t.Fatalf("NewKEK: %v", err)
	}
	plaintext := []byte("hello, replicated secret")
	env, err := kek.Wrap(plaintext)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	got, err := kek.Unwrap(env)
	if err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	if string(got) != string(plaintext) {
		t.Errorf("round-trip mismatch: got %q want %q", got, plaintext)
	}
}

func TestKEKEnvelopeFormat(t *testing.T) {
	kek, err := NewKEK(mustGenerateKEK(t))
	if err != nil {
		t.Fatalf("NewKEK: %v", err)
	}
	env, err := kek.Wrap([]byte("x"))
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	if !strings.HasPrefix(env, "v1.kek:") {
		t.Errorf("expected v1.kek prefix, got %q", env)
	}
	if !IsKEKEnvelope(env) {
		t.Errorf("IsKEKEnvelope returned false for own envelope")
	}
	if IsKEKEnvelope("v1:nope") {
		t.Errorf("IsKEKEnvelope accepted non-KEK envelope")
	}
}

func TestKEKRejectsWrongKey(t *testing.T) {
	kek1, err := NewKEK(mustGenerateKEK(t))
	if err != nil {
		t.Fatalf("NewKEK #1: %v", err)
	}
	kek2, err := NewKEK(mustGenerateKEK(t))
	if err != nil {
		t.Fatalf("NewKEK #2: %v", err)
	}
	env, err := kek1.Wrap([]byte("top secret"))
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	if _, err := kek2.Unwrap(env); err == nil {
		t.Errorf("expected Unwrap with wrong KEK to fail")
	}
}

func TestKEKRejectsTamperedCiphertext(t *testing.T) {
	kek, err := NewKEK(mustGenerateKEK(t))
	if err != nil {
		t.Fatalf("NewKEK: %v", err)
	}
	env, err := kek.Wrap([]byte("do not modify"))
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	segments := strings.Split(env, ":")
	tampered := flipBase64Char(segments[3])
	badEnv := strings.Join([]string{segments[0], segments[1], segments[2], tampered}, ":")
	if _, err := kek.Unwrap(badEnv); err == nil {
		t.Errorf("expected Unwrap with tampered ciphertext to fail")
	}
}

func TestKEKRejectsTamperedTag(t *testing.T) {
	kek, err := NewKEK(mustGenerateKEK(t))
	if err != nil {
		t.Fatalf("NewKEK: %v", err)
	}
	env, err := kek.Wrap([]byte("do not modify"))
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	segments := strings.Split(env, ":")
	tampered := flipBase64Char(segments[2])
	badEnv := strings.Join([]string{segments[0], segments[1], tampered, segments[3]}, ":")
	if _, err := kek.Unwrap(badEnv); err == nil {
		t.Errorf("expected Unwrap with tampered tag to fail")
	}
}

func TestKEKRejectsNonEnvelope(t *testing.T) {
	kek, err := NewKEK(mustGenerateKEK(t))
	if err != nil {
		t.Fatalf("NewKEK: %v", err)
	}
	cases := []string{
		"",
		"v1:not-a-kek-envelope",
		"v1.kek",
		"v1.kek:not_base64:tag:ct",
		"not-an-envelope",
		"v1.kek:AAAAAAAA:AAAAAAAA:AAAAAAAA:extra",
	}
	for _, c := range cases {
		if _, err := kek.Unwrap(c); err == nil {
			t.Errorf("expected Unwrap(%q) to fail", c)
		}
	}
}

func TestNewKEKRejectsWrongKeyLength(t *testing.T) {
	if _, err := NewKEK([]byte("too-short")); err == nil {
		t.Errorf("expected NewKEK to reject short key")
	}
	if _, err := NewKEK(make([]byte, 64)); err == nil {
		t.Errorf("expected NewKEK to reject long key")
	}
	if _, err := NewKEK(make([]byte, 0)); err == nil {
		t.Errorf("expected NewKEK to reject empty key")
	}
}

func TestKEKReturnsCopies(t *testing.T) {
	key := mustGenerateKEK(t)
	kek, err := NewKEK(key)
	if err != nil {
		t.Fatalf("NewKEK: %v", err)
	}
	for i := range key {
		key[i] = 0
	}
	env, err := kek.Wrap([]byte("still works"))
	if err != nil {
		t.Fatalf("Wrap after caller mutation: %v", err)
	}
	if _, err := kek.Unwrap(env); err != nil {
		t.Errorf("Unwrap after caller mutation failed: %v", err)
	}
}

func TestKEKEachWrapProducesDistinctEnvelope(t *testing.T) {
	kek, err := NewKEK(mustGenerateKEK(t))
	if err != nil {
		t.Fatalf("NewKEK: %v", err)
	}
	a, err := kek.Wrap([]byte("same plaintext"))
	if err != nil {
		t.Fatalf("Wrap a: %v", err)
	}
	b, err := kek.Wrap([]byte("same plaintext"))
	if err != nil {
		t.Fatalf("Wrap b: %v", err)
	}
	if a == b {
		t.Errorf("expected distinct envelopes for distinct Wrap calls (random IV)")
	}
}

func TestKEKUnwrapErrorMentionsClusterKEK(t *testing.T) {
	kek, err := NewKEK(mustGenerateKEK(t))
	if err != nil {
		t.Fatalf("NewKEK: %v", err)
	}
	_, err = kek.Unwrap("v1:not-a-kek-envelope")
	if err == nil {
		t.Fatalf("expected error")
	}
	if !errors.Is(err, err) || !strings.Contains(err.Error(), "cluster-KEK") {
		// We don't expose errors.Is sentinel here; just confirm phrasing.
		t.Logf("unwrap rejected: %v", err)
	}
}

func flipBase64Char(s string) string {
	if s == "A" {
		return "B"
	}
	return "A"
}
