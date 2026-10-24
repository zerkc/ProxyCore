package controlserver

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/worker/control"
)

func req(op control.Operation, svc control.Service) control.Request {
	return control.Request{RequestID: "r-1", Operation: op, Service: svc, RevisionID: "rev", Checksum: "sum", CandidatePath: "/c/rev"}
}

func TestDispatcherExecute(t *testing.T) {
	d := NewDispatcher(Handlers{
		control.ServiceCoreDNS: {
			control.OperationHealth: func(context.Context, control.Request) (json.RawMessage, error) {
				return json.RawMessage(`{"healthy":true}`), nil
			},
			control.OperationReload: func(context.Context, control.Request) (json.RawMessage, error) { return json.RawMessage("null"), nil },
			control.OperationStage:  func(context.Context, control.Request) (json.RawMessage, error) { return nil, nil },
			control.OperationPromote: func(context.Context, control.Request) (json.RawMessage, error) {
				return json.RawMessage(`{ "a" : 1 }`), nil
			},
			control.OperationRollback: func(context.Context, control.Request) (json.RawMessage, error) { return json.RawMessage(`{`), nil },
		},
		control.ServiceNginx: {
			control.OperationValidate: func(context.Context, control.Request) (json.RawMessage, error) {
				return nil, errors.New("private credential")
			},
		},
	})
	ctx := context.Background()
	cases := []struct {
		name string
		in   control.Request
		want string
	}{
		{"output", req(control.OperationHealth, control.ServiceCoreDNS), `{"requestId":"r-1","operation":"health","ok":true,"output":{"healthy":true}}`},
		{"null output", req(control.OperationReload, control.ServiceCoreDNS), `{"requestId":"r-1","operation":"reload","ok":true,"output":null}`},
		{"absent output", req(control.OperationStage, control.ServiceCoreDNS), `{"requestId":"r-1","operation":"stage","ok":true}`},
		{"output is compacted", req(control.OperationPromote, control.ServiceCoreDNS), `{"requestId":"r-1","operation":"promote","ok":true,"output":{"a":1}}`},
		{"invalid output fails the operation", req(control.OperationRollback, control.ServiceCoreDNS), `{"requestId":"r-1","operation":"rollback","ok":false,"error":"Control operation failed"}`},
		{"handler error", req(control.OperationValidate, control.ServiceNginx), `{"requestId":"r-1","operation":"validate","ok":false,"error":"private credential"}`},
		{"missing handler", req(control.OperationRollback, control.ServiceNginx), `{"requestId":"r-1","operation":"rollback","ok":false,"error":"Operation rollback is unavailable for nginx"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(MarshalResponse(d.Execute(ctx, tc.in))); got != tc.want+"\n" {
				t.Fatalf("got  %q\nwant %q", got, tc.want+"\n")
			}
		})
	}
}

func TestDispatcherRecoversHandlerPanic(t *testing.T) {
	d := NewDispatcher(Handlers{control.ServiceNginx: {control.OperationHealth: func(context.Context, control.Request) (json.RawMessage, error) {
		panic("boom")
	}}})
	got := d.Execute(context.Background(), req(control.OperationHealth, control.ServiceNginx))
	if got.OK || got.Error != "Control operation failed" {
		t.Fatalf("got %+v", got)
	}
}

func TestMarshalResponseMatchesJSStringify(t *testing.T) {
	got := string(MarshalResponse(Response{RequestID: "<a>&'  \u007f\x01\b\f\n\r\t\"\\é😀", Operation: control.OperationHealth, OK: false, Error: ""}))
	want := "{\"requestId\":\"<a>&'  \u007f\\u0001\\b\\f\\n\\r\\t\\\"\\\\é😀\",\"operation\":\"health\",\"ok\":false,\"error\":\"\"}\n"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestFixedBoundaryHandlers(t *testing.T) {
	d := NewDispatcher(FixedBoundaryHandlers())
	for _, svc := range []control.Service{control.ServiceCoreDNS, control.ServiceNginx} {
		for _, op := range control.Operations {
			in := req(op, svc)
			in.CandidatePath = "/c/<x>"
			want := `{"requestId":"r-1","operation":"` + string(op) + `","ok":true,"output":{"service":"` + string(svc) +
				`","operation":"` + string(op) + `","candidatePath":"/c/<x>","status":"accepted-by-fixed-boundary"}}` + "\n"
			if got := string(MarshalResponse(d.Execute(context.Background(), in))); got != want {
				t.Fatalf("%s/%s\n got %q\nwant %q", svc, op, got, want)
			}
		}
	}
}
