package main

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/apply"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/config"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/control"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/materialize"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/poller"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/render"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/revisions"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/schemawait"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/store"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/wakeup"
)

// workerIdentity prefixes the startup line, like workerIdentity() in TS.
const workerIdentity = "proxycore-worker"

// runOptions overrides production timing for tests; zero values select the
// production defaults.
type runOptions struct {
	heartbeatInterval time.Duration
	schema            schemawait.Options
	wakeupPoll        time.Duration
}

// lineLogger writes "<ISO> <message>\n" lines, safe for concurrent use.
func lineLogger(w io.Writer) func(string) {
	var mu sync.Mutex
	return func(message string) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(w, "%s %s\n", isoTime(time.Now()), message)
	}
}

// run is the worker process: it returns nil after a clean shutdown (ctx
// cancelled) and an error for a fatal startup or configuration failure.
func run(ctx context.Context, lookup config.Lookup, stdout, stderr io.Writer, options runOptions) error {
	cfg, err := config.Load(lookup)
	if err != nil {
		return err
	}
	logOut, logErr := lineLogger(stdout), lineLogger(stderr)

	beat := heartbeat{path: cfg.HeartbeatPath, now: time.Now}
	if err := beat.prepare(); err != nil {
		return err
	}
	interval := options.heartbeatInterval
	if interval <= 0 {
		interval = heartbeatInterval
	}
	background, stopBackground := context.WithCancel(context.WithoutCancel(ctx))
	var tasks sync.WaitGroup
	defer func() {
		stopBackground()
		tasks.Wait()
	}()
	tasks.Add(1)
	go func() {
		defer tasks.Done()
		beat.tick(background, interval, func(err error) { logErr("worker heartbeat error: " + err.Error()) })
	}()

	schemaOptions := options.schema
	schemaOptions.Log = logOut
	if err := schemawait.Wait(ctx, cfg.SQLitePath, schemaOptions); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}

	writeDB, err := sqlitedb.Open(cfg.SQLitePath)
	if err != nil {
		return err
	}
	defer writeDB.Close()
	readDB, err := sqlitedb.Open(cfg.SQLitePath)
	if err != nil {
		return err
	}
	defer readDB.Close()
	// Registered after the DB closers so it runs first: background tasks that
	// read the database stop before the handles close.
	defer func() {
		stopBackground()
		tasks.Wait()
	}()

	jobs, revs := store.New(writeDB), revisions.New(writeDB)
	var secretReader materialize.SecretReader
	if cfg.MasterKeyBase64 != "" {
		secretReader = secrets.NewSQLiteStore(writeDB, cfg.MasterKeyBase64)
	}
	orchestrator := &apply.Orchestrator{
		Jobs: jobs, Revisions: revs,
		Control:       control.NewClient(cfg.SocketPath),
		CandidateRoot: cfg.CandidateRoot,
		Log:           logErr,
	}

	wake := wakeup.NewWakeup()
	tasks.Add(1)
	go func() {
		defer tasks.Done()
		_ = wakeup.Run(background, wakeup.Options{
			ReadCursor:   wakeup.SQLCursor(readDB),
			OnJob:        wake.Notify,
			OnError:      func(err error) { logOut("worker job wakeup error: " + err.Error()) },
			PollInterval: options.wakeupPoll,
		})
	}()

	logOut(fmt.Sprintf("%s listening for jobs; reconciling every %dms", workerIdentity, cfg.ReconciliationIntervalMs))
	return poller.Run(ctx, poller.Options{
		Jobs: jobs, Revisions: revs, Applier: orchestrator,
		Render: render.ForJob(render.Options{
			CandidateRoot: cfg.CandidateRoot,
			Capabilities:  cfg.Capabilities,
			Secrets:       secretReader,
			ACMEUpstream:  cfg.ACMEUpstream,
		}),
		LeaseMs:  cfg.JobLeaseMs,
		Interval: time.Duration(cfg.ReconciliationIntervalMs) * time.Millisecond,
		Wakeup:   wake,
		Heartbeat: func() {
			if err := beat.beat(); err != nil {
				logErr("worker heartbeat error: " + err.Error())
			}
		},
		OnError: func(err error) { logErr("worker poll error: " + err.Error()) },
	})
}
