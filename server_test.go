package cmdc_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fujiwara/cmdc"
)

// tempSocket returns a socket path short enough for sun_path.
func tempSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cmdc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "t.sock")
}

func startServer(t *testing.T, s *cmdc.Server) chan error {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("server did not stop")
		}
	})
	return done
}

func dial(t *testing.T, socket string) *cmdc.Client {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := cmdc.Dial(t.Context(), socket)
		if err == nil {
			t.Cleanup(func() { c.Close() })
			return c
		}
		if time.Now().After(deadline) {
			t.Fatalf("failed to dial: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestServer(t *testing.T) {
	socket := tempSocket(t)
	startServer(t, &cmdc.Server{Socket: socket})
	c := dial(t, socket)

	if c.ServerVersion != cmdc.Version || c.ServerPID != os.Getpid() {
		t.Errorf("unexpected hello: %s %d", c.ServerVersion, c.ServerPID)
	}
	if fi, err := os.Stat(socket); err != nil {
		t.Error(err)
	} else if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("unexpected socket permission: %o", perm)
	}

	if _, hit, err := c.Get("k", time.Minute); err != nil || hit {
		t.Fatalf("unexpected get: hit=%v err=%v", hit, err)
	}
	chunks := []cmdc.Chunk{
		{Stream: cmdc.Stdout, Data: []byte("out\x00\xff")},
		{Stream: cmdc.Stderr, Data: []byte("err")},
	}
	if stored, err := c.Set("k", "cmd arg", chunks, 300*time.Millisecond); err != nil || !stored {
		t.Fatalf("unexpected set: stored=%v err=%v", stored, err)
	}

	// another connection sees the entry
	c2 := dial(t, socket)
	got, hit, err := c2.Get("k", time.Minute)
	if err != nil || !hit {
		t.Fatalf("unexpected get: hit=%v err=%v", hit, err)
	}
	if len(got) != 2 || got[0].Stream != cmdc.Stdout || string(got[0].Data) != "out\x00\xff" ||
		got[1].Stream != cmdc.Stderr || string(got[1].Data) != "err" {
		t.Errorf("unexpected chunks: %v", got)
	}

	st, err := c.Status()
	if err != nil {
		t.Fatal(err)
	}
	if !st.Running || st.Entries != 1 || st.TotalSize != 8 || st.Items[0].Command != "cmd arg" {
		t.Errorf("unexpected status: %+v", st)
	}

	time.Sleep(400 * time.Millisecond)
	if _, hit, err := c.Get("k", time.Minute); err != nil || hit {
		t.Errorf("unexpected get after expiration: hit=%v err=%v", hit, err)
	}

	c.Set("k", "cmd", chunks, time.Minute)
	if n, err := c.Clear(); err != nil || n != 1 {
		t.Errorf("unexpected clear: n=%d err=%v", n, err)
	}
	if _, hit, _ := c.Get("k", time.Minute); hit {
		t.Error("unexpected hit after clear")
	}
}

func TestServerStop(t *testing.T) {
	socket := tempSocket(t)
	done := startServer(t, &cmdc.Server{Socket: socket})
	c := dial(t, socket)
	idle := dial(t, socket) // an open connection must not block stopping
	_ = idle
	if err := c.Stop(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
		done <- nil // for cleanup
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop")
	}
	if _, err := os.Stat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket must be removed: %v", err)
	}
}

func TestServerExclusive(t *testing.T) {
	socket := tempSocket(t)
	startServer(t, &cmdc.Server{Socket: socket})
	c := dial(t, socket)
	c.Set("k", "cmd", []cmdc.Chunk{}, time.Minute)

	// the second server gives up, and does not break the first one
	err := (&cmdc.Server{Socket: socket}).Run(t.Context())
	if !errors.Is(err, cmdc.ErrAlreadyRunning) {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, hit, err := dial(t, socket).Get("k", time.Minute); err != nil || !hit {
		t.Errorf("unexpected get: hit=%v err=%v", hit, err)
	}
}

func TestServerStaleSocket(t *testing.T) {
	socket := tempSocket(t)
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	startServer(t, &cmdc.Server{Socket: socket})
	dial(t, socket)
}

func TestServerIdleTimeout(t *testing.T) {
	socket := tempSocket(t)
	done := startServer(t, &cmdc.Server{
		Socket:      socket,
		IdleTimeout: 200 * time.Millisecond,
		SweepPeriod: 20 * time.Millisecond,
	})
	c := dial(t, socket)
	c.Set("k", "cmd", []cmdc.Chunk{}, 300*time.Millisecond)
	c.Close()
	start := time.Now()
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
		done <- nil // for cleanup
		// alive while it has an entry
		if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
			t.Errorf("server stopped too early: %s", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop")
	}
}
