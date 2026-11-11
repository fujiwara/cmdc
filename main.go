package cmdc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/alecthomas/kong"
)

// CLI defines the command line interface.
type CLI struct {
	Socket  string           `help:"Path to the unix domain socket of the daemon." env:"CMDC_SOCKET" placeholder:"PATH"`
	Debug   bool             `help:"Enable debug log." env:"CMDC_DEBUG"`
	Version kong.VersionFlag `help:"Show version."`

	Exec   ExecCmd   `cmd:"" help:"Run a command, or print its cached output."`
	Status struct{}  `cmd:"" help:"Show the status of the daemon."`
	Clear  struct{}  `cmd:"" help:"Remove all cached outputs."`
	Stop   struct{}  `cmd:"" help:"Stop the daemon."`
	Daemon DaemonCmd `cmd:"" hidden:"" help:"Run the daemon in the foreground."`
}

// ExecCmd is the options of the exec subcommand.
type ExecCmd struct {
	Expires time.Duration `help:"How long the output is cached (e.g. 30s, 5m, 1h)." short:"e" required:"" env:"CMDC_EXPIRES"`
	KeyEnv  []string      `help:"Names of environment variables whose values are part of the cache key." env:"CMDC_KEY_ENV" placeholder:"NAME"`
	MaxSize int64         `help:"Maximum size in bytes of an output to cache." env:"CMDC_MAX_SIZE" default:"10485760"`
	Command []string      `arg:"" passthrough:"" help:"Command and its arguments."`
}

// DaemonCmd is the options of the daemon subcommand.
type DaemonCmd struct {
	IdleTimeout  time.Duration `help:"Exit after this duration without cached outputs and connections." env:"CMDC_IDLE_TIMEOUT" default:"10m"`
	MaxTotalSize int64         `help:"Maximum total size in bytes of cached outputs." env:"CMDC_MAX_TOTAL_SIZE" default:"268435456"`
}

// Run runs cmdc with the command line arguments of the process.
func Run(ctx context.Context) error {
	var cli CLI
	kctx := kong.Parse(&cli,
		kong.Name("cmdc"),
		kong.Description("Caches the output of commands in the memory of a resident process."),
		kong.Vars{"version": Version},
	)
	if cli.Debug {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}
	switch cmd, _, _ := strings.Cut(kctx.Command(), " "); cmd {
	case "exec":
		return cli.runExec(ctx)
	case "status":
		return cli.runStatus(ctx)
	case "clear":
		return cli.runClear(ctx)
	case "stop":
		return cli.runStop(ctx)
	case "daemon":
		return cli.runDaemon(ctx)
	default:
		return fmt.Errorf("unknown command: %s", cmd)
	}
}

func (cli *CLI) runExec(ctx context.Context) error {
	opt := cli.Exec
	argv := opt.Command
	if len(argv) > 0 && argv[0] == "--" {
		argv = argv[1:]
	}
	if len(argv) == 0 {
		return errors.New("command is required")
	}
	if opt.Expires <= 0 {
		return errors.New("--expires must be positive")
	}
	key := CacheKey(argv, opt.KeyEnv)

	// Any trouble with the daemon must not prevent the command from
	// running. In that case the command runs without the cache.
	client, err := cli.dialOrStart(ctx)
	if err != nil {
		slog.Warn("running without cache", "error", err)
	} else {
		defer client.Close()
		chunks, hit, err := client.Get(key, opt.Expires)
		if err != nil {
			slog.Warn("running without cache", "error", err)
			client.Close()
			client = nil
		} else if hit {
			slog.Debug("cache hit", "key", key)
			return replay(chunks, os.Stdout, os.Stderr)
		}
	}

	chunks, err := runCommand(argv, os.Stdin, os.Stdout, os.Stderr, opt.MaxSize)
	if err != nil {
		return err
	}
	if client == nil {
		return nil
	}
	if chunks == nil {
		slog.Debug("output is too large to cache", "max_size", opt.MaxSize)
		return nil
	}
	if stored, err := client.Set(key, strings.Join(argv, " "), chunks, opt.Expires); err != nil {
		slog.Warn("failed to store the output", "error", err)
	} else {
		slog.Debug("cache set", "key", key, "stored", stored)
	}
	return nil
}

func (cli *CLI) dialOrStart(ctx context.Context) (*Client, error) {
	socket, err := SocketPath(cli.Socket)
	if err != nil {
		return nil, err
	}
	return DialOrStart(ctx, socket)
}

// dial connects to the daemon without starting it. It returns nil client
// if the daemon is not running.
func (cli *CLI) dial(ctx context.Context) (*Client, string, error) {
	socket, err := SocketPath(cli.Socket)
	if err != nil {
		return nil, "", err
	}
	client, err := Dial(ctx, socket)
	if err != nil {
		slog.Debug("daemon is not running", "error", err)
		return nil, socket, nil
	}
	return client, socket, nil
}

func (cli *CLI) runStatus(ctx context.Context) error {
	client, socket, err := cli.dial(ctx)
	if err != nil {
		return err
	}
	status := &Status{Socket: socket}
	if client != nil {
		defer client.Close()
		if status, err = client.Status(); err != nil {
			return err
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(status); err != nil {
		return err
	}
	if !status.Running {
		return &ExitError{Code: 1}
	}
	return nil
}

func (cli *CLI) runClear(ctx context.Context) error {
	client, _, err := cli.dial(ctx)
	if err != nil || client == nil {
		return err
	}
	defer client.Close()
	n, err := client.Clear()
	if err != nil {
		return err
	}
	slog.Debug("cache cleared", "entries", n)
	return nil
}

func (cli *CLI) runStop(ctx context.Context) error {
	client, _, err := cli.dial(ctx)
	if err != nil || client == nil {
		return err
	}
	defer client.Close()
	return client.Stop()
}

func (cli *CLI) runDaemon(ctx context.Context) error {
	socket, err := SocketPath(cli.Socket)
	if err != nil {
		return err
	}
	s := &Server{
		Socket:       socket,
		IdleTimeout:  cli.Daemon.IdleTimeout,
		MaxTotalSize: cli.Daemon.MaxTotalSize,
	}
	if err := s.Run(ctx); err != nil && !errors.Is(err, ErrAlreadyRunning) {
		return err
	}
	return nil
}
