# cmdc

cmdc is a command output cache.

`cmdc exec` runs a command and caches its output (stdout and stderr) in the memory of a resident process. While the cache is alive, running the same command again prints the cached output without running the command.

- The resident process (daemon) is started automatically at the first run. No setup is needed.
- The daemon does not listen on any TCP port. Clients talk to it over a unix domain socket.
- The output is cached only when the command succeeds (exit status 0).
- Nothing is written to the disk. The cached outputs are gone when the daemon exits.

## Install

```console
$ go install github.com/fujiwara/cmdc/cmd/cmdc@latest
```

Or download a binary from [Releases](https://github.com/fujiwara/cmdc/releases).

cmdc works on Linux and macOS.

## Usage

```
Usage: cmdc <command> [flags]

Caches the output of commands in the memory of a resident process.

Flags:
  -h, --help           Show context-sensitive help.
      --socket=PATH    Path to the unix domain socket of the daemon ($CMDC_SOCKET).
      --debug          Enable debug log ($CMDC_DEBUG).
      --version        Show version.

Commands:
  exec --expires=DURATION <command> ... [flags]
    Run a command, or print its cached output.

  status [flags]
    Show the status of the daemon.

  clear [flags]
    Remove all cached outputs.

  stop [flags]
    Stop the daemon.
```

### cmdc exec

```console
$ cmdc exec --expires 5m -- aws sts get-caller-identity
```

The first run executes the command, passes its output through, and caches the output for 5 minutes. Subsequent runs within 5 minutes print the cached output immediately.

```console
$ export CMDC_EXPIRES=10s
$ time cmdc exec -- sh -c 'sleep 3; date'
Wed Nov 11 14:30:03 JST 2026
real    0m3.021s

$ time cmdc exec -- sh -c 'sleep 3; date'
Wed Nov 11 14:30:03 JST 2026
real    0m0.004s
```

| Flag | Environment variable | Description |
|---|---|---|
| `--expires`, `-e` | `CMDC_EXPIRES` | How long the output is cached, e.g. `30s`, `5m`, `1h`. **Required.** |
| `--key-env` | `CMDC_KEY_ENV` | Names of environment variables whose values are part of the cache key. Repeatable, or comma separated. |
| `--max-size` | `CMDC_MAX_SIZE` | Maximum size in bytes of an output to cache. Default 10MiB. |

There is no default value for the expiration, because a suitable value depends on the command.

#### Cache key

The cache key consists of the command and its arguments.

The working directory and environment variables are **not** part of the key. If the output depends on environment variables, name them with `--key-env`.

```console
$ cmdc exec --expires 5m --key-env AWS_PROFILE --key-env AWS_REGION -- aws sts get-caller-identity
```

#### Expiration

An entry lives for the `--expires` given when it was stored. A run with a shorter `--expires` than the age of the entry treats it as stale, runs the command again, and replaces the entry.

#### Exit status

- When the cached output is used, cmdc exits with 0.
- Otherwise, cmdc exits with the exit status of the command. A failed command is not cached, so the next run executes it again.

#### Notes

- The command runs as a child process of `cmdc exec`, not of the daemon. It inherits the environment variables, the working directory, and stdin as usual.
- stdout and stderr of the command are pipes, not a terminal. Some commands change their output in that case (e.g. disable colors).
- stdout and stderr are replayed separately, in the order they were read. The interleaving of the two may be slightly different from the original.
- stdin is passed to the command, but is not part of the cache key and is not consumed on a cache hit.
- An output larger than `--max-size` is not cached.
- If the daemon is not available for any reason, the command runs without the cache.

### cmdc status

Shows the status of the daemon and the cached entries as JSON. It exits with 1 if the daemon is not running.

```console
$ cmdc status
{
  "running": true,
  "socket": "/run/user/1000/cmdc/cmdc.sock",
  "pid": 12345,
  "version": "v0.1.0",
  "uptime": "2m10s",
  "entries": 1,
  "total_size": 430,
  "items": [
    {
      "command": "aws sts get-caller-identity",
      "size": 430,
      "age": "1m3.21s",
      "expires_in": "3m56.79s"
    }
  ]
}
```

### cmdc clear

Removes all cached outputs. The daemon keeps running.

### cmdc stop

Stops the daemon. All cached outputs are removed.

## Daemon

The daemon is started by `cmdc exec` when it is not running, one per user (per socket).

- The socket is `$XDG_RUNTIME_DIR/cmdc/cmdc.sock`, or `$TMPDIR/cmdc-$UID/cmdc.sock` if `XDG_RUNTIME_DIR` is not set. It is accessible only by the owner. `--socket` or `CMDC_SOCKET` overrides the path.
- The daemon never executes commands. It only stores the outputs sent by `cmdc exec`.
- The daemon exits by itself when it has no cached outputs and no activity for 10 minutes.
- When `cmdc exec` finds a daemon of a different version, it replaces the daemon with its own version. The cached outputs are removed at that time.

The following environment variables are read when the daemon starts.

| Environment variable | Description |
|---|---|
| `CMDC_IDLE_TIMEOUT` | The daemon exits after this duration without cached outputs and activity. Default `10m`. |
| `CMDC_MAX_TOTAL_SIZE` | Maximum total size in bytes of cached outputs. When it is exceeded, the oldest entries are removed. Default 256MiB. |

## LICENSE

MIT

## Author

FUJIWARA Shunichiro
