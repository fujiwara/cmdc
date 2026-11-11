package cmdc

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
)

// ExitError tells the main function to exit with Code. Err is reported
// to the user if it is not nil.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("exit status %d", e.Code)
}

func (e *ExitError) Unwrap() error { return e.Err }

// recorder keeps the output of a command as chunks up to max bytes.
type recorder struct {
	mu       sync.Mutex
	chunks   []Chunk
	size     int64
	max      int64
	overflow bool
}

func (r *recorder) record(stream Stream, p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.overflow {
		return
	}
	r.size += int64(len(p))
	if r.size > r.max {
		r.overflow = true
		r.chunks = nil
		return
	}
	if n := len(r.chunks); n > 0 && r.chunks[n-1].Stream == stream {
		r.chunks[n-1].Data = append(r.chunks[n-1].Data, p...)
		return
	}
	r.chunks = append(r.chunks, Chunk{Stream: stream, Data: append([]byte(nil), p...)})
}

type recordWriter struct {
	r      *recorder
	stream Stream
	w      io.Writer
}

func (w *recordWriter) Write(p []byte) (int, error) {
	w.r.record(w.stream, p)
	return w.w.Write(p)
}

// runCommand runs argv, passing its output through to stdout and stderr
// while recording it. The returned chunks are nil if the output is larger
// than maxSize. A failure of the command is returned as *ExitError.
func runCommand(argv []string, stdin io.Reader, stdout, stderr io.Writer, maxSize int64) ([]Chunk, error) {
	rec := &recorder{max: maxSize}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = stdin
	cmd.Stdout = &recordWriter{r: rec, stream: Stdout, w: stdout}
	cmd.Stderr = &recordWriter{r: rec, stream: Stderr, w: stderr}

	// SIGINT is not forwarded: on a terminal it is delivered to the whole
	// foreground process group, so the command receives it by itself.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigCh)

	if err := cmd.Start(); err != nil {
		code := 126
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			code = 127
		}
		return nil, &ExitError{Code: code, Err: err}
	}
	done := make(chan struct{})
	go func() {
		for {
			select {
			case sig := <-sigCh:
				cmd.Process.Signal(sig)
			case <-done:
				return
			}
		}
	}()
	err := cmd.Wait()
	close(done)

	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				return nil, &ExitError{Code: 128 + int(ws.Signal())}
			}
			return nil, &ExitError{Code: ee.ExitCode()}
		}
		return nil, &ExitError{Code: 1, Err: err}
	}
	if rec.overflow {
		return nil, nil
	}
	if rec.chunks == nil {
		return []Chunk{}, nil
	}
	return rec.chunks, nil
}

// replay writes cached chunks to stdout and stderr.
func replay(chunks []Chunk, stdout, stderr io.Writer) error {
	for _, ch := range chunks {
		w := stdout
		if ch.Stream == Stderr {
			w = stderr
		}
		if _, err := w.Write(ch.Data); err != nil {
			return err
		}
	}
	return nil
}
