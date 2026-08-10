package acme

import (
	"encoding/json"
	"testing"
)

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
