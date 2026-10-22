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

All three settings can also be changed from the settings sidebar in the UI,
which saves them straight back to this file (comments and all) and applies
them at once.

### `~/.horizon/ports.txt`

Saved ports for forwarding — one per line, `name spec`, `#` for comments. Any
of them can be forwarded through any server. The spec takes the same shapes
as ssh's `-L`:

```
postgres  5432                      # same port here and on the server
grafana   13000:3000                # local 13000 → the server's localhost:3000
kafka     9092:broker.internal:9092 # local 9092 → broker.internal:9092, as seen from the server
https     *:443                     # local 443 on ALL interfaces → the server's localhost:443
```

A leading `*:` opens the local port on all network interfaces instead of
localhost only (ssh's `-L *:…` bind address). See the warning under
[Port forwarding](#port-forwarding) before using it.

They can be added, edited and deleted from the sidebar's **Ports** pane too.

### `~/.horizon/sockets/`

ControlMaster sockets for live connections, plus a `.forwards` record of
each connection's port forwards and a `.log` of ssh's messages for
connections opened to forward ports. Managed automatically.

## Using the UI

Everything is clickable with the mouse, and fully usable from the keyboard.
When a pane holds more than fits, a classic Mac scroll bar appears on its
right edge — click the arrows to scroll by one item, the track to scroll by
a page, or just use the mouse wheel and arrow keys.

| Key            | Action                                     |
| -------------- | ------------------------------------------ |
| ↑/↓, Enter     | choose a server and open its menu          |
| Enter on a folder | open / close the folder                 |
| `c` / `e` / `l` / `d` | connect / edit / clone / delete the selected server |
| `p`            | forward ports through the selected server  |
| `x`            | stop all of the selected server's forwards |
| `d` on a folder | delete the folder (keep or delete its servers) |
| `n`            | new server form                            |
| `f`            | new folder                                 |
| `r`            | refresh the list, statuses, config and ports |
| `s`            | show / hide the settings sidebar           |
| `q`            | quit (live connections keep running)       |
| Tab            | switch pane: server list → Settings → Ports |
| ↑/↓            | move within the pane or between a dialog's fields |
| Tab in a dialog | next field or button, round and round (Shift-Tab back); ←/→ between buttons |
| Esc            | close a dialog or menu                     |

Clicking a server, or pressing Enter on it, pops up a small menu under the
row: **Connect** (`c`), **Forward ports…** (`p`), **Edit…** (`e`), **Clone…** (`l`) and **Delete…** (`d`). The same keys
also work straight from the list on the highlighted server, with no menu
open. **Edit** opens
a form with every setting of the host — name, user, host, port and folder —
and saves it back to `list_of_servers.txt` in place, comments and all. Moving
a server to another folder moves its line into that folder's section.
**Clone** opens the new-server form already filled in with the selected
server's settings under a fresh name (`web1-copy`); edit whatever differs and
save it as a separate server, leaving the original untouched.
Renaming a server, or changing its address, and deleting it each stop the old
background connection from taking new sessions (sessions already open over it
keep running).

**New Folder** (`f`) adds an empty `[folder]` section to the end of
`list_of_servers.txt`; it shows up in the list straight away, ready for
servers to be created in it or moved into it with **Edit**.

When a server already has a live connection, its menu starts with **Reuse
connection** (`r`), which opens a new session over that connection instantly
(no re-authentication). **Connect** then closes the old connection and
reconnects fresh.

When you pick a server, Horizon's UI closes and hands your terminal directly to
`ssh` — typing `exit` on the remote host drops you straight back into the local
shell you started from. The underlying connection stays alive in the background
(`ControlPersist=yes`), so the next `./horizon` run offers instant re-use. It
lives until you choose **Connect** on it again or kill it yourself with
`ssh -O exit -o ControlPath=~/.horizon/sockets/<name>-<port> <target>`.

### Settings sidebar

The right side of the window holds Horizon's settings: **Ping at startup**,
**Ping count** and **Port check** (see `config.txt` above), and the **Ports**
pane with your saved ports — Enter edits one, `n` adds one, `d` deletes one.
Tab moves between the server list, Settings and Ports; inside Settings the
arrow keys move between its controls. The menu bar at the top is for the
mouse and as a reminder of the shortcut keys — Tab never stops there. `s` (or
**Settings** in the menu bar) hides and shows it, and it folds away by itself
on terminals narrower than 90 columns.

### Port forwarding

**Forward ports…** (`p`) on a server opens a checklist of your saved ports.
Tick the ones you want (Space or Enter), add one-off ports in the **Custom**
field (`8080, 13000:3000`, optionally saving them to the list), and press
**Apply**. A line under Custom shows what will be forwarded as you type.
Next to **Save to list**, an **All interfaces** box appears on macOS once
you type a port below 1024, already ticked; it opens only those low ports
to the network — higher ones stay on localhost (write `*:9000` to open one
anyway). Ports already forwarded through that server show up ticked and
marked `(active)`; untick one and Apply to stop it, or press **Stop all**.
The server's row shows what it is forwarding, e.g. `⇄ 5432,6379`, and its
menu then offers **Stop forwards** (`x`, also straight from the list),
which closes them all and keeps the connection.

Forwards ride on the server's background connection (`ssh -O forward`), so
they need no extra login. If the server isn't connected yet, Horizon steps
aside for a moment so you can log in as usual (password, 2FA, host key),
then comes straight back with the connection held open in the background.
Like connections, forwards keep running after you quit Horizon. They end
with the connection: a fresh **Connect**, or editing the server's name,
host or port, or deleting it, closes them — Horizon asks first when there
are any. A local port that is already taken is reported and skipped.

Local ports below 1024 (80, 443, …) can only be opened on localhost by
root, and the connection that opens them runs as you — running Horizon
with `sudo` doesn't help. They are refused up front, with two ways out:

- **Forward from a high local port** and keep the remote one: `8443:443`
  reaches the server's 443 at `localhost:8443`. This is the safe choice.
- **Tick "All interfaces"** on the saved port (or write it as `*:443`).
  On macOS the port form ticks it for you as soon as the local port is
  below 1024; untick it if you'd rather not, and it stays your choice.
  macOS lets a normal user open a low port on all interfaces, so
  `localhost:443` then works. **The port is then reachable from every
  network your machine is on** — office Wi-Fi, VPN, anything — and through
  it the server's service. Horizon warns in the port form, asks again every
  time such a port is forwarded, and flags it with `⚠ network` in the lists
  (`443⚠` on the server's row). On Linux even this needs root.

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
