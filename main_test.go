package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

func TestParseEnvFileAndScript(t *testing.T) {
	p := filepath.Join(t.TempDir(), "env.txt")
	os.WriteFile(p, []byte(`# comment
APP_ENV=staging
GREETING=it's a 'test'

cd /srv/app
echo ready
`), 0o600)

	vars, cmds, err := parseEnvFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(vars) != 2 || vars[0] != "APP_ENV=staging" {
		t.Fatalf("vars = %q", vars)
	}
	if len(cmds) != 2 || cmds[0] != "cd /srv/app" {
		t.Fatalf("cmds = %q", cmds)
	}

	got := buildScript(vars, cmds)
	want := strings.Join([]string{
		`export APP_ENV='staging'`,
		`export GREETING='it'\''s a '\''test'\'''`,
		`cd /srv/app`,
		`echo ready`,
		`printf '\n== Horizon: environment applied ==\n'`,
		`printf 'Variables set:\n'`,
		`printf '  %s=%s\n' 'APP_ENV' "$APP_ENV"`,
		`printf '  %s=%s\n' 'GREETING' "$GREETING"`,
		`printf 'Commands run:\n'`,
		`printf '  %s\n' 'cd /srv/app'`,
		`printf '  %s\n' 'echo ready'`,
		`printf '\n'`,
		`exec "$SHELL" -l`,
	}, "\n")
	if got != want {
		t.Fatalf("script:\n got %s\nwant %s", got, want)
	}
}

// A user command carrying an inline "#" comment (or a trailing "&") must not
// swallow the rest of the script: each part sits on its own line, so the
// final exec of the login shell always survives.
func TestBuildScriptSurvivesInlineComment(t *testing.T) {
	got := buildScript(nil, []string{"echo hi  # just saying hi"})
	if !strings.HasSuffix(got, "\nexec \"$SHELL\" -l") {
		t.Fatalf("login-shell exec lost after inline comment:\n%s", got)
	}
	if strings.Contains(got, "; ") {
		t.Fatalf("script joined with semicolons:\n%s", got)
	}
}

func TestLoadServers(t *testing.T) {
	baseDir = t.TempDir()
	os.WriteFile(filepath.Join(baseDir, serversFileName), []byte(`# comment
web1 deploy@203.0.113.10
db   admin@db.internal:2222
malformed-line
`), 0o600)

	s := loadServers()
	if len(s) != 2 {
		t.Fatalf("servers = %+v", s)
	}
	if s[0] != (Server{"web1", "deploy@203.0.113.10", "22", ""}) {
		t.Fatalf("s[0] = %+v", s[0])
	}
	if s[1] != (Server{"db", "admin@db.internal", "2222", ""}) {
		t.Fatalf("s[1] = %+v", s[1])
	}
}

func TestLoadServersGroups(t *testing.T) {
	baseDir = t.TempDir()
	os.WriteFile(filepath.Join(baseDir, serversFileName), []byte(`jump ops@jump.example.com
[production]
web1 deploy@203.0.113.10
[ staging ]
web2 deploy@203.0.113.11:2222
`), 0o600)

	s := loadServers()
	if len(s) != 3 {
		t.Fatalf("servers = %+v", s)
	}
	if s[0] != (Server{"jump", "ops@jump.example.com", "22", ""}) {
		t.Fatalf("s[0] = %+v", s[0])
	}
	if s[1] != (Server{"web1", "deploy@203.0.113.10", "22", "production"}) {
		t.Fatalf("s[1] = %+v", s[1])
	}
	if s[2] != (Server{"web2", "deploy@203.0.113.11", "2222", "staging"}) {
		t.Fatalf("s[2] = %+v", s[2])
	}
}

// serverRowLines rebuilds the pane and returns just the server rows' text
// (skipping folder headers), in display order.
func serverRowLines(t *testing.T) []string {
	t.Helper()
	rebuildServerList()
	var out []string
	for i, r := range serverRows {
		if r.header != "" {
			continue
		}
		main, _ := serverList.GetItemText(i)
		out = append(out, main)
	}
	return out
}

// The name, address and probe columns must begin at the same rune offset on
// every server row — top-level and folder-indented, connected and not,
// probe-answered and not — so the list reads as tidy columns.
func TestServerRowColumnsAlign(t *testing.T) {
	baseDir = t.TempDir()
	if err := ensureFiles(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(baseDir, serversFileName), []byte(
		"jump  ops@jump.example.com\n"+
			"[production]\n"+
			"web1  deploy@203.0.113.10\n"+
			"longhostname  deploy@10.0.0.9:2222\n"), 0o600)
	loadConfig()
	config.Ping, config.PortCheck = true, false

	app = tview.NewApplication()
	pages = tview.NewPages()
	buildMain()
	servers = loadServers()
	collapsed["production"] = false

	aliveMu.Lock()
	aliveState = map[string]bool{"jump": true}
	aliveMu.Unlock()
	probeMu.Lock()
	probeText = map[string]string{
		"jump":         "14.2 ms",
		"web1":         "no ping — port 22 open",
		"longhostname": "", // not answered yet
	}
	probeMu.Unlock()

	lines := serverRowLines(t)
	if len(lines) != 3 {
		t.Fatalf("rows = %q", lines)
	}

	// The address column starts one space past the widest budgeted name. The
	// widest name is "longhostname" with its 2-space folder indent (14), so the
	// address begins at rune 1 (leading space) + 14 + 1 = 16 on every row.
	const addrCol = 16
	for _, ln := range lines {
		r := []rune(ln)
		if len(r) <= addrCol || r[addrCol-1] != ' ' {
			t.Fatalf("no gap before address column in %q", ln)
		}
		// The address text (a host) must sit exactly at addrCol.
		if r[addrCol] == ' ' {
			t.Fatalf("address column not filled at %d in %q", addrCol, ln)
		}
	}

	// Every server line begins with a single leading space then the name, so
	// the indented folder members and the top-level row share a name column.
	for _, ln := range lines {
		if !strings.HasPrefix(ln, " ") || strings.HasPrefix(ln, "  ▸") {
			t.Fatalf("unexpected row prefix: %q", ln)
		}
	}

	// The connected marker only shows on jump, and its probe is padded so the
	// marker lands past the probe column; the unconnected rows carry no marker.
	if !strings.Contains(lines[0], "14.2 ms") || !strings.Contains(lines[0], "● connected") {
		t.Fatalf("connected row = %q", lines[0])
	}
	for _, ln := range lines[1:] {
		if strings.Contains(ln, "● connected") {
			t.Fatalf("unexpected marker on %q", ln)
		}
	}
}

// Hand-edited entries that would escape the sockets folder, read as an ssh
// or ping option, or collide with an earlier name are dropped on load.
func TestLoadServersSkipsUnsafeEntries(t *testing.T) {
	baseDir = t.TempDir()
	os.WriteFile(filepath.Join(baseDir, serversFileName), []byte(`ok deploy@203.0.113.10
../../evil deploy@203.0.113.11
-dash deploy@203.0.113.12
sub/dir deploy@203.0.113.13
ok deploy@203.0.113.14
badtarget -oProxyCommand=payload
badport deploy@203.0.113.15:-1
`), 0o600)

	s := loadServers()
	if len(s) != 1 {
		t.Fatalf("servers = %+v", s)
	}
	if s[0] != (Server{"ok", "deploy@203.0.113.10", "22", ""}) {
		t.Fatalf("s[0] = %+v", s[0])
	}
}

func TestValidName(t *testing.T) {
	for _, name := range []string{"web1", "db.internal", "a_b-c"} {
		if !validName(name) {
			t.Errorf("validName(%q) = false", name)
		}
	}
	for _, name := range []string{"", ".", "..", "-dash", "[group", "a/b", "../x", "a b"} {
		if validName(name) {
			t.Errorf("validName(%q) = true", name)
		}
	}
}

func TestValidPort(t *testing.T) {
	for _, p := range []string{"1", "22", "65535"} {
		if !validPort(p) {
			t.Errorf("validPort(%q) = false", p)
		}
	}
	for _, p := range []string{"", "0", "-1", "65536", "abc", "2 2"} {
		if validPort(p) {
			t.Errorf("validPort(%q) = true", p)
		}
	}
}

func TestInsertServerLine(t *testing.T) {
	base := "jump ops@jump.example.com\n[production]\nweb1 deploy@10.0.0.1:22\n[staging]\nweb2 deploy@10.0.0.2:22\n"

	cases := []struct{ name, group, want string }{
		{"top level goes before first group", "",
			"jump ops@jump.example.com\nnew u@h:22\n[production]\nweb1 deploy@10.0.0.1:22\n[staging]\nweb2 deploy@10.0.0.2:22\n"},
		{"existing group extends its section", "production",
			"jump ops@jump.example.com\n[production]\nweb1 deploy@10.0.0.1:22\nnew u@h:22\n[staging]\nweb2 deploy@10.0.0.2:22\n"},
		{"last group appends at end", "staging",
			"jump ops@jump.example.com\n[production]\nweb1 deploy@10.0.0.1:22\n[staging]\nweb2 deploy@10.0.0.2:22\nnew u@h:22\n"},
		{"new group appended with header", "db",
			"jump ops@jump.example.com\n[production]\nweb1 deploy@10.0.0.1:22\n[staging]\nweb2 deploy@10.0.0.2:22\n[db]\nnew u@h:22\n"},
	}
	for _, c := range cases {
		if got := insertServerLine(base, "new u@h:22", c.group); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}

	if got := insertServerLine("", "new u@h:22", ""); got != "new u@h:22\n" {
		t.Errorf("empty file: %q", got)
	}
	if got := insertServerLine("", "new u@h:22", "prod"); got != "[prod]\nnew u@h:22\n" {
		t.Errorf("empty file with group: %q", got)
	}
}

func TestScrollBarGeometry(t *testing.T) {
	newPane := func(lines int) *macScrollList {
		l := tview.NewList()
		if lines == 1 {
			l.ShowSecondaryText(false)
		}
		l.SetBorderPadding(1, 0, 1, 1)
		l.SetBorder(true)
		m := newMacScrollList(l, lines)
		m.SetRect(0, 0, 40, 12) // inner: x=2 y=2 w=36 h=9
		return m
	}

	m := newPane(1)
	for i := 0; i < 5; i++ {
		m.AddItem("item", "", 0, nil)
	}
	if _, _, _, _, _, ok := m.barGeometry(); ok {
		t.Fatal("no bar expected when the list fits")
	}
	for i := 0; i < 15; i++ {
		m.AddItem("item", "", 0, nil)
	}
	barX, top, height, thumbTop, thumbH, ok := m.barGeometry()
	if !ok {
		t.Fatal("bar expected for 20 items in 9 rows")
	}
	// Bar sits in the right padding column, spans the inner height, and the
	// thumb starts at the top of the track while scrolled to item 0.
	if barX != 38 || top != 2 || height != 9 || thumbTop != 3 {
		t.Fatalf("bar = x%d top%d h%d thumbTop%d", barX, top, height, thumbTop)
	}
	if thumbH != 7*9/20 {
		t.Fatalf("thumbH = %d", thumbH)
	}
	m.SetOffset(11, 0) // bottom: maxOff = 20-9
	if _, _, _, thumbTop, _, _ = m.barGeometry(); thumbTop != 3+7-thumbH {
		t.Fatalf("thumb not at track bottom: %d", thumbTop)
	}

	// Two-line items: 9 rows show 4 items, so 4 items fit and 5 overflow.
	m = newPane(2)
	for i := 0; i < 4; i++ {
		m.AddItem("item", "", 0, nil)
	}
	if _, _, _, _, _, ok := m.barGeometry(); ok {
		t.Fatal("no bar expected for 4 two-line items")
	}
	m.AddItem("item", "", 0, nil)
	if _, _, _, _, _, ok := m.barGeometry(); !ok {
		t.Fatal("bar expected for 5 two-line items")
	}
}

// Every item's background must run the full width of the pane, from border to
// border — both the selected row and the rest, whatever their text width — and
// the border columns themselves must keep the pane's own background.
func TestRowBandsSpanFullWidth(t *testing.T) {
	m := newMacScrollList(tview.NewList().ShowSecondaryText(false), 1)
	m.setRowStyles(rowStyle, selectedRowStyle)
	m.SetSelectedFocusOnly(true).SetHighlightFullLine(true)
	m.SetBackgroundColor(tcell.ColorWhite)
	m.SetBorderPadding(1, 0, 1, 1)
	m.SetBorder(true)
	m.AddItem("short", "", 0, nil)
	m.AddItem("a considerably longer entry", "", 0, nil)
	m.AddItem("tagged [::b]bold tail[::-]", "", 0, nil)
	m.SetRect(0, 0, 40, 10)
	m.Focus(nil) // the selection only paints while the pane has focus

	sc := tcell.NewSimulationScreen("UTF-8")
	if err := sc.Init(); err != nil {
		t.Fatal(err)
	}
	sc.SetSize(40, 10)
	m.Draw(sc)

	bgAt := func(x, y int) tcell.Color {
		_, _, style, _ := sc.GetContent(x, y)
		_, bg, _ := style.Decompose()
		return bg
	}
	bx, _, bw, _ := m.GetRect()
	_, iy, _, _ := m.GetInnerRect()
	// Item 0 is the current one, so it wears the selected background; the rest
	// wear the normal one.
	for i, want := range []tcell.Style{selectedRowStyle, rowStyle, rowStyle} {
		_, wantBg, _ := want.Decompose()
		y := iy + i
		for x := bx + 1; x <= bx+bw-2; x++ {
			if got := bgAt(x, y); got != wantBg {
				t.Fatalf("row %d col %d bg = %v, want %v", i, x, got, wantBg)
			}
		}
		for _, x := range []int{bx, bx + bw - 1} {
			if got := bgAt(x, y); got != tcell.ColorWhite {
				t.Fatalf("row %d border col %d bg = %v, want the pane's white", i, x, got)
			}
		}
	}
	// Rows past the last item stay plain pane, no band.
	if got := bgAt(bx+1, iy+3); got != tcell.ColorWhite {
		t.Fatalf("empty row bg = %v, want the pane's white", got)
	}
}

// unreachable only fires on the probes' final verdict, and only while a
// probe is configured to run at all.
func TestUnreachable(t *testing.T) {
	oldCfg := config
	defer func() { config = oldCfg }()
	probeMu.Lock()
	probeText = map[string]string{
		"dead":    probeUnreachable,
		"fine":    "14.2 ms",
		"pending": "pinging…",
	}
	probeMu.Unlock()

	config = Config{Ping: true, PingCount: 3}
	if !unreachable("dead") {
		t.Fatal("failed probe not reported unreachable")
	}
	for _, name := range []string{"fine", "pending", "unknown"} {
		if unreachable(name) {
			t.Fatalf("%q reported unreachable", name)
		}
	}

	// With every probe off nothing can be called unreachable, whatever a
	// stale probeText holds.
	config = Config{PingCount: 3}
	if unreachable("dead") {
		t.Fatal("unreachable with probes off")
	}
}

// A row given a tint override must wear it across the whole band — under its
// own text as well as the padding — while the selected row keeps the
// selection style and the other rows the normal one.
func TestRowTintPaintsFullRow(t *testing.T) {
	m := newMacScrollList(tview.NewList().ShowSecondaryText(false), 1)
	m.setRowStyles(rowStyle, selectedRowStyle)
	m.SetSelectedFocusOnly(true).SetHighlightFullLine(true)
	m.SetBackgroundColor(tcell.ColorWhite)
	m.SetBorderPadding(1, 0, 1, 1)
	m.SetBorder(true)
	m.AddItem("selected", "", 0, nil)
	m.AddItem("dead server", "", 0, nil)
	m.AddItem("plain", "", 0, nil)
	m.setRowTint(func(item int) (tcell.Style, bool) {
		if item == 1 {
			return unreachableRowStyle, true
		}
		return tcell.Style{}, false
	})
	m.SetRect(0, 0, 40, 10)
	m.Focus(nil)

	sc := tcell.NewSimulationScreen("UTF-8")
	if err := sc.Init(); err != nil {
		t.Fatal(err)
	}
	sc.SetSize(40, 10)
	m.Draw(sc)

	bgAt := func(x, y int) tcell.Color {
		_, _, style, _ := sc.GetContent(x, y)
		_, bg, _ := style.Decompose()
		return bg
	}
	bx, _, bw, _ := m.GetRect()
	_, iy, _, _ := m.GetInnerRect()
	for i, want := range []tcell.Style{selectedRowStyle, unreachableRowStyle, rowStyle} {
		_, wantBg, _ := want.Decompose()
		for x := bx + 1; x <= bx+bw-2; x++ {
			if got := bgAt(x, iy+i); got != wantBg {
				t.Fatalf("row %d col %d bg = %v, want %v", i, x, got, wantBg)
			}
		}
	}
	// The tinted row's text survives the recolour.
	var text []rune
	for x := bx + 2; x < bx+2+len("dead server"); x++ {
		ch, _, _, _ := sc.GetContent(x, iy+1)
		text = append(text, ch)
	}
	if string(text) != "dead server" {
		t.Fatalf("tinted row text = %q", string(text))
	}
	// Selecting the tinted row swaps it to the selection style.
	m.SetCurrentItem(1)
	m.Draw(sc)
	_, selBg, _ := selectedRowStyle.Decompose()
	if got := bgAt(bx+1, iy+1); got != selBg {
		t.Fatalf("selected tinted row bg = %v, want %v", got, selBg)
	}
}

func TestParseConfig(t *testing.T) {
	if c := parseConfig(""); c.Ping || c.PingCount != 3 || c.PortCheck {
		t.Fatalf("defaults = %+v", c)
	}
	if c := parseConfig(configTemplate); c.Ping || c.PingCount != 3 || c.PortCheck {
		t.Fatalf("template = %+v", c)
	}
	c := parseConfig("# comment\nping = on\nping_count = 5\nport_check = yes\njunk line\nunknown=1\n")
	if !c.Ping || c.PingCount != 5 || !c.PortCheck {
		t.Fatalf("parsed = %+v", c)
	}
	// Invalid count falls back to the default; the flags stay off unless truthy.
	c = parseConfig("ping=maybe\nping_count=zero\nport_check=sometimes\n")
	if c.Ping || c.PingCount != 3 || c.PortCheck {
		t.Fatalf("invalid values = %+v", c)
	}
}

func TestHostOf(t *testing.T) {
	if h := hostOf("deploy@203.0.113.10"); h != "203.0.113.10" {
		t.Fatalf("with user = %q", h)
	}
	if h := hostOf("db.internal"); h != "db.internal" {
		t.Fatalf("without user = %q", h)
	}
}

// portOpen must answer for a listener that is up and for a port that is not,
// without taking anywhere near the full timeout on the failing case.
func TestPortOpen(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	if !portOpen(Server{Target: "user@127.0.0.1", Port: port}) {
		t.Fatal("live listener reported closed")
	}
	ln.Close()
	if portOpen(Server{Target: "127.0.0.1", Port: port}) {
		t.Fatal("closed port reported open")
	}
}

func TestProbeServerFallsBackToPortCheck(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	s := Server{Target: "127.0.0.1", Port: port}

	oldPing := pingProbe
	defer func() { pingProbe = oldPing }()
	// Loopback answers ICMP, so stub the ping leg into always failing —
	// the point of the fallback is exactly the host that drops ICMP.
	pingProbe = func(Server, Config) (string, bool) { return "", false }

	// Port check off: a failed ping is the whole answer.
	if got := probeServer(s, Config{Ping: true, PingCount: 1}); got != "not reachable" {
		t.Fatalf("ping only = %q", got)
	}

	// Port check on: the open port rescues an unreachable-by-ICMP host.
	cfg := Config{Ping: true, PingCount: 1, PortCheck: true}
	if want, got := "no ping — port "+port+" open", probeServer(s, cfg); got != want {
		t.Fatalf("fallback = %q, want %q", got, want)
	}

	// Port check alone, with ping off.
	if want, got := "port "+port+" open", probeServer(s, Config{PingCount: 3, PortCheck: true}); got != want {
		t.Fatalf("port only = %q, want %q", got, want)
	}

	// Neither answers.
	ln.Close()
	if got := probeServer(s, cfg); got != "not reachable" {
		t.Fatalf("both failed = %q", got)
	}
}

func TestParsePingAvg(t *testing.T) {
	mac := "--- 203.0.113.10 ping statistics ---\n" +
		"3 packets transmitted, 3 packets received, 0.0% packet loss\n" +
		"round-trip min/avg/max/stddev = 13.6/14.2/14.9/0.5 ms\n"
	linux := "--- db.internal ping statistics ---\n" +
		"3 packets transmitted, 3 received, 0% packet loss, time 2003ms\n" +
		"rtt min/avg/max/mdev = 20.1/21.0/22.0/0.5 ms\n"

	if avg, ok := parsePingAvg(mac); !ok || avg != "14.2" {
		t.Fatalf("mac avg = %q %v", avg, ok)
	}
	if avg, ok := parsePingAvg(linux); !ok || avg != "21.0" {
		t.Fatalf("linux avg = %q %v", avg, ok)
	}
	if _, ok := parsePingAvg("ping: cannot resolve nope.invalid: Unknown host\n"); ok {
		t.Fatal("expected no match for error output")
	}
}
