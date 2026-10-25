package controldocker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/worker/control"
)

// fakeDocker implements Docker in memory. Container files live in files; calls
// records every call in order as a readable line.
type fakeDocker struct {
	mu      sync.Mutex
	calls   []string
	rawPuts []string          // base64 archives, in call order
	files   map[string][]byte // container path -> contents
	running bool

	runningErr error
	restartErr error
	putErr     error
	getErr     map[string]error // container path -> error
	execExit   map[string]int   // joined command -> exit code
	execErr    map[string]error // joined command -> transport error
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		files:    map[string][]byte{},
		running:  true,
		getErr:   map[string]error{},
		execExit: map[string]int{},
		execErr:  map[string]error{},
	}
}

func (f *fakeDocker) record(line string) {
	f.calls = append(f.calls, line)
}

func (f *fakeDocker) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeDocker) Running(_ context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("inspect " + name)
	return f.running, f.runningErr
}

func (f *fakeDocker) Restart(_ context.Context, name string, seconds int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("restart " + name + " t=" + strconv.Itoa(seconds))
	return f.restartErr
}

func (f *fakeDocker) Exec(_ context.Context, name string, cmd []string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	joined := strings.Join(cmd, " ")
	f.record("exec " + name + " " + joined)
	if err := f.execErr[joined]; err != nil {
		return 0, err
	}
	code := f.execExit[joined]
	if code == 0 && len(cmd) == 3 && cmd[0] == "cp" {
		if data, err := os.ReadFile(cmd[1]); err == nil {
			f.files[cmd[2]] = data
		} else if data, ok := f.files[cmd[1]]; ok {
			f.files[cmd[2]] = data
		}
	}
	return code, nil
}

func (f *fakeDocker) PutArchive(_ context.Context, name, dir string, archive []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	reader := tar.NewReader(bytes.NewReader(archive))
	header, err := reader.Next()
	if err != nil {
		return err
	}
	data, _ := io.ReadAll(reader)
	f.rawPuts = append(f.rawPuts, base64.StdEncoding.EncodeToString(archive))
	f.record("put " + name + " " + dir + " " + header.Name + "=" + string(data))
	if f.putErr != nil {
		return f.putErr
	}
	f.files[path.Join(dir, header.Name)] = data
	return nil
}

func (f *fakeDocker) GetArchive(_ context.Context, name, p string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("get " + name + " " + p)
	if err := f.getErr[p]; err != nil {
		return nil, err
	}
	data, ok := f.files[p]
	if !ok {
		return nil, errors.New("no such file")
	}
	return createTar([]tarEntry{{Name: path.Base(p), Contents: data}}), nil
}

// layout is a temp tree matching the control container's volumes.
type layout struct {
	root, candidates, zones, config string
}

func newLayout(t *testing.T) layout {
	t.Helper()
	root := t.TempDir()
	l := layout{
		root:       root,
		candidates: root + "/candidates",
		zones:      root + "/zones",
		config:     root + "/config",
	}
	if err := os.MkdirAll(l.candidates, 0o755); err != nil {
		t.Fatal(err)
	}
	return l
}

func (l layout) backend(d Docker) *Backend {
	return New(d, Options{
		CandidateRoot: l.candidates,
		ZonesRoot:     l.zones,
		ConfigRoot:    l.config,
		Containers:    Containers{CoreDNS: "proxycore-coredns", Nginx: "proxycore-nginx"},
	})
}

func (l layout) candidate(t *testing.T, rev string, files map[string]string) string {
	t.Helper()
	dir := l.candidates + "/" + rev
	for name, contents := range files {
		write(t, dir+"/"+name, contents, 0o644)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func write(t *testing.T, p, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(path.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func request(service control.Service, op control.Operation, candidatePath string) control.Request {
	return control.Request{
		RequestID: "req-1", Operation: op, Service: service,
		RevisionID: "rev-1", Checksum: "sum-1", CandidatePath: candidatePath,
	}
}

// run executes one operation through the handler table, like the dispatcher.
func run(t *testing.T, b *Backend, req control.Request) (string, error) {
	t.Helper()
	handler := b.Handlers()[req.Service][req.Operation]
	if handler == nil {
		t.Fatalf("no handler for %s %s", req.Service, req.Operation)
	}
	out, err := handler(context.Background(), req)
	return string(out), err
}

func mustRun(t *testing.T, b *Backend, req control.Request) string {
	t.Helper()
	out, err := run(t, b, req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Service, req.Operation, err)
	}
	return out
}

func wantErr(t *testing.T, err error, text string) {
	t.Helper()
	if err == nil || err.Error() != text {
		t.Fatalf("error = %v, want %q", err, text)
	}
}

func wantCalls(t *testing.T, f *fakeDocker, want ...string) {
	t.Helper()
	got := f.Calls()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
