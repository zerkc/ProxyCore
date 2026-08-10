package configuration

import (
	"context"
	"log"
	"time"
)

// RunRenewalLoop periodically renews due Let's Encrypt and internal certificates until ctx is done.
func RunRenewalLoop(ctx context.Context, store *Store, opts RenewalOptions, interval time.Duration) {
	if store == nil {
		return
	}
	if interval <= 0 {
		interval = time.Hour
	}
	logger := opts.Log
	if logger == nil {
		logger = log.Default()
	}

	run := func() {
		renewed, failed, err := store.RenewDueCertificates(ctx, opts)
		if err != nil {
			logger.Printf("certificate renewal sweep failed: %v", err)
			return
		}
		if renewed > 0 || failed > 0 {
			logger.Printf("certificate renewal sweep: renewed=%d failed=%d", renewed, failed)
		}
	}

	// Run once shortly after startup, then on the interval.
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			run()
			timer.Reset(interval)
		}
	}
}
