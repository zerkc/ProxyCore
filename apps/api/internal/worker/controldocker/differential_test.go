package controldocker

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/worker/control"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/testsupport"
)

// scenario drives the real TypeScript docker-control.ts (Docker stubbed in
// testdata/docker-control.diff.bun.ts) and this package with identical inputs,
// then compares responses, Docker calls, archive bytes and the resulting tree.
type scenario struct {
	name        string
	setup       func(t *testing.T, l layout) // populates a fresh tree
	requests    func(l layout) []map[string]string
	files       map[string]string // container files
	execExit    map[string]int
	getErr      []string
	putFail     bool
	restartFail bool
	notRunning  bool
}

type specFile struct {
	CandidateRoot string              `json:"candidateRoot"`
	ZonesRoot     string              `json:"zonesRoot"`
	ConfigRoot    string              `json:"configRoot"`
	Requests      []map[string]string `json:"requests"`
	Files         map[string]string   `json:"files"`
	ExecExit      map[string]int      `json:"execExit"`
	GetErr        []string            `json:"getErr"`
	PutFail       bool                `json:"putFail"`
	RestartFail   bool                `json:"restartFail"`
	Running       bool                `json:"running"`
}

type outcome struct {
	Responses []struct {
		OK     bool            `json:"ok"`
		Output json.RawMessage `json:"output"`
		Error  string          `json:"error"`
	} `json:"responses"`
	Calls []string `json:"calls"`
	Puts  []string `json:"puts"`
}

func req(service, op, dir string) map[string]string {
	return map[string]string{"service": service, "operation": op, "revisionId": "rev-1", "checksum": "sum-1", "candidatePath": dir}
}

func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		info, _ := d.Info()
		switch {
		case d.IsDir():
			tree[rel] = "dir"
		case info.Mode()&os.ModeSymlink != 0:
			tree[rel] = "symlink"
		default:
			data, _ := os.ReadFile(p)
			tree[rel] = info.Mode().Perm().String() + " " + string(data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

var nodeFSError = regexp.MustCompile(`^E[A-Z]+: `)

func TestDifferentialAgainstTheTypeScriptImplementation(t *testing.T) {
	if testing.Short() {
		t.Skip("differential test runs Bun; skipped with -short")
	}
	testsupport.Bun(t)
	script := filepath.Join("testdata", "docker-control.diff.bun.ts")
	nginxDir := func(l layout) string { return l.candidates + "/rev-1" }
	baseNginx := func(t *testing.T, l layout) {
		write(t, nginxDir(l)+"/nginx.conf", promotedConf, 0o644)
	}
	baseCore := func(t *testing.T, l layout) {
		write(t, nginxDir(l)+"/Corefile", appliedCorefile, 0o644)
		write(t, nginxDir(l)+"/zones/a.zone", "a zone\n", 0o644)
		write(t, nginxDir(l)+"/zones/sub/b.zone", "b zone\n", 0o644)
		write(t, l.zones+"/old.zone", "old zone\n", 0o600)
	}
	nginxAll := func(l layout) []map[string]string {
		var out []map[string]string
		for _, op := range []string{"stage", "validate", "promote", "reload", "health", "rollback"} {
			out = append(out, req("nginx", op, nginxDir(l)))
		}
		return out
	}
	coreAll := func(l layout) []map[string]string {
		var out []map[string]string
		for _, op := range []string{"stage", "validate", "promote", "reload", "health", "rollback", "rollback"} {
			out = append(out, req("coredns", op, nginxDir(l)))
		}
		return out
	}

	scenarios := []scenario{
		{name: "nginx all operations, first apply", setup: baseNginx, requests: nginxAll,
			execExit: map[string]int{"cp /etc/nginx/nginx.conf /etc/nginx/.proxycore.previous.conf": 1}},
		{name: "nginx all operations, existing container config", setup: baseNginx, requests: nginxAll,
			files: map[string]string{"/etc/nginx/nginx.conf": "old config\n"}},
		{name: "nginx health healthy then invalid", setup: func(t *testing.T, l layout) {
			baseNginx(t, l)
			write(t, NginxLiveConfigPath(l.candidates), promotedConf, 0o644)
		}, requests: func(l layout) []map[string]string { return []map[string]string{req("nginx", "health", nginxDir(l))} },
			files: map[string]string{"/etc/nginx/nginx.conf": promotedConf}},
		{name: "nginx health invalid active config", setup: func(t *testing.T, l layout) {
			baseNginx(t, l)
			write(t, NginxLiveConfigPath(l.candidates), promotedConf, 0o644)
		}, requests: func(l layout) []map[string]string { return []map[string]string{req("nginx", "health", nginxDir(l))} },
			files: map[string]string{"/etc/nginx/nginx.conf": promotedConf}, execExit: map[string]int{"nginx -t -c /etc/nginx/nginx.conf": 1}},
		{name: "nginx health baked fallback and drift", setup: baseNginx,
			requests: func(l layout) []map[string]string { return []map[string]string{req("nginx", "health", nginxDir(l))} },
			files:    map[string]string{"/etc/nginx/nginx.conf": "events {}\nhttp { server { return 503; } }\n"}},
		{name: "nginx health without active config", setup: baseNginx, getErr: []string{"/etc/nginx/nginx.conf"},
			requests: func(l layout) []map[string]string { return []map[string]string{req("nginx", "health", nginxDir(l))} }},
		{name: "nginx health mismatched candidate", setup: func(t *testing.T, l layout) {
			baseNginx(t, l)
			write(t, NginxLiveConfigPath(l.candidates), "older\n"+promotedConf, 0o644)
		}, requests: func(l layout) []map[string]string { return []map[string]string{req("nginx", "health", nginxDir(l))} },
			files: map[string]string{"/etc/nginx/nginx.conf": "older\n" + promotedConf}},
		{name: "nginx health container not running", setup: baseNginx, notRunning: true,
			requests: func(l layout) []map[string]string { return []map[string]string{req("nginx", "health", nginxDir(l))} }},
		{name: "nginx promote copy failure with archive restore", setup: baseNginx,
			files:    map[string]string{"/etc/nginx/nginx.conf": "old config\n"},
			execExit: map[string]int{"cp " + "@CAND" + "/nginx.conf /etc/nginx/nginx.conf": 2},
			requests: func(l layout) []map[string]string { return []map[string]string{req("nginx", "promote", nginxDir(l))} }},
		{name: "nginx promote copy failure without archive", setup: baseNginx, getErr: []string{"/etc/nginx/nginx.conf"},
			execExit: map[string]int{"cp " + "@CAND" + "/nginx.conf /etc/nginx/nginx.conf": 1},
			requests: func(l layout) []map[string]string { return []map[string]string{req("nginx", "promote", nginxDir(l))} }},
		{name: "nginx promote stable write failure", setup: func(t *testing.T, l layout) {
			baseNginx(t, l)
			if err := os.MkdirAll(NginxLiveConfigPath(l.candidates)+"/x", 0o755); err != nil {
				t.Fatal(err)
			}
		}, files: map[string]string{"/etc/nginx/nginx.conf": "old config\n"},
			requests: func(l layout) []map[string]string { return []map[string]string{req("nginx", "promote", nginxDir(l))} }},
		{name: "nginx rollback with previous stable", setup: func(t *testing.T, l layout) {
			baseNginx(t, l)
			write(t, NginxLiveConfigPath(l.candidates), "current\n", 0o644)
			write(t, NginxPreviousStableConfigPath(l.candidates), "previous\n", 0o644)
		}, requests: func(l layout) []map[string]string { return []map[string]string{req("nginx", "rollback", nginxDir(l))} }},
		{name: "nginx rollback put failure", setup: func(t *testing.T, l layout) {
			baseNginx(t, l)
			write(t, NginxLiveConfigPath(l.candidates), "current\n", 0o644)
			write(t, NginxPreviousStableConfigPath(l.candidates), "previous\n", 0o644)
		}, putFail: true, requests: func(l layout) []map[string]string { return []map[string]string{req("nginx", "rollback", nginxDir(l))} }},
		{name: "nginx rollback without previous", setup: func(t *testing.T, l layout) {
			baseNginx(t, l)
			write(t, NginxLiveConfigPath(l.candidates), "current\n", 0o644)
		}, requests: func(l layout) []map[string]string {
			return []map[string]string{req("nginx", "rollback", nginxDir(l)), req("nginx", "rollback", nginxDir(l))}
		}},
		{name: "coredns all operations, live Corefile present", setup: func(t *testing.T, l layout) {
			baseCore(t, l)
			write(t, CorednsLiveCorefilePath(l.config), forwardOnlyCorefile, 0o600)
		}, requests: coreAll},
		{name: "coredns previous Corefile from container volume", setup: baseCore, requests: coreAll,
			files: map[string]string{"/etc/coredns-config/Corefile": "from volume\n"}},
		{name: "coredns previous Corefile from image", setup: baseCore, requests: coreAll,
			files: map[string]string{"/etc/coredns/Corefile": "image default\n"}},
		{name: "coredns no previous Corefile", setup: baseCore,
			requests: func(l layout) []map[string]string { return []map[string]string{req("coredns", "promote", nginxDir(l))} }},
		{name: "coredns put failure restores", setup: func(t *testing.T, l layout) {
			baseCore(t, l)
			write(t, CorednsLiveCorefilePath(l.config), forwardOnlyCorefile, 0o644)
		}, putFail: true, requests: func(l layout) []map[string]string { return []map[string]string{req("coredns", "promote", nginxDir(l))} }},
		{name: "coredns unsupported zone entry", setup: func(t *testing.T, l layout) {
			baseCore(t, l)
			write(t, CorednsLiveCorefilePath(l.config), forwardOnlyCorefile, 0o644)
			if err := os.Symlink("/etc/passwd", nginxDir(l)+"/zones/evil.zone"); err != nil {
				t.Fatal(err)
			}
		}, requests: func(l layout) []map[string]string { return []map[string]string{req("coredns", "promote", nginxDir(l))} }},
		{name: "coredns health not running and reload failure", setup: baseCore, notRunning: true, restartFail: true,
			requests: func(l layout) []map[string]string {
				return []map[string]string{req("coredns", "health", nginxDir(l)), req("coredns", "reload", nginxDir(l))}
			}},
		{name: "path outside the fixed root", setup: baseNginx,
			requests: func(l layout) []map[string]string {
				return []map[string]string{req("nginx", "stage", "/etc/passwd"), req("coredns", "rollback", l.candidates+"/../x")}
			}},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			l := newLayout(t)
			prepare := func() {
				if err := os.RemoveAll(l.root); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(l.candidates, 0o755); err != nil {
					t.Fatal(err)
				}
				sc.setup(t, l)
			}
			execExit := map[string]int{}
			for k, v := range sc.execExit {
				execExit[strings.ReplaceAll(k, "@CAND", nginxDir(l))] = v
			}
			spec := specFile{
				CandidateRoot: l.candidates, ZonesRoot: l.zones, ConfigRoot: l.config,
				Requests: sc.requests(l), Files: sc.files, ExecExit: execExit, GetErr: sc.getErr,
				PutFail: sc.putFail, RestartFail: sc.restartFail, Running: !sc.notRunning,
			}
			if spec.GetErr == nil {
				spec.GetErr = []string{}
			}
			if spec.Files == nil {
				spec.Files = map[string]string{}
			}

			// TypeScript run.
			prepare()
			dir := t.TempDir()
			specPath, outPath := filepath.Join(dir, "spec.json"), filepath.Join(dir, "out.json")
			raw, _ := json.Marshal(spec)
			if err := os.WriteFile(specPath, raw, 0o644); err != nil {
				t.Fatal(err)
			}
			script, _ := filepath.Abs(script)
			if out := testsupport.RunBun(t, script, specPath, outPath); strings.Contains(out, "error:") {
				t.Fatalf("bun failed: %s", out)
			}
			var want outcome
			data, err := os.ReadFile(outPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &want); err != nil {
				t.Fatal(err)
			}
			wantTree := snapshot(t, l.root)

			// Go run.
			prepare()
			fake := newFakeDocker()
			fake.running = !sc.notRunning
			for p, c := range spec.Files {
				fake.files[p] = []byte(c)
			}
			for k, v := range execExit {
				fake.execExit[k] = v
			}
			for _, p := range sc.getErr {
				fake.getErr[p] = errors.New("boom")
			}
			if sc.putFail {
				fake.putErr = errors.New("daemon down")
			}
			if sc.restartFail {
				fake.restartErr = errors.New("restart failed")
			}
			backend := l.backend(fake)
			for i, r := range spec.Requests {
				handler := backend.Handlers()[control.Service(r["service"])][control.Operation(r["operation"])]
				output, herr := handler(context.Background(), control.Request{
					RequestID: "req-1", Operation: control.Operation(r["operation"]), Service: control.Service(r["service"]),
					RevisionID: r["revisionId"], Checksum: r["checksum"], CandidatePath: r["candidatePath"],
				})
				w := want.Responses[i]
				if (herr == nil) != w.OK {
					t.Fatalf("request %d (%s %s): go err=%v, ts ok=%v error=%q", i, r["service"], r["operation"], herr, w.OK, w.Error)
				}
				if herr != nil {
					if !nodeFSError.MatchString(w.Error) && herr.Error() != w.Error {
						t.Errorf("request %d error = %q, ts %q", i, herr.Error(), w.Error)
					}
					continue
				}
				if string(output) != string(w.Output) {
					t.Errorf("request %d output = %s, ts %s", i, output, w.Output)
				}
			}
			if got, want := strings.Join(fake.Calls(), "\n"), strings.Join(want.Calls, "\n"); got != want {
				t.Errorf("docker calls differ:\ngo:\n%s\nts:\n%s", got, want)
			}
			if got, want := strings.Join(fake.rawPuts, "\n"), strings.Join(want.Puts, "\n"); got != want {
				t.Errorf("archive bytes differ:\ngo:\n%s\nts:\n%s", got, want)
			}
			gotTree := snapshot(t, l.root)
			keys := map[string]bool{}
			for k := range gotTree {
				keys[k] = true
			}
			for k := range wantTree {
				keys[k] = true
			}
			var names []string
			for k := range keys {
				names = append(names, k)
			}
			sort.Strings(names)
			for _, k := range names {
				if gotTree[k] != wantTree[k] {
					t.Errorf("tree %q: go %q, ts %q", k, gotTree[k], wantTree[k])
				}
			}
		})
	}
}
