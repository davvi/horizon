# Horizon

[![CI](https://github.com/davvi/horizon/actions/workflows/ci.yml/badge.svg)](https://github.com/davvi/horizon/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/davvi/horizon)](https://github.com/davvi/horizon/releases/latest)
[![Go Reference](https://pkg.go.dev/badge/github.com/davvi/horizon.svg)](https://pkg.go.dev/github.com/davvi/horizon)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

A small terminal UI for managing SSH connections on macOS and Linux. Pick a
server, pick an environment file, and Horizon connects you with your variables
exported and your setup commands already run. Connections are kept alive in the
background and reused instantly.

Horizon is deliberately tiny: the UI is [tview](https://github.com/rivo/tview),
and all SSH work is done by your system's own OpenSSH client using
**ControlMaster/ControlPersist** multiplexing. That means your keys, ssh-agent,
`~/.ssh/config`, passwords and 2FA all work exactly as they do with plain `ssh`,
and live connections survive between sessions with no daemon.

## Install

### Pre-built binary (macOS and Linux)

Download the binary for your platform from the
[latest release](https://github.com/davvi/horizon/releases/latest) and put it
on your `PATH`:

```sh
curl -sL "https://github.com/davvi/horizon/releases/latest/download/horizon_$(uname -s)_$(uname -m).tar.gz" | tar xz horizon
sudo install -m 0755 horizon /usr/local/bin/horizon
```

> On macOS `uname -m` prints `arm64` on Apple Silicon and `x86_64` on Intel;
> both are published, so the one-liner above picks the right one automatically.

### With `go install`

Requires Go 1.25 or newer:

```sh
go install github.com/davvi/horizon@latest
```

The binary lands in `$(go env GOPATH)/bin` (usually `~/go/bin`) — make sure
that directory is on your `PATH`.

### Build from source

```sh
git clone https://github.com/davvi/horizon.git
cd horizon
make build            # builds ./horizon
sudo make install     # installs to /usr/local/bin (PREFIX=... to change)
```

Or with plain Go:

```sh
go build -o horizon .
```

## Run

```sh
./horizon              # uses ~/.horizon
./horizon -f /some/dir # use a different config folder
```

The config folder (and template files) are created automatically on first run
with `0700`/`0600` permissions.

## Files (all plain text)

### `~/.horizon/list_of_servers.txt`

One server per line — `name user@host[:port]`, `#` for comments. A `[group]`
line starts a folder: servers below it belong to that group until the next
`[group]` line, and show up in the UI as a collapsible folder. Servers above
the first group stay at the top level.

```
jump  ops@jump.example.com
[production]
web1  deploy@203.0.113.10
db    admin@db.internal:2222
```

### `~/.horizon/config.txt`

Horizon's own settings, `KEY=value` lines with `#` comments. Created with
defaults on first run:

```
ping=off
ping_count=3
```

With `ping=on`, Horizon pings every server once at startup (`ping_count`
pings each, concurrently) and shows the average round-trip time on the
server's line — or `not reachable` if the host doesn't answer. It is off by
default so starting the UI never waits on the network.

### `~/.horizon/*.txt` — environment files

Every other `.txt` file in the folder (besides `list_of_servers.txt` and
`config.txt`) is an environment file. Lines like
`KEY=value` are exported on the server after connecting; every other
non-comment line runs as a command, in order. You then land in an interactive
login shell.

```
APP_ENV=staging
DB_URL=postgres://app@db/main
cd /srv/app
```

### `~/.horizon/sockets/`

ControlMaster sockets for live connections. Managed automatically.

## Using the UI

Everything is clickable with the mouse, and fully usable from the keyboard.
When a pane holds more than fits, a classic Mac scroll bar appears on its
right edge — click the arrows to scroll by one item, the track to scroll by
a page, or just use the mouse wheel and arrow keys.

| Key            | Action                                     |
| -------------- | ------------------------------------------ |
| ↑/↓, Enter     | choose a server and connect                |
| Enter on a folder | open / close the group                  |
| `n`            | new server form                            |
| `e`            | new environment file form                  |
| `r`            | refresh the list and connection statuses   |
| `q`            | quit (live connections keep running)       |
| `d`            | duplicate the selected env file            |
| Tab / Esc      | move focus / close a dialog                |

With the Env Files pane focused, a **Duplicate (d)** button appears in the
top bar; it, the `d` key, or the **Duplicate** option after clicking a file
all open the new-file form prefilled with the selected file's content and a
suggested `<name>_copy` name — edit and save it as a new file. Saving never
overwrites an existing file — pick a new name or use **Edit**.

Connecting to a server that already has a live connection pops up a choice:
**Reuse** opens a new session over the existing connection instantly (no
re-authentication); **New connection** closes the old one, lets you pick an
environment file, and reconnects fresh.

When you pick a server, Horizon's UI closes and hands your terminal directly to
`ssh` — typing `exit` on the remote host drops you straight back into the local
shell you started from. The underlying connection stays alive in the background
(`ControlPersist=yes`), so the next `./horizon` run offers instant re-use. It
lives until you choose "New connection" or kill it yourself with
`ssh -O exit -o ControlPath=~/.horizon/sockets/<name>-<port> <target>`.

## Development

```sh
make test    # go test -race ./...
make vet     # go vet ./...
make fmt     # gofmt -w .
```

Releases are cut by pushing a tag: `git tag v0.1.0 && git push origin v0.1.0`.
GitHub Actions then builds macOS and Linux binaries (amd64/arm64) with
[GoReleaser](https://goreleaser.com) and attaches them to the GitHub release.

## Contributing

Issues and pull requests are welcome. Please run `make fmt test vet` before
submitting.

## License

[MIT](LICENSE)
