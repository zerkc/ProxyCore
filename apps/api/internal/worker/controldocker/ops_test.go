package controldocker

import (
	"errors"
	"os"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/worker/control"
)

func TestAssertCandidatePath(t *testing.T) {
	const root = "/var/lib/proxycore/candidates"
	accepted := []string{root + "/revision-1", root + "/rev-1/nginx/nginx.conf"}
	rejected := []string{"/etc/passwd", root, root + "/", root + "/../secrets", root + "/a..b", root + "/x\x00y",
		"/var/lib/proxycore/candidates-evil/x", "/etc/shadow", root + "/../../../etc/passwd"}
	for _, p := range accepted {
		if err := AssertCandidatePath(p, root); err != nil {
			t.Errorf("%q rejected: %v", p, err)
		}
	}
	for _, p := range rejected {
		err := AssertCandidatePath(p, root)
		if p == root+"/" {
			// startsWith(root + "/") accepts the bare trailing slash.
			if err != nil {
				t.Errorf("%q should be accepted like TS", p)
			}
			continue
		}
		if err == nil || err.Error() != "Candidate path is outside the fixed worker root" {
			t.Errorf("%q: err = %v", p, err)
		}
	}
}

func TestEveryOperationRejectsPathsOutsideTheRootBeforeTouchingAnything(t *testing.T) {
	l := newLayout(t)
	d := newFakeDocker()
	b := l.backend(d)
	for _, service := range []control.Service{control.ServiceCoreDNS, control.ServiceNginx} {
		for _, op := range control.Operations {
			_, err := run(t, b, request(service, op, "/etc/passwd"))
			wantErr(t, err, "Candidate path is outside the fixed worker root")
		}
	}
	wantCalls(t, d)
}

func TestHandlersCoverEveryServiceAndOperation(t *testing.T) {
	handlers := newLayout(t).backend(newFakeDocker()).Handlers()
	for _, service := range []control.Service{control.ServiceCoreDNS, control.ServiceNginx} {
		for _, op := range control.Operations {
			if handlers[service][op] == nil {
				t.Errorf("missing handler %s %s", service, op)
			}
		}
	}
}

func TestStage(t *testing.T) {
	l := newLayout(t)
	d := newFakeDocker()
	dir := l.candidate(t, "rev-1", nil)
	for _, service := range []control.Service{control.ServiceCoreDNS, control.ServiceNginx} {
		got := mustRun(t, l.backend(d), request(service, control.OperationStage, dir))
		if got != `{"status":"staged","candidatePath":"`+dir+`"}` {
			t.Fatal(got)
		}
		if _, err := run(t, l.backend(d), request(service, control.OperationStage, l.candidates+"/missing")); err == nil {
			t.Fatal("stage of a missing candidate must fail")
		}
	}
	wantCalls(t, d)
}

func TestStageOutputEscapesLikeJSONStringify(t *testing.T) {
	l := newLayout(t)
	dir := l.candidate(t, "rev<&> ", nil)
	got := mustRun(t, l.backend(newFakeDocker()), request(control.ServiceNginx, control.OperationStage, dir))
	if got != `{"status":"staged","candidatePath":"`+dir+`"}` {
		t.Fatal(got)
	}
}

func TestValidateNginxRunsNginxT(t *testing.T) {
	l := newLayout(t)
	d := newFakeDocker()
	dir := l.candidate(t, "rev-1", nil)
	got := mustRun(t, l.backend(d), request(control.ServiceNginx, control.OperationValidate, dir))
	if got != `{"status":"validated","checksum":"sum-1"}` {
		t.Fatal(got)
	}
	wantCalls(t, d, "exec proxycore-nginx nginx -t -c "+dir+"/nginx.conf")
}

func TestValidateNginxNonZeroExit(t *testing.T) {
	l := newLayout(t)
	d := newFakeDocker()
	dir := l.candidate(t, "rev-1", nil)
	d.execExit["nginx -t -c "+dir+"/nginx.conf"] = 1
	_, err := run(t, l.backend(d), request(control.ServiceNginx, control.OperationValidate, dir))
	wantErr(t, err, "Fixed service operation failed with exit 1")
}

func TestExecTransportErrorPropagates(t *testing.T) {
	l := newLayout(t)
	d := newFakeDocker()
	dir := l.candidate(t, "rev-1", nil)
	d.execErr["nginx -s reload"] = errors.New("daemon down")
	_, err := run(t, l.backend(d), request(control.ServiceNginx, control.OperationReload, dir))
	wantErr(t, err, "daemon down")
}

func TestValidateCoreDNSChecksTheCorefileExists(t *testing.T) {
	l := newLayout(t)
	d := newFakeDocker()
	dir := l.candidate(t, "rev-1", map[string]string{"Corefile": appliedCorefile})
	got := mustRun(t, l.backend(d), request(control.ServiceCoreDNS, control.OperationValidate, dir))
	if got != `{"status":"validated","checksum":"sum-1"}` {
		t.Fatal(got)
	}
	if err := os.Remove(dir + "/Corefile"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, l.backend(d), request(control.ServiceCoreDNS, control.OperationValidate, dir)); err == nil {
		t.Fatal("expected error for a missing Corefile")
	}
	wantCalls(t, d)
}

func TestReload(t *testing.T) {
	l := newLayout(t)
	d := newFakeDocker()
	dir := l.candidate(t, "rev-1", nil)
	if got := mustRun(t, l.backend(d), request(control.ServiceNginx, control.OperationReload, dir)); got != `{"status":"reloaded"}` {
		t.Fatal(got)
	}
	if got := mustRun(t, l.backend(d), request(control.ServiceCoreDNS, control.OperationReload, dir)); got != `{"status":"reloaded"}` {
		t.Fatal(got)
	}
	wantCalls(t, d, "exec proxycore-nginx nginx -s reload", "restart proxycore-coredns t=10")
	d.execExit["nginx -s reload"] = 1
	_, err := run(t, l.backend(d), request(control.ServiceNginx, control.OperationReload, dir))
	wantErr(t, err, "Fixed service operation failed with exit 1")
	d.restartErr = errors.New("restart failed")
	_, err = run(t, l.backend(d), request(control.ServiceCoreDNS, control.OperationReload, dir))
	wantErr(t, err, "restart failed")
}

func TestHealthRequiresARunningContainer(t *testing.T) {
	l := newLayout(t)
	dir := l.candidate(t, "rev-1", nil)
	for _, service := range []control.Service{control.ServiceCoreDNS, control.ServiceNginx} {
		d := newFakeDocker()
		d.running = false
		_, err := run(t, l.backend(d), request(service, control.OperationHealth, dir))
		wantErr(t, err, string(service)+" container is not running")
		wantCalls(t, d, "inspect "+map[control.Service]string{control.ServiceCoreDNS: "proxycore-coredns", control.ServiceNginx: "proxycore-nginx"}[service])
	}
	d := newFakeDocker()
	d.runningErr = errors.New("No such container")
	_, err := run(t, l.backend(d), request(control.ServiceCoreDNS, control.OperationHealth, dir))
	wantErr(t, err, "No such container")
}

func TestHealthCoreDNSIsHealthyWhenRunning(t *testing.T) {
	l := newLayout(t)
	d := newFakeDocker()
	dir := l.candidate(t, "rev-1", nil)
	got := mustRun(t, l.backend(d), request(control.ServiceCoreDNS, control.OperationHealth, dir))
	if got != `{"status":"healthy"}` {
		t.Fatal(got)
	}
	wantCalls(t, d, "inspect proxycore-coredns")
}

func TestContainerNamesAreConfigurable(t *testing.T) {
	l := newLayout(t)
	d := newFakeDocker()
	dir := l.candidate(t, "rev-1", nil)
	b := New(d, Options{CandidateRoot: l.candidates, ZonesRoot: l.zones, ConfigRoot: l.config,
		Containers: Containers{CoreDNS: "dns-x", Nginx: "web-x"}})
	mustRun(t, b, request(control.ServiceNginx, control.OperationReload, dir))
	mustRun(t, b, request(control.ServiceCoreDNS, control.OperationReload, dir))
	wantCalls(t, d, "exec web-x nginx -s reload", "restart dns-x t=10")
}

func TestDefaultOptions(t *testing.T) {
	o := DefaultOptions()
	if o.CandidateRoot != "/var/lib/proxycore/candidates" || o.ZonesRoot != "/var/lib/proxycore/coredns-zones" ||
		o.ConfigRoot != "/var/lib/proxycore/coredns-config" || o.Containers.CoreDNS != "proxycore-coredns" ||
		o.Containers.Nginx != "proxycore-nginx" {
		t.Fatalf("defaults = %+v", o)
	}
}

func TestRestartCoreDNSAfterSeed(t *testing.T) {
	l := newLayout(t)
	d := newFakeDocker()
	write(t, l.candidates+"/rev-1/coredns/Corefile", appliedCorefile, 0o644)
	var out, errOut []string
	err := StartupSeed(t.Context(), d, DefaultOptionsWith(l), func(s string) { out = append(out, s) }, func(s string) { errOut = append(errOut, s) })
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0] != "proxycore-control restored persisted CoreDNS Corefile from "+l.candidates+"/rev-1/coredns/Corefile\n" ||
		out[1] != "proxycore-control restarted proxycore-coredns to load Corefile\n" || len(errOut) != 0 {
		t.Fatalf("out=%q err=%q", out, errOut)
	}
	wantCalls(t, d, "restart proxycore-coredns t=5")
}

func TestStartupSeedReportsRestartFailureOnStderr(t *testing.T) {
	l := newLayout(t)
	d := newFakeDocker()
	d.restartErr = errors.New("daemon down")
	write(t, l.candidates+"/rev-1/coredns/Corefile", appliedCorefile, 0o644)
	var out, errOut []string
	if err := StartupSeed(t.Context(), d, DefaultOptionsWith(l), func(s string) { out = append(out, s) }, func(s string) { errOut = append(errOut, s) }); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || len(errOut) != 1 ||
		errOut[0] != "proxycore-control could not restart CoreDNS after Corefile seed: daemon down\n" {
		t.Fatalf("out=%q err=%q", out, errOut)
	}
}

func TestStartupSeedIsSilentWithNothingToSeed(t *testing.T) {
	l := newLayout(t)
	d := newFakeDocker()
	var lines int
	if err := StartupSeed(t.Context(), d, DefaultOptionsWith(l), func(string) { lines++ }, func(string) { lines++ }); err != nil || lines != 0 {
		t.Fatalf("lines=%d err=%v", lines, err)
	}
	wantCalls(t, d)
}

func TestStartupSeedFailsWhenTheConfigRootCannotBeCreated(t *testing.T) {
	l := newLayout(t)
	write(t, l.config, "a file", 0o644) // config root is a regular file
	err := StartupSeed(t.Context(), newFakeDocker(), DefaultOptionsWith(l), func(string) {}, func(string) {})
	if err == nil {
		t.Fatal("expected error")
	}
}

// DefaultOptionsWith points the defaults at a test layout.
func DefaultOptionsWith(l layout) Options {
	o := DefaultOptions()
	o.CandidateRoot, o.ZonesRoot, o.ConfigRoot = l.candidates, l.zones, l.config
	return o
}
