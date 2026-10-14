package configuration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
)

func (f *grantFixture) enrollOne() (nodeID string) {
	f.t.Helper()
	token, _ := f.insertToken("a1", 0x41, f.now.Add(time.Hour))
	if _, err := f.service.IssueGrant(context.Background(), f.request(token, newRecipient(f.t))); err != nil {
		f.t.Fatal(err)
	}
	if err := f.db.QueryRow(`select node_id from enrolled_nodes`).Scan(&nodeID); err != nil {
		f.t.Fatal(err)
	}
	return nodeID
}

func TestSQLiteUnlinkEnrolledNodeRevokesAndMarksOnce(t *testing.T) {
	f := newGrantFixture(t)
	node := f.enrollOne()
	at := f.now.Add(time.Minute)
	got, err := f.store.UnlinkEnrolledNode(context.Background(), node, at)
	if err != nil || !got.Equal(at) {
		t.Fatalf("unlink = %v, %v", got, err)
	}
	if f.count(`select count(*) from enrolled_nodes where revoked_at is not null and unlinked_at = revoked_at`) != 1 ||
		f.count(`select count(*) from node_credentials where revoked_at is not null`) != 1 {
		t.Fatal("unlink must revoke the credential and mark the node in one write")
	}
	// Idempotent: a second unlink keeps the first timestamp.
	again, err := f.store.UnlinkEnrolledNode(context.Background(), node, at.Add(time.Hour))
	if err != nil || !again.Equal(at) {
		t.Fatalf("second unlink = %v, %v", again, err)
	}
}

func TestSQLiteUnlinkEnrolledNodeUnknownAndMalformed(t *testing.T) {
	f := newGrantFixture(t)
	f.enrollOne()
	if _, err := f.store.UnlinkEnrolledNode(context.Background(), uuid.NewString(), f.now); !errors.Is(err, configuration.ErrEnrolledNodeNotFound) {
		t.Fatalf("unknown node err = %v", err)
	}
	if _, err := f.store.UnlinkEnrolledNode(context.Background(), "not-a-uuid", f.now); !errors.Is(err, configuration.ErrEnrolledNodeNotFound) {
		t.Fatalf("malformed node err = %v", err)
	}
	if f.count(`select count(*) from enrolled_nodes where revoked_at is not null or unlinked_at is not null`) != 0 {
		t.Fatal("failed unlink must not mutate rows")
	}
}

func TestSQLitePrimaryGrantReenrollClearsUnlink(t *testing.T) {
	f := newGrantFixture(t)
	ctx := context.Background()
	node := f.enrollOne()
	if _, err := f.store.UnlinkEnrolledNode(ctx, node, f.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(2 * time.Minute)
	second, _ := f.insertToken("b2", 0x42, f.now.Add(time.Hour))
	if _, err := f.service.IssueGrant(ctx, f.reenrollRequest(second, "66667777-3333-4444-8999-aabbccddeeff")); err != nil {
		t.Fatalf("re-enroll after unlink: %v", err)
	}
	if f.count(`select count(*) from enrolled_nodes where unlinked_at is null and revoked_at is null`) != 1 ||
		f.count(`select count(*) from node_credentials where revoked_at is null`) != 1 {
		t.Fatal("re-enrollment must clear the unlink marker and revocation")
	}
}
