// Horizon — a small SSH connection manager TUI for macOS and Linux.
//
// All data lives as plain text files in ~/.horizon (override with -f <folder>).
// Connections are held and reused via OpenSSH ControlMaster/ControlPersist,
// so authentication, keys, agents and 2FA behave exactly like plain `ssh`.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// version is stamped by the release build (goreleaser) via -ldflags.
var version = "dev"

const (
	serversFileName = "list_of_servers.txt"
	configFileName  = "config.txt"
)

// memberIndent is how far a server sitting inside an open folder is pushed in.
const memberIndent = "  "

const serversTemplate = `# Horizon servers — one per line:
#   name  user@host[:port]
# A [group] line starts a folder; servers below it belong to that group
# until the next [group] line. Servers above the first group stay at the
# top level.
# Example:
#   jump  ops@jump.example.com
#   [production]
#   web1  deploy@203.0.113.10
#   db    admin@db.internal:2222
`

const configTemplate = `# Horizon config file — KEY=value lines, # for comments.
#
#   ping        on/off — ping every server at startup and show the
#               average round-trip time on its line (default off)
#   ping_count  how many pings to send per server (default 3)
#   port_check  on/off — when the ping gets no answer, try to open a TCP
#               connection to the server's ssh port (1s timeout) so hosts
#               that only block ICMP still show up as reachable
#               (default off)
ping=off
ping_count=3
port_check=off
`

type Server struct{ Name, Target, Port, Group string }

// Config holds settings read from config.txt in the base folder.
type Config struct {
	Ping      bool // ping servers at startup and show the average RTT
	PingCount int  // pings sent per server
	PortCheck bool // fall back to a TCP probe of the ssh port when ping fails
}

// serverRow is one visible line of the server list: a collapsible group
// header (Header set) or a server.
type serverRow struct {
	header string
	server Server
}

// modalEntry is one open dialog; the stack makes dialogs truly modal.
type modalEntry struct {
	name string
	prim tview.Primitive
	drop *tview.DropDown // a picker in the dialog that may hang outside it
	menu bool            // a pop-up menu: a click outside dismisses it
}

// pendingConn is an ssh session to start after the TUI has fully shut down,
// so that when ssh exits the user lands back in their original local shell.
type pendingConn struct {
	name     string
	target   string
	args     []string
	killSock string // when non-empty, tear down this stale master first
}

var (
	baseDir    string
	app        *tview.Application
	pages      *tview.Pages
	serverList *macScrollList
	servers    []Server
	groups     []string // folder names, in file order
	serverRows []serverRow
	collapsed  = map[string]bool{} // group name -> folded shut
	config     Config
	aliveMu    sync.Mutex
	aliveState = map[string]bool{} // server name -> has a live reusable master
	probeMu    sync.Mutex
	probeText  = map[string]string{} // server name -> RTT / port state / "not reachable"
	probeGen   int                   // bumped by startProbes; stale probe goroutines see a mismatch and drop their result
	pending    *pendingConn
	modals     []modalEntry
	focusRing  []tview.Primitive
)

func main() {
	flag.StringVar(&baseDir, "f", "",
		"folder holding Horizon's txt files (default ~/.horizon)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("horizon", version)
		return
	}

	if baseDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, "horizon: cannot determine home directory:", err)
			os.Exit(1)
		}
		baseDir = filepath.Join(home, ".horizon")
	}

	if err := ensureFiles(); err != nil {
		fmt.Fprintln(os.Stderr, "horizon:", err)
		os.Exit(1)
	}
	loadConfig()

	// Classic Mac OS "Platinum" look: silver-grey windows, black text and
	// borders, white fields, black-on-white inverted highlights.
	tview.Styles = tview.Theme{
		PrimitiveBackgroundColor:    tcell.ColorSilver,
		ContrastBackgroundColor:     tcell.ColorWhite,
		MoreContrastBackgroundColor: tcell.ColorBlack,
		BorderColor:                 tcell.ColorBlack,
		TitleColor:                  tcell.ColorBlack,
		GraphicsColor:               tcell.ColorBlack,
		PrimaryTextColor:            tcell.ColorBlack,
		SecondaryTextColor:          tcell.ColorDimGray,
		TertiaryTextColor:           tcell.ColorGray,
		InverseTextColor:            tcell.ColorBlack,
		ContrastSecondaryTextColor:  tcell.ColorDimGray,
	}
	// Plain single-line window chrome, like classic Mac — tview's default
	// double-line "focused" borders would break the look.
	tview.Borders.HorizontalFocus = tview.Borders.Horizontal
	tview.Borders.VerticalFocus = tview.Borders.Vertical
	tview.Borders.TopLeftFocus = tview.Borders.TopLeft
	tview.Borders.TopRightFocus = tview.Borders.TopRight
	tview.Borders.BottomLeftFocus = tview.Borders.BottomLeft
	tview.Borders.BottomRightFocus = tview.Borders.BottomRight

	app = tview.NewApplication().EnableMouse(true)
	pages = tview.NewPages()
	pages.AddPage("main", buildMain(), true, true)
	refreshData()

	// Nothing that touches the network runs before the first frame: the list is
	// drawn from the text files alone, and the scans (ssh master checks, and the
	// optional reachability probes) start once the UI is on screen and fill
	// their columns in as answers arrive.
	var scans sync.Once
	app.SetAfterDrawFunc(func(tcell.Screen) {
		scans.Do(func() {
			list := servers
			go func() {
				app.QueueUpdateDraw(startProbes)
				refreshAlive(list)
			}()
		})
	})

	if err := app.SetRoot(pages, true).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "horizon:", err)
		os.Exit(1)
	}

	// The TUI is torn down and the terminal restored; run the chosen ssh
	// session (if any) directly, then fall back to the user's local shell.
	if pending != nil {
		if pending.killSock != "" {
			// Bounded like alive(): a wedged master accepts the socket
			// connection and never replies, and ssh would wait forever.
			ctx, cancel := context.WithTimeout(context.Background(), aliveTimeout)
			exec.CommandContext(ctx, "ssh",
				"-o", "ControlPath="+pending.killSock, "-O", "exit", "--", pending.target).Run()
			cancel()
		}
		fmt.Printf("horizon: connecting to %s (%s)...\n", pending.name, pending.target)
		cmd := exec.Command("ssh", pending.args...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				os.Exit(ee.ExitCode())
			}
			fmt.Fprintln(os.Stderr, "horizon:", err)
			os.Exit(1)
		}
	}
}

// ---------- data files ----------

func ensureFiles() error {
	if err := os.MkdirAll(filepath.Join(baseDir, "sockets"), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(baseDir, 0o700); err != nil {
		return err
	}
	templates := map[string]string{
		serversFileName: serversTemplate,
		configFileName:  configTemplate,
	}
	for name, tmpl := range templates {
		p := filepath.Join(baseDir, name)
		if _, err := os.Stat(p); os.IsNotExist(err) {
			if err := os.WriteFile(p, []byte(tmpl), 0o600); err != nil {
				return err
			}
		}
	}
	return nil
}

// loadServers reads the servers file and returns its servers and its folders.
// Folders are every [group] header, in order of first appearance, so a folder
// with no servers in it yet still shows up.
func loadServers() ([]Server, []string) {
	data, err := os.ReadFile(filepath.Join(baseDir, serversFileName))
	if err != nil {
		return nil, nil
	}
	var out []Server
	var groups []string
	group := ""
	seen := map[string]bool{}
	seenGroup := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if g, ok := groupHeader(line); ok {
			group = g
			if g != "" && !seenGroup[g] {
				seenGroup[g] = true
				groups = append(groups, g)
			}
			continue
		}
		s, ok := parseServerLine(line)
		if !ok || seen[s.Name] {
			continue
		}
		s.Group = group
		seen[s.Name] = true
		out = append(out, s)
	}
	return out, groups
}

// parseServerLine parses one trimmed "name user@host[:port]" line. The name
// doubles as the socket file name and the key of every per-server map, and
// target/port become ssh arguments; entries a hand-edit made unsafe are
// rejected rather than let through.
func parseServerLine(line string) (Server, bool) {
	f := strings.Fields(line)
	if len(f) < 2 {
		return Server{}, false
	}
	s := Server{Name: f[0], Target: f[1], Port: "22"}
	if host, port, ok := strings.Cut(s.Target, ":"); ok {
		s.Target, s.Port = host, port
	}
	if !validName(s.Name) || strings.HasPrefix(s.Target, "-") || strings.HasPrefix(s.Port, "-") {
		return Server{}, false
	}
	return s, true
}

// validName reports whether a server name is safe to use as a map key and as
// the socket file's name: no path tricks (it must be its own base name), no
// leading "-" (would read as an ssh option) or "[" (would read as a group
// header when written back), no whitespace.
func validName(name string) bool {
	switch name {
	case "", ".", "..":
		return false
	}
	if strings.HasPrefix(name, "-") || strings.HasPrefix(name, "[") {
		return false
	}
	if strings.ContainsAny(name, " \t") {
		return false
	}
	return name == filepath.Base(name)
}

// validPort reports whether p is a usable TCP port number.
func validPort(p string) bool {
	n, err := strconv.Atoi(p)
	return err == nil && n >= 1 && n <= 65535
}

// parseConfig reads KEY=value lines; unknown keys and malformed lines are
// ignored so a hand-edited file can never prevent startup.
func parseConfig(data string) Config {
	c := Config{PingCount: 3}
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "ping":
			c.Ping = v == "on" || v == "true" || v == "yes" || v == "1"
		case "ping_count":
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				c.PingCount = n
			}
		case "port_check":
			c.PortCheck = v == "on" || v == "true" || v == "yes" || v == "1"
		}
	}
	return c
}

func loadConfig() {
	data, _ := os.ReadFile(filepath.Join(baseDir, configFileName))
	config = parseConfig(string(data))
}

// groupHeader reports whether a trimmed line is a [group] section header
// and returns the group name.
func groupHeader(line string) (string, bool) {
	if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
		return strings.TrimSpace(line[1 : len(line)-1]), true
	}
	return "", false
}

// insertServerLine returns the servers file content with entry added to the
// given group's section, creating the section if needed. An empty group puts
// the entry at the top level, i.e. before the first section header.
func insertServerLine(data, entry, group string) string {
	lines := strings.Split(data, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	insertAt := len(lines)
	if group == "" {
		for i, l := range lines {
			if _, ok := groupHeader(strings.TrimSpace(l)); ok {
				insertAt = i
				break
			}
		}
	} else {
		found := -1
		for i, l := range lines {
			if g, ok := groupHeader(strings.TrimSpace(l)); ok && g == group {
				found = i
				break
			}
		}
		if found == -1 {
			lines = append(lines, "["+group+"]")
			insertAt = len(lines)
		} else {
			insertAt = len(lines)
			for i := found + 1; i < len(lines); i++ {
				if _, ok := groupHeader(strings.TrimSpace(lines[i])); ok {
					insertAt = i
					break
				}
			}
		}
	}
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:insertAt]...)
	out = append(out, entry)
	out = append(out, lines[insertAt:]...)
	return strings.Join(out, "\n") + "\n"
}

// replaceServerLine returns the servers file content with the line for the
// named server swapped for entry, or removed when entry is empty. Only the
// line loadServers would use counts — the first valid one with that name — and
// every other line, comments included, is left exactly as it was.
func replaceServerLine(data, name, entry string) string {
	lines := strings.Split(data, "\n")
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if _, ok := groupHeader(t); ok {
			continue
		}
		if s, ok := parseServerLine(t); !ok || s.Name != name {
			continue
		}
		if entry == "" {
			return strings.Join(append(lines[:i:i], lines[i+1:]...), "\n")
		}
		lines[i] = entry
		break
	}
	return strings.Join(lines, "\n")
}

// appendGroup returns the servers file content with an empty [group] section
// added at the end.
func appendGroup(data, group string) string {
	data = strings.TrimRight(data, " \t\n")
	if data != "" {
		data += "\n"
	}
	return data + "[" + group + "]\n"
}

// validGroup reports whether a folder name survives a round trip through a
// [group] header line.
func validGroup(g string) bool {
	return g != "" && g == strings.TrimSpace(g) && !strings.ContainsAny(g, "[]#\n")
}

// ---------- ssh via system OpenSSH (ControlMaster keeps connections alive) ----------

func socketPath(s Server) string {
	return filepath.Join(baseDir, "sockets", s.Name+"-"+s.Port)
}

// aliveTimeout caps a single master check. The check only talks to a local
// unix socket and normally answers in a few milliseconds; a master that is
// wedged on a dead network accepts the connection and then never replies, and
// ssh itself would wait forever.
const aliveTimeout = 1500 * time.Millisecond

func alive(s Server) bool {
	ctx, cancel := context.WithTimeout(context.Background(), aliveTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "ssh",
		"-o", "ControlPath="+socketPath(s), "-O", "check", "--", s.Target).Run() == nil
}

// refreshAlive checks every server in the background and returns immediately.
// Each result lands on its own, so one wedged master delays only its own line
// and never the other servers. The list renders from aliveState, so neither the
// first paint nor a folder toggle waits on ssh. Takes the server slice as an
// argument because the checks run off the UI goroutine, which owns the global.
func refreshAlive(list []Server) {
	aliveMu.Lock()
	current := make(map[string]bool, len(list))
	for _, s := range list {
		current[s.Name] = true
	}
	for name := range aliveState {
		if !current[name] { // server dropped from the file
			delete(aliveState, name)
		}
	}
	aliveMu.Unlock()

	for _, s := range list {
		s := s
		go func() {
			ok := alive(s)
			aliveMu.Lock()
			changed := aliveState[s.Name] != ok
			aliveState[s.Name] = ok
			aliveMu.Unlock()
			if changed {
				app.QueueUpdateDraw(rebuildServerList)
			}
		}()
	}
}

// stopMaster asks a server's master to stop accepting new sessions. Unlike
// "-O exit" it leaves sessions already running over it alone; the master goes
// away once the last of them ends. Used when a server is deleted or its
// address changes, so the old master is neither orphaned nor reused for the
// wrong host.
func stopMaster(s Server) {
	ctx, cancel := context.WithTimeout(context.Background(), aliveTimeout)
	defer cancel()
	exec.CommandContext(ctx, "ssh",
		"-o", "ControlPath="+socketPath(s), "-O", "stop", "--", s.Target).Run()
}

func isAlive(name string) bool {
	aliveMu.Lock()
	defer aliveMu.Unlock()
	return aliveState[name]
}

// ---------- startup probes (optional, see config.txt) ----------

// probeUnreachable is the probe verdict when every enabled check failed; the
// row wearing it is tinted with unreachableRowStyle.
const probeUnreachable = "not reachable"

// portCheckTimeout bounds the TCP probe of the ssh port. It is short on
// purpose: the probe only runs after a ping already failed, and a server that
// needs longer than this to answer a SYN is not worth calling reachable.
const portCheckTimeout = time.Second

// pingAvgRe matches the summary line of both macOS ("round-trip
// min/avg/max/stddev = a/b/c/d ms") and Linux ("rtt min/avg/max/mdev = ...")
// ping output; the second number is the average.
var pingAvgRe = regexp.MustCompile(`min/avg/max[^=]*= *[0-9.]+/([0-9.]+)/`)

func parsePingAvg(out string) (string, bool) {
	m := pingAvgRe.FindStringSubmatch(out)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// startProbes checks every server concurrently and redraws the list as each
// result arrives. Runs on the UI goroutine (startup, and every refresh).
// probeText is rebuilt from scratch so removed servers don't linger, and the
// generation counter keeps stragglers from an earlier round from writing
// stale results into the new map. With both probes off it only clears any
// leftover probe text.
func startProbes() {
	cfg := config
	probeMu.Lock()
	probeGen++
	gen := probeGen
	if !cfg.Ping && !cfg.PortCheck {
		probeText = map[string]string{}
		probeMu.Unlock()
		rebuildServerList()
		return
	}
	wait := "checking port…"
	if cfg.Ping {
		wait = "pinging…"
	}
	probeText = make(map[string]string, len(servers))
	for _, s := range servers {
		probeText[s.Name] = wait
	}
	probeMu.Unlock()
	rebuildServerList()
	for _, s := range servers {
		s := s
		go func() {
			res := probeServer(s, cfg)
			probeMu.Lock()
			if probeGen == gen {
				probeText[s.Name] = res
			}
			probeMu.Unlock()
			app.QueueUpdateDraw(rebuildServerList)
		}()
	}
}

// probeServer returns the status text for one server's line: its average RTT
// when the ping answers, otherwise — with port_check=on — whether the ssh port
// still accepts connections, which is the useful answer for the many hosts
// that drop ICMP but serve ssh fine.
func probeServer(s Server, cfg Config) string {
	if cfg.Ping {
		if avg, ok := pingProbe(s, cfg); ok {
			return avg + " ms"
		}
	}
	if cfg.PortCheck && portOpen(s) {
		if cfg.Ping {
			return "no ping — port " + s.Port + " open"
		}
		return "port " + s.Port + " open"
	}
	return probeUnreachable
}

// unreachable reports whether the startup probes have concluded the server
// cannot be reached: the ping got no answer and — when port_check is on — the
// ssh port refused too. Always false while the probes are off or still
// running.
func unreachable(name string) bool {
	if !config.Ping && !config.PortCheck {
		return false
	}
	probeMu.Lock()
	defer probeMu.Unlock()
	return probeText[name] == probeUnreachable
}

// pingProbe is the ping leg of probeServer, indirected so tests can drive the
// fallback without a host that really drops ICMP.
var pingProbe func(Server, Config) (string, bool) = pingServer

// pingServer returns the average round-trip time in milliseconds, or ok=false
// on timeout, packet loss or any error. The config travels as an argument
// because the probes run off the UI goroutine, which owns the global.
func pingServer(s Server, cfg Config) (string, bool) {
	// N pings go out one second apart; the margin covers DNS and the last
	// reply.
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(cfg.PingCount+5)*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ping", "-n", "-q",
		"-c", strconv.Itoa(cfg.PingCount), "--", hostOf(s.Target)).Output()
	if err != nil {
		return "", false
	}
	return parsePingAvg(string(out))
}

// portOpen reports whether the server's ssh port completes a TCP handshake
// within portCheckTimeout. Nothing is sent, so no ssh banner exchange or
// authentication attempt is logged on the far side.
func portOpen(s Server) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(hostOf(s.Target), s.Port),
		portCheckTimeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// hostOf strips the optional user@ prefix from a target.
func hostOf(target string) string {
	if _, h, ok := strings.Cut(target, "@"); ok {
		return h
	}
	return target
}

// connect closes the TUI and queues the ssh session; main runs it once the
// terminal is restored, so exiting ssh drops back to the original local shell.
// With reuse=false any stale master is torn down and a fresh one is created.
func connect(s Server, reuse bool) {
	sock := socketPath(s)
	args := []string{
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + sock,
		"-o", "ControlPersist=yes",
		"-p", s.Port, "-t", "--", s.Target,
	}
	pending = &pendingConn{name: s.Name, target: s.Target, args: args}
	if !reuse {
		pending.killSock = sock
	}
	app.Stop()
}

// ---------- UI ----------

// Row styles for both list panes: black on the Platinum silver, inverted when
// selected. The silver is stated outright rather than left to the terminal's
// default background, so a row is the same grey on every terminal — and
// extendRowBands carries it across the full row, border to border.
var (
	rowStyle = tcell.StyleDefault.
			Foreground(tcell.ColorBlack).
			Background(tcell.ColorSilver)
	selectedRowStyle = tcell.StyleDefault.
				Foreground(tcell.ColorWhite).
				Background(tcell.ColorBlack)
	// unreachableRowStyle tints a server row whose probes all failed (see
	// unreachable) a very light red-orange, so dead servers stand out at a
	// glance without shouting. Terminals without 24-bit colour get tcell's
	// nearest palette match.
	unreachableRowStyle = tcell.StyleDefault.
				Foreground(tcell.ColorBlack).
				Background(tcell.NewRGBColor(255, 221, 204))
)

func buildMain() tview.Primitive {
	// Each server item is two screen lines; the empty secondary line is the
	// spacing.
	serverList = newMacScrollList(tview.NewList(), 2)
	serverList.setRowStyles(rowStyle, selectedRowStyle)
	serverList.SetSelectedFocusOnly(true). // no highlight while the pane is unfocused
						SetHighlightFullLine(true) // selection spans the whole row, Mac style
	serverList.SetBackgroundColor(tcell.ColorWhite) // Finder-style white pane
	serverList.SetBorderPadding(1, 0, 1, 1)
	serverList.SetBorder(true).SetTitle(" Servers ")
	// A server the probes gave up on wears a light red-orange band. A live
	// master trumps the probes: connected means reachable, whatever ICMP says.
	serverList.setRowTint(func(item int) (tcell.Style, bool) {
		if item >= len(serverRows) {
			return tcell.Style{}, false
		}
		r := serverRows[item]
		if r.header != "" || isAlive(r.server.Name) || !unreachable(r.server.Name) {
			return tcell.Style{}, false
		}
		return unreachableRowStyle, true
	})
	serverList.SetSelectedFunc(func(i int, _, _ string, _ rune) {
		if i >= len(serverRows) {
			return
		}
		if r := serverRows[i]; r.header != "" {
			collapsed[r.header] = !collapsed[r.header]
			rebuildServerList()
		} else {
			showServerMenu(r.server)
		}
	})

	newSrv := tview.NewButton("New Server (n)").SetSelectedFunc(showServerForm)
	newFolder := tview.NewButton("New Folder (f)").SetSelectedFunc(showFolderForm)
	refresh := tview.NewButton("Refresh (r)").SetSelectedFunc(refreshServers)
	quit := tview.NewButton("Quit (q)").SetSelectedFunc(app.Stop)
	for _, b := range []*tview.Button{newSrv, newFolder, refresh, quit} {
		b.SetStyle(tcell.StyleDefault.Background(tcell.ColorWhite).Foreground(tcell.ColorBlack))
		b.SetActivatedStyle(tcell.StyleDefault.Background(tcell.ColorBlack).Foreground(tcell.ColorWhite))
	}
	focusRing = []tview.Primitive{serverList, newSrv, newFolder, refresh, quit}

	// White strip across the top with flat items, like the classic menu bar.
	bar := tview.NewFlex()
	bar.AddItem(tview.NewTextView().SetText(" ⌘").SetBackgroundColor(tcell.ColorWhite), 3, 0, false)
	for _, p := range focusRing[1:] {
		b := p.(*tview.Button)
		bar.AddItem(b, len(b.GetLabel())+2, 0, false)
	}
	bar.AddItem(tview.NewBox().SetBackgroundColor(tcell.ColorWhite), 0, 1, false)

	help := tview.NewTextView().
		SetText(" Enter: server menu / open-close folder   Tab: switch focus   arrows/mouse: navigate").
		SetTextColor(tcell.ColorDimGray)

	root := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(bar, 1, 0, false).
		AddItem(serverList, 0, 1, true).
		AddItem(help, 1, 0, false)

	app.SetInputCapture(func(e *tcell.EventKey) *tcell.EventKey {
		if len(modals) > 0 {
			// An open folder picker gets the key first, so Esc closes just
			// the picker and not the whole form.
			if d := modals[len(modals)-1].drop; d != nil && d.IsOpen() {
				return e
			}
			// Esc always closes the top dialog, wherever focus is.
			if e.Key() == tcell.KeyEscape {
				closeModal()
				return nil
			}
			// On a focused button the arrow keys move between buttons just
			// like Tab/Backtab. The check keeps arrows editing text normally
			// when a form field (input or text area) has focus instead.
			if _, ok := app.GetFocus().(*tview.Button); ok {
				switch e.Key() {
				case tcell.KeyLeft, tcell.KeyUp:
					return tcell.NewEventKey(tcell.KeyBacktab, 0, tcell.ModNone)
				case tcell.KeyRight, tcell.KeyDown:
					return tcell.NewEventKey(tcell.KeyTab, 0, tcell.ModNone)
				}
			}
			return e
		}
		switch e.Key() {
		case tcell.KeyTab, tcell.KeyBacktab:
			cur := 0
			for i, p := range focusRing {
				if app.GetFocus() == p {
					cur = i
					break
				}
			}
			step := 1
			if e.Key() == tcell.KeyBacktab {
				step = len(focusRing) - 1
			}
			app.SetFocus(focusRing[(cur+step)%len(focusRing)])
			return nil
		}
		if app.GetFocus() == serverList {
			switch e.Rune() {
			case 'n':
				showServerForm()
				return nil
			case 'f':
				showFolderForm()
				return nil
			case 'r':
				refreshServers()
				return nil
			case 'q':
				app.Stop()
				return nil
			}
		}
		return e
	})
	// While a dialog is open, swallow mouse events outside it so nothing
	// behind can be clicked. An open folder picker may hang past the bottom of
	// its form, so it gets every event until it closes.
	app.SetMouseCapture(func(ev *tcell.EventMouse, act tview.MouseAction) (*tcell.EventMouse, tview.MouseAction) {
		if len(modals) == 0 {
			return ev, act
		}
		top := modals[len(modals)-1]
		if top.drop != nil && top.drop.IsOpen() {
			return ev, act
		}
		x, y, w, h := top.prim.GetRect()
		mx, my := ev.Position()
		if mx < x || mx >= x+w || my < y || my >= y+h {
			// A click outside a popup menu dismisses it, like a real menu.
			if top.menu && act == tview.MouseLeftDown {
				closeModal()
			}
			return nil, 0
		}
		return ev, act
	})
	return root
}

// showModal opens p as a centered dialog on top of everything else.
func showModal(name string, p tview.Primitive, w, h int) {
	modals = append(modals, modalEntry{name: name, prim: p})
	pages.AddPage(name, center(p, w, h), true, true)
}

// closeModal closes the top dialog and returns focus to what is beneath.
func closeModal() {
	if len(modals) == 0 {
		return
	}
	top := modals[len(modals)-1]
	modals = modals[:len(modals)-1]
	pages.RemovePage(top.name)
	if len(modals) > 0 {
		app.SetFocus(modals[len(modals)-1].prim)
	} else {
		app.SetFocus(serverList)
	}
}

// refreshData redraws the server pane straight from the text files. It
// touches nothing but local disk, so it is safe on the path to the first frame.
func refreshData() {
	servers, groups = loadServers()
	rebuildServerList()
}

func refreshServers() {
	loadConfig() // config.txt edits take effect without a restart
	refreshData()
	refreshAlive(servers)
	startProbes()
}

// rebuildServerList redraws the server pane: servers with a live connection
// first (hoisted out of their folders so they stay reachable while folders are
// shut), then the remaining top-level servers, then each folder, collapsible,
// in the order its header appears in the file. Folders are folded shut the
// first time they are seen.
func rebuildServerList() {
	cur := serverList.GetCurrentItem()
	serverList.Clear()
	serverRows = serverRows[:0]

	connected := map[string]bool{}
	for _, s := range servers {
		connected[s.Name] = isAlive(s.Name)
	}

	// Column widths are measured across every server so the name, address and
	// probe columns each start at the same screen column on every row —
	// including rows indented inside a folder, and rows whose probe hasn't
	// answered yet. Names are budgeted with the indent they'd have inside their
	// folder, so hoisting a connected server to the top level doesn't shift the
	// columns. The probe column is only padded as wide as the connected servers
	// need, because the marker is all that follows it: padding it to a long
	// probe on some unreachable row would just shove every marker off the pane.
	probeOn := config.Ping || config.PortCheck
	nameW, addrW, probeW := 0, 0, 0
	probeMu.Lock()
	for _, s := range servers {
		w := utf8.RuneCountInString(s.Name)
		if s.Group != "" {
			w += utf8.RuneCountInString(memberIndent)
		}
		nameW = max(nameW, w)
		addrW = max(addrW, utf8.RuneCountInString(s.Target+":"+s.Port))
		if probeOn && connected[s.Name] {
			probeW = max(probeW, utf8.RuneCountInString(probeText[s.Name]))
		}
	}
	probeMu.Unlock()

	addServer := func(s Server, indent string) {
		serverRows = append(serverRows, serverRow{server: s})
		line := fmt.Sprintf(" %-*s %-*s", nameW, indent+s.Name, addrW, s.Target+":"+s.Port)
		if probeOn {
			probeMu.Lock()
			p := probeText[s.Name]
			probeMu.Unlock()
			// Pad only when a marker follows, so the probe stays the last thing
			// on an unconnected row and never trails needless spaces.
			if connected[s.Name] {
				line += fmt.Sprintf("  %-*s", probeW, p)
			} else if p != "" {
				line += "  " + p
			}
		}
		if connected[s.Name] {
			line += "  [::b]● connected — reusable[::-]"
		}
		serverList.AddItem(strings.TrimRight(line, " "), "", 0, nil)
	}

	for _, s := range servers {
		if connected[s.Name] {
			addServer(s, "")
		}
	}
	for _, s := range servers {
		if !connected[s.Name] && s.Group == "" {
			addServer(s, "")
		}
	}
	for _, g := range groups {
		var members []Server
		for _, s := range servers {
			if s.Group == g && !connected[s.Name] {
				members = append(members, s)
			}
		}
		if _, known := collapsed[g]; !known {
			collapsed[g] = true // folders start shut
		}
		serverRows = append(serverRows, serverRow{header: g})
		if collapsed[g] {
			serverList.AddItem(fmt.Sprintf(" ▸ %s (%d)", g, len(members)), "", 0, nil)
			continue
		}
		serverList.AddItem(" ▾ "+g, "", 0, nil)
		for _, s := range members {
			addServer(s, memberIndent)
		}
	}
	if len(servers) == 0 {
		serverList.AddItem("(no servers yet)", "press n or click New Server to add one", 0, nil)
	}
	if n := serverList.GetItemCount(); cur >= n {
		cur = n - 1
	}
	if cur >= 0 {
		serverList.SetCurrentItem(cur)
	}
}

// showServerMenu pops up the small Connect / Edit / Delete menu for a server,
// hanging just under its row like a classic Mac pop-up menu (or just above
// it, when the row is too close to the bottom of the screen).
func showServerMenu(s Server) {
	l := tview.NewList().ShowSecondaryText(false).SetHighlightFullLine(true)
	l.SetMainTextStyle(rowStyle).SetSelectedStyle(selectedRowStyle).
		SetShortcutStyle(rowStyle.Foreground(tcell.ColorDimGray))
	l.SetBorder(true).SetTitle(" " + s.Name + " ")
	l.AddItem("Connect", "", 'c', func() { closeModal(); selectServer(s) })
	l.AddItem("Edit…", "", 'e', func() { closeModal(); showServerEditForm(s) })
	l.AddItem("Delete…", "", 'd', func() { closeModal(); confirmDeleteServer(s) })

	w := max(utf8.RuneCountInString(s.Name)+6, 18)
	h := l.GetItemCount() + 2
	ix, iy, _, _ := serverList.GetInnerRect()
	off, _ := serverList.GetOffset()
	row := iy + (serverList.GetCurrentItem()-off)*serverList.lines
	_, _, sw, sh := pages.GetRect()
	x, y := ix+2, row+1
	if y+h >= sh { // +1 for the drop shadow
		y = row - h
	}
	x = max(0, min(x, sw-w-1))
	y = max(0, y)

	modals = append(modals, modalEntry{name: "serverMenu", prim: l, menu: true})
	pages.AddPage("serverMenu", placeAt(l, x, y, w, h), true, true)
}

// selecting is true while selectServer's fresh master check is in flight, so
// pressing Enter again cannot stack a second check and a duplicate dialog.
// Only touched on the UI goroutine.
var selecting bool

// selectServer freshly checks the server's master and connects, asking first
// whether to reuse it when one is live. The check runs off the UI goroutine:
// against a wedged master it takes the full aliveTimeout, which would
// otherwise freeze the whole UI.
func selectServer(s Server) {
	if selecting {
		return
	}
	selecting = true
	go func() {
		ok := alive(s)
		aliveMu.Lock()
		changed := aliveState[s.Name] != ok
		aliveState[s.Name] = ok
		aliveMu.Unlock()
		app.QueueUpdateDraw(func() {
			selecting = false
			if changed {
				rebuildServerList()
			}
			if !ok {
				connect(s, false)
				return
			}
			dialog("reuse",
				fmt.Sprintf("A live connection to “%s” already exists.\nReuse it or create a new one?", s.Name),
				[]string{"Reuse", "New connection", "Cancel"},
				func(label string) {
					switch label {
					case "Reuse":
						connect(s, true)
					case "New connection":
						connect(s, false)
					}
				})
		})
	}()
}

// currentFolder is the folder the highlighted row belongs to (or is), so a
// new server lands where the user is looking.
func currentFolder() string {
	i := serverList.GetCurrentItem()
	if i < 0 || i >= len(serverRows) {
		return ""
	}
	if r := serverRows[i]; r.header != "" {
		return r.header
	}
	return serverRows[i].server.Group
}

func showServerForm() { serverForm(nil) }

func showServerEditForm(s Server) { serverForm(&s) }

// serverForm opens the server dialog: blank for a new server, or filled in
// with every setting of orig for editing it.
func serverForm(orig *Server) {
	const noFolder = "(none)"
	s := Server{Port: "22", Group: currentFolder()}
	title, name := " New server ", "serverForm"
	if orig != nil {
		s = *orig
		title, name = fmt.Sprintf(" Edit %s ", orig.Name), "serverEdit"
	}
	user, host, hasUser := strings.Cut(s.Target, "@")
	if !hasUser {
		user, host = "", s.Target
	}
	folders := append([]string{noFolder}, groups...)
	folderIdx := 0
	for i, g := range groups {
		if g == s.Group {
			folderIdx = i + 1
		}
	}

	form := tview.NewForm().
		AddInputField("Name", s.Name, 30, nil, nil).
		AddInputField("User (optional)", user, 30, nil, nil).
		AddInputField("Host", host, 30, nil, nil).
		AddInputField("Port", s.Port, 6, nil, nil).
		AddDropDown("Folder", folders, folderIdx, nil)
	styleForm(form)
	drop := form.GetFormItemByLabel("Folder").(*tview.DropDown)
	drop.SetListStyles(rowStyle, selectedRowStyle)
	text := func(label string) string {
		return strings.TrimSpace(form.GetFormItemByLabel(label).(*tview.InputField).GetText())
	}
	form.AddButton("Save", func() {
		next := Server{Name: text("Name"), Port: text("Port")}
		user, host := text("User (optional)"), text("Host")
		if i, _ := drop.GetCurrentOption(); i > 0 {
			next.Group = groups[i-1]
		}
		if !validName(next.Name) {
			errModal("Name is required and may not contain spaces, slashes\nor a leading “-” or “[”.")
			return
		}
		for _, o := range servers {
			if o.Name == next.Name && (orig == nil || o.Name != orig.Name) {
				errModal(fmt.Sprintf("A server named “%s” already exists.", next.Name))
				return
			}
		}
		if strings.ContainsAny(user, " \t:@") || strings.HasPrefix(user, "-") {
			errModal("User may not contain spaces, “:” or “@”,\nor start with “-”.")
			return
		}
		if host == "" || strings.ContainsAny(host, " \t:@") || strings.HasPrefix(host, "-") {
			errModal("Host is required, e.g. 203.0.113.10 or db.internal\n(no user or port here — they have their own fields).")
			return
		}
		if !validPort(next.Port) {
			errModal("Port must be a number between 1 and 65535.")
			return
		}
		next.Target = host
		if user != "" {
			next.Target = user + "@" + host
		}

		path := filepath.Join(baseDir, serversFileName)
		data, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			errModal(err.Error())
			return
		}
		content := string(data)
		entry := fmt.Sprintf("%s %s:%s", next.Name, next.Target, next.Port)
		switch {
		case orig == nil:
			content = insertServerLine(content, entry, next.Group)
		case orig.Group == next.Group:
			content = replaceServerLine(content, orig.Name, entry)
		default:
			content = insertServerLine(replaceServerLine(content, orig.Name, ""), entry, next.Group)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			errModal(err.Error())
			return
		}
		collapsed[next.Group] = false // keep the entry in view
		closeModal()
		// A live master is keyed by name and port and was opened to the old
		// target; once any of those change it would be orphaned, or worse,
		// reused for the wrong host.
		var stale *Server
		if orig != nil && (orig.Name != next.Name || orig.Target != next.Target || orig.Port != next.Port) {
			stale = orig
		}
		serversChanged(stale)
	})
	form.AddButton("Cancel", closeModal)
	form.SetCancelFunc(closeModal)
	form.SetBorder(true)
	form.SetTitle(title)
	showModal(name, form, 56, 15)
	modals[len(modals)-1].drop = drop
}

func confirmDeleteServer(s Server) {
	dialog("serverDelete",
		fmt.Sprintf("Delete “%s” (%s:%s)?\nThis cannot be undone.", s.Name, s.Target, s.Port),
		[]string{"Delete", "Cancel"},
		func(label string) {
			if label != "Delete" {
				return
			}
			path := filepath.Join(baseDir, serversFileName)
			data, err := os.ReadFile(path)
			if err != nil {
				errModal(err.Error())
				return
			}
			if err := os.WriteFile(path, []byte(replaceServerLine(string(data), s.Name, "")), 0o600); err != nil {
				errModal(err.Error())
				return
			}
			serversChanged(&s)
		})
}

// serversChanged reloads the list after the servers file was written. When
// stale is set, that server's master is stopped first — off the UI goroutine,
// since a wedged one takes the full aliveTimeout — so the status scan that
// follows doesn't still find it live.
func serversChanged(stale *Server) {
	if stale == nil {
		refreshServers()
		return
	}
	refreshData()
	s := *stale
	go func() {
		stopMaster(s)
		app.QueueUpdateDraw(refreshServers)
	}()
}

// showFolderForm opens the New-folder dialog. The folder is written as an
// empty [group] section at the end of the servers file, ready for servers to
// be added or moved into it.
func showFolderForm() {
	form := tview.NewForm().AddInputField("Folder name", "", 30, nil, nil)
	styleForm(form)
	form.AddButton("Save", func() {
		g := strings.TrimSpace(form.GetFormItemByLabel("Folder name").(*tview.InputField).GetText())
		if !validGroup(g) {
			errModal("Folder name is required and may not contain\nbrackets or #.")
			return
		}
		for _, o := range groups {
			if o == g {
				errModal(fmt.Sprintf("A folder named “%s” already exists.", g))
				return
			}
		}
		path := filepath.Join(baseDir, serversFileName)
		data, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			errModal(err.Error())
			return
		}
		if err := os.WriteFile(path, []byte(appendGroup(string(data), g)), 0o600); err != nil {
			errModal(err.Error())
			return
		}
		collapsed[g] = false
		closeModal()
		refreshData()
	})
	form.AddButton("Cancel", closeModal)
	form.SetCancelFunc(closeModal)
	form.SetBorder(true)
	form.SetTitle(" New folder ")
	showModal("folderForm", form, 50, 7)
}

func errModal(msg string) {
	dialog("error", msg, []string{"OK"}, func(string) {})
}

// dialog shows a classic Mac style alert: grey box, black border, drop
// shadow, centered text and buttons. done receives the pressed label
// ("" when cancelled with Esc).
func dialog(name, text string, buttons []string, done func(label string)) {
	form := tview.NewForm().SetButtonsAlign(tview.AlignCenter)
	styleForm(form)
	for _, l := range buttons {
		l := l
		form.AddButton(l, func() { closeModal(); done(l) })
	}
	form.SetCancelFunc(func() { closeModal(); done("") })

	tv := tview.NewTextView().SetTextAlign(tview.AlignCenter).SetWordWrap(true).SetText(text)
	f := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(tv, 0, 1, false).
		AddItem(form, 3, 0, true)
	f.SetBorderPadding(1, 0, 2, 2)
	f.SetBorder(true)
	// Size the box by the lines the text will actually occupy once wrapped
	// to the dialog's inner width, so long one-line messages (typically OS
	// error strings) aren't clipped. Word-wrap can break a line early, so
	// wrapped lines get one spare row.
	const dialogW = 56
	const innerW = dialogW - 6 // border + side padding
	lines := 0
	for _, ln := range strings.Split(text, "\n") {
		n := (utf8.RuneCountInString(ln) + innerW - 1) / innerW
		if n > 1 {
			n++
		}
		lines += max(n, 1)
	}
	// borders (2) + top padding (1) + buttons (3) + a blank spacer row.
	showModal(name, f, dialogW, lines+7)
}

// styleForm gives a form classic Mac push buttons: white with black labels,
// inverted while active.
func styleForm(form *tview.Form) {
	form.SetButtonStyle(tcell.StyleDefault.Background(tcell.ColorWhite).Foreground(tcell.ColorBlack)).
		SetButtonActivatedStyle(tcell.StyleDefault.Background(tcell.ColorBlack).Foreground(tcell.ColorWhite))
}

// macScrollList wraps a List and draws a classic Mac scroll bar — arrow
// caps, stippled track, solid thumb — in the list's right border-padding
// column whenever the items overflow the pane. The arrows scroll by one
// item, the track by a page. lines is how many screen lines one item takes
// (2 with secondary text, 1 without).
type macScrollList struct {
	*tview.List
	lines            int
	rowStyle, selRow tcell.Style
	rowTint          func(item int) (tcell.Style, bool)
}

func newMacScrollList(l *tview.List, linesPerItem int) *macScrollList {
	return &macScrollList{List: l, lines: linesPerItem}
}

// setRowStyles sets how a row is painted, normally and when selected. The
// styles are kept here as well as on the List so extendRowBands can carry each
// row's background out to the border.
func (m *macScrollList) setRowStyles(normal, selected tcell.Style) {
	m.rowStyle, m.selRow = normal, selected
	m.SetMainTextStyle(normal)
	m.SetSelectedStyle(selected)
}

// setRowTint registers a per-item override of the normal row style. A tinted
// row's band is painted in the override and the item's own text is
// re-backgrounded to match; the selection style still wins on the selected
// row.
func (m *macScrollList) setRowTint(f func(item int) (tcell.Style, bool)) {
	m.rowTint = f
}

// extendRowBands paints the rest of every item's row in that item's own
// background. tview colours only the cells its text actually covers — and for
// the selected item, out to the inner rect — so a row's band otherwise stops
// at the end of its text and short of the border padding. Classic Mac lists
// run their rows edge to edge, so fill from border to border.
//
// Called after List.Draw (which clears the whole box first) and before the
// scroll bar, which owns the right padding column and paints over this.
func (m *macScrollList) extendRowBands(screen tcell.Screen) {
	ix, iy, iw, ih := m.GetInnerRect()
	bx, _, bw, _ := m.GetRect()
	_, screenH := screen.Size()
	bottom := iy + ih
	if bottom > screenH {
		bottom = screenH
	}
	if iw <= 0 || bw < 3 {
		return
	}
	left, right := bx+1, bx+bw-2 // the columns between the two borders
	useTags, _ := m.GetUseStyleTags()
	off, hOff := m.GetOffset()
	if off < 0 {
		off = 0
	}
	for i := off; i < m.GetItemCount(); i++ {
		row := iy + (i-off)*m.lines
		if row >= bottom {
			break
		}
		style := m.rowStyle
		tinted := false
		if m.rowTint != nil {
			if s, ok := m.rowTint(i); ok {
				style, tinted = s, true
			}
		}
		// Mirrors List.Draw: with SetSelectedFocusOnly the selected row keeps
		// the normal background while the pane is unfocused.
		if i == m.GetCurrentItem() && m.HasFocus() {
			style, tinted = m.selRow, false
		}
		main, _ := m.GetItemText(i)
		width := len([]rune(main))
		if useTags {
			width = tview.TaggedStringWidth(main)
		}
		// First column past the item's own text, which must not be repainted.
		end := ix + width - hOff
		if end < ix {
			end = ix
		}
		if end > ix+iw {
			end = ix + iw
		}
		_, tintBg, _ := style.Decompose()
		for x := left; x <= right; x++ {
			if x >= ix && x < end {
				// List.Draw painted the item's text on the normal background;
				// on a tinted row recolour just the background underneath it,
				// keeping the glyphs and their attributes.
				if tinted {
					ch, comb, st, _ := screen.GetContent(x, row)
					screen.SetContent(x, row, ch, comb, st.Background(tintBg))
				}
				continue
			}
			screen.SetContent(x, row, ' ', nil, style)
		}
	}
}

// barGeometry reports where the scroll bar goes: the column, the top row
// and height of the whole bar (arrows included), and the thumb's rows.
// ok is false when everything fits and no bar should be drawn.
func (m *macScrollList) barGeometry() (barX, top, height, thumbTop, thumbH int, ok bool) {
	x, y, w, h := m.GetInnerRect()
	total := m.GetItemCount()
	vis := h / m.lines
	trackH := h - 2
	if vis < 1 || total <= vis || trackH < 1 {
		return 0, 0, 0, 0, 0, false
	}
	barX = x + w // the right border-padding column
	thumbH = trackH * vis / total
	if thumbH < 1 {
		thumbH = 1
	}
	off, _ := m.GetOffset()
	maxOff := total - vis
	if off < 0 {
		off = 0
	}
	if off > maxOff {
		off = maxOff
	}
	thumbTop = y + 1 + (trackH-thumbH)*off/maxOff
	return barX, y, h, thumbTop, thumbH, true
}

func (m *macScrollList) Draw(screen tcell.Screen) {
	m.List.Draw(screen)
	m.extendRowBands(screen)
	barX, top, height, thumbTop, thumbH, ok := m.barGeometry()
	if !ok {
		return
	}
	arrow := tcell.StyleDefault.Background(tcell.ColorWhite).Foreground(tcell.ColorBlack)
	track := tcell.StyleDefault.Background(tcell.ColorWhite).Foreground(tcell.ColorGray)
	screen.SetContent(barX, top, '▲', nil, arrow)
	screen.SetContent(barX, top+height-1, '▼', nil, arrow)
	for row := top + 1; row < top+height-1; row++ {
		ch, st := '▒', track
		if row >= thumbTop && row < thumbTop+thumbH {
			ch, st = '█', arrow
		}
		screen.SetContent(barX, row, ch, nil, st)
	}
}

func (m *macScrollList) MouseHandler() func(tview.MouseAction, *tcell.EventMouse, func(tview.Primitive)) (bool, tview.Primitive) {
	return func(action tview.MouseAction, event *tcell.EventMouse, setFocus func(tview.Primitive)) (bool, tview.Primitive) {
		mx, my := event.Position()
		if barX, top, height, thumbTop, thumbH, ok := m.barGeometry(); ok && mx == barX &&
			my >= top && my < top+height {
			if action == tview.MouseLeftDown {
				_, _, _, h := m.GetInnerRect()
				vis := h / m.lines
				off, _ := m.GetOffset()
				switch {
				case my == top:
					off--
				case my == top+height-1:
					off++
				case my < thumbTop:
					off -= vis
				case my >= thumbTop+thumbH:
					off += vis
				}
				if off < 0 {
					off = 0
				}
				m.SetOffset(off, 0)
				setFocus(m)
			}
			// Swallow the whole press so the list never sees it.
			return action == tview.MouseLeftDown || action == tview.MouseLeftClick, nil
		}
		// Redirect focus to the wrapper so focus comparisons (Tab ring,
		// keyboard shortcuts) keep seeing one primitive.
		return m.List.MouseHandler()(action, event, func(tview.Primitive) { setFocus(m) })
	}
}

// shadowed draws a classic Mac one-cell drop shadow behind its primitive.
type shadowed struct{ tview.Primitive }

func (s shadowed) Draw(screen tcell.Screen) {
	x, y, w, h := s.GetRect()
	st := tcell.StyleDefault.Background(tcell.ColorBlack)
	for i := x + 1; i <= x+w; i++ {
		screen.SetContent(i, y+h, ' ', nil, st)
	}
	for j := y + 1; j <= y+h; j++ {
		screen.SetContent(x+w, j, ' ', nil, st)
	}
	s.Primitive.Draw(screen)
}

// placeAt wraps p in a fixed-size box at screen position x, y, with a drop
// shadow.
func placeAt(p tview.Primitive, x, y, w, h int) tview.Primitive {
	return tview.NewFlex().
		AddItem(nil, x, 0, false).
		AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(nil, y, 0, false).
			AddItem(shadowed{p}, h, 0, true).
			AddItem(nil, 0, 1, false), w, 0, true).
		AddItem(nil, 0, 1, false)
}

// center wraps p in a fixed-size centered box with a drop shadow.
func center(p tview.Primitive, w, h int) tview.Primitive {
	return tview.NewFlex().
		AddItem(nil, 0, 1, false).
		AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(nil, 0, 1, false).
			AddItem(shadowed{p}, h, 0, true).
			AddItem(nil, 0, 1, false), w, 0, true).
		AddItem(nil, 0, 1, false)
}
