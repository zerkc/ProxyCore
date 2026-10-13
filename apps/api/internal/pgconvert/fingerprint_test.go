package pgconvert

import (
	"context"
	"regexp"
	"testing"
)

func fingerprintOf(t *testing.T, f pgFixture) string {
	t.Helper()
	ctx := context.Background()
	src, err := OpenSource(ctx, f.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close(ctx)
	fp, err := src.SchemaFingerprint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

func TestSchemaFingerprintIsStableAndSchemaSensitive(t *testing.T) {
	f := newPGFixture(t)
	first := fingerprintOf(t, f)
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(first) {
		t.Fatalf("fingerprint %q", first)
	}
	if again := fingerprintOf(t, f); again != first {
		t.Errorf("fingerprint not stable: %s vs %s", first, again)
	}
	f.exec(t, `alter table zones add column extra text`)
	if changed := fingerprintOf(t, f); changed == first {
		t.Error("fingerprint ignored a schema change")
	}
}
