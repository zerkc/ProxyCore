package secrets

import "context"

// Store persists encrypted secrets.
type Store interface {
	Put(ctx context.Context, purpose, plaintext string) (string, error)
	Get(ctx context.Context, id string) (string, error)
}
