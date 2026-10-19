package httpserver

import (
	"strings"
	"testing"

	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

func TestValidationSummaryNamesIssueCodesWithoutMessages(t *testing.T) {
	result := replicationsnapshot.ValidationResult{
		OK: false,
		Issues: []replicationsnapshot.ValidationIssue{
			{Code: "SECRET_ID", Field: "replicated.secrets[].id", Message: "secret 3f2b1c4e-0000-4000-8000-000000000000 pcenr1_abcdef0123456789abcdef0123456789 leaked"},
			{Code: "SECRET_ID", Field: "replicated.secrets[].id", Message: "another message"},
			{Code: "SECRET_DECRYPT", Field: "replicated.secrets[].envelope", Message: "cannot decrypt: key=supersecretvalue"},
		},
	}
	got := validationSummary(result)

	for _, want := range []string{
		"ok=false", "issues=3",
		"SECRET_DECRYPT@replicated.secrets[].envelope x1",
		"SECRET_ID@replicated.secrets[].id x2",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q is missing %q", got, want)
		}
	}
	for _, leak := range []string{"3f2b1c4e", "pcenr1_", "supersecretvalue", "leaked", "another message"} {
		if strings.Contains(got, leak) {
			t.Errorf("summary %q leaks %q", got, leak)
		}
	}
}

func TestValidationSummaryHandlesNoIssuesAndUnsafeTokens(t *testing.T) {
	if got := validationSummary(replicationsnapshot.ValidationResult{OK: true}); got != "ok=true issues=0 codes=[]" {
		t.Errorf("clean summary = %q", got)
	}
	unsafe := replicationsnapshot.ValidationResult{Issues: []replicationsnapshot.ValidationIssue{
		{Code: "BAD code=hunter2", Field: "a b/c"},
	}}
	got := validationSummary(unsafe)
	if strings.Contains(got, "hunter2") && strings.Contains(got, " code=") {
		t.Errorf("unsafe token passed through: %q", got)
	}
	if strings.ContainsAny(strings.TrimPrefix(got, "ok=false issues=1 codes=["), " =/") && strings.Contains(got, "BAD code") {
		t.Errorf("free text survived: %q", got)
	}
}
