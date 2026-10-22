package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

func TestParsePortSpec(t *testing.T) {
	cases := map[string]PortSpec{
		"5432":                      {"5432", "localhost", "5432", ""},
		"13000:3000":                {"13000", "localhost", "3000", ""},
		"9092:broker.internal:9092": {"9092", "broker.internal", "9092", ""},
		" 05432 ":                   {"5432", "localhost", "5432", ""},
		"*:443":                     {"443", "localhost", "443", "*"},
		"*:8443:443":                {"8443", "localhost", "443", "*"},
		"*:443:web.internal:443":    {"443", "web.internal", "443", "*"},
	}
	for in, want := range cases {
		if got, ok := parsePortSpec(in); !ok || got != want {
			t.Errorf("parsePortSpec(%q) = %+v, %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "0", "70000", "abc", "1:2:3:4", "80:-oProxy:80", "80:a b:80", "80::80", "80:user@h:80", "*", "*:*:443", "0.0.0.0:443:h:443"} {
		if _, ok := parsePortSpec(in); ok {
			t.Errorf("parsePortSpec(%q) accepted", in)
		}
	}
}

// String is what goes into ports.txt, so it must read back to the same spec.
func TestPortSpecRoundTrip(t *testing.T) {
	for in, want := range map[string]string{"5432": "5432", "13000:3000": "13000:3000",
		"5432:localhost:5432": "5432", "9092:broker:9092": "9092:broker:9092",
		"*:443": "*:443", "*:8443:localhost:443": "*:8443:443", "*:443:web:443": "*:443:web:443"} {
		p, _ := parsePortSpec(in)
		if p.String() != want {
			t.Errorf("String(%q) = %q, want %q", in, p.String(), want)
		}
		if q, ok := parsePortSpec(p.String()); !ok || q != p {
			t.Errorf("round trip of %q = %+v", in, q)
		}
	}
	p, _ := parsePortSpec("13000:3000")
	if p.arg() != "13000:localhost:3000" || p.describe() != "13000 → 3000" {
		t.Fatalf("arg %q describe %q", p.arg(), p.describe())
	}
	p, _ = parsePortSpec("*:443")
	if p.arg() != "*:443:localhost:443" || p.describe() != "443 ⚠ network" {
		t.Fatalf("open: arg %q describe %q", p.arg(), p.describe())
	}
	if q, _ := parsePortSpec("443"); q == p {
		t.Fatal("the open and the localhost forward compare equal")
	}
	if got := forwardSummary([]PortSpec{q443(), p}); got != "⇄ 443,443⚠" {
		t.Fatalf("summary = %q", got)
	}
}

func TestLoadPorts(t *testing.T) {
	baseDir = t.TempDir()
	os.WriteFile(filepath.Join(baseDir, portsFileName), []byte(portsTemplate+`postgres 5432
grafana  13000:3000
bad      notaport
postgres 6543
-x       80
`), 0o600)
	got := loadPorts()
	if len(got) != 2 || got[0].Name != "postgres" || got[0].Spec.Local != "5432" ||
		got[1].Name != "grafana" || got[1].Spec.Remote != "3000" {
		t.Fatalf("ports = %+v", got)
	}
}

func TestPortLineEdits(t *testing.T) {
	data := "# saved ports\npostgres 5432\n\nredis 6379\n"
	if got := appendPortLine(data, "web 8080"); got != "# saved ports\npostgres 5432\n\nredis 6379\nweb 8080\n" {
		t.Fatalf("append = %q", got)
	}
	if got := appendPortLine("", "web 8080"); got != "web 8080\n" {
		t.Fatalf("append to empty = %q", got)
	}
	if got := replacePortLine(data, "redis", "cache 16379:6379"); got != "# saved ports\npostgres 5432\n\ncache 16379:6379\n" {
		t.Fatalf("replace = %q", got)
	}
	if got := replacePortLine(data, "postgres", ""); got != "# saved ports\n\nredis 6379\n" {
		t.Fatalf("remove = %q", got)
	}
	if got := replacePortLine(data, "nope", ""); got != data {
		t.Fatalf("missing name changed the file: %q", got)
	}
}

func TestUniquePortName(t *testing.T) {
	list := []Port{{Name: "port8080"}, {Name: "port80802"}}
	if got := uniquePortName("port8080", list); got != "port80803" {
		t.Fatalf("got %q", got)
	}
	if got := uniquePortName("port9000", list); got != "port9000" {
		t.Fatalf("got %q", got)
	}
}

func TestDiffForwards(t *testing.T) {
	a, _ := parsePortSpec("5432")
	b, _ := parsePortSpec("6379")
	b2, _ := parsePortSpec("6379:cache:6379")
	c, _ := parsePortSpec("8080")
	add, cancel := diffForwards([]PortSpec{a, b}, []PortSpec{a, b2, c})
	if len(add) != 2 || add[0] != b2 || add[1] != c {
		t.Fatalf("add = %+v", add)
	}
	if len(cancel) != 1 || cancel[0] != b {
		t.Fatalf("cancel = %+v", cancel)
	}
}

func TestForwardsRecord(t *testing.T) {
	baseDir = t.TempDir()
	ensureFiles()
	s := Server{Name: "web1", Target: "deploy@h", Port: "22"}
	a, _ := parsePortSpec("5432")
	b, _ := parsePortSpec("13000:3000")
	if err := writeForwards(s, []PortSpec{a, b}); err != nil {
		t.Fatal(err)
	}
	if got := readForwards(s); len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatalf("read = %+v", got)
	}
	// A record written after the dead-master check began belongs to a master
	// just started, and survives.
	clearStaleForwards(s, time.Now().Add(-time.Minute))
	if len(readForwards(s)) != 2 {
		t.Fatal("fresh record was cleared")
	}
	clearStaleForwards(s, time.Now().Add(time.Minute))
	if len(readForwards(s)) != 0 {
		t.Fatal("stale record survived")
	}
	writeForwards(s, []PortSpec{a})
	writeForwards(s, nil)
	if _, err := os.Stat(forwardsPath(s)); !os.IsNotExist(err) {
		t.Fatal("empty set left a record file")
	}
}

func TestForwardChoices(t *testing.T) {
	pg, _ := parsePortSpec("5432")
	rd, _ := parsePortSpec("6379")
	extra, _ := parsePortSpec("9000")
	got := forwardChoices([]Port{{"postgres", pg}, {"redis", rd}}, []PortSpec{rd, extra})
	if len(got) != 3 {
		t.Fatalf("choices = %+v", got)
	}
	if got[0].on || !got[1].on || !got[1].active || got[2].label != "custom" || !got[2].on {
		t.Fatalf("choices = %+v", got)
	}
}

func TestParseCustomPorts(t *testing.T) {
	specs, bad := parseCustomPorts(" 8080, 13000:3000  9092:broker:9092,,x ")
	if len(specs) != 3 || specs[1].Remote != "3000" || specs[2].Host != "broker" {
		t.Fatalf("specs = %+v", specs)
	}
	if len(bad) != 1 || bad[0] != "x" {
		t.Fatalf("bad = %q", bad)
	}
}

// applyForwards with no live master starts one, adds what is wanted, skips
// local ports that are taken, and records exactly what got forwarded.
func TestApplyForwards(t *testing.T) {
	baseDir = t.TempDir()
	ensureFiles()
	app = tview.NewApplication()
	pages = tview.NewPages()
	pages.AddPage("main", buildMain(), true, true)
	modals = nil
	s := Server{Name: "web1", Target: "deploy@203.0.113.10", Port: "22"}

	var mu sync.Mutex
	var calls []string
	started := false
	oldMux, oldFree, oldStart := muxRun, localPortFree, startMaster
	defer func() { muxRun, localPortFree, startMaster = oldMux, oldFree, oldStart }()
	muxRun = func(args []string) error {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, args[3]+" "+args[5])
		if args[5] == "7000:localhost:7000" {
			return errors.New("Port forwarding failed")
		}
		return nil
	}
	localPortFree = func(p PortSpec) error {
		if p.Local == "6379" {
			return errors.New("address already in use")
		}
		return nil
	}
	startMaster = func(Server) error { started = true; return nil }

	var want []PortSpec
	for _, in := range []string{"5432", "6379", "7000", "13000:3000"} {
		p, _ := parsePortSpec(in)
		want = append(want, p)
	}
	applyForwards(s, want)
	if !started || !isAlive("web1") {
		t.Fatal("no master was started")
	}
	forwardsBusy.Wait()
	got := readForwards(s)
	if len(got) != 2 || got[0].Local != "5432" || got[1].Local != "13000" {
		t.Fatalf("recorded = %+v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(calls, ",") != "forward 5432:localhost:5432,forward 7000:localhost:7000,forward 13000:localhost:3000" {
		t.Fatalf("calls = %q", calls)
	}
	aliveMu.Lock()
	aliveState = map[string]bool{}
	aliveMu.Unlock()
}

// The dialog lists the saved ports, ticks them with Enter, and grows a row for
// a live forward the list doesn't know.
func TestForwardDialogRows(t *testing.T) {
	baseDir = t.TempDir()
	ensureFiles()
	os.WriteFile(filepath.Join(baseDir, portsFileName), []byte("postgres 5432\ngrafana 13000:3000\n"), 0o600)
	app = tview.NewApplication()
	pages = tview.NewPages()
	pages.AddPage("main", buildMain(), true, true)
	pages.SetRect(0, 0, 120, 40)
	modals = nil
	reloadPorts()
	s := Server{Name: "web1", Target: "deploy@h", Port: "22"}
	extra, _ := parsePortSpec("9000")
	writeForwards(s, []PortSpec{extra})
	aliveMu.Lock()
	aliveState = map[string]bool{"web1": true}
	aliveMu.Unlock()
	defer func() {
		aliveMu.Lock()
		aliveState = map[string]bool{}
		aliveMu.Unlock()
	}()

	showForwardDialog(s)
	defer closeModal()
	f := modals[len(modals)-1].prim.(*tview.Flex)
	list := f.GetItem(1).(*macScrollList)
	rows := func() []string {
		var out []string
		for i := 0; i < list.GetItemCount(); i++ {
			m, _ := list.GetItemText(i)
			out = append(out, m)
		}
		return out
	}
	got := rows()
	if len(got) != 3 || !strings.HasPrefix(got[0], "[ []") || !strings.Contains(got[1], "13000 → 3000") ||
		!strings.Contains(got[2], "custom") || !strings.Contains(got[2], "(active)") {
		t.Fatalf("rows = %q", got)
	}
	list.SetCurrentItem(0)
	list.InputHandler()(tcellKey(' '), func(tview.Primitive) {})
	if got := rows(); !strings.HasPrefix(got[0], "[x[]") {
		t.Fatalf("after toggle = %q", got[0])
	}
}

func tcellKey(r rune) *tcell.EventKey { return tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone) }

// Below 1024 only a root master can open the local end; anyone else is told
// to pick a high local port, with the remote side kept.
func TestCheckLocalPortPrivileged(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
	baseDir = t.TempDir()
	ensureFiles()
	oldFree := localPortFree
	defer func() { localPortFree = oldFree }()
	localPortFree = func(PortSpec) error { return nil }
	s := Server{Name: "web1", Target: "deploy@h", Port: "22"}

	p, _ := parsePortSpec("443")
	err := checkLocalPort(s, p, false)
	if err == nil || !strings.Contains(err.Error(), "below 1024") || !strings.Contains(err.Error(), "8443:443") {
		t.Fatalf("443 = %v", err)
	}
	p, _ = parsePortSpec("80:web.internal:80")
	if err := checkLocalPort(s, p, false); err == nil || !strings.Contains(err.Error(), "8080:web.internal:80") {
		t.Fatalf("80 = %v", err)
	}
	p, _ = parsePortSpec("8443:443")
	if err := checkLocalPort(s, p, false); err != nil {
		t.Fatalf("8443 = %v", err)
	}
	// Opted in to all interfaces, the low port goes on to the real bind check.
	p, _ = parsePortSpec("*:443")
	if err := checkLocalPort(s, p, false); err != nil {
		t.Fatalf("*:443 = %v", err)
	}
}

func q443() PortSpec { p, _ := parsePortSpec("443"); return p }

// The open forward is recorded and read back as open, so the dialog and the
// row keep flagging it.
func TestForwardsRecordKeepsBind(t *testing.T) {
	baseDir = t.TempDir()
	ensureFiles()
	s := Server{Name: "web1", Target: "deploy@h", Port: "22"}
	p, _ := parsePortSpec("*:443")
	writeForwards(s, []PortSpec{p})
	if got := readForwards(s); len(got) != 1 || !got[0].open() {
		t.Fatalf("read = %+v", got)
	}
}

// Custom shows what it will forward as it is typed, and on macOS ticks All
// interfaces for a port below 1024 until the user sets the box.
func TestForwardDialogCustomField(t *testing.T) {
	newMainForTest(t)
	app.SetFocus(serverList)
	showForwardDialog(Server{Name: "web1", Target: "deploy@h", Port: "22"})
	defer closeModal()
	form := modals[len(modals)-1].prim.(*tview.Flex).GetItem(2).(*tview.Form)
	custom := form.GetFormItemByLabel("Custom").(*tview.InputField)
	preview := form.GetFormItem(1).(*tview.TextView)
	checks := form.GetFormItem(2).(*checkRow)
	open := checks.boxes[1]
	text := func() string { return strings.TrimSpace(preview.GetText(true)) }

	custom.SetText("8080, 13000:3000")
	if text() != "→ 8080, 13000 → 3000" || open.IsChecked() || checks.shown[1] {
		t.Fatalf("high ports: %q, open %v, shown %v", text(), open.IsChecked(), checks.shown[1])
	}
	custom.SetText("8080, nope")
	if !strings.Contains(text(), "not a port: nope") {
		t.Fatalf("bad entry: %q", text())
	}
	if runtime.GOOS != "darwin" {
		return
	}
	custom.SetText("443")
	if !checks.shown[1] || !open.IsChecked() || text() != "→ 443 ⚠ network" {
		t.Fatalf("443: %q, open %v", text(), open.IsChecked())
	}
	// Only the low port is opened to the network; 8080 stays on localhost,
	// and one written as *: keeps its own choice.
	custom.SetText("8080, 443, *:9000")
	if text() != "→ 8080, 443 ⚠ network, 9000 ⚠ network" {
		t.Fatalf("mixed: %q", text())
	}
	open.SetChecked(false) // the user unticks it
	custom.SetText("80")
	if open.IsChecked() || text() != "→ 80" {
		t.Fatalf("after untick: %q, open %v", text(), open.IsChecked())
	}
}

// With ports forwarded the menu offers Stop forwards, and it, x and the
// dialog's Stop all cancel every one of them.
func TestStopForwards(t *testing.T) {
	newMainForTest(t)
	s := Server{Name: "web1", Target: "deploy@h", Port: "22"}
	a, _ := parsePortSpec("5432")
	b, _ := parsePortSpec("*:443")
	if got := strings.Join(menuItems(t, s), ","); strings.Contains(got, "Stop forwards") {
		t.Fatalf("idle menu = %q", got)
	}

	writeForwards(s, []PortSpec{a, b})
	aliveMu.Lock()
	aliveState = map[string]bool{"web1": true}
	aliveMu.Unlock()
	defer func() {
		aliveMu.Lock()
		aliveState = map[string]bool{}
		aliveMu.Unlock()
	}()
	if got := strings.Join(menuItems(t, s), ","); !strings.Contains(got, "Forward ports…,Stop forwards,Edit…") {
		t.Fatalf("forwarding menu = %q", got)
	}
	showForwardDialog(s)
	form := modals[len(modals)-1].prim.(*tview.Flex).GetItem(2).(*tview.Form)
	if form.GetButtonIndex("Stop all") < 0 {
		t.Fatal("no Stop all button")
	}
	closeModal()

	var mu sync.Mutex
	var cancelled []string
	oldMux := muxRun
	defer func() { muxRun = oldMux }()
	muxRun = func(args []string) error {
		mu.Lock()
		defer mu.Unlock()
		if args[3] == "cancel" {
			cancelled = append(cancelled, args[5])
		}
		return nil
	}
	// No real master here, so stand one in.
	oldAlive := aliveCheck
	defer func() { aliveCheck = oldAlive }()
	aliveCheck = func(Server) bool { return true }

	stopForwards(s)
	forwardsBusy.Wait()
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(cancelled, ",") != "5432:localhost:5432,*:443:localhost:443" || len(readForwards(s)) != 0 {
		t.Fatalf("cancelled %q, left %+v", cancelled, readForwards(s))
	}
}
