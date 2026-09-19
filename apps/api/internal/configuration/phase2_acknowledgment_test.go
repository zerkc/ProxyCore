package configuration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestPhase2AcknowledgmentRejectsMalformedTuple(t *testing.T) {
	cases := []SnapshotAcknowledgement{
		{},
		{NodeID: "not-a-uuid", ContentHash: strings.Repeat("a", 64), RevisionID: "11112222-3333-4444-8999-aabbccddeeff", SnapshotVersion: 1, ReplicationVersion: 1, LeadershipGeneration: 1, AppliedAt: time.Unix(1, 0).UTC()},
		{NodeID: "11112222-3333-4444-8999-aabbccddeeff", ContentHash: strings.Repeat("A", 64), RevisionID: "11112222-3333-4444-8999-aabbccddeeff", SnapshotVersion: 1, ReplicationVersion: 1, LeadershipGeneration: 1, AppliedAt: time.Unix(1, 0).UTC()},
	}
	for _, ack := range cases {
		if err := (&PgPhase2Store{}).RecordSnapshotAcknowledgement(context.Background(), ack); !errors.Is(err, ErrSnapshotAcknowledgementDenied) {
			t.Fatalf("ack=%+v error=%v want ErrSnapshotAcknowledgementDenied", ack, err)
		}
	}
}

func TestPhase2AcknowledgmentRequestRedactsPresentedCredential(t *testing.T) {
	request := SnapshotAcknowledgementRequest{PresentedCredential: "pcnode1_secret-material"}
	if strings.Contains(fmt.Sprintf("%+v", request), "secret-material") {
		t.Fatal("acknowledgement request formatter exposed presented credential")
	}
}

func TestPhase2AcknowledgmentUnavailableSentinel(t *testing.T) {
	ack := SnapshotAcknowledgement{
		NodeID:               "11112222-3333-4444-8999-aabbccddeeff",
		ContentHash:          "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		SnapshotVersion:      1,
		ReplicationVersion:   1,
		RevisionID:           "22223333-4444-4555-8666-bbbbbbbbbbbb",
		LeadershipGeneration: 1,
		AppliedAt:            time.Unix(1, 0).UTC(),
	}
	if err := (&PgPhase2Store{}).RecordSnapshotAcknowledgement(context.Background(), ack); !errors.Is(err, ErrSnapshotAcknowledgementUnavailable) {
		t.Fatalf("error=%v, want ErrSnapshotAcknowledgementUnavailable", err)
	}
}
