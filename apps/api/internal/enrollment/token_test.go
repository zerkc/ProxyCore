package enrollment

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/acme"
	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

type stubStore struct{}

func (*stubStore) CreateEnrollmentToken(context.Context, string, string, string, string, string, time.Time, time.Time) (domain.TopologyRole, error) {
	return domain.TopologyRolePrimary, nil
}
func (*stubStore) CheckEnrollmentToken(context.Context, string, string, time.Time, bool) error {
	return nil
}
func (*stubStore) RevokeEnrollmentToken(context.Context, string, string, time.Time) error { return nil }
func (*stubStore) DeleteEnrollmentToken(context.Context, string, string, time.Time) error {
	return nil
}

func owner() auth.User { return auth.User{ID: uuid.NewString(), Role: auth.RoleOwner, Active: true} }

func TestCreateTokenRequiresActiveOwner(t *testing.T) {
	for _, user := range []auth.User{
		{ID: "operator", Role: auth.RoleOperator, Active: true},
		{ID: "inactive", Role: auth.RoleOwner},
	} {
		_, err := NewService(nil, Options{}).CreateToken(context.Background(), user)
		if !errors.Is(err, ErrOwnerRequired) {
			t.Fatalf("CreateToken(%+v) error = %v", user, err)
		}
	}
}

func TestCreateTokenFormatAndEntropy(t *testing.T) {
	created, err := NewService(&stubStore{}, Options{TTL: 10 * time.Minute}).CreateToken(context.Background(), owner())
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	parts := strings.SplitN(created.Token, "_", 3)
	if len(parts) != 3 {
		t.Fatalf("token format has %d sections, want 3", len(parts))
	}
	if parts[0] != tokenPrefix || len(parts[1]) != tokenSelectorLen*2 || len(parts[2]) != 43 {
		t.Fatalf("token format lengths invalid: prefix=%q selectorLen=%d secretLen=%d", parts[0], len(parts[1]), len(parts[2]))
	}
}

func v2Fixture(t *testing.T) (selector string, secret []byte, caPEM string) {
	t.Helper()
	ca, err := acme.CreateInternalCA(30)
	if err != nil {
		t.Fatalf("CreateInternalCA: %v", err)
	}
	secret = make([]byte, tokenSecretLen)
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	return strings.Repeat("ab", tokenSelectorLen), secret, ca.CertificatePEM
}

func TestEncodeTokenV2RoundTrips(t *testing.T) {
	selector, secret, caPEM := v2Fixture(t)
	token, err := EncodeTokenV2(selector, secret, "https://192.0.2.10:3443/", caPEM)
	if err != nil {
		t.Fatalf("EncodeTokenV2: %v", err)
	}
	if !strings.HasPrefix(token, TokenPrefixV2+"_") || !IsTokenCredential(token) {
		t.Fatalf("token prefix wrong: %.12s", token)
	}
	claims, ok := ParseTokenClaims(token)
	if !ok || claims.Version != 2 || claims.Selector != selector || string(claims.Secret) != string(secret) ||
		claims.PrimaryURL != "https://192.0.2.10:3443/" || claims.CAPEM != strings.TrimSpace(caPEM)+"\n" {
		t.Fatalf("claims = %+v ok=%v", claims, ok)
	}
	legacy := claims.LegacyWire()
	if legacy != TokenPrefixV1+"_"+selector+"_"+base64.RawURLEncoding.EncodeToString(secret) {
		t.Fatalf("legacy wire = %q", legacy)
	}
	// Selector and secret verify exactly like a v1 token (same stored hash).
	gotSelector, gotSecret, ok := parseToken(token)
	if !ok || gotSelector != selector || string(gotSecret) != string(secret) {
		t.Fatalf("parseToken(v2) = %q %v", gotSelector, ok)
	}
}

func TestParseTokenClaimsV1StaysUnchanged(t *testing.T) {
	secret := make([]byte, tokenSecretLen)
	selector := hex.EncodeToString(make([]byte, tokenSelectorLen))
	value := TokenPrefixV1 + "_" + selector + "_" + base64.RawURLEncoding.EncodeToString(secret)
	claims, ok := ParseTokenClaims(value)
	if !ok || claims.Version != 1 || claims.PrimaryURL != "" || claims.CAPEM != "" || claims.Selector != selector {
		t.Fatalf("claims = %+v ok=%v", claims, ok)
	}
	if !IsTokenCredential(value) {
		t.Fatal("v1 token not recognized")
	}
}

func TestParseTokenClaimsRejectsMalformedV2(t *testing.T) {
	selector, secret, caPEM := v2Fixture(t)
	enc := func(payload string) string {
		return TokenPrefixV2 + "_" + base64.RawURLEncoding.EncodeToString([]byte(payload))
	}
	good := `"selector":"` + selector + `","secret":"` + base64.RawURLEncoding.EncodeToString(secret) +
		`","primaryUrl":"https://192.0.2.10:3443/","caPem":` + jsonString(caPEM)
	cases := map[string]string{
		"empty":            TokenPrefixV2 + "_",
		"not base64":       TokenPrefixV2 + "_!!!",
		"not json":         enc("nope"),
		"unknown version":  enc(`{"v":3,` + good + `}`),
		"missing version":  enc(`{` + good + `}`),
		"unknown field":    enc(`{"v":2,"extra":1,` + good + `}`),
		"trailing data":    enc(`{"v":2,` + good + `}{}`),
		"bad selector":     enc(`{"v":2,"selector":"zz","secret":"` + base64.RawURLEncoding.EncodeToString(secret) + `","primaryUrl":"https://192.0.2.10:3443/","caPem":` + jsonString(caPEM) + `}`),
		"short secret":     enc(`{"v":2,"selector":"` + selector + `","secret":"AAAA","primaryUrl":"https://192.0.2.10:3443/","caPem":` + jsonString(caPEM) + `}`),
		"non canonical":    enc(`{"v":2,"selector":"` + selector + `","secret":"` + base64.RawURLEncoding.EncodeToString(secret) + `","primaryUrl":"https://192.0.2.10:3443","caPem":` + jsonString(caPEM) + `}`),
		"http url":         enc(`{"v":2,"selector":"` + selector + `","secret":"` + base64.RawURLEncoding.EncodeToString(secret) + `","primaryUrl":"http://192.0.2.10:3443/","caPem":` + jsonString(caPEM) + `}`),
		"garbage ca":       enc(`{"v":2,"selector":"` + selector + `","secret":"` + base64.RawURLEncoding.EncodeToString(secret) + `","primaryUrl":"https://192.0.2.10:3443/","caPem":"junk"}`),
		"oversized":        TokenPrefixV2 + "_" + strings.Repeat("A", MaxTokenV2PayloadBytes*2),
		"unknown prefix":   "pcenr9_" + selector,
		"v1 shape on v2":   TokenPrefixV2 + "_" + selector + "_" + base64.RawURLEncoding.EncodeToString(secret),
		"plain whitespace": " " + TokenPrefixV2 + "_x",
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			if _, ok := ParseTokenClaims(value); ok {
				t.Fatalf("accepted %q", name)
			}
		})
	}
}

func TestEncodeTokenV2RejectsOversizedAndInvalidMaterial(t *testing.T) {
	selector, secret, caPEM := v2Fixture(t)
	if _, err := EncodeTokenV2(selector, secret, "https://192.0.2.10:3443", caPEM); err == nil {
		t.Fatal("non-canonical URL accepted")
	}
	if _, err := EncodeTokenV2(selector, secret, "https://192.0.2.10:3443/", "junk"); err == nil {
		t.Fatal("invalid CA accepted")
	}
	if _, err := EncodeTokenV2("short", secret, "https://192.0.2.10:3443/", caPEM); err == nil {
		t.Fatal("invalid selector accepted")
	}
	if _, err := EncodeTokenV2(selector, secret[:4], "https://192.0.2.10:3443/", caPEM); err == nil {
		t.Fatal("short secret accepted")
	}
}

func jsonString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
