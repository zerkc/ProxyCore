package controldocker

import (
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeDaemon is a minimal Docker Engine API served on a Unix socket. It records
// "METHOD path?query" for every request.
type fakeDaemon struct {
	mu       sync.Mutex
	requests []string
	bodies   map[string]string
	handler  func(w http.ResponseWriter, r *http.Request)
}

func (d *fakeDaemon) seen() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.requests...)
}

func startFakeDaemon(t *testing.T, handler func(d *fakeDaemon, w http.ResponseWriter, r *http.Request)) (*Client, *fakeDaemon) {
	t.Helper()
	dir, err := os.MkdirTemp("", "pcd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "d.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	daemon := &fakeDaemon{bodies: map[string]string{}}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Strip the negotiated /vX.Y prefix so assertions stay version-agnostic.
		path := r.URL.Path
		if strings.HasPrefix(path, "/v") {
			if i := strings.Index(path[1:], "/"); i >= 0 {
				path = path[i+1:]
			}
		}
		key := r.Method + " " + path
		if r.URL.RawQuery != "" {
			key += "?" + r.URL.RawQuery
		}
		body, _ := io.ReadAll(r.Body)
		daemon.mu.Lock()
		if path != "/_ping" {
			daemon.requests = append(daemon.requests, key)
		}
		daemon.bodies[key] = string(body)
		daemon.mu.Unlock()
		if path == "/_ping" {
			w.Header().Set("API-Version", "1.43")
			w.WriteHeader(http.StatusOK)
			return
		}
		handler(daemon, w, r)
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	c, err := NewClient(socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, daemon
}

func TestClientRunningReadsStateRunning(t *testing.T) {
	for _, running := range []string{"true", "false"} {
		c, daemon := startFakeDaemon(t, func(_ *fakeDaemon, w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"Id":"abc","State":{"Running":`+running+`}}`)
		})
		got, err := c.Running(context.Background(), "proxycore-nginx")
		if err != nil || got != (running == "true") {
			t.Fatalf("Running = %v, %v for %s", got, err, running)
		}
		if seen := daemon.seen(); len(seen) != 1 || seen[0] != "GET /containers/proxycore-nginx/json" {
			t.Fatalf("requests = %v", seen)
		}
	}
}

func TestClientRunningSurfacesDaemonError(t *testing.T) {
	c, _ := startFakeDaemon(t, func(_ *fakeDaemon, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"No such container: x"}`)
	})
	if _, err := c.Running(context.Background(), "x"); err == nil {
		t.Fatal("expected error")
	}
}

func TestClientRestartSendsTimeout(t *testing.T) {
	c, daemon := startFakeDaemon(t, func(_ *fakeDaemon, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.Restart(context.Background(), "proxycore-coredns", 10); err != nil {
		t.Fatal(err)
	}
	if seen := daemon.seen(); len(seen) != 1 || seen[0] != "POST /containers/proxycore-coredns/restart?t=10" {
		t.Fatalf("requests = %v", seen)
	}
}

func TestClientExecDrainsOutputAndReturnsExitCode(t *testing.T) {
	for _, code := range []string{"0", "3"} {
		c, daemon := startFakeDaemon(t, func(d *fakeDaemon, w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/containers/proxycore-nginx/exec"):
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				io.WriteString(w, `{"Id":"exec-1"}`)
			case strings.HasSuffix(r.URL.Path, "/exec/exec-1/start"):
				conn, rw, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				rw.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
				// One multiplexed stdout frame: stream 1, length 5.
				rw.Write([]byte{1, 0, 0, 0, 0, 0, 0, 5, 'h', 'e', 'l', 'l', 'o'})
				rw.Flush()
			case strings.HasSuffix(r.URL.Path, "/exec/exec-1/json"):
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"ExitCode":`+code+`,"Running":false}`)
			default:
				t.Errorf("unexpected request %s", r.URL)
			}
		})
		got, err := c.Exec(context.Background(), "proxycore-nginx", []string{"nginx", "-s", "reload"})
		if err != nil || got != map[string]int{"0": 0, "3": 3}[code] {
			t.Fatalf("Exec = %d, %v for %s", got, err, code)
		}
		body := daemon.bodies["POST /containers/proxycore-nginx/exec"]
		for _, want := range []string{`"Cmd":["nginx","-s","reload"]`, `"AttachStdout":true`, `"AttachStderr":true`} {
			if !strings.Contains(body, want) {
				t.Fatalf("exec create body %s lacks %s", body, want)
			}
		}
	}
}

func TestClientExecWaitsWhileExecIsStillRunning(t *testing.T) {
	var mu sync.Mutex
	polls := 0
	c, _ := startFakeDaemon(t, func(d *fakeDaemon, w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/exec"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"Id":"exec-2"}`)
		case strings.HasSuffix(r.URL.Path, "/start"):
			conn, rw, _ := w.(http.Hijacker).Hijack()
			defer conn.Close()
			rw.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			rw.Flush()
		default:
			mu.Lock()
			polls++
			running := polls < 3
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if running {
				io.WriteString(w, `{"ExitCode":0,"Running":true}`)
				return
			}
			io.WriteString(w, `{"ExitCode":7,"Running":false}`)
		}
	})
	got, err := c.Exec(context.Background(), "c", []string{"true"})
	if err != nil || got != 7 {
		t.Fatalf("Exec = %d, %v", got, err)
	}
}

func TestClientPutArchiveSendsTarToDirectory(t *testing.T) {
	c, daemon := startFakeDaemon(t, func(_ *fakeDaemon, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	archive := createTar([]tarEntry{{Name: "Corefile", Contents: []byte("x")}})
	if err := c.PutArchive(context.Background(), "proxycore-coredns", "/etc/coredns-config", archive); err != nil {
		t.Fatal(err)
	}
	key := "PUT /containers/proxycore-coredns/archive?path=%2Fetc%2Fcoredns-config"
	if got, ok := daemon.bodies[key]; !ok || got != string(archive) {
		t.Fatalf("archive request missing or body mismatch; requests=%v", daemon.seen())
	}
}

func TestClientGetArchiveReadsTheWholeStream(t *testing.T) {
	archive := createTar([]tarEntry{{Name: "nginx.conf", Contents: []byte("events {}\n")}})
	c, daemon := startFakeDaemon(t, func(_ *fakeDaemon, w http.ResponseWriter, r *http.Request) {
		stat := base64.StdEncoding.EncodeToString([]byte(`{"name":"nginx.conf","size":10,"mode":420}`))
		w.Header().Set("X-Docker-Container-Path-Stat", stat)
		w.Header().Set("Content-Type", "application/x-tar")
		w.Write(archive)
	})
	got, err := c.GetArchive(context.Background(), "proxycore-nginx", "/etc/nginx/nginx.conf")
	if err != nil || string(got) != string(archive) {
		t.Fatalf("GetArchive = %d bytes, %v", len(got), err)
	}
	if seen := daemon.seen(); len(seen) != 1 || seen[0] != "GET /containers/proxycore-nginx/archive?path=%2Fetc%2Fnginx%2Fnginx.conf" {
		t.Fatalf("requests = %v", seen)
	}
}
