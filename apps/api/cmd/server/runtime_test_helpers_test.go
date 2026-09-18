package main

import (
	"strings"
	"sync"
	"time"
)

type runtimeTestLog struct {
	mu      sync.Mutex
	text    strings.Builder
	changed chan struct{}
}

func (w *runtimeTestLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.changed == nil {
		w.changed = make(chan struct{})
	}
	n, err := w.text.Write(p)
	close(w.changed)
	w.changed = make(chan struct{})
	return n, err
}

func (w *runtimeTestLog) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.text.String()
}

func (w *runtimeTestLog) WaitFor(want string, timeout time.Duration) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		w.mu.Lock()
		if strings.Contains(w.text.String(), want) {
			w.mu.Unlock()
			return true
		}
		changed := w.changed
		if changed == nil {
			changed = make(chan struct{})
			w.changed = changed
		}
		w.mu.Unlock()
		select {
		case <-changed:
		case <-deadline.C:
			return false
		}
	}
}
