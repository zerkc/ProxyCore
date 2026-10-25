package controldocker

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/worker/control"
)

const promotedConf = "events {}\nhttp {\n    map $http_upgrade $connection_upgrade { default upgrade; }\n}\n"

func TestNginxStableConfigPaths(t *testing.T) {
	if got := NginxLiveConfigPath("/var/lib/proxycore/candidates"); got != "/var/lib/proxycore/candidates/nginx-live.conf" {
		t.Fatal(got)
	}
	if got := NginxLiveConfigPath("/some/other/root"); got != "/some/other/root/nginx-live.conf" {
		t.Fatal(got)
	}
	if got := NginxPreviousStableConfigPath("/var/lib/proxycore/candidates"); got != "/var/lib/proxycore/candidates/nginx-previous-live.conf" {
		t.Fatal(got)
	}
	if NginxStableConfig != "nginx-live.conf" || NginxPreviousStableConfig != "nginx-previous-live.conf" {
		t.Fatal("stable file names changed")
	}
}

func TestWriteStableNginxConfigWritesAndKeepsPrevious(t *testing.T) {
	root := filepath.Join(t.TempDir(), "candidates")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	first, second := []byte("first config content\n"), []byte("second config content\n")
	if err := WriteStableNginxConfig(root, first, nil); err != nil {
		t.Fatal(err)
	}
	if got := read(t, NginxLiveConfigPath(root)); got != string(first) {
		t.Fatal(got)
	}
	if err := WriteStableNginxConfig(root, second, first); err != nil {
		t.Fatal(err)
	}
	if read(t, NginxLiveConfigPath(root)) != string(second) || read(t, NginxPreviousStableConfigPath(root)) != string(first) {
		t.Fatal("stable and previous contents are wrong")
	}
	if mode(t, NginxLiveConfigPath(root)) != 0o644 || mode(t, NginxPreviousStableConfigPath(root)) != 0o644 {
		t.Fatal("stable files must be 0644")
	}
}

func TestWriteStableNginxConfigRemovesPreviousWhenThereIsNone(t *testing.T) {
	root := t.TempDir()
	write(t, NginxPreviousStableConfigPath(root), "old", 0o644)
	for _, previous := range [][]byte{nil, {}} {
		write(t, NginxPreviousStableConfigPath(root), "old", 0o644)
		if err := WriteStableNginxConfig(root, []byte("x"), previous); err != nil {
			t.Fatal(err)
		}
		if exists(NginxPreviousStableConfigPath(root)) {
			t.Fatalf("previous %#v should have been removed", previous)
		}
	}
}

func TestWriteStableNginxConfigCreatesTheParentDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "does-not-exist")
	if err := WriteStableNginxConfig(root, []byte("content"), nil); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		t.Fatalf("parent not created: %v", err)
	}
}

func TestWriteStableNginxConfigLeavesNoTemporaryFiles(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 3; i++ {
		if err := WriteStableNginxConfig(root, []byte("c"), []byte("p")); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("leftover temporary file %s", e.Name())
		}
	}
}

func TestWriteStableNginxConfigFailureCleansTheTemporaryFile(t *testing.T) {
	root := t.TempDir()
	// A directory at the destination makes the rename fail.
	if err := os.MkdirAll(filepath.Join(root, NginxStableConfig, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteStableNginxConfig(root, []byte("c"), nil); err == nil {
		t.Fatal("expected rename failure")
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("leftover temporary file %s", e.Name())
		}
	}
}

func TestClassifyNginxActiveConfig(t *testing.T) {
	promoted := []byte(promotedConf)
	older := []byte("events {}\nhttp {\n    map $http_upgrade $connection_upgrade { default close; }\n}\n")
	baked, err := os.ReadFile("../../../../../infra/nginx/nginx.conf")
	if err != nil {
		t.Fatal(err)
	}
	drift := func(reason string) Observation { return Observation{Status: "drift", Reason: reason} }
	healthy := Observation{Status: "healthy"}

	tests := []struct {
		name string
		got  Observation
		want Observation
	}{
		{"matching promoted config (2-arg form)", ClassifyNginxActiveConfig(promoted, promoted), healthy},
		{"baked fallback", ClassifyNginxActiveConfig(baked, promoted), drift("baked-fallback")},
		{"baked fallback wins over missing stable", ClassifyNginxActiveConfig(baked, nil), drift("baked-fallback")},
		{"nil active", ClassifyNginxActiveConfig(nil, promoted), drift("missing-active-config")},
		{"empty active", ClassifyNginxActiveConfig([]byte{}, promoted), drift("missing-active-config")},
		{"nil stable", ClassifyNginxActiveConfig(promoted, nil), drift("missing-stable-config")},
		{"empty stable", ClassifyNginxActiveConfig(promoted, []byte{}), drift("missing-stable-config")},
		{"mismatched", ClassifyNginxActiveConfig([]byte("different"), promoted), drift("mismatched-config")},
		{"explicit nil candidate (3-arg form)", ClassifyNginxActiveConfig(promoted, promoted, nil), drift("missing-candidate-config")},
		{"explicit empty candidate", ClassifyNginxActiveConfig(promoted, promoted, []byte{}), drift("missing-candidate-config")},
		{"older candidate", ClassifyNginxActiveConfig(older, older, promoted), drift("mismatched-candidate-config")},
		{"matching candidate", ClassifyNginxActiveConfig(promoted, promoted, promoted), healthy},
		{"unmanaged config", ClassifyNginxActiveConfig([]byte("events {}\n"), []byte("events {}\n")), drift("unmanaged-config")},
		{"candidate checked before mismatch", ClassifyNginxActiveConfig([]byte("a"), []byte("b"), nil), drift("missing-candidate-config")},
		{"mismatched wins over candidate mismatch", ClassifyNginxActiveConfig([]byte("a"), []byte("b"), []byte("c")), drift("mismatched-config")},
		{"return 503 with managed marker is not baked", ClassifyNginxActiveConfig([]byte(promotedConf+"return 503;"), []byte(promotedConf+"return 503;")), healthy},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s: got %+v, want %+v", tt.name, tt.got, tt.want)
		}
	}
}

func TestFindNewestLegacyNginxCandidate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "candidates")
	old, newer := filepath.Join(root, "rev-1", "nginx", "nginx.conf"), filepath.Join(root, "rev-2", "nginx", "nginx.conf")
	write(t, old, "# rev-1\n", 0o644)
	write(t, newer, "# rev-2\n", 0o644)
	write(t, filepath.Join(root, "rev-3", "nginx.conf"), "# not legacy\n", 0o644)
	write(t, filepath.Join(root, "rev-2", "nginx", "other.conf"), "# ignore\n", 0o644)
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	got, err := FindNewestLegacyNginxCandidate(root)
	if err != nil || got != newer {
		t.Fatalf("found %q, %v", got, err)
	}
}

func TestFindNewestLegacyNginxCandidateReturnsEmptyWithoutMatches(t *testing.T) {
	got, err := FindNewestLegacyNginxCandidate(filepath.Join(t.TempDir(), "candidates"))
	if err != nil || got != "" {
		t.Fatalf("found %q, %v", got, err)
	}
}

// nginx operations --------------------------------------------------------

func nginxFixture(t *testing.T) (layout, *fakeDocker, *Backend, string) {
	t.Helper()
	l := newLayout(t)
	d := newFakeDocker()
	dir := l.candidate(t, "rev-1", map[string]string{"nginx.conf": promotedConf})
	return l, d, l.backend(d), dir
}

func TestPromoteNginxFirstApplyCopiesAndPersists(t *testing.T) {
	l, d, b, dir := nginxFixture(t)
	d.execExit["cp /etc/nginx/nginx.conf /etc/nginx/.proxycore.previous.conf"] = 1 // nothing to back up yet
	out := mustRun(t, b, request(control.ServiceNginx, control.OperationPromote, dir))
	if out != `{"status":"promoted","revisionId":"rev-1"}` {
		t.Fatal(out)
	}
	wantCalls(t, d,
		"get proxycore-nginx /etc/nginx/nginx.conf",
		"exec proxycore-nginx cp /etc/nginx/nginx.conf /etc/nginx/.proxycore.previous.conf",
		"exec proxycore-nginx cp "+dir+"/nginx.conf /etc/nginx/nginx.conf",
	)
	if read(t, NginxLiveConfigPath(l.candidates)) != promotedConf {
		t.Fatal("stable config not persisted")
	}
	if exists(NginxPreviousStableConfigPath(l.candidates)) {
		t.Fatal("no previous stable config expected on first apply")
	}
}

func TestPromoteNginxKeepsThePreviousContainerConfig(t *testing.T) {
	l, d, b, dir := nginxFixture(t)
	d.files["/etc/nginx/nginx.conf"] = []byte("old config\n")
	mustRun(t, b, request(control.ServiceNginx, control.OperationPromote, dir))
	if read(t, NginxPreviousStableConfigPath(l.candidates)) != "old config\n" {
		t.Fatal("previous config not stored")
	}
	if string(d.files["/etc/nginx/nginx.conf"]) != promotedConf {
		t.Fatal("container config not replaced")
	}
}

func TestPromoteNginxMissingCandidateFailsBeforeTouchingDocker(t *testing.T) {
	l, d, b, _ := nginxFixture(t)
	_, err := run(t, b, request(control.ServiceNginx, control.OperationPromote, l.candidates+"/missing"))
	if err == nil {
		t.Fatal("expected error")
	}
	wantCalls(t, d)
}

func TestPromoteNginxCopyFailureRestoresPreviousConfigFromArchive(t *testing.T) {
	l, d, b, dir := nginxFixture(t)
	d.files["/etc/nginx/nginx.conf"] = []byte("old config\n")
	d.execExit["cp "+dir+"/nginx.conf /etc/nginx/nginx.conf"] = 2
	_, err := run(t, b, request(control.ServiceNginx, control.OperationPromote, dir))
	wantErr(t, err, "Fixed service operation failed with exit 2")
	wantCalls(t, d,
		"get proxycore-nginx /etc/nginx/nginx.conf",
		"exec proxycore-nginx cp /etc/nginx/nginx.conf /etc/nginx/.proxycore.previous.conf",
		"exec proxycore-nginx cp "+dir+"/nginx.conf /etc/nginx/nginx.conf",
		"put proxycore-nginx /etc/nginx nginx.conf=old config\n",
	)
	if exists(NginxLiveConfigPath(l.candidates)) {
		t.Fatal("stable config must not be written when the copy failed")
	}
}

func TestPromoteNginxCopyFailureWithoutArchiveUsesTheBackupCopy(t *testing.T) {
	_, d, b, dir := nginxFixture(t)
	d.getErr["/etc/nginx/nginx.conf"] = errors.New("boom")
	d.execExit["cp "+dir+"/nginx.conf /etc/nginx/nginx.conf"] = 1
	_, err := run(t, b, request(control.ServiceNginx, control.OperationPromote, dir))
	wantErr(t, err, "Fixed service operation failed with exit 1")
	calls := d.Calls()
	if last := calls[len(calls)-1]; last != "exec proxycore-nginx cp /etc/nginx/.proxycore.previous.conf /etc/nginx/nginx.conf" {
		t.Fatalf("last call = %q", last)
	}
}

func TestPromoteNginxStableWriteFailureRestoresAndReportsTheOriginalError(t *testing.T) {
	l, d, b, dir := nginxFixture(t)
	d.files["/etc/nginx/nginx.conf"] = []byte("old config\n")
	if err := os.MkdirAll(filepath.Join(NginxLiveConfigPath(l.candidates), "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := run(t, b, request(control.ServiceNginx, control.OperationPromote, dir))
	if err == nil {
		t.Fatal("expected stable write failure")
	}
	calls := d.Calls()
	if last := calls[len(calls)-1]; last != "put proxycore-nginx /etc/nginx nginx.conf=old config\n" {
		t.Fatalf("last call = %q", last)
	}
}

func TestPromoteNginxRestoreFailureKeepsTheOriginalError(t *testing.T) {
	_, d, b, dir := nginxFixture(t)
	d.files["/etc/nginx/nginx.conf"] = []byte("old config\n")
	d.execExit["cp "+dir+"/nginx.conf /etc/nginx/nginx.conf"] = 9
	d.putErr = errors.New("restore failed")
	_, err := run(t, b, request(control.ServiceNginx, control.OperationPromote, dir))
	wantErr(t, err, "Fixed service operation failed with exit 9")
}

func TestPromoteNginxEmptyPreviousArchiveIsStillRestoredByArchive(t *testing.T) {
	// JS treats an empty Buffer as truthy: an empty previous config is restored
	// through putArchive, not through the in-container backup copy.
	_, d, b, dir := nginxFixture(t)
	d.files["/etc/nginx/nginx.conf"] = []byte{}
	d.execExit["cp "+dir+"/nginx.conf /etc/nginx/nginx.conf"] = 1
	_, _ = run(t, b, request(control.ServiceNginx, control.OperationPromote, dir))
	calls := d.Calls()
	if last := calls[len(calls)-1]; last != "put proxycore-nginx /etc/nginx nginx.conf=" {
		t.Fatalf("last call = %q", last)
	}
}

func TestRollbackNginxWithPreviousStableConfig(t *testing.T) {
	l, d, b, dir := nginxFixture(t)
	write(t, NginxLiveConfigPath(l.candidates), "current\n", 0o644)
	write(t, NginxPreviousStableConfigPath(l.candidates), "previous\n", 0o644)
	out := mustRun(t, b, request(control.ServiceNginx, control.OperationRollback, dir))
	if out != `{"status":"rolled-back"}` {
		t.Fatal(out)
	}
	wantCalls(t, d, "put proxycore-nginx /etc/nginx nginx.conf=previous\n")
	if read(t, NginxLiveConfigPath(l.candidates)) != "previous\n" {
		t.Fatal("stable config not rolled back")
	}
}

func TestRollbackNginxPutFailureRestoresTheCurrentStableConfig(t *testing.T) {
	l, d, b, dir := nginxFixture(t)
	write(t, NginxLiveConfigPath(l.candidates), "current\n", 0o644)
	write(t, NginxPreviousStableConfigPath(l.candidates), "previous\n", 0o644)
	d.putErr = errors.New("daemon down")
	_, err := run(t, b, request(control.ServiceNginx, control.OperationRollback, dir))
	wantErr(t, err, "daemon down")
	if read(t, NginxLiveConfigPath(l.candidates)) != "current\n" {
		t.Fatal("stable config must be restored after a failed rollback")
	}
}

func TestRollbackNginxPutFailureWithoutCurrentStableLeavesPreviousWritten(t *testing.T) {
	l, d, b, dir := nginxFixture(t)
	write(t, NginxPreviousStableConfigPath(l.candidates), "previous\n", 0o644)
	d.putErr = errors.New("daemon down")
	_, err := run(t, b, request(control.ServiceNginx, control.OperationRollback, dir))
	wantErr(t, err, "daemon down")
	if read(t, NginxLiveConfigPath(l.candidates)) != "previous\n" {
		t.Fatal("TS keeps the written previous config when there was no current stable config")
	}
}

func TestRollbackNginxWithoutPreviousUsesTheContainerBackup(t *testing.T) {
	l, d, b, dir := nginxFixture(t)
	write(t, NginxLiveConfigPath(l.candidates), "current\n", 0o644)
	mustRun(t, b, request(control.ServiceNginx, control.OperationRollback, dir))
	wantCalls(t, d, "exec proxycore-nginx cp /etc/nginx/.proxycore.previous.conf /etc/nginx/nginx.conf")
	if exists(NginxLiveConfigPath(l.candidates)) {
		t.Fatal("stable config must be removed")
	}
	// Idempotent: the stable file is already gone and rm is forced.
	mustRun(t, b, request(control.ServiceNginx, control.OperationRollback, dir))
}

func TestRollbackNginxBackupFailureKeepsTheStableConfig(t *testing.T) {
	l, d, b, dir := nginxFixture(t)
	write(t, NginxLiveConfigPath(l.candidates), "current\n", 0o644)
	d.execExit["cp /etc/nginx/.proxycore.previous.conf /etc/nginx/nginx.conf"] = 1
	_, err := run(t, b, request(control.ServiceNginx, control.OperationRollback, dir))
	wantErr(t, err, "Fixed service operation failed with exit 1")
	if read(t, NginxLiveConfigPath(l.candidates)) != "current\n" {
		t.Fatal("stable config must stay when the backup copy failed")
	}
}

func TestHealthNginx(t *testing.T) {
	tests := []struct {
		name     string
		active   *string // nil: archive read fails
		stable   *string
		cand     *string
		nginxT   int
		want     string
		wantExec bool
	}{
		{"healthy", ptr(promotedConf), ptr(promotedConf), ptr(promotedConf), 0, `{"status":"healthy","revisionId":"rev-1"}`, true},
		{"invalid active config", ptr(promotedConf), ptr(promotedConf), ptr(promotedConf), 1, `{"status":"drift","reason":"invalid-active-config","revisionId":"rev-1"}`, true},
		{"archive read failure", nil, ptr(promotedConf), ptr(promotedConf), 0, `{"status":"drift","reason":"missing-active-config","revisionId":"rev-1"}`, false},
		{"missing stable", ptr(promotedConf), nil, ptr(promotedConf), 0, `{"status":"drift","reason":"missing-stable-config","revisionId":"rev-1"}`, false},
		{"missing candidate", ptr(promotedConf), ptr(promotedConf), nil, 0, `{"status":"drift","reason":"missing-candidate-config","revisionId":"rev-1"}`, false},
		{"older candidate", ptr(promotedConf), ptr(promotedConf), ptr("other " + promotedConf), 0, `{"status":"drift","reason":"mismatched-candidate-config","revisionId":"rev-1"}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, d, b, _ := nginxFixture(t)
			dir := l.candidate(t, "rev-h", nil)
			if tt.active != nil {
				d.files["/etc/nginx/nginx.conf"] = []byte(*tt.active)
			}
			if tt.stable != nil {
				write(t, NginxLiveConfigPath(l.candidates), *tt.stable, 0o644)
			}
			if tt.cand != nil {
				write(t, dir+"/nginx.conf", *tt.cand, 0o644)
			}
			d.execExit["nginx -t -c /etc/nginx/nginx.conf"] = tt.nginxT
			got := mustRun(t, b, request(control.ServiceNginx, control.OperationHealth, dir))
			if got != tt.want {
				t.Fatalf("output = %s, want %s", got, tt.want)
			}
			calls := d.Calls()
			if calls[0] != "inspect proxycore-nginx" || calls[1] != "get proxycore-nginx /etc/nginx/nginx.conf" {
				t.Fatalf("calls = %v", calls)
			}
			ran := len(calls) == 3 && calls[2] == "exec proxycore-nginx nginx -t -c /etc/nginx/nginx.conf"
			if ran != tt.wantExec || (!tt.wantExec && len(calls) != 2) {
				t.Fatalf("calls = %v (nginx -t expected: %v)", calls, tt.wantExec)
			}
		})
	}
}

func TestHealthNginxExecTransportFailureIsInvalidActiveConfig(t *testing.T) {
	_, d, b, dir := nginxFixture(t)
	write(t, NginxLiveConfigPath(filepath.Dir(dir)), promotedConf, 0o644)
	d.files["/etc/nginx/nginx.conf"] = []byte(promotedConf)
	d.execErr["nginx -t -c /etc/nginx/nginx.conf"] = errors.New("exec failed")
	got := mustRun(t, b, request(control.ServiceNginx, control.OperationHealth, dir))
	if got != `{"status":"drift","reason":"invalid-active-config","revisionId":"rev-1"}` {
		t.Fatal(got)
	}
}

func TestHealthNginxUnreadableStableConfigFails(t *testing.T) {
	l, d, b, dir := nginxFixture(t)
	d.files["/etc/nginx/nginx.conf"] = []byte(promotedConf)
	if err := os.MkdirAll(NginxLiveConfigPath(l.candidates), 0o755); err != nil { // a directory is not ENOENT
		t.Fatal(err)
	}
	if _, err := run(t, b, request(control.ServiceNginx, control.OperationHealth, dir)); err == nil {
		t.Fatal("non-ENOENT read errors must propagate")
	}
}

func ptr(s string) *string { return &s }
