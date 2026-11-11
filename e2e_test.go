package cmdc_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fujiwara/cmdc"
)

var cmdcBin string

func TestMain(m *testing.M) {
	os.Exit(func() int {
		dir, err := os.MkdirTemp("", "cmdc-bin")
		if err != nil {
			panic(err)
		}
		defer os.RemoveAll(dir)
		cmdcBin = filepath.Join(dir, "cmdc")
		if out, err := exec.Command("go", "build", "-o", cmdcBin, "./cmd/cmdc").CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to build: %v\n%s", err, out)
			return 1
		}
		return m.Run()
	}())
}

type e2e struct {
	t       *testing.T
	env     []string
	counter string
}

func newE2E(t *testing.T) *e2e {
	t.Helper()
	socket := tempSocket(t)
	e := &e2e{
		t:       t,
		counter: filepath.Join(filepath.Dir(socket), "counter"),
	}
	e.env = append(os.Environ(), "CMDC_SOCKET="+socket, "COUNTER="+e.counter)
	t.Cleanup(func() { e.run("stop") })
	return e
}

type result struct {
	stdout, stderr string
	code           int
}

func (e *e2e) run(args ...string) result {
	e.t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(cmdcBin, args...)
	cmd.Env = e.env
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	r := result{stdout: stdout.String(), stderr: stderr.String()}
	if err != nil {
		ee, ok := errors.AsType[*exec.ExitError](err)
		if !ok {
			e.t.Fatalf("failed to run cmdc %v: %v", args, err)
		}
		r.code = ee.ExitCode()
	}
	return r
}

// runs returns how many times the test script was really executed.
func (e *e2e) runs() int {
	e.t.Helper()
	b, err := os.ReadFile(e.counter)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		e.t.Fatal(err)
	}
	return strings.Count(string(b), "\n")
}

func (e *e2e) status() cmdc.Status {
	e.t.Helper()
	var st cmdc.Status
	if err := json.Unmarshal([]byte(e.run("status").stdout), &st); err != nil {
		e.t.Fatal(err)
	}
	return st
}

const script = `echo run >> "$COUNTER"; echo out; echo err >&2`

func TestE2ECache(t *testing.T) {
	e := newE2E(t)
	if st := e.status(); st.Running {
		t.Fatal("daemon must not be started by status")
	}
	for i := range 3 {
		r := e.run("exec", "--expires", "1m", "--", "sh", "-c", script)
		if r.code != 0 || r.stdout != "out\n" || r.stderr != "err\n" {
			t.Fatalf("unexpected result #%d: %+v", i, r)
		}
	}
	if n := e.runs(); n != 1 {
		t.Errorf("command must run once: %d", n)
	}
	// a different command line is a different entry
	if r := e.run("exec", "--expires", "1m", "--", "sh", "-c", script+";"); r.code != 0 {
		t.Fatalf("unexpected result: %+v", r)
	}
	if n := e.runs(); n != 2 {
		t.Errorf("unexpected runs: %d", n)
	}
	st := e.status()
	if !st.Running || st.Entries != 2 {
		t.Errorf("unexpected status: %+v", st)
	}

	if r := e.run("clear"); r.code != 0 {
		t.Fatalf("unexpected result: %+v", r)
	}
	e.run("exec", "--expires", "1m", "--", "sh", "-c", script)
	if n := e.runs(); n != 3 {
		t.Errorf("command must run again after clear: %d", n)
	}

	if r := e.run("stop"); r.code != 0 {
		t.Fatalf("unexpected result: %+v", r)
	}
	if st := e.status(); st.Running {
		t.Error("daemon must be stopped")
	}
	// the daemon is started again, with an empty cache
	e.run("exec", "--expires", "1m", "--", "sh", "-c", script)
	if n := e.runs(); n != 4 {
		t.Errorf("command must run again after stop: %d", n)
	}
	if st := e.status(); !st.Running || st.Entries != 1 {
		t.Errorf("unexpected status: %+v", st)
	}
}

func TestE2EExpires(t *testing.T) {
	e := newE2E(t)
	e.env = append(e.env, "CMDC_EXPIRES=500ms")
	e.run("exec", "--", "sh", "-c", script)
	e.run("exec", "--", "sh", "-c", script)
	if n := e.runs(); n != 1 {
		t.Errorf("command must run once: %d", n)
	}
	time.Sleep(600 * time.Millisecond)
	if r := e.run("exec", "--", "sh", "-c", script); r.stdout != "out\n" {
		t.Errorf("unexpected result: %+v", r)
	}
	if n := e.runs(); n != 2 {
		t.Errorf("command must run again after expiration: %d", n)
	}
}

func TestE2EExpiresRequired(t *testing.T) {
	e := newE2E(t)
	if r := e.run("exec", "--", "sh", "-c", script); r.code == 0 || !strings.Contains(r.stderr, "--expires") {
		t.Errorf("unexpected result: %+v", r)
	}
	if r := e.run("exec", "--expires", "0s", "--", "sh", "-c", script); r.code == 0 {
		t.Errorf("unexpected result: %+v", r)
	}
	if n := e.runs(); n != 0 {
		t.Errorf("command must not run: %d", n)
	}
}

func TestE2EFailure(t *testing.T) {
	e := newE2E(t)
	for i := range 2 {
		r := e.run("exec", "--expires", "1m", "--", "sh", "-c", script+"; exit 3")
		if r.code != 3 || r.stdout != "out\n" || r.stderr != "err\n" {
			t.Fatalf("unexpected result #%d: %+v", i, r)
		}
	}
	if n := e.runs(); n != 2 {
		t.Errorf("failed command must not be cached: %d", n)
	}
	if st := e.status(); st.Entries != 0 {
		t.Errorf("unexpected status: %+v", st)
	}
	if r := e.run("exec", "--expires", "1m", "--", "sh", "-c", "kill -TERM $$"); r.code != 128+15 {
		t.Errorf("unexpected result: %+v", r)
	}
	if r := e.run("exec", "--expires", "1m", "--", "cmdc-no-such-command"); r.code != 127 {
		t.Errorf("unexpected result: %+v", r)
	}
}

func TestE2EKeyEnv(t *testing.T) {
	e := newE2E(t)
	base := e.env
	run := func(v string, args ...string) {
		t.Helper()
		e.env = append(base[:len(base):len(base)], "CMDC_TEST_VAR="+v)
		r := e.run(append(append([]string{"exec", "--expires", "1m"}, args...), "--", "sh", "-c", script+`; echo "$CMDC_TEST_VAR"`)...)
		if r.code != 0 {
			t.Fatalf("unexpected result: %+v", r)
		}
	}
	// environment variables are not part of the key by default
	run("a")
	run("b")
	if n := e.runs(); n != 1 {
		t.Errorf("unexpected runs: %d", n)
	}
	run("a", "--key-env", "CMDC_TEST_VAR")
	run("b", "--key-env", "CMDC_TEST_VAR")
	run("a", "--key-env", "CMDC_TEST_VAR")
	if n := e.runs(); n != 3 {
		t.Errorf("unexpected runs: %d", n)
	}
}

func TestE2EMaxSize(t *testing.T) {
	e := newE2E(t)
	for range 2 {
		r := e.run("exec", "--expires", "1m", "--max-size", "5", "--", "sh", "-c", script)
		if r.code != 0 || r.stdout != "out\n" || r.stderr != "err\n" {
			t.Fatalf("unexpected result: %+v", r)
		}
	}
	if n := e.runs(); n != 2 {
		t.Errorf("too large output must not be cached: %d", n)
	}
}

func TestE2EStdin(t *testing.T) {
	e := newE2E(t)
	cmd := exec.Command(cmdcBin, "exec", "--expires", "1m", "--", "tr", "a-z", "A-Z")
	cmd.Env = e.env
	cmd.Stdin = strings.NewReader("hello\n")
	out, err := cmd.Output()
	if err != nil || string(out) != "HELLO\n" {
		t.Errorf("unexpected result: %q %v", out, err)
	}
}

func TestE2EConcurrentStart(t *testing.T) {
	e := newE2E(t)
	var wg sync.WaitGroup
	results := make([]result, 10)
	for i := range results {
		wg.Go(func() {
			results[i] = e.run("exec", "--expires", "1m", "--", "sh", "-c", script)
		})
	}
	wg.Wait()
	for i, r := range results {
		if r.code != 0 || r.stdout != "out\n" || r.stderr != "err\n" {
			t.Errorf("unexpected result #%d: %+v", i, r)
		}
	}
	// all of them must have reached the same daemon
	if st := e.status(); !st.Running || st.Entries != 1 {
		t.Errorf("unexpected status: %+v", st)
	}
	before := e.runs()
	e.run("exec", "--expires", "1m", "--", "sh", "-c", script)
	if n := e.runs(); n != before {
		t.Errorf("unexpected runs: %d -> %d", before, n)
	}
}

func TestE2EWithoutDaemon(t *testing.T) {
	e := newE2E(t)
	// the daemon can not listen on a socket in a missing, uncreatable directory
	for i, kv := range e.env {
		if strings.HasPrefix(kv, "CMDC_SOCKET=") {
			e.env[i] = "CMDC_SOCKET=/dev/null/cmdc/t.sock"
		}
	}
	r := e.run("exec", "--expires", "1m", "--", "sh", "-c", script)
	if r.code != 0 || r.stdout != "out\n" || !strings.Contains(r.stderr, "err\n") || !strings.Contains(r.stderr, "without cache") {
		t.Errorf("command must run without the daemon: %+v", r)
	}
	if n := e.runs(); n != 1 {
		t.Errorf("unexpected runs: %d", n)
	}
}
