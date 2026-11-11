package cmdc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// ErrAlreadyRunning is returned by Server.Run when another daemon holds
// the lock for the same socket.
var ErrAlreadyRunning = errors.New("daemon is already running")

const (
	DefaultIdleTimeout  = 10 * time.Minute
	DefaultMaxTotalSize = 256 << 20
	defaultSweepPeriod  = 5 * time.Second
	lockWait            = 2 * time.Second
)

// Server is the resident process which keeps cached outputs in memory.
// It never executes commands; it only stores what clients send.
type Server struct {
	Socket string
	// IdleTimeout is the duration after which the server exits when it
	// has no cached entries and no connections.
	IdleTimeout  time.Duration
	MaxTotalSize int64
	SweepPeriod  time.Duration

	cache     *Cache
	startedAt time.Time
	stop      context.CancelFunc

	mu         sync.Mutex
	conns      map[net.Conn]struct{}
	lastActive time.Time
}

// Run listens on the socket and serves until ctx is canceled, a stop
// request is received, or the server becomes idle.
func (s *Server) Run(ctx context.Context) error {
	if s.IdleTimeout <= 0 {
		s.IdleTimeout = DefaultIdleTimeout
	}
	if s.MaxTotalSize <= 0 {
		s.MaxTotalSize = DefaultMaxTotalSize
	}
	if s.SweepPeriod <= 0 {
		s.SweepPeriod = defaultSweepPeriod
	}
	if err := os.MkdirAll(filepath.Dir(s.Socket), 0o700); err != nil {
		return err
	}

	// The lock is held for the lifetime of the process, so that only one
	// daemon serves a socket and a stale socket file can be removed safely.
	lock, err := acquireLock(ctx, s.Socket+".lock", lockWait)
	if err != nil {
		return err
	}
	defer lock.Close()

	if err := os.Remove(s.Socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ln, err := listenPrivate(s.Socket)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.stop = cancel
	s.cache = NewCache(s.MaxTotalSize)
	s.conns = make(map[net.Conn]struct{})
	s.startedAt = time.Now()
	s.lastActive = s.startedAt
	slog.Debug("daemon started", "socket", s.Socket, "pid", os.Getpid())

	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns[conn] = struct{}{}
			s.mu.Unlock()
			wg.Go(func() { s.handle(conn) })
		}
	})

	ticker := time.NewTicker(s.SweepPeriod)
	defer ticker.Stop()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-ticker.C:
			s.cache.Sweep()
			if s.idle() {
				slog.Debug("daemon is idle, exiting")
				break loop
			}
		}
	}

	// Closing the listener also removes the socket file.
	ln.Close()
	s.mu.Lock()
	for conn := range s.conns {
		conn.Close()
	}
	s.mu.Unlock()
	wg.Wait()
	return nil
}

func (s *Server) idle() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns) == 0 && s.cache.Len() == 0 && time.Since(s.lastActive) >= s.IdleTimeout
}

func (s *Server) touch() {
	s.mu.Lock()
	s.lastActive = time.Now()
	s.mu.Unlock()
}

func (s *Server) handle(conn net.Conn) {
	defer func() {
		conn.Close()
		s.mu.Lock()
		delete(s.conns, conn)
		s.lastActive = time.Now()
		s.mu.Unlock()
	}()
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	for {
		var req Request
		if err := dec.Decode(&req); err != nil {
			return
		}
		s.touch()
		if err := enc.Encode(s.dispatch(&req)); err != nil {
			return
		}
		if req.Op == OpStop {
			s.stop()
			return
		}
	}
}

func (s *Server) dispatch(req *Request) *Response {
	resp := &Response{Version: Version, PID: os.Getpid()}
	switch req.Op {
	case OpHello, OpStop:
	case OpGet:
		resp.Chunks, resp.Hit = s.cache.Get(req.Key, req.Expires)
	case OpSet:
		resp.Stored = s.cache.Set(req.Key, req.Label, req.Chunks, req.Expires)
	case OpClear:
		resp.Cleared = s.cache.Clear()
	case OpStatus:
		items, total := s.cache.Stats()
		resp.Status = &Status{
			Running:   true,
			Socket:    s.Socket,
			PID:       os.Getpid(),
			Version:   Version,
			Uptime:    time.Since(s.startedAt).Round(time.Second).String(),
			Entries:   len(items),
			TotalSize: total,
			Items:     items,
		}
	default:
		resp.Error = fmt.Sprintf("unknown op: %q", req.Op)
	}
	return resp
}

// acquireLock takes an exclusive lock on path, waiting up to wait for a
// previous daemon which is shutting down.
func acquireLock(ctx context.Context, path string, wait time.Duration) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, fmt.Errorf("failed to lock %s: %w", path, err)
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, ErrAlreadyRunning
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

var umaskMu sync.Mutex

// listenPrivate creates a unix domain socket accessible only by the owner.
func listenPrivate(path string) (net.Listener, error) {
	umaskMu.Lock()
	defer umaskMu.Unlock()
	old := syscall.Umask(0o177)
	defer syscall.Umask(old)
	return net.Listen("unix", path)
}
