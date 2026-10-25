package controldocker

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/worker/control"
)

const appliedCorefile = "home.arpa:53 {\n    file /etc/coredns/zones/home.arpa.zone\n}\n"
const forwardOnlyCorefile = ".:53 {\n    forward . 1.1.1.1:53\n}\n"

func TestSeedLiveCorefileFromLatestAppliedCandidate(t *testing.T) {
	l := newLayout(t)
	write(t, filepath.Join(l.candidates, "rev-9", "coredns", "Corefile"), appliedCorefile, 0o644)
	write(t, CorednsLiveCorefilePath(l.config), forwardOnlyCorefile, 0o644)
	seeded, err := SeedLiveCorefileFromCandidates(l.candidates, l.config)
	if err != nil || seeded != filepath.Join(l.candidates, "rev-9", "coredns", "Corefile") {
		t.Fatalf("seeded %q, %v", seeded, err)
	}
	if read(t, CorednsLiveCorefilePath(l.config)) != appliedCorefile {
		t.Fatal("live Corefile not seeded")
	}
}

func TestSeedCreatesTheConfigRootAndWritesMode0644(t *testing.T) {
	l := newLayout(t)
	write(t, filepath.Join(l.candidates, "rev-1", "coredns", "Corefile"), appliedCorefile, 0o600)
	seeded, err := SeedLiveCorefileFromCandidates(l.candidates, l.config)
	if err != nil || seeded == "" {
		t.Fatalf("seeded %q, %v", seeded, err)
	}
	if mode(t, CorednsLiveCorefilePath(l.config)) != 0o644 {
		t.Fatal("live Corefile must be 0644")
	}
}

func TestSeedDoesNotOverwriteAnAppliedLiveCorefile(t *testing.T) {
	l := newLayout(t)
	write(t, filepath.Join(l.candidates, "rev-1", "coredns", "Corefile"), "other.com:53 {\n    file /etc/coredns/zones/other.com.zone\n}\n", 0o644)
	keep := "keep.com:53 {\n    file /etc/coredns/zones/keep.com.zone\n}\n"
	write(t, CorednsLiveCorefilePath(l.config), keep, 0o644)
	seeded, err := SeedLiveCorefileFromCandidates(l.candidates, l.config)
	if err != nil || seeded != "" || read(t, CorednsLiveCorefilePath(l.config)) != keep {
		t.Fatalf("seeded %q, %v", seeded, err)
	}
}

func TestSeedSkipsCandidatesThatAreNotApplied(t *testing.T) {
	l := newLayout(t)
	write(t, filepath.Join(l.candidates, "rev-1", "coredns", "Corefile"), forwardOnlyCorefile, 0o644)
	write(t, filepath.Join(l.candidates, "rev-2", "Corefile"), appliedCorefile, 0o644) // wrong layout
	seeded, err := SeedLiveCorefileFromCandidates(l.candidates, l.config)
	if err != nil || seeded != "" || exists(CorednsLiveCorefilePath(l.config)) {
		t.Fatalf("seeded %q, %v", seeded, err)
	}
}

func TestSeedWithoutCandidateRootDoesNothing(t *testing.T) {
	l := newLayout(t)
	seeded, err := SeedLiveCorefileFromCandidates(filepath.Join(l.root, "absent"), l.config)
	if err != nil || seeded != "" {
		t.Fatalf("seeded %q, %v", seeded, err)
	}
}

func coreFixture(t *testing.T) (layout, *fakeDocker, *Backend, string) {
	t.Helper()
	l := newLayout(t)
	d := newFakeDocker()
	dir := l.candidate(t, "rev-1", map[string]string{
		"Corefile":         appliedCorefile,
		"zones/a.zone":     "a zone\n",
		"zones/sub/b.zone": "b zone\n",
	})
	write(t, filepath.Join(l.zones, "old.zone"), "old zone\n", 0o600)
	return l, d, l.backend(d), dir
}

func previousCorefilePath(l layout) string {
	return filepath.Join(l.candidates, ".proxycore-previous-coredns-corefile")
}
func previousZonesPath(l layout) string {
	return filepath.Join(l.candidates, ".proxycore-previous-coredns-zones")
}

func TestPromoteCoreDNSReplacesZonesAndCorefile(t *testing.T) {
	l, d, b, dir := coreFixture(t)
	write(t, CorednsLiveCorefilePath(l.config), forwardOnlyCorefile, 0o600)
	out := mustRun(t, b, request(control.ServiceCoreDNS, control.OperationPromote, dir))
	if out != `{"status":"promoted","revisionId":"rev-1"}` {
		t.Fatal(out)
	}
	wantCalls(t, d, "put proxycore-coredns /etc/coredns-config Corefile="+appliedCorefile)
	if read(t, CorednsLiveCorefilePath(l.config)) != appliedCorefile {
		t.Fatal("live Corefile not replaced")
	}
	if read(t, previousCorefilePath(l)) != forwardOnlyCorefile || mode(t, previousCorefilePath(l)) != 0o600 {
		t.Fatal("previous Corefile wrong")
	}
	if read(t, filepath.Join(l.zones, "a.zone")) != "a zone\n" || read(t, filepath.Join(l.zones, "sub", "b.zone")) != "b zone\n" {
		t.Fatal("zones not replaced")
	}
	if exists(filepath.Join(l.zones, "old.zone")) {
		t.Fatal("old zone must be removed")
	}
	if read(t, filepath.Join(previousZonesPath(l), "old.zone")) != "old zone\n" || mode(t, filepath.Join(previousZonesPath(l), "old.zone")) != 0o644 {
		t.Fatal("previous zones not saved with mode 0644")
	}
	if mode(t, filepath.Join(l.zones, "a.zone")) != 0o644 {
		t.Fatal("copied zones must be 0644")
	}
}

func TestPromoteCoreDNSReadsThePreviousCorefileFromTheContainer(t *testing.T) {
	l, d, b, dir := coreFixture(t)
	d.files["/etc/coredns-config/Corefile"] = []byte("from config volume\n")
	mustRun(t, b, request(control.ServiceCoreDNS, control.OperationPromote, dir))
	if read(t, previousCorefilePath(l)) != "from config volume\n" {
		t.Fatal(read(t, previousCorefilePath(l)))
	}
	calls := d.Calls()
	if calls[0] != "get proxycore-coredns /etc/coredns-config/Corefile" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestPromoteCoreDNSFallsBackToTheImageCorefile(t *testing.T) {
	l, d, b, dir := coreFixture(t)
	d.files["/etc/coredns/Corefile"] = []byte("image default\n")
	mustRun(t, b, request(control.ServiceCoreDNS, control.OperationPromote, dir))
	if read(t, previousCorefilePath(l)) != "image default\n" {
		t.Fatal("fallback not used")
	}
	wantCalls(t, d,
		"get proxycore-coredns /etc/coredns-config/Corefile",
		"get proxycore-coredns /etc/coredns/Corefile",
		"put proxycore-coredns /etc/coredns-config Corefile="+appliedCorefile,
	)
}

func TestPromoteCoreDNSFailsWhenNoPreviousCorefileExists(t *testing.T) {
	l, d, b, dir := coreFixture(t)
	_, err := run(t, b, request(control.ServiceCoreDNS, control.OperationPromote, dir))
	wantErr(t, err, "no such file")
	if exists(previousCorefilePath(l)) || exists(filepath.Join(l.zones, "a.zone")) {
		t.Fatal("nothing may change when the previous Corefile is unavailable")
	}
	_ = d
}

func TestPromoteCoreDNSPutFailureRestoresEverything(t *testing.T) {
	l, d, b, dir := coreFixture(t)
	write(t, CorednsLiveCorefilePath(l.config), forwardOnlyCorefile, 0o644)
	d.putErr = errors.New("daemon down")
	_, err := run(t, b, request(control.ServiceCoreDNS, control.OperationPromote, dir))
	wantErr(t, err, "daemon down")
	if read(t, CorednsLiveCorefilePath(l.config)) != forwardOnlyCorefile {
		t.Fatal("live Corefile not restored")
	}
	if read(t, filepath.Join(l.zones, "old.zone")) != "old zone\n" || exists(filepath.Join(l.zones, "a.zone")) {
		t.Fatal("zones not restored")
	}
}

func TestPromoteCoreDNSMissingCandidateCorefileRestores(t *testing.T) {
	l, _, b, dir := coreFixture(t)
	write(t, CorednsLiveCorefilePath(l.config), forwardOnlyCorefile, 0o644)
	if err := os.Remove(filepath.Join(dir, "Corefile")); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, b, request(control.ServiceCoreDNS, control.OperationPromote, dir)); err == nil {
		t.Fatal("expected error")
	}
	if read(t, filepath.Join(l.zones, "old.zone")) != "old zone\n" || read(t, CorednsLiveCorefilePath(l.config)) != forwardOnlyCorefile {
		t.Fatal("state not restored")
	}
}

func TestPromoteCoreDNSRejectsUnsupportedZoneEntries(t *testing.T) {
	l, _, b, dir := coreFixture(t)
	write(t, CorednsLiveCorefilePath(l.config), forwardOnlyCorefile, 0o644)
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "zones", "evil.zone")); err != nil {
		t.Fatal(err)
	}
	_, err := run(t, b, request(control.ServiceCoreDNS, control.OperationPromote, dir))
	wantErr(t, err, "Unsupported file type in CoreDNS zone directory: evil.zone")
	// The failure happened inside the try block, so the old zones are restored.
	if read(t, filepath.Join(l.zones, "old.zone")) != "old zone\n" {
		t.Fatal("zones not restored")
	}
}

func TestPromoteCoreDNSRejectsUnsupportedLiveZoneEntriesBeforeChanging(t *testing.T) {
	l, _, b, dir := coreFixture(t)
	write(t, CorednsLiveCorefilePath(l.config), forwardOnlyCorefile, 0o644)
	if err := os.Symlink("/etc/passwd", filepath.Join(l.zones, "link")); err != nil {
		t.Fatal(err)
	}
	_, err := run(t, b, request(control.ServiceCoreDNS, control.OperationPromote, dir))
	wantErr(t, err, "Unsupported file type in CoreDNS zone directory: link")
	if read(t, CorednsLiveCorefilePath(l.config)) != forwardOnlyCorefile || exists(filepath.Join(l.zones, "a.zone")) {
		t.Fatal("nothing may change")
	}
}

func TestPromoteCoreDNSCreatesMissingCandidateZonesDirectory(t *testing.T) {
	// copyDirectory mkdirs its source, so a candidate without zones/ is created
	// empty and the live zones become empty.
	l := newLayout(t)
	d := newFakeDocker()
	dir := l.candidate(t, "rev-2", map[string]string{"Corefile": appliedCorefile})
	write(t, CorednsLiveCorefilePath(l.config), forwardOnlyCorefile, 0o644)
	write(t, filepath.Join(l.zones, "old.zone"), "x", 0o644)
	mustRun(t, l.backend(d), request(control.ServiceCoreDNS, control.OperationPromote, dir))
	if !exists(filepath.Join(dir, "zones")) || exists(filepath.Join(l.zones, "old.zone")) {
		t.Fatal("zones not emptied")
	}
}

func TestRollbackCoreDNSRestoresPreviousState(t *testing.T) {
	l, d, b, dir := coreFixture(t)
	write(t, CorednsLiveCorefilePath(l.config), forwardOnlyCorefile, 0o644)
	mustRun(t, b, request(control.ServiceCoreDNS, control.OperationPromote, dir))
	d.mu.Lock()
	d.calls = nil
	d.mu.Unlock()
	out := mustRun(t, b, request(control.ServiceCoreDNS, control.OperationRollback, dir))
	if out != `{"status":"rolled-back"}` {
		t.Fatal(out)
	}
	wantCalls(t, d, "put proxycore-coredns /etc/coredns-config Corefile="+forwardOnlyCorefile)
	if read(t, CorednsLiveCorefilePath(l.config)) != forwardOnlyCorefile || mode(t, CorednsLiveCorefilePath(l.config)) != 0o644 {
		t.Fatal("live Corefile not rolled back")
	}
	if read(t, filepath.Join(l.zones, "old.zone")) != "old zone\n" || exists(filepath.Join(l.zones, "a.zone")) {
		t.Fatal("zones not rolled back")
	}
	// Idempotent: a second rollback restores the same snapshot.
	mustRun(t, b, request(control.ServiceCoreDNS, control.OperationRollback, dir))
	if read(t, CorednsLiveCorefilePath(l.config)) != forwardOnlyCorefile {
		t.Fatal("second rollback changed state")
	}
}

func TestRollbackCoreDNSWithoutPreviousCorefileFails(t *testing.T) {
	_, d, b, dir := coreFixture(t)
	if _, err := run(t, b, request(control.ServiceCoreDNS, control.OperationRollback, dir)); err == nil {
		t.Fatal("expected error")
	}
	wantCalls(t, d)
}

func TestRollbackCoreDNSWithoutPreviousZonesCreatesEmptyZones(t *testing.T) {
	l, d, b, dir := coreFixture(t)
	write(t, previousCorefilePath(l), forwardOnlyCorefile, 0o600)
	mustRun(t, b, request(control.ServiceCoreDNS, control.OperationRollback, dir))
	if exists(filepath.Join(l.zones, "old.zone")) || !exists(previousZonesPath(l)) {
		t.Fatal("zones should be replaced by the (created) empty snapshot")
	}
	_ = d
}

func TestRollbackCoreDNSPutFailureDoesNotTouchZones(t *testing.T) {
	l, d, b, dir := coreFixture(t)
	write(t, previousCorefilePath(l), forwardOnlyCorefile, 0o600)
	d.putErr = errors.New("daemon down")
	_, err := run(t, b, request(control.ServiceCoreDNS, control.OperationRollback, dir))
	wantErr(t, err, "daemon down")
	if read(t, filepath.Join(l.zones, "old.zone")) != "old zone\n" {
		t.Fatal("zones must not change when the put failed")
	}
}
