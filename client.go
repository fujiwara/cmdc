package cmdc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

const (
	callTimeout  = 10 * time.Second
	startTimeout = 5 * time.Second
)

// Client talks to the daemon over a unix domain socket.
type Client struct {
	conn          net.Conn
	enc           *json.Encoder
	dec           *json.Decoder
	ServerVersion string
	ServerPID     int
}

// Dial connects to a running daemon.
func Dial(ctx context.Context, socket string) (*Client, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	c := &Client{
		conn: conn,
		enc:  json.NewEncoder(conn),
		dec:  json.NewDecoder(conn),
	}
	resp, err := c.call(&Request{Op: OpHello})
	if err != nil {
		conn.Close()
		return nil, err
	}
	c.ServerVersion = resp.Version
	c.ServerPID = resp.PID
	return c, nil
}

// DialOrStart connects to the daemon, starting it if it is not running.
// A daemon of a different version is replaced with the current one.
func DialOrStart(ctx context.Context, socket string) (*Client, error) {
	c, err := Dial(ctx, socket)
	if err == nil {
		if c.ServerVersion == Version {
			return c, nil
		}
		c.Stop()
		c.Close()
	}
	deadline := time.Now().Add(startTimeout)
	var lastSpawn time.Time
	var exited <-chan error
	for {
		if time.Since(lastSpawn) >= time.Second {
			if exited, err = spawnDaemon(socket); err != nil {
				return nil, fmt.Errorf("failed to start daemon: %w", err)
			}
			lastSpawn = time.Now()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case err := <-exited:
			// A daemon which lost the race to another one exits
			// successfully. Otherwise it is not able to run at all.
			if err != nil {
				return nil, fmt.Errorf("daemon exited: %w", err)
			}
			exited = nil
		case <-time.After(20 * time.Millisecond):
		}
		c, err := Dial(ctx, socket)
		if err == nil {
			if c.ServerVersion == Version {
				return c, nil
			}
			c.Close()
			err = fmt.Errorf("daemon version mismatch: %s", c.ServerVersion)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("failed to connect to daemon: %w", err)
		}
	}
}

func (c *Client) call(req *Request) (*Response, error) {
	c.conn.SetDeadline(time.Now().Add(callTimeout))
	if err := c.enc.Encode(req); err != nil {
		return nil, err
	}
	var resp Response
	if err := c.dec.Decode(&resp); err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, errors.New(resp.Error)
	}
	return &resp, nil
}

// Get looks up the cached output for key which is younger than expires.
func (c *Client) Get(key string, expires time.Duration) ([]Chunk, bool, error) {
	resp, err := c.call(&Request{Op: OpGet, Key: key, Expires: expires})
	if err != nil {
		return nil, false, err
	}
	return resp.Chunks, resp.Hit, nil
}

// Set stores the output for key. label is shown in the status.
func (c *Client) Set(key, label string, chunks []Chunk, expires time.Duration) (bool, error) {
	resp, err := c.call(&Request{Op: OpSet, Key: key, Label: label, Chunks: chunks, Expires: expires})
	if err != nil {
		return false, err
	}
	return resp.Stored, nil
}

// Status returns the status of the daemon.
func (c *Client) Status() (*Status, error) {
	resp, err := c.call(&Request{Op: OpStatus})
	if err != nil {
		return nil, err
	}
	if resp.Status == nil {
		return nil, errors.New("no status in response")
	}
	return resp.Status, nil
}

// Clear removes all cached entries and returns the number of them.
func (c *Client) Clear() (int, error) {
	resp, err := c.call(&Request{Op: OpClear})
	if err != nil {
		return 0, err
	}
	return resp.Cleared, nil
}

// Stop asks the daemon to exit.
func (c *Client) Stop() error {
	_, err := c.call(&Request{Op: OpStop})
	return err
}

// Close closes the connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// spawnDaemon starts the daemon as a detached process in its own session.
// The returned channel receives the result of the daemon when it exits.
func spawnDaemon(socket string) (<-chan error, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, "daemon", "--socket", socket)
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	return exited, nil
}

// SocketPath resolves the socket path. When path is empty, the default
// path in a private per-user directory is used.
func SocketPath(path string) (string, error) {
	path, err := socketPath(path)
	if err != nil {
		return "", err
	}
	if len(path) > maxSocketPathLen {
		return "", fmt.Errorf("socket path %s is too long (max %d bytes)", path, maxSocketPathLen)
	}
	return path, nil
}

// maxSocketPathLen is the most portable limit of sun_path (macOS: 104
// bytes including the terminating NUL, Linux: 108).
const maxSocketPathLen = 103

func socketPath(path string) (string, error) {
	if path != "" {
		return filepath.Abs(path)
	}
	var dir string
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		dir = filepath.Join(d, "cmdc")
	} else {
		dir = filepath.Join(os.TempDir(), fmt.Sprintf("cmdc-%d", os.Getuid()))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// The directory may be in a world writable place such as /tmp.
	// Refuse to use it unless it is a private directory of ours.
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || !ok || int(st.Uid) != os.Getuid() || fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("socket directory %s is not a private directory", dir)
	}
	return filepath.Join(dir, "cmdc.sock"), nil
}
