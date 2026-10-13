package configuration

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sync"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
	"modernc.org/sqlite"
)

func streamRoute(port int, protocol string) domain.StreamRoute {
	return domain.StreamRoute{Enabled: true, Protocol: protocol, ListenAddress: "0.0.0.0", ListenPort: port,
		Upstream: domain.UpstreamTarget{IP: "192.0.2.1", Port: 53, Protocol: protocol}}
}

func TestSQLiteStreamsCycle(t *testing.T) {
	db, _, path := sqliteSettingsFixture(t)
	ctx := context.Background()
	seconds := int64(123)
	writer := sqlitedb.NewWriter(db, func() sqlitedb.FiniteTimestamp { return sqlitedb.FiniteTimestamp{Seconds: seconds} })
	store := NewSQLiteStreamStore(db, writer)
	routes, err := store.ListStreams(ctx)
	if err != nil || routes == nil || len(routes) != 0 {
		t.Fatal(routes, err)
	}
	first, err := store.AddStream(ctx, streamRoute(8053, "tcp"))
	if err != nil || !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(first.ID) {
		t.Fatal(first, err)
	}
	var created, updated, kind, raw string
	if err := db.QueryRow("select created_at,updated_at,typeof(upstream),upstream from stream_routes").Scan(&created, &updated, &kind, &raw); err != nil {
		t.Fatal(err)
	}
	stamp, _ := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: seconds})
	if created != *stamp || updated != created || kind != "text" || raw != `{"ip":"192.0.2.1","port":53,"protocol":"tcp"}` {
		t.Fatal(created, updated, kind, raw)
	}
	seconds++
	first.Enabled = false
	first.Upstream.IP = "2001:db8::1"
	saved, err := store.AddStream(ctx, first)
	if err != nil || !reflect.DeepEqual(saved, first) {
		t.Fatal(saved, err)
	}
	var original string
	if err := db.QueryRow("select created_at,updated_at from stream_routes").Scan(&original, &updated); err != nil {
		t.Fatal(err)
	}
	stamp, _ = sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: seconds})
	if original != created || updated != *stamp {
		t.Fatal(original, updated)
	}
	second := streamRoute(53, "udp")
	second.ID = "00000000-0000-4000-8000-000000000001"
	if got, err := store.AddStream(ctx, second); err != nil || got != second {
		t.Fatal(got, err)
	}
	routes, err = store.ListStreams(ctx)
	if err != nil || !reflect.DeepEqual(routes, []domain.StreamRoute{second, first}) {
		t.Fatal(routes, err)
	}
	for _, table := range []string{"installation_settings", "zones", "dns_records", "resolver_pools", "forwarding_rules", "users", "sessions", "audit_events"} {
		var count int
		if err := db.QueryRow("select count(*) from " + table).Scan(&count); err != nil || count != 0 {
			t.Fatal(table, count, err)
		}
	}
	var jobs int
	if err := db.QueryRow("select count(*) from apply_jobs").Scan(&jobs); err != nil || jobs != 0 {
		t.Fatal("stream writes must not enqueue apply jobs", jobs, err)
	}
	if err := db.QueryRow("select count(*) from sqlite_schema where type='table' and name in ('revisions','notifications')").Scan(&jobs); err != nil || jobs != 0 {
		t.Fatal(jobs, err)
	}
	if err := store.DeleteStream(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteStream(ctx, first.ID); err == nil || err.Error() != "Stream not found: "+first.ID {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	routes, err = NewSQLiteStreamStore(reopened, settingsWriter(reopened)).ListStreams(ctx)
	if err != nil || !reflect.DeepEqual(routes, []domain.StreamRoute{second}) {
		t.Fatal(routes, err)
	}
}

func TestSQLiteStreamsRejectAndRollback(t *testing.T) {
	db, _, _ := sqliteSettingsFixture(t)
	ctx := context.Background()
	store := NewSQLiteStreamStore(db, settingsWriter(db))
	first, err := store.AddStream(ctx, streamRoute(53, "tcp"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(*domain.StreamRoute)
	}{
		{"port zero", func(r *domain.StreamRoute) { r.ListenPort = 0 }},
		{"port high", func(r *domain.StreamRoute) { r.ListenPort = 65536 }},
		{"listen IP", func(r *domain.StreamRoute) { r.ListenAddress = "hostname" }},
		{"protocol mismatch", func(r *domain.StreamRoute) { r.Upstream.Protocol = "udp" }},
		{"upstream IP", func(r *domain.StreamRoute) { r.Upstream.IP = "hostname" }},
		{"upstream port", func(r *domain.StreamRoute) { r.Upstream.Port = 0 }},
		{"conflict", func(r *domain.StreamRoute) { r.ID = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := first
			tc.edit(&bad)
			_, expected := domain.ValidateStreamRoutes([]domain.StreamRoute{bad})
			if tc.name == "conflict" {
				_, expected = domain.ValidateStreamRoutes([]domain.StreamRoute{first, bad})
			}
			if _, err := store.AddStream(ctx, bad); expected == nil || err == nil || err.Error() != expected.Error() {
				t.Fatal(err, expected)
			}
			got, err := store.ListStreams(ctx)
			if err != nil || !reflect.DeepEqual(got, []domain.StreamRoute{first}) {
				t.Fatal(got, err)
			}
		})
	}
	for _, bad := range []domain.StreamRoute{streamRoute(54, "sctp"), func() domain.StreamRoute {
		r := streamRoute(54, "udp")
		r.ID = "invalid"
		return r
	}()} {
		_, err := store.AddStream(ctx, bad)
		var constraint *sqlite.Error
		if !errors.As(err, &constraint) || constraint.Code() != 275 {
			t.Fatal("expected CHECK constraint", err)
		}
	}
	settingsExec(t, db, `create trigger reject_stream before update on stream_routes begin select raise(abort,'stream write rejected'); end`)
	first.Enabled = false
	if _, err := store.AddStream(ctx, first); err == nil {
		t.Fatal("trigger accepted update")
	}
	got, err := store.ListStreams(ctx)
	first.Enabled = true
	if err != nil || !reflect.DeepEqual(got, []domain.StreamRoute{first}) {
		t.Fatal(got, err)
	}
	settingsExec(t, db, "insert into stream_routes values (?,?,?,?,?,?,?,?)", "00000000-0000-4000-8000-000000000002", 1, "udp", "0.0.0.0", 70000, `{"ip":"192.0.2.1","port":53,"protocol":"udp"}`, "0", "0")
	if _, err := store.ListStreams(ctx); err == nil || err.Error() != "Port is invalid" {
		t.Fatal(err)
	}
}

func TestSQLiteStreamsConcurrent(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprint(conflict), func(t *testing.T) {
			db, _, _ := sqliteSettingsFixture(t)
			store := NewSQLiteStreamStore(db, settingsWriter(db))
			start := make(chan struct{})
			results := make(chan error, 2)
			var wg sync.WaitGroup
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					port := 53
					if !conflict {
						port += i
					}
					_, err := store.AddStream(context.Background(), streamRoute(port, "tcp"))
					results <- err
				}(i)
			}
			close(start)
			wg.Wait()
			close(results)
			failures := 0
			for err := range results {
				if err != nil {
					failures++
					if err.Error() != "Stream listener conflict: tcp:0.0.0.0:53" {
						t.Fatal(err)
					}
				}
			}
			want := 2
			if conflict {
				want = 1
			}
			got, err := store.ListStreams(context.Background())
			if err != nil || len(got) != want || failures != 2-want {
				t.Fatal(got, failures, err)
			}
		})
	}
}

func TestSQLiteStreamsStrictBoolean(t *testing.T) {
	db, _, _ := sqliteSettingsFixture(t)
	rows, err := db.Query("select ?,2,'tcp','0.0.0.0',53,?", "id", `{"ip":"192.0.2.1","port":53,"protocol":"tcp"}`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal(rows.Err())
	}
	if _, err := scanStream(sqliteStreamScanner{rows}); err == nil {
		t.Fatal("invalid boolean accepted")
	}
}
