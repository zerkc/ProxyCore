package httpserver_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

// PATCH /api/zones/{zoneId}/records/{recordId}: every spelling the database
// accepts for the record id (or the zone id) addresses the same record, updated
// in place; unknown and malformed ids keep their 404.
func TestPatchRecordAcceptsEveryRecordIDSpelling(t *testing.T) {
	srv := newConfigTestServer(t)
	handler := srv.Handler()
	doJSON(t, handler, http.MethodPost, "/api/auth/bootstrap", map[string]any{"username": "owner", "password": "correct horse battery staple"}, nil)
	login := doJSON(t, handler, http.MethodPost, "/api/auth/login", map[string]any{"username": "owner", "password": "correct horse battery staple"}, nil)
	if login.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", login.Code, login.Body.String())
	}
	cookie := login.Result().Cookies()[0]
	put := doJSON(t, handler, http.MethodPut, "/api/settings", map[string]any{
		"defaultPool": map[string]any{"id": "default", "endpoints": []map[string]any{{"host": "1.1.1.1", "port": 53}}},
	}, cookie)
	if put.Code != http.StatusOK {
		t.Fatalf("settings status=%d body=%s", put.Code, put.Body.String())
	}
	zoneRes := doJSON(t, handler, http.MethodPost, "/api/zones", map[string]any{"name": "example.test"}, cookie)
	var zoneBody struct {
		Zone domain.ZoneState `json:"zone"`
	}
	if err := json.Unmarshal(zoneRes.Body.Bytes(), &zoneBody); err != nil || zoneRes.Code != http.StatusCreated {
		t.Fatalf("create zone status=%d err=%v body=%s", zoneRes.Code, err, zoneRes.Body.String())
	}
	zoneID := zoneBody.Zone.ID
	recRes := doJSON(t, handler, http.MethodPost, "/api/zones/"+zoneID+"/records", map[string]any{"name": "www", "type": "A", "value": "10.0.0.5"}, cookie)
	var recBody struct {
		Record domain.DNSRecord `json:"record"`
	}
	if err := json.Unmarshal(recRes.Body.Bytes(), &recBody); err != nil || recRes.Code != http.StatusCreated {
		t.Fatalf("add record status=%d err=%v body=%s", recRes.Code, err, recRes.Body.String())
	}
	recordID := recBody.Record.ID

	patch := func(zone, record, value string) *httpTestResponse {
		res := doJSON(t, handler, http.MethodPatch, "/api/zones/"+zone+"/records/"+record,
			map[string]any{"name": "www", "type": "A", "value": value}, cookie)
		return &httpTestResponse{code: res.Code, body: res.Body.String()}
	}
	currentRecords := func() []domain.DNSRecord {
		res := doJSON(t, handler, http.MethodGet, "/api/zones/"+zoneID+"/records", nil, cookie)
		var body struct {
			Zone domain.ZoneState `json:"zone"`
		}
		if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode zone: %v body=%s", err, res.Body.String())
		}
		return body.Zone.Records
	}

	spellings := []struct{ name, id string }{
		{"canonical", recordID},
		{"uppercase", strings.ToUpper(recordID)},
		{"hyphen-less", strings.ReplaceAll(recordID, "-", "")},
		{"braced", "%7B" + recordID + "%7D"},
	}
	for i, sp := range spellings {
		value := "10.0.0." + string(rune('6'+i))
		t.Run(sp.name, func(t *testing.T) {
			res := patch(zoneID, sp.id, value)
			if res.code != http.StatusOK {
				t.Fatalf("status=%d body=%s", res.code, res.body)
			}
			var body struct {
				Record domain.DNSRecord `json:"record"`
			}
			if err := json.Unmarshal([]byte(res.body), &body); err != nil || body.Record.ID != recordID || body.Record.Value != value {
				t.Fatalf("record = %+v err=%v", body.Record, err)
			}
			if records := currentRecords(); len(records) != 1 || records[0].ID != recordID || records[0].Value != value {
				t.Fatalf("records after patch = %+v", records)
			}
		})
	}

	t.Run("alias zone id and alias record id together", func(t *testing.T) {
		res := patch(strings.ToUpper(zoneID), strings.ToUpper(recordID), "10.0.0.99")
		if res.code != http.StatusOK {
			t.Fatalf("status=%d body=%s", res.code, res.body)
		}
		if records := currentRecords(); len(records) != 1 || records[0].Value != "10.0.0.99" {
			t.Fatalf("records = %+v", records)
		}
	})
	t.Run("unknown record id", func(t *testing.T) {
		res := patch(zoneID, "eeeeeeee-0000-4000-8000-0000000000ff", "10.0.0.50")
		if res.code != http.StatusNotFound || !strings.Contains(res.body, "Record not found") {
			t.Fatalf("status=%d body=%s", res.code, res.body)
		}
	})
	t.Run("malformed record id keeps its 404", func(t *testing.T) {
		res := patch(zoneID, "not-a-record", "10.0.0.50")
		if res.code != http.StatusNotFound || !strings.Contains(res.body, "Record not found") {
			t.Fatalf("status=%d body=%s", res.code, res.body)
		}
	})
	if records := currentRecords(); len(records) != 1 || records[0].Value != "10.0.0.99" {
		t.Fatalf("final records = %+v", records)
	}
}

type httpTestResponse struct {
	code int
	body string
}
