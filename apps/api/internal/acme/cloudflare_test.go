package acme

import (
	"encoding/json"
	"testing"
)

func TestChallengeRecordNameStripsWildcard(t *testing.T) {
	t.Parallel()
	got, err := challengeRecordName("*.home.ggzdeveloper.com", "ggzdeveloper.com")
	if err != nil {
		t.Fatalf("challengeRecordName: %v", err)
	}
	want := "_acme-challenge.home.ggzdeveloper.com"
	if got != want {
		t.Fatalf("got=%q want=%q", got, want)
	}
	apex, err := challengeRecordName("home.ggzdeveloper.com", "ggzdeveloper.com")
	if err != nil {
		t.Fatalf("apex: %v", err)
	}
	if apex != want {
		t.Fatalf("apex got=%q want=%q", apex, want)
	}
}

func TestNormalizeTXTContent(t *testing.T) {
	t.Parallel()
	if got := normalizeTXTContent(`"abc"`); got != "abc" {
		t.Fatalf("got=%q", got)
	}
}

func TestCloudflareResponseAcceptsObjectOrArrayResult(t *testing.T) {
	t.Parallel()

	var list cloudflareResponse[cloudflareRecord]
	if err := json.Unmarshal([]byte(`{
		"success": true,
		"result": [{"id":"rec-1","type":"TXT","content":"token"}],
		"errors": []
	}`), &list); err != nil {
		t.Fatalf("list unmarshal: %v", err)
	}
	if len(list.Result) != 1 || list.Result[0].ID != "rec-1" {
		t.Fatalf("list result=%#v", list.Result)
	}

	var created cloudflareResponse[cloudflareRecord]
	if err := json.Unmarshal([]byte(`{
		"success": true,
		"result": {"id":"rec-2","type":"TXT","content":"token"},
		"errors": []
	}`), &created); err != nil {
		t.Fatalf("object unmarshal: %v", err)
	}
	if len(created.Result) != 1 || created.Result[0].ID != "rec-2" {
		t.Fatalf("object result=%#v", created.Result)
	}

	var empty cloudflareResponse[cloudflareRecord]
	if err := json.Unmarshal([]byte(`{"success":true,"result":null,"errors":[]}`), &empty); err != nil {
		t.Fatalf("null unmarshal: %v", err)
	}
	if len(empty.Result) != 0 {
		t.Fatalf("null result=%#v", empty.Result)
	}
}
