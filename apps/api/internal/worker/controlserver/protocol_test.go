package controlserver

import (
	"errors"
	"strings"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/worker/control"
)

const validLine = `{"requestId":"request-1","operation":"validate","service":"coredns","revisionId":"revision-1","checksum":"abc","candidatePath":"/candidates/revision-1"}`

func TestParseRequestAcceptsClientBytes(t *testing.T) {
	want := control.Request{RequestID: "request-1", Operation: control.OperationValidate, Service: control.ServiceCoreDNS,
		RevisionID: "revision-1", Checksum: "abc", CandidatePath: "/candidates/revision-1"}
	line, err := control.MarshalRequest(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseRequest(line[:len(line)-1])
	if err != nil || got != want {
		t.Fatalf("ParseRequest = %+v, %v", got, err)
	}
}

func TestParseRequestDropsUnknownKeysAndKeepsLastDuplicate(t *testing.T) {
	line := `{"extra":1,"requestId":"a","requestId":"b","operation":"stage","service":"nginx","revisionId":"","checksum":"","candidatePath":"/x"}`
	got, err := ParseRequest([]byte(line))
	if err != nil || got.RequestID != "b" || got.Service != control.ServiceNginx {
		t.Fatalf("ParseRequest = %+v, %v", got, err)
	}
}

func TestParseRequestRejections(t *testing.T) {
	base := func(mutate string) string { return strings.Replace(validLine, mutate, "", 1) }
	cases := []struct {
		name, line, msg string
		invalidJSON     bool
	}{
		{"command key", `{"requestId":"r","operation":"exec","service":"nginx","command":"rm -rf /"}`, "Arbitrary command fields are not allowed", false},
		{"shell key even when null", strings.Replace(validLine, "{", `{"shell":null,`, 1), "Arbitrary command fields are not allowed", false},
		{"args key", strings.Replace(validLine, "{", `{"args":[],`, 1), "Arbitrary command fields are not allowed", false},
		{"missing field", base(`,"checksum":"abc"`), "Control request fields are invalid", false},
		{"non-string field", strings.Replace(validLine, `"checksum":"abc"`, `"checksum":1`, 1), "Control request fields are invalid", false},
		{"null field", strings.Replace(validLine, `"revisionId":"revision-1"`, `"revisionId":null`, 1), "Control request fields are invalid", false},
		{"array body", `[]`, "Control request fields are invalid", false},
		{"unknown operation", strings.Replace(validLine, `"validate"`, `"exec"`, 1), "Unknown control operation: exec", false},
		{"unknown service", strings.Replace(validLine, `"coredns"`, `"redis"`, 1), "Unknown control service: redis", false},
		{"relative path", strings.Replace(validLine, `"/candidates/revision-1"`, `"candidates"`, 1), "Candidate path is invalid", false},
		{"dot dot path", strings.Replace(validLine, `/candidates/revision-1`, `/candidates/../etc`, 1), "Candidate path is invalid", false},
		{"dot dot inside name", strings.Replace(validLine, `/candidates/revision-1`, `/a..b`, 1), "Candidate path is invalid", false},
		{"nul path", strings.Replace(validLine, `/candidates/revision-1`, `/a\u0000b`, 1), "Candidate path is invalid", false},
		{"empty path", strings.Replace(validLine, `"/candidates/revision-1"`, `""`, 1), "Candidate path is invalid", false},
		{"operation checked before service", `{"requestId":"r","operation":"x","service":"y","revisionId":"","checksum":"","candidatePath":"bad"}`, "Unknown control operation: x", false},
		{"fields checked before operation", `{"requestId":"r","operation":"x","service":"y"}`, "Control request fields are invalid", false},
		{"null body", `null`, `parsed is not an Object. (evaluating '"command" in parsed')`, false},
		{"number body", `5`, `parsed is not an Object. (evaluating '"command" in parsed')`, false},
		{"string body", `"s"`, `parsed is not an Object. (evaluating '"command" in parsed')`, false},
		{"empty line", ``, "JSON Parse error: Unexpected EOF", true},
		{"truncated array", `[1,`, "JSON Parse error: Unexpected EOF", true},
		{"identifier", `nul`, `JSON Parse error: Unexpected identifier "nul"`, true},
		{"other syntax error", `{"a" 1}`, "JSON Parse error: Unable to parse JSON string", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseRequest([]byte(tc.line))
			var parseErr *ParseError
			if !errors.As(err, &parseErr) {
				t.Fatalf("err = %v, want *ParseError", err)
			}
			if parseErr.Error() != tc.msg || parseErr.InvalidJSON != tc.invalidJSON {
				t.Fatalf("err = %q (invalidJSON=%v), want %q (%v)", parseErr.Error(), parseErr.InvalidJSON, tc.msg, tc.invalidJSON)
			}
		})
	}
}

func TestParseRequestLengthLimitCountsUTF16Units(t *testing.T) {
	pad := func(units int, filler string) string {
		overhead := len(validLine) - len(`"abc"`) + 2
		per := 1
		if filler == "😀" {
			per = 2
		}
		return strings.Replace(validLine, `"abc"`, `"`+strings.Repeat(filler, (units-overhead)/per)+`"`, 1)
	}
	for _, filler := range []string{"a", "é", "😀"} {
		ok := pad(65_536, filler)
		if _, err := ParseRequest([]byte(ok)); err != nil {
			t.Fatalf("%q at the limit: %v", filler, err)
		}
		over := strings.Replace(ok, `"}`, filler+filler+`"}`, 1)
		if _, err := ParseRequest([]byte(over)); err == nil || err.Error() != "Control request is too large" {
			t.Fatalf("%q over the limit: err = %v", filler, err)
		}
	}
}
