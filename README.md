# Horizon

[![CI](https://github.com/davvi/horizon/actions/workflows/ci.yml/badge.svg)](https://github.com/davvi/horizon/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/davvi/horizon)](https://github.com/davvi/horizon/releases/latest)
[![Go Reference](https://pkg.go.dev/badge/github.com/davvi/horizon.svg)](https://pkg.go.dev/github.com/davvi/horizon)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

A small terminal UI for managing SSH connections on macOS and Linux. Keep your
servers in folders, pick one, and Horizon connects you. Connections are kept
alive in the background and reused instantly.

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
the first group stay at the top level. Folders start closed; servers with a
live reusable connection are listed at the very top, outside their folder.

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
port_check=off
```

With `ping=on`, Horizon pings every server once at startup (`ping_count`
pings each, concurrently) and shows the average round-trip time on the
server's line — or `not reachable` if the host doesn't answer. It is off by
default so starting the UI never waits on the network.

With `port_check=on`, a host that fails the ping gets a second chance: Horizon
opens a TCP connection to its ssh port (the one from `list_of_servers.txt`, or
22) with a 1 second timeout. Plenty of servers drop ICMP but accept ssh
perfectly well, and those show `no ping — port 22 open` instead of being
written off. Only when both checks fail is the line `not reachable`. The probe
just completes the handshake and hangs up — nothing is sent, so no ssh banner
exchange or login attempt is logged on the far side. This is also off by
default, and works on its own with `ping=off` (the line then reads
`port 22 open`).

### `~/.horizon/sockets/`

ControlMaster sockets for live connections. Managed automatically.

## Using the UI

Everything is clickable with the mouse, and fully usable from the keyboard.
When a pane holds more than fits, a classic Mac scroll bar appears on its
right edge — click the arrows to scroll by one item, the track to scroll by
a page, or just use the mouse wheel and arrow keys.

| Key            | Action                                     |
| -------------- | ------------------------------------------ |
| ↑/↓, Enter     | choose a server and open its menu          |
| Enter on a folder | open / close the folder                 |
| `n`            | new server form                            |
| `f`            | new folder                                 |
| `r`            | refresh the list and connection statuses   |
| `q`            | quit (live connections keep running)       |
| Tab / Esc      | move focus / close a dialog or menu        |

Clicking a server, or pressing Enter on it, pops up a small menu under the
row: **Connect** (`c`), **Edit…** (`e`) and **Delete…** (`d`). **Edit** opens
a form with every setting of the host — name, user, host, port and folder —
and saves it back to `list_of_servers.txt` in place, comments and all. Moving
a server to another folder moves its line into that folder's section.
Renaming a server, or changing its address, and deleting it each stop the old
background connection from taking new sessions (sessions already open over it
keep running).

**New Folder** (`f`) adds an empty `[folder]` section to the end of
`list_of_servers.txt`; it shows up in the list straight away, ready for
servers to be created in it or moved into it with **Edit**.

Connecting to a server that already has a live connection pops up a choice:
**Reuse** opens a new session over the existing connection instantly (no
re-authentication); **New connection** closes the old one and reconnects
fresh.

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
