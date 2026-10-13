package domain

import (
	"errors"
	"testing"
)

func TestValidateRecordSetCNAMECoexistenceIsValidationError(t *testing.T) {
	cname := DNSRecord{ID: "c", Name: "www", Type: "CNAME", Value: "target.example.test", Enabled: true}
	others := map[string]DNSRecord{
		"A":     {ID: "o", Name: "www", Type: "A", Value: "192.0.2.1", Enabled: true},
		"AAAA":  {ID: "o", Name: "www", Type: "AAAA", Value: "2001:db8::1", Enabled: true},
		"TXT":   {ID: "o", Name: "www", Type: "TXT", Value: "hello", Enabled: true},
		"MX":    {ID: "o", Name: "www", Type: "MX", Value: map[string]any{"priority": 10, "exchange": "mx.example.test"}, Enabled: true},
		"CNAME": {ID: "o", Name: "www.example.test", Type: "CNAME", Value: "other.example.test", Enabled: true},
	}
	const want = "CNAME cannot coexist with other records for www.example.test"
	for label, other := range others {
		for _, order := range []string{"cname-first", "cname-last"} {
			t.Run(label+"/"+order, func(t *testing.T) {
				set := []DNSRecord{cname, other}
				if order == "cname-last" {
					set = []DNSRecord{other, cname}
				}
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("ValidateRecordSet panicked: %v", r)
					}
				}()
				got, err := ValidateRecordSet(set, RecordSetOptions{ZoneName: "example.test"})
				var ve *ValidationError
				if !errors.As(err, &ve) {
					t.Fatalf("error = %T %v, want *ValidationError", err, err)
				}
				if ve.Message != want || ve.Code != "CNAME_COEXISTENCE" || got != nil {
					t.Fatalf("message=%q code=%q result=%v", ve.Message, ve.Code, got)
				}
			})
		}
	}
}

func TestValidateRecordSetCNAMEAllowsDisabledSiblingsAndOtherNames(t *testing.T) {
	set := []DNSRecord{
		{ID: "c", Name: "www", Type: "CNAME", Value: "target.example.test", Enabled: true},
		{ID: "d", Name: "www", Type: "A", Value: "192.0.2.1", Enabled: false},
		{ID: "e", Name: "api", Type: "A", Value: "192.0.2.2", Enabled: true},
	}
	if _, err := ValidateRecordSet(set, RecordSetOptions{ZoneName: "example.test"}); err != nil {
		t.Fatal(err)
	}
}
