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
	"sort"
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

const envTemplate = `# Horizon environment file.
# Lines like KEY=value are exported on the server after connecting.
# Every other non-comment line is executed as a command, in order.
# Afterwards you are dropped into an interactive login shell.
# Example:
#   APP_ENV=staging
#   cd /srv/app
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
	envList    *macScrollList
	servers    []Server
	serverRows []serverRow
	collapsed  = map[string]bool{} // group name -> folded shut
	config     Config
	aliveMu    sync.Mutex
	aliveState = map[string]bool{} // server name -> has a live reusable master
	probeMu    sync.Mutex
	probeText  = map[string]string{} // server name -> RTT / port state / "not reachable"
	pending    *pendingConn
	modals     []modalEntry
	focusRing  []tview.Primitive
	envVarRe   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
)

func main() {
	flag.StringVar(&baseDir, "f", filepath.Join(os.Getenv("HOME"), ".horizon"),
		"folder holding Horizon's txt files")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("horizon", version)
		return
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
			exec.Command("ssh", "-o", "ControlPath="+pending.killSock, "-O", "exit", pending.target).Run()
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
	if len(envFiles()) == 0 {
		return os.WriteFile(filepath.Join(baseDir, "example_env.txt"), []byte(envTemplate), 0o600)
	}
	return nil
}

func loadServers() []Server {
	data, err := os.ReadFile(filepath.Join(baseDir, serversFileName))
	if err != nil {
		return nil
	}
	var out []Server
	group := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if g, ok := groupHeader(line); ok {
			group = g
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		s := Server{Name: f[0], Target: f[1], Port: "22", Group: group}
		if host, port, ok := strings.Cut(s.Target, ":"); ok {
			s.Target, s.Port = host, port
		}
		out = append(out, s)
	}
	return out
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

func envFiles() []string {
	matches, _ := filepath.Glob(filepath.Join(baseDir, "*.txt"))
	var out []string
	for _, m := range matches {
		if b := filepath.Base(m); b != serversFileName && b != configFileName {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

// parseEnvFile splits a txt file into exported variables and commands.
func parseEnvFile(path string) (vars, cmds []string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if envVarRe.MatchString(line) {
			vars = append(vars, line)
		} else {
			cmds = append(cmds, line)
		}
	}
	return vars, cmds, nil
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
		"-o", "ControlPath="+socketPath(s), "-O", "check", s.Target).Run() == nil
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

func isAlive(name string) bool {
	aliveMu.Lock()
	defer aliveMu.Unlock()
	return aliveState[name]
}

// ---------- startup probes (optional, see config.txt) ----------

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
// result arrives. Does nothing unless ping or port_check is on in config.txt.
func startProbes() {
	if !config.Ping && !config.PortCheck {
		return
	}
	wait := "checking port…"
	if config.Ping {
		wait = "pinging…"
	}
	probeMu.Lock()
	for _, s := range servers {
		probeText[s.Name] = wait
	}
	probeMu.Unlock()
	rebuildServerList()
	for _, s := range servers {
		s := s
		go func() {
			res := probeServer(s)
			probeMu.Lock()
			probeText[s.Name] = res
			probeMu.Unlock()
			app.QueueUpdateDraw(rebuildServerList)
		}()
	}
}

// probeServer returns the status text for one server's line: its average RTT
// when the ping answers, otherwise — with port_check=on — whether the ssh port
// still accepts connections, which is the useful answer for the many hosts
// that drop ICMP but serve ssh fine.
func probeServer(s Server) string {
	if config.Ping {
		if avg, ok := pingProbe(s); ok {
			return avg + " ms"
		}
	}
	if config.PortCheck && portOpen(s) {
		if config.Ping {
			return "no ping — port " + s.Port + " open"
		}
		return "port " + s.Port + " open"
	}
	return "not reachable"
}

// pingProbe is the ping leg of probeServer, indirected so tests can drive the
// fallback without a host that really drops ICMP.
var pingProbe = pingServer

// pingServer returns the average round-trip time in milliseconds, or ok=false
// on timeout, packet loss or any error.
func pingServer(s Server) (string, bool) {
	// N pings go out one second apart; the margin covers DNS and the last
	// reply.
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(config.PingCount+5)*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ping", "-n", "-q",
		"-c", strconv.Itoa(config.PingCount), hostOf(s.Target)).Output()
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

func shellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

func buildScript(vars, cmds []string) string {
	var parts []string
	for _, v := range vars {
		k, val, _ := strings.Cut(v, "=")
		parts = append(parts, "export "+k+"="+shellQuote(val))
	}
	parts = append(parts, cmds...)
	// Once the connection is up and the environment is applied, show the user
	// what Horizon did on the remote host: every variable with the value it
	// now holds, and each command that ran, before handing over the shell.
	parts = append(parts, buildReport(vars, cmds)...)
	parts = append(parts, `exec "$SHELL" -l`)
	return strings.Join(parts, "; ")
}

// buildReport emits shell snippets that print a summary of the applied
// environment. Variable values are read back with "$NAME" so they reflect
// what was actually set on the server, not just the file's text.
func buildReport(vars, cmds []string) []string {
	if len(vars) == 0 && len(cmds) == 0 {
		return nil
	}
	out := []string{`printf '\n== Horizon: environment applied ==\n'`}
	if len(vars) > 0 {
		out = append(out, `printf 'Variables set:\n'`)
		for _, v := range vars {
			k, _, _ := strings.Cut(v, "=")
			out = append(out, `printf '  %s=%s\n' `+shellQuote(k)+` "$`+k+`"`)
		}
	}
	if len(cmds) > 0 {
		out = append(out, `printf 'Commands run:\n'`)
		for _, c := range cmds {
			out = append(out, `printf '  %s\n' `+shellQuote(c))
		}
	}
	return append(out, `printf '\n'`)
}

// connect closes the TUI and queues the ssh session; main runs it once the
// terminal is restored, so exiting ssh drops back to the original local shell.
// With reuse=false any stale master is torn down and a fresh one is created.
func connect(s Server, envPath string, reuse bool) {
	sock := socketPath(s)
	args := []string{
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + sock,
		"-o", "ControlPersist=yes",
		"-p", s.Port, "-t", s.Target,
	}
	if !reuse && envPath != "" {
		vars, cmds, err := parseEnvFile(envPath)
		if err != nil {
			errModal(err.Error())
			return
		}
		args = append(args, buildScript(vars, cmds))
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
)

func buildMain() tview.Primitive {
	// Each server item is two screen lines (the empty secondary line is the
	// spacing), env file items are one.
	serverList = newMacScrollList(tview.NewList(), 2)
	serverList.setRowStyles(rowStyle, selectedRowStyle)
	serverList.SetSelectedFocusOnly(true). // no highlight while the pane is unfocused
						SetHighlightFullLine(true) // selection spans the whole row, Mac style
	serverList.SetBackgroundColor(tcell.ColorWhite) // Finder-style white pane
	serverList.SetBorderPadding(1, 0, 1, 1)
	serverList.SetBorder(true).SetTitle(" Servers ")
	serverList.SetSelectedFunc(func(i int, _, _ string, _ rune) {
		if i >= len(serverRows) {
			return
		}
		if r := serverRows[i]; r.header != "" {
			collapsed[r.header] = !collapsed[r.header]
			rebuildServerList()
		} else {
			selectServer(r.server)
		}
	})

	envList = newMacScrollList(tview.NewList().ShowSecondaryText(false), 1)
	envList.setRowStyles(rowStyle, selectedRowStyle)
	envList.SetSelectedFocusOnly(true).
		SetHighlightFullLine(true)
	envList.SetBackgroundColor(tcell.ColorWhite)
	envList.SetBorderPadding(1, 0, 1, 1)
	envList.SetBorder(true).SetTitle(" Env Files ")
	envList.SetSelectedFunc(func(i int, _, _ string, _ rune) {
		files := envFiles()
		if i < len(files) {
			selectEnvFile(files[i])
		}
	})

	newSrv := tview.NewButton("New Server (n)").SetSelectedFunc(showServerForm)
	newEnv := tview.NewButton("New Env File (e)").SetSelectedFunc(showEnvFileForm)
	refresh := tview.NewButton("Refresh (r)").SetSelectedFunc(refreshServers)
	quit := tview.NewButton("Quit (q)").SetSelectedFunc(app.Stop)
	dup := tview.NewButton("Duplicate (d)").SetSelectedFunc(duplicateCurrentEnv)
	for _, b := range []*tview.Button{newSrv, newEnv, refresh, quit, dup} {
		b.SetStyle(tcell.StyleDefault.Background(tcell.ColorWhite).Foreground(tcell.ColorBlack))
		b.SetActivatedStyle(tcell.StyleDefault.Background(tcell.ColorBlack).Foreground(tcell.ColorWhite))
	}
	focusRing = []tview.Primitive{serverList, envList, newSrv, newEnv, refresh, quit}

	// White strip across the top with flat items, like the classic menu bar.
	bar := tview.NewFlex()
	bar.AddItem(tview.NewTextView().SetText(" ⌘").SetBackgroundColor(tcell.ColorWhite), 3, 0, false)
	for _, p := range focusRing[2:] {
		b := p.(*tview.Button)
		bar.AddItem(b, len(b.GetLabel())+2, 0, false)
	}
	// The Duplicate item sits collapsed at width 0 until the Env Files pane
	// is active. Visibility is driven by whoever GAINS focus — hiding on the
	// env pane's blur would collapse the bar between mouse-down (focus moves
	// to the button) and mouse-up (the click), making the button unclickable.
	bar.AddItem(dup, 0, 0, false)
	bar.AddItem(tview.NewBox().SetBackgroundColor(tcell.ColorWhite), 0, 1, false)

	showDup := func(show bool) {
		w := 0
		if show {
			w = len(dup.GetLabel()) + 2
		}
		bar.ResizeItem(dup, w, 0)
	}
	envList.SetFocusFunc(func() {
		envList.SetTitle(" Env Files — d: duplicate ")
		showDup(true)
	})
	envList.SetBlurFunc(func() { envList.SetTitle(" Env Files ") })
	serverList.SetFocusFunc(func() { showDup(false) })
	for _, p := range focusRing[2:] {
		p.(*tview.Button).SetFocusFunc(func() { showDup(false) })
	}

	help := tview.NewTextView().
		SetText(" Enter: connect / open-close folder   Tab: switch focus   arrows/mouse: navigate").
		SetTextColor(tcell.ColorDimGray)

	// Split the main area: servers on the left (70%), env files on the right.
	split := tview.NewFlex().
		AddItem(serverList, 0, 7, true).
		AddItem(envList, 0, 3, false)

	root := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(bar, 1, 0, false).
		AddItem(split, 0, 1, true).
		AddItem(help, 1, 0, false)

	app.SetInputCapture(func(e *tcell.EventKey) *tcell.EventKey {
		if len(modals) > 0 {
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
		if app.GetFocus() == serverList || app.GetFocus() == envList {
			switch e.Rune() {
			case 'n':
				showServerForm()
				return nil
			case 'e':
				showEnvFileForm()
				return nil
			case 'd':
				if app.GetFocus() == envList {
					duplicateCurrentEnv()
					return nil
				}
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
	// behind can be clicked.
	app.SetMouseCapture(func(ev *tcell.EventMouse, act tview.MouseAction) (*tcell.EventMouse, tview.MouseAction) {
		if len(modals) == 0 {
			return ev, act
		}
		x, y, w, h := modals[len(modals)-1].prim.GetRect()
		mx, my := ev.Position()
		if mx < x || mx >= x+w || my < y || my >= y+h {
			return nil, 0
		}
		return ev, act
	})
	return root
}

// showModal opens p as a centered dialog on top of everything else.
func showModal(name string, p tview.Primitive, w, h int) {
	modals = append(modals, modalEntry{name, p})
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

// refreshData redraws both panes straight from the text files. It touches
// nothing but local disk, so it is safe on the path to the first frame.
func refreshData() {
	servers = loadServers()
	rebuildServerList()
	refreshEnvList()
}

func refreshServers() {
	refreshData()
	refreshAlive(servers)
}

// rebuildServerList redraws the server pane: servers with a live connection
// first (hoisted out of their folders so they stay reachable while folders are
// shut), then the remaining top-level servers, then each group as a
// collapsible folder in order of first appearance. Groups are folded shut the
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

	var groups []string
	seen := map[string]bool{}
	for _, s := range servers {
		if connected[s.Name] {
			addServer(s, "")
		}
	}
	for _, s := range servers {
		if connected[s.Name] {
			continue
		}
		if s.Group == "" {
			addServer(s, "")
		} else if !seen[s.Group] {
			seen[s.Group] = true
			groups = append(groups, s.Group)
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

func refreshEnvList() {
	envList.Clear()
	files := envFiles()
	for _, f := range files {
		envList.AddItem(" "+filepath.Base(f), "", 0, nil)
	}
	if len(files) == 0 {
		envList.AddItem("(no env files)", "", 0, nil)
	}
}

func selectEnvFile(path string) {
	dialog("envAction",
		fmt.Sprintf("“%s”", filepath.Base(path)),
		[]string{"Edit", "Duplicate", "Delete", "Cancel"},
		func(label string) {
			switch label {
			case "Edit":
				showEnvEditForm(path)
			case "Duplicate":
				newEnvFileForm(path)
			case "Delete":
				dialog("envDelete",
					fmt.Sprintf("Delete “%s”?\nThis cannot be undone.", filepath.Base(path)),
					[]string{"Delete", "Cancel"},
					func(label string) {
						if label != "Delete" {
							return
						}
						if err := os.Remove(path); err != nil {
							errModal(err.Error())
							return
						}
						refreshEnvList()
					})
			}
		})
}

func showEnvEditForm(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		errModal(err.Error())
		return
	}
	form := tview.NewForm().
		AddTextArea("Content", string(data), 60, 8, 0, nil)
	styleForm(form)
	form.AddButton("Save", func() {
		content := form.GetFormItemByLabel("Content").(*tview.TextArea).GetText()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			errModal(err.Error())
			return
		}
		closeModal()
	})
	form.AddButton("Cancel", closeModal)
	form.SetCancelFunc(closeModal)
	form.SetBorder(true)
	form.SetTitle(fmt.Sprintf(" Edit %s ", filepath.Base(path)))
	showModal("envEdit", form, 76, 14)
}

func selectServer(s Server) {
	if !alive(s) {
		showEnvChooser(s)
		return
	}
	dialog("reuse",
		fmt.Sprintf("A live connection to “%s” already exists.\nReuse it or create a new one?", s.Name),
		[]string{"Reuse", "New connection", "Cancel"},
		func(label string) {
			switch label {
			case "Reuse":
				connect(s, "", true)
			case "New connection":
				showEnvChooser(s)
			}
		})
}

func showEnvChooser(s Server) {
	files := envFiles()
	l := tview.NewList().ShowSecondaryText(false)
	l.SetBorder(true).SetTitle(fmt.Sprintf(" Env file for %s ", s.Name))
	for _, f := range files {
		f := f
		l.AddItem(filepath.Base(f), "", 0, func() {
			closeModal()
			connect(s, f, false)
		})
	}
	l.AddItem("(plain shell — no env file)", "", 0, func() {
		closeModal()
		connect(s, "", false)
	})
	l.AddItem("Cancel", "", 0, closeModal)
	l.SetDoneFunc(closeModal)
	showModal("env", l, 50, len(files)+4)
}

func showServerForm() {
	form := tview.NewForm().
		AddInputField("Name", "", 30, nil, nil).
		AddInputField("Target (user@host)", "", 30, nil, nil).
		AddInputField("Port", "22", 6, nil, nil).
		AddInputField("Group (optional)", "", 30, nil, nil)
	styleForm(form)
	form.AddButton("Save", func() {
		name := strings.TrimSpace(form.GetFormItemByLabel("Name").(*tview.InputField).GetText())
		target := strings.TrimSpace(form.GetFormItemByLabel("Target (user@host)").(*tview.InputField).GetText())
		port := strings.TrimSpace(form.GetFormItemByLabel("Port").(*tview.InputField).GetText())
		group := strings.TrimSpace(form.GetFormItemByLabel("Group (optional)").(*tview.InputField).GetText())
		if name == "" || strings.ContainsAny(name, " \t/") {
			errModal("Name is required and may not contain spaces or slashes.")
			return
		}
		if target == "" || strings.ContainsAny(target, " \t") {
			errModal("Target is required, e.g. deploy@203.0.113.10")
			return
		}
		if _, err := strconv.Atoi(port); err != nil {
			errModal("Port must be a number.")
			return
		}
		if strings.ContainsAny(group, "[]#") {
			errModal("Group may not contain brackets or #.")
			return
		}
		path := filepath.Join(baseDir, serversFileName)
		data, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			errModal(err.Error())
			return
		}
		content := insertServerLine(string(data), fmt.Sprintf("%s %s:%s", name, target, port), group)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			errModal(err.Error())
			return
		}
		collapsed[group] = false // make the new entry visible right away
		closeModal()
		refreshServers()
	})
	form.AddButton("Cancel", closeModal)
	form.SetCancelFunc(closeModal)
	form.SetBorder(true)
	form.SetTitle(" New server ")
	showModal("serverForm", form, 56, 13)
}

func showEnvFileForm() { newEnvFileForm("") }

// duplicateCurrentEnv opens the new-file form prefilled with the env file
// highlighted in the env pane ("d" key or the bar's Duplicate button).
func duplicateCurrentEnv() {
	files := envFiles()
	if i := envList.GetCurrentItem(); i >= 0 && i < len(files) {
		newEnvFileForm(files[i])
	}
}

// newEnvFileForm opens the New-env-file dialog. A non-empty copyPath (the
// Duplicate flow: "d" on a highlighted env file, or the Duplicate button)
// prefills the form with that file's content and a name for the copy.
func newEnvFileForm(copyPath string) {
	name, content := "", envTemplate
	title := " New env file "
	if copyPath != "" {
		data, err := os.ReadFile(copyPath)
		if err != nil {
			errModal(err.Error())
			return
		}
		content = string(data)
		name = strings.TrimSuffix(filepath.Base(copyPath), ".txt") + "_copy"
		title = fmt.Sprintf(" New env file (copy of %s) ", filepath.Base(copyPath))
	}

	form := tview.NewForm().
		AddInputField("File name", name, 30, nil, nil).
		AddTextArea("Content", content, 60, 8, 0, nil)
	styleForm(form)
	form.AddButton("Save", func() {
		name := strings.TrimSpace(form.GetFormItemByLabel("File name").(*tview.InputField).GetText())
		name = filepath.Base(name) // keep it inside baseDir
		if name == "" || name == "." || name == serversFileName || name == configFileName {
			errModal("Please enter a valid file name.")
			return
		}
		if !strings.HasSuffix(name, ".txt") {
			name += ".txt"
		}
		if _, err := os.Stat(filepath.Join(baseDir, name)); err == nil {
			errModal(fmt.Sprintf("“%s” already exists.\nPick another name, or use Edit to change it.", name))
			return
		}
		content := form.GetFormItemByLabel("Content").(*tview.TextArea).GetText()
		if err := os.WriteFile(filepath.Join(baseDir, name), []byte(content), 0o600); err != nil {
			errModal(err.Error())
			return
		}
		closeModal()
		refreshEnvList()
	})
	form.AddButton("Cancel", closeModal)
	form.SetCancelFunc(closeModal)
	form.SetBorder(true)
	form.SetTitle(title)
	// 8-line text area + file-name row + buttons + form padding + border.
	showModal("envForm", form, 76, 16)
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
	showModal(name, f, 56, strings.Count(text, "\n")+8)
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
		// Mirrors List.Draw: with SetSelectedFocusOnly the selected row keeps
		// the normal background while the pane is unfocused.
		if i == m.GetCurrentItem() && m.HasFocus() {
			style = m.selRow
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
		for x := left; x <= right; x++ {
			if x >= ix && x < end {
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
