package configuration

import "context"

// RenewalStore is the single operation the certificate renewal worker needs
// from a persistence backend.
type RenewalStore interface {
	RenewDueCertificates(ctx context.Context, opts RenewalOptions) (renewed int, failed int, err error)
}

// ApplyJobEnqueuer enqueues an apply job for the current desired state. The
// post-restore apply trigger in backupcore/httpexport consumes it.
type ApplyJobEnqueuer interface {
	CreateApplyJob(ctx context.Context, actorUserID string) (ApplyResult, error)
}

var (
	_ RenewalStore     = (*Store)(nil)
	_ ApplyJobEnqueuer = (*Store)(nil)
)
