package httpserver_test

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	syncpublication "github.com/zerkc/ProxyCore/apps/api/internal/sync"
)

type recordingHeartbeat struct {
	mu    sync.Mutex
	beats []configuration.NodeHeartbeat
	err   error
}

func (r *recordingHeartbeat) RecordNodeHeartbeat(_ context.Context, beat configuration.NodeHeartbeat) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.beats = append(r.beats, beat)
	return r.err
}

func (r *recordingHeartbeat) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.beats)
}

func publicationRequestWithIngress(fixture *publicationFixture, handler http.Handler, bearer, v4, v6 string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, httpserver.EnrollmentSnapshotPublicationPath, nil)
	request.Host = "primary.example:3443"
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set(syncpublication.IngressIPv4Header, v4)
	request.Header.Set(syncpublication.IngressIPv6Header, v6)
	request.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true, ServerName: "primary.example"}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestSnapshotPublicationRecordsHeartbeatOnlyOnSuccessfulAuth(t *testing.T) {
	for _, tc := range []struct {
		name      string
		revoked   bool
		sinkErr   error
		wantBeats int
		wantCode  int
	}{
		{name: "authenticated", wantBeats: 1, wantCode: http.StatusOK},
		{name: "revoked", revoked: true, wantBeats: 0, wantCode: http.StatusGone},
		{name: "sink failure never fails the pull", sinkErr: errors.New("db down"), wantBeats: 1, wantCode: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPublicationFixture(t, tc.revoked)
			sink := &recordingHeartbeat{err: tc.sinkErr}
			handler := httpserver.NewEnrollmentSnapshotPublicationHandler(httpserver.EnrollmentSnapshotPublicationHandlerOptions{
				Store: fixture.store, Identity: fixture.identity, Certificate: publicationCertificate, Heartbeat: sink,
			})
			response := publicationRequestWithIngress(fixture, handler, fixture.credential.BearerCopy(), "203.0.113.7", "2001:db8::1")
			if response.Code != tc.wantCode || sink.count() != tc.wantBeats {
				t.Fatalf("status=%d beats=%d", response.Code, sink.count())
			}
			if tc.wantBeats == 1 {
				beat := sink.beats[0]
				if beat.NodeID != fixture.nodeID || beat.IngressIPv4 != "203.0.113.7" || beat.IngressIPv6 != "2001:db8::1" || beat.At.IsZero() {
					t.Fatalf("beat=%+v", beat)
				}
			}
		})
	}
}

func TestAcknowledgementRecordsHeartbeatOnlyWhenAccepted(t *testing.T) {
	for _, accept := range []bool{true, false} {
		fixture := newAcknowledgementFixture(t)
		sink := &recordingHeartbeat{}
		authorizer := acknowledgementAuthorizerFunc(func(_ context.Context, request configuration.SnapshotAcknowledgementRequest) error {
			if _, err := request.Authenticate(request.PresentedCredential, fixture.credentialRecord()); err != nil {
				return err
			}
			if !accept {
				return configuration.ErrSnapshotAcknowledgementDenied
			}
			return nil
		})
		handler := httpserver.NewEnrollmentSnapshotAcknowledgementHandler(httpserver.EnrollmentSnapshotAcknowledgementHandlerOptions{
			Authorizer: authorizer, Identity: fixture.identity, Certificate: acknowledgementCertificate, Heartbeat: sink,
		})
		response := fixture.request(handler, "https://primary.example:3443/", fixture.credential.BearerCopy())
		want := 0
		if accept {
			want = 1
			if response.Code != http.StatusNoContent {
				t.Fatalf("accepted ack status=%d", response.Code)
			}
		}
		if sink.count() != want {
			t.Fatalf("accept=%v beats=%d", accept, sink.count())
		}
	}
}

func TestThrottledNodeHeartbeatSkipsRepeatsUnlessGatewayChanges(t *testing.T) {
	inner := &recordingHeartbeat{}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	sink := httpserver.NewThrottledNodeHeartbeat(inner, 15*time.Second, func() time.Time { return now })
	node := "99990000-1111-4222-8333-444455556666"
	beat := func(v4 string) {
		_ = sink.RecordNodeHeartbeat(context.Background(), configuration.NodeHeartbeat{NodeID: node, At: now, IngressIPv4: v4})
	}
	beat("203.0.113.7")
	beat("203.0.113.7")
	if inner.count() != 1 {
		t.Fatalf("repeat within interval wrote %d times", inner.count())
	}
	beat("203.0.113.8")
	if inner.count() != 2 {
		t.Fatalf("gateway change must write, got %d", inner.count())
	}
	now = now.Add(16 * time.Second)
	beat("203.0.113.8")
	if inner.count() != 3 {
		t.Fatalf("after interval must write, got %d", inner.count())
	}
}
