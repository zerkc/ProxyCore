package configuration

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSQLitePublicationCredentialRecordCarriesNodeUnlinkedAt(t *testing.T) {
	f := newSQLitePublicationFixture(t)
	body, hash := publicationEnvelopeBody(t, f.primaryID)
	f.insertSnapshot(t, body, hash, true)
	capture := func() SnapshotPublicationCredentialRecord {
		var got SnapshotPublicationCredentialRecord
		request := publicationRequest(f)
		request.Authenticate = func(_ string, record SnapshotPublicationCredentialRecord) (SnapshotPublicationPrincipal, error) {
			got = record
			return SnapshotPublicationPrincipal{}, errors.New("captured")
		}
		_, _ = f.store.ReadSnapshotPublication(context.Background(), request)
		return got
	}
	if record := capture(); record.ID == "" || record.NodeUnlinkedAt != nil {
		t.Fatalf("live node record=%+v", record)
	}
	phase2 := &SQLitePhase2Store{writer: f.writer}
	if _, err := phase2.UnlinkEnrolledNode(context.Background(), f.peerID, time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	record := capture()
	if record.NodeUnlinkedAt == nil || record.NodeRevokedAt == nil || record.CredentialRevokedAt == nil {
		t.Fatalf("unlinked node record=%+v", record)
	}
}
