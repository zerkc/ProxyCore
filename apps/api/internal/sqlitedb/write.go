package sqlitedb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"time"
)

// Writer owns an instance clock, never a global or SQL function. Callbacks must
// use the supplied context/connection, not manage transactions or retain them.
// Detached work and nesting with an unrelated context are unsupported.
type Writer struct {
	db    *sql.DB
	clock func() FiniteTimestamp
}

type writeContextKey struct{}

// WriteValue is an invocation snapshot; accessors cannot change its timestamp.
// Historical timestamps remain explicit SQL parameters, independent of this value.
type WriteValue struct {
	conn  *sql.Conn
	ctx   context.Context
	stamp string
}

func (v WriteValue) Conn() *sql.Conn          { return v.conn }
func (v WriteValue) Context() context.Context { return v.ctx }
func (v WriteValue) Timestamp() string        { return v.stamp }

// NewUUID creates a fresh entity identifier, not a transaction identifier.
func (v WriteValue) NewUUID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", bytes[:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:]), nil
}

func NewWriter(db *sql.DB, clock func() FiniteTimestamp) *Writer {
	if clock == nil {
		clock = func() FiniteTimestamp {
			now := time.Now()
			return FiniteTimestamp{Seconds: now.Unix(), Nanos: int32(now.Nanosecond())}
		}
	}
	return &Writer{db: db, clock: clock}
}

// Write reserves a connection and captures one finite clock after BEGIN IMMEDIATE.
// Rollback is bounded independently of caller cancellation; panic is rethrown.
func (w *Writer) Write(ctx context.Context, callback func(WriteValue) error) (err error) {
	if ctx.Value(writeContextKey{}) != nil {
		return fmt.Errorf("nested write transaction")
	}
	conn, err := w.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, rollbackErr := conn.ExecContext(cleanup, "ROLLBACK"); rollbackErr != nil {
			discardErr := conn.Raw(func(any) error { return driver.ErrBadConn })
			err = errors.Join(err, fmt.Errorf("rollback cleanup: %w (discard=%v)", rollbackErr, discardErr))
		}
	}()
	stamp, err := EncodeTimestamp(w.clock())
	if err != nil {
		return err
	}
	value := WriteValue{conn: conn, ctx: context.WithValue(ctx, writeContextKey{}, true), stamp: *stamp}
	if err = callback(value); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	committed = err == nil
	return err
}
