package auth

import (
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
)

func TestPasswordHashRoundTripUsesNodeCompatibleScryptFormat(t *testing.T) {
	const password = "correct horse battery staple"

	encoded, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if strings.Contains(encoded, password) {
		t.Fatalf("hash contains password: %q", encoded)
	}
	if !regexp.MustCompile(`^scrypt\$16384\$8\$1\$[A-Za-z0-9_-]{22}\$[A-Za-z0-9_-]{86}$`).MatchString(encoded) {
		t.Fatalf("unexpected encoded format: %q", encoded)
	}
	if strings.Contains(encoded, "=") {
		t.Fatalf("encoded hash should use base64url without padding: %q", encoded)
	}

	ok := VerifyPassword(password, encoded)
	if !ok {
		t.Fatal("VerifyPassword rejected the original password")
	}
	if VerifyPassword("wrong password", encoded) {
		t.Fatal("VerifyPassword accepted the wrong password")
	}
}

func TestVerifyPasswordAcceptsNodeScryptVector(t *testing.T) {
	const encoded = "scrypt$16384$8$1$AAECAwQFBgcICQoLDA0ODw$11kKyiyYAc8G7rp3KmncMc44YlkdllIqxOa7pq0fMaUtb3NvK4WtqmJiM16xEuVvAU9BejfXS-De92abLFHCng"

	if !VerifyPassword("correct horse battery staple", encoded) {
		t.Fatal("VerifyPassword rejected a Node-generated scrypt hash")
	}
}

func TestHashPasswordRejectsShortPassword(t *testing.T) {
	if _, err := HashPassword("four"); err == nil {
		t.Fatal("HashPassword accepted a password shorter than 5 characters")
	}
}

func TestOpaqueTokenHelpers(t *testing.T) {
	token, err := CreateOpaqueToken()
	if err != nil {
		t.Fatalf("CreateOpaqueToken: %v", err)
	}
	if strings.Contains(token, "=") {
		t.Fatalf("token should use base64url without padding: %q", token)
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("token is not base64url: %v", err)
	}
	if len(raw) != 32 {
		t.Fatalf("token raw length=%d, want 32", len(raw))
	}

	if got := HashOpaqueToken("session-token"); got != "c101e911469c969171040b50d70543313cf968fdef5bacc780776f8fb399ab36" {
		t.Fatalf("HashOpaqueToken mismatch: %s", got)
	}
}
