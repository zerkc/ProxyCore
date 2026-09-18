package sync

import (
	"context"
	"sync"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
)

type gatedSnapshotIdentityReader struct {
	inner   SnapshotIdentityReader
	ready   chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gatedSnapshotIdentityReader) SnapshotPublicationIdentity() (identity.Identity, error) {
	current, err := g.inner.SnapshotPublicationIdentity()
	g.once.Do(func() { close(g.ready) })
	if err != nil {
		return identity.Identity{}, err
	}
	<-g.release
	return current, nil
}

func TestPostgresPublisherLocksAuthRowsBeforeConcurrentRevoke(t *testing.T) {
	fixture := newPublisherPostgresFixture(t)
	defer fixture.credential.Destroy()
	authLocked := make(chan struct{})
	release := make(chan struct{})
	hookedStore := fixture.store.WithSnapshotPublicationHooks(configuration.SnapshotPublicationHooks{
		AfterCredentialAuthorization: func() {
			close(authLocked)
			<-release
		},
	})
	publisher := NewSnapshotPublisher(hookedStore, fixture.publisher.identity, SnapshotPublisherOptions{})
	resultCh := make(chan SnapshotResult, 1)
	errCh := make(chan error, 1)
	go func() {
		result, err := publisher.Publish(context.Background(), SnapshotRequest{Credential: fixture.credential.BearerCopy()})
		resultCh <- result
		errCh <- err
	}()
	<-authLocked
	revokeDone := make(chan error, 1)
	go func() {
		tx, err := fixture.pool.Begin(context.Background())
		if err != nil {
			revokeDone <- err
			return
		}
		defer tx.Rollback(context.Background())
		if _, err := tx.Exec(context.Background(), `update node_credentials set revoked_at = now() where id = $1`, fixture.credential.ID()); err != nil {
			revokeDone <- err
			return
		}
		if _, err := tx.Exec(context.Background(), `update enrolled_nodes set revoked_at = now() where credential_id = $1`, fixture.credential.ID()); err != nil {
			revokeDone <- err
			return
		}
		revokeDone <- tx.Commit(context.Background())
	}()
	waitForBlockedPublicationRowLock(t, fixture.pool)
	close(release)
	result := <-resultCh
	if err := <-errCh; err != nil {
		t.Fatalf("publisher with first lock error = %v", err)
	}
	if result.Current || len(result.Bytes) == 0 {
		t.Fatalf("publisher with first lock result = %#v", result)
	}
	if err := <-revokeDone; err != nil {
		t.Fatalf("revoke after publication lock: %v", err)
	}
	if _, err := publisher.Publish(context.Background(), SnapshotRequest{Credential: fixture.credential.BearerCopy()}); err != ErrSnapshotDenied {
		t.Fatalf("post-commit revoke error = %v, want %v", err, ErrSnapshotDenied)
	}
}

func TestPostgresPublisherDeniesRevocationCommittedDuringRequest(t *testing.T) {
	fixture := newPublisherPostgresFixture(t)
	defer fixture.credential.Destroy()
	identityReader := &gatedSnapshotIdentityReader{
		inner: fixture.publisher.identity,
		ready: make(chan struct{}), release: make(chan struct{}),
	}
	fixture.publisher.identity = identityReader
	resultCh := make(chan SnapshotResult, 1)
	errCh := make(chan error, 1)
	go func() {
		result, err := fixture.publisher.Publish(context.Background(), SnapshotRequest{Credential: fixture.credential.BearerCopy()})
		resultCh <- result
		errCh <- err
	}()
	<-identityReader.ready
	tx, err := fixture.pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin revoke transaction: %v", err)
	}
	if _, err := tx.Exec(context.Background(), `update node_credentials set revoked_at = now() where id = $1`, fixture.credential.ID()); err != nil {
		t.Fatalf("revoke credential: %v", err)
	}
	if _, err := tx.Exec(context.Background(), `update enrolled_nodes set revoked_at = now() where credential_id = $1`, fixture.credential.ID()); err != nil {
		t.Fatalf("revoke node: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit revocation: %v", err)
	}
	close(identityReader.release)
	result := <-resultCh
	if err := <-errCh; err != ErrSnapshotDenied {
		t.Fatalf("revoked request error = %v, want %v", err, ErrSnapshotDenied)
	}
	if result.Current || len(result.Bytes) != 0 {
		t.Fatalf("revoked request exposed a body: %#v", result)
	}
}
