package main

// Port forwarding over the servers' ControlMaster connections. Saved ports
// live in ports.txt and can be forwarded through any server; the forwards
// themselves are added to (and cancelled on) the live master with
// `ssh -O forward` / `ssh -O cancel`, so they never need a second login and
// outlive Horizon exactly like the connection does.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

const portsFileName = "ports.txt"

const portsTemplate = `# Horizon ports — one per line, forwardable through any server:
#   name  port                 same port here and on the server
#   name  local:remote         a different local port, remote on localhost
#   name  local:host:remote    full ssh -L form; host as seen from the server
# A leading "*:" opens the local port on all network interfaces instead of
# localhost only. That makes it reachable from any network this machine is
# on, so use it only where you must — typically for ports below 1024, which
# macOS lets a normal user open on all interfaces but not on localhost.
# Example:
#   postgres  5432
#   grafana   13000:3000
#   kafka     9092:broker.internal:9092
#   https     *:443
`

// PortSpec is one local forward: Local on this machine reaches Host:Remote as
// seen from the server, like ssh's -L local:host:remote. Bind is "" for the
// default, localhost only, or allInterfaces.
type PortSpec struct{ Local, Host, Remote, Bind string }

// allInterfaces is ssh's bind address for "every interface": the forward is
// then reachable from the network, not just from this machine.
const allInterfaces = "*"

// open reports whether the forward listens on all interfaces.
func (p PortSpec) open() bool { return p.Bind == allInterfaces }

// Port is one saved entry of ports.txt.
type Port struct {
	Name string
	Spec PortSpec
}

// parsePortSpec reads "port", "local:remote" or "local:host:remote", each
// optionally led by "*:" for all interfaces. Ports are normalised ("05432" is
// 5432) so the same forward always compares equal.
func parsePortSpec(s string) (PortSpec, bool) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	bind := ""
	if len(parts) > 1 && parts[0] == allInterfaces {
		bind, parts = allInterfaces, parts[1:]
	}
	var p PortSpec
	switch len(parts) {
	case 1:
		p = PortSpec{Local: parts[0], Host: "localhost", Remote: parts[0]}
	case 2:
		p = PortSpec{Local: parts[0], Host: "localhost", Remote: parts[1]}
	case 3:
		p = PortSpec{Local: parts[0], Host: parts[1], Remote: parts[2]}
	default:
		return PortSpec{}, false
	}
	p.Bind = bind
	if !validPort(p.Local) || !validPort(p.Remote) || !validForwardHost(p.Host) {
		return PortSpec{}, false
	}
	p.Local, p.Remote = normPort(p.Local), normPort(p.Remote)
	return p, true
}

func normPort(p string) string {
	n, _ := strconv.Atoi(p)
	return strconv.Itoa(n)
}

// validForwardHost reports whether h is safe inside an -L argument: no
// separators that would shift the fields, no whitespace, no leading "-".
func validForwardHost(h string) bool {
	return h != "" && !strings.HasPrefix(h, "-") && !strings.ContainsAny(h, " \t:@/[]")
}

// String is the shortest form parsePortSpec reads back to the same spec, as
// written to ports.txt.
func (p PortSpec) String() string {
	prefix := ""
	if p.open() {
		prefix = allInterfaces + ":"
	}
	if p.Host != "localhost" {
		return prefix + p.Local + ":" + p.Host + ":" + p.Remote
	}
	if p.Local == p.Remote {
		return prefix + p.Local
	}
	return prefix + p.Local + ":" + p.Remote
}

// arg is the spec as ssh's -L takes it.
func (p PortSpec) arg() string {
	a := p.Local + ":" + p.Host + ":" + p.Remote
	if p.open() {
		a = allInterfaces + ":" + a
	}
	return a
}

// openMark flags a forward that is reachable from the network.
const openMark = " ⚠ network"

// describe is the spec for display: "5432", "13000 → 3000" or
// "9092 → broker.internal:9092", flagged when open to the network.
func (p PortSpec) describe() string {
	d := p.Local
	switch {
	case p.Host != "localhost":
		d = p.Local + " → " + p.Host + ":" + p.Remote
	case p.Local != p.Remote:
		d = p.Local + " → " + p.Remote
	}
	if p.open() {
		d += openMark
	}
	return d
}

// parsePortLine parses one trimmed "name spec" line of ports.txt.
func parsePortLine(line string) (Port, bool) {
	f := strings.Fields(line)
	if len(f) != 2 || !validName(f[0]) {
		return Port{}, false
	}
	spec, ok := parsePortSpec(f[1])
	if !ok {
		return Port{}, false
	}
	return Port{Name: f[0], Spec: spec}, true
}

// loadPorts reads ports.txt; malformed lines and repeated names are skipped.
func loadPorts() []Port {
	data, err := os.ReadFile(filepath.Join(baseDir, portsFileName))
	if err != nil {
		return nil
	}
	var out []Port
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p, ok := parsePortLine(line)
		if !ok || seen[p.Name] {
			continue
		}
		seen[p.Name] = true
		out = append(out, p)
	}
	return out
}

func portEntry(p Port) string { return p.Name + " " + p.Spec.String() }

// appendPortLine returns the ports file content with entry added at the end.
func appendPortLine(data, entry string) string {
	data = strings.TrimRight(data, " \t\n")
	if data != "" {
		data += "\n"
	}
	return data + entry + "\n"
}

// replacePortLine returns the ports file content with the line loadPorts
// would use for the named port swapped for entry, or removed when entry is
// empty. Every other line, comments included, is left as it was.
func replacePortLine(data, name, entry string) string {
	lines := strings.Split(data, "\n")
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if p, ok := parsePortLine(t); !ok || p.Name != name {
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

// writePorts applies edit to ports.txt and reloads the saved list.
func writePorts(edit func(data string) string) error {
	path := filepath.Join(baseDir, portsFileName)
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.WriteFile(path, []byte(edit(string(data))), 0o600); err != nil {
		return err
	}
	reloadPorts()
	return nil
}

// uniquePortName suggests a name no saved port uses: base, then base2, base3…
func uniquePortName(base string, list []Port) string {
	taken := map[string]bool{}
	for _, p := range list {
		taken[p.Name] = true
	}
	n, name := 2, base
	for taken[name] {
		name = fmt.Sprintf("%s%d", base, n)
		n++
	}
	return name
}

// ---------- forwards on a master ----------

// ssh cannot list the forwards a master holds, so Horizon keeps its own
// record next to the socket: one -L argument per line. It only means
// anything while that master is alive.
func forwardsPath(s Server) string { return socketPath(s) + ".forwards" }

func readForwards(s Server) []PortSpec {
	data, err := os.ReadFile(forwardsPath(s))
	if err != nil {
		return nil
	}
	var out []PortSpec
	for _, l := range strings.Split(string(data), "\n") {
		if p, ok := parsePortSpec(l); ok {
			out = append(out, p)
		}
	}
	return out
}

func writeForwards(s Server, list []PortSpec) error {
	if len(list) == 0 {
		return removeForwards(s)
	}
	var b strings.Builder
	for _, p := range list {
		b.WriteString(p.arg() + "\n")
	}
	return os.WriteFile(forwardsPath(s), []byte(b.String()), 0o600)
}

func removeForwards(s Server) error {
	if err := os.Remove(forwardsPath(s)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// clearStaleForwards drops the record of a master found dead, unless it was
// written after the check began — then a new master was just started and the
// record is its own.
func clearStaleForwards(s Server, checkedAt time.Time) {
	if st, err := os.Stat(forwardsPath(s)); err == nil && st.ModTime().Before(checkedAt) {
		os.Remove(forwardsPath(s))
	}
}

// activeForwards is what s's master is forwarding, as far as the last status
// scan knows; nothing when it has no live master.
func activeForwards(s Server) []PortSpec {
	if !isAlive(s.Name) {
		return nil
	}
	return readForwards(s)
}

// diffForwards works out how to get from the forwards in place to the ones
// wanted. Forwards on the same local port but to a different destination are
// cancelled and re-added.
func diffForwards(current, want []PortSpec) (add, cancel []PortSpec) {
	has := func(list []PortSpec, p PortSpec) bool {
		for _, q := range list {
			if q == p {
				return true
			}
		}
		return false
	}
	for _, p := range current {
		if !has(want, p) {
			cancel = append(cancel, p)
		}
	}
	for _, p := range want {
		if !has(current, p) {
			add = append(add, p)
		}
	}
	return add, cancel
}

// muxArgs builds an ssh control command (-O op) against s's master.
func muxArgs(s Server, op string, p PortSpec) []string {
	return []string{"-o", "ControlPath=" + socketPath(s), "-O", op, "-L", p.arg(), "--", s.Target}
}

// muxRun runs one control command, bounded like alive(); indirected so tests
// can stand in for a master.
var muxRun = func(args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), aliveTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ssh", args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		// The client only hears that the master said no; the reason (most
		// often the local port) stays in the master's own log.
		if strings.Contains(msg, "Port forwarding failed") {
			return errors.New("ssh could not open the local port (taken by\nanother program, or not allowed for this user)")
		}
		if msg != "" {
			return errors.New(msg)
		}
	}
	return err
}

// privilegedPort is the error for a local port below 1024 on localhost: ssh
// only opens those as root, which the master Horizon talks to usually isn't.
const privilegedPort = "ports below 1024 can only be opened on localhost\nby root. Use a local port of 1024 or above (the\nremote side can stay, e.g. %s), or tick\n“All interfaces” on the port — it is then\nreachable from your network."

// masterUID is the user s's master runs as: the owner of its socket, or —
// with no master yet — this process, which is who will start it.
func masterUID(s Server, live bool) int {
	if live {
		if st, err := os.Stat(socketPath(s)); err == nil {
			if sys, ok := st.Sys().(*syscall.Stat_t); ok {
				return int(sys.Uid)
			}
		}
	}
	return os.Geteuid()
}

// checkLocalPort reports why the local end of p can't be opened by s's
// master, if it can't.
func checkLocalPort(s Server, p PortSpec, live bool) error {
	if n, _ := strconv.Atoi(p.Local); n < 1024 && !p.open() && masterUID(s, live) != 0 {
		alt := PortSpec{Local: strconv.Itoa(8000 + n), Host: p.Host, Remote: p.Remote}
		return fmt.Errorf(privilegedPort, alt.String())
	}
	return localPortFree(p)
}

// localPortFree reports why the local end of a forward can't be bound where
// ssh will bind it, if it can't: already taken, or not allowed — on Linux a
// port below 1024 needs root on every interface.
var localPortFree = func(p PortSpec) error {
	addr := "127.0.0.1"
	if p.open() {
		addr = ""
	}
	l, err := net.Listen("tcp", net.JoinHostPort(addr, p.Local))
	if err != nil {
		return err
	}
	return l.Close()
}

// startMaster opens a background master for s while the TUI is suspended, so
// passwords, 2FA and host-key prompts appear in the terminal just as with
// Connect. ssh's own messages go to a log next to the socket instead of over
// the UI once it comes back; on failure they are the error.
var startMaster = func(s Server) error {
	sock := socketPath(s)
	logPath := sock + ".log"
	os.Remove(logPath)
	os.Remove(sock) // the master is known dead; a leftover socket would block a new one
	ran := false
	var err error
	app.Suspend(func() {
		ran = true
		fmt.Printf("horizon: connecting to %s (%s) to forward ports...\n", s.Name, s.Target)
		cmd := exec.Command("ssh",
			"-o", "ControlMaster=yes",
			"-o", "ControlPath="+sock,
			"-o", "ControlPersist=yes",
			"-E", logPath, "-f", "-N",
			"-p", s.Port, "--", s.Target)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		err = cmd.Run()
	})
	if !ran {
		return errors.New("could not hand the terminal over to ssh")
	}
	if err != nil {
		if msg, _ := os.ReadFile(logPath); len(strings.TrimSpace(string(msg))) > 0 {
			return errors.New(strings.TrimSpace(string(msg)))
		}
		return err
	}
	return nil
}

// aliveCheck is applyForwards' master check, indirected so tests can stand
// in for a live master.
var aliveCheck = alive

// forwardsBusy tracks applyForwards' background work, so tests can wait it out.
var forwardsBusy sync.WaitGroup

// applyForwards makes want the complete set of s's forwards: new ones are
// added, ones left out are cancelled. Without a live master one is started
// first (needing the terminal, so this runs on the UI goroutine); the control
// commands then run in the background and the list redraws when they're done.
func applyForwards(s Server, want []PortSpec) {
	live := aliveCheck(s)
	var current []PortSpec
	if live {
		current = readForwards(s)
	}
	add, cancel := diffForwards(current, want)

	var failed []string
	var ready []PortSpec
	for _, p := range add {
		// A spec being replaced on the same local port still holds it.
		replacing := false
		for _, c := range cancel {
			replacing = replacing || c.Local == p.Local
		}
		if !replacing {
			if err := checkLocalPort(s, p, live); err != nil {
				failed = append(failed, fmt.Sprintf("%s: %v", p.describe(), err))
				continue
			}
		}
		ready = append(ready, p)
	}
	if len(ready) > 0 && !live {
		if err := startMaster(s); err != nil {
			errModal(fmt.Sprintf("Could not connect to “%s”:\n%v", s.Name, err))
			return
		}
		aliveMu.Lock()
		aliveState[s.Name] = true
		aliveMu.Unlock()
	}

	ui := app
	forwardsBusy.Add(1)
	go func() {
		var kept []PortSpec
		for _, p := range current {
			cancelled := false
			for _, c := range cancel {
				cancelled = cancelled || c == p
			}
			if !cancelled {
				kept = append(kept, p)
			}
		}
		// A cancel that fails is dropped from the record all the same: the
		// forward is no longer wanted, and most likely was already gone.
		for _, p := range cancel {
			muxRun(muxArgs(s, "cancel", p))
		}
		for _, p := range ready {
			if err := muxRun(muxArgs(s, "forward", p)); err != nil {
				failed = append(failed, fmt.Sprintf("%s: %v", p.describe(), err))
				continue
			}
			kept = append(kept, p)
		}
		werr := writeForwards(s, kept)
		// Done before queueing: the update only runs once the UI loop picks it
		// up, which a waiting test never lets happen.
		forwardsBusy.Done()
		ui.QueueUpdateDraw(func() {
			rebuildServerList()
			if werr != nil {
				failed = append(failed, werr.Error())
			}
			if len(failed) > 0 {
				errModal("Some ports could not be forwarded:\n" + strings.Join(failed, "\n"))
			}
		})
	}()
}

// cancelForwards takes every recorded forward off s's master and forgets
// them. Used before the master is stopped or replaced.
func cancelForwards(s Server) {
	for _, p := range readForwards(s) {
		muxRun(muxArgs(s, "cancel", p))
	}
	removeForwards(s)
}

// stopForwards closes every port s is forwarding (x, Stop all); the
// connection itself stays.
func stopForwards(s Server) { applyForwards(s, nil) }

// forwardSummary is the marker on a server row with forwards: "⇄ 5432,6379".
func forwardSummary(list []PortSpec) string {
	locals := make([]string, len(list))
	for i, p := range list {
		locals[i] = p.Local
		if p.open() {
			locals[i] += "⚠"
		}
	}
	return "⇄ " + strings.Join(locals, ",")
}

// ---------- Forward ports dialog ----------

// forwardChoice is one row of the Forward ports dialog.
type forwardChoice struct {
	label  string // saved port's name, or "custom" for a live forward not in the list
	spec   PortSpec
	on     bool
	active bool // already forwarded when the dialog opened
}

// forwardChoices lists every saved port, then any live forward the saved
// list doesn't cover, with the live ones ticked.
func forwardChoices(saved []Port, active []PortSpec) []forwardChoice {
	isActive := func(p PortSpec) bool {
		for _, a := range active {
			if a == p {
				return true
			}
		}
		return false
	}
	var out []forwardChoice
	covered := map[PortSpec]bool{}
	for _, p := range saved {
		on := isActive(p.Spec)
		covered[p.Spec] = true
		out = append(out, forwardChoice{label: p.Name, spec: p.Spec, on: on, active: on})
	}
	for _, a := range active {
		if !covered[a] {
			covered[a] = true
			out = append(out, forwardChoice{label: "custom", spec: a, on: true, active: true})
		}
	}
	return out
}

// parseCustomPorts reads the dialog's Custom field: specs separated by commas
// or spaces. bad holds every entry that isn't a spec.
func parseCustomPorts(text string) (specs []PortSpec, bad []string) {
	for _, f := range strings.FieldsFunc(text, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		if p, ok := parsePortSpec(f); ok {
			specs = append(specs, p)
		} else {
			bad = append(bad, f)
		}
	}
	return specs, bad
}

// placeholderStyle greys out a field's example text well below what is
// typed, so it never reads as a value.
var placeholderStyle = tcell.StyleDefault.Background(tcell.ColorWhite).
	Foreground(tcell.NewRGBColor(178, 178, 178)).Italic(true)

// showForwardDialog opens the Forward ports dialog for s: a checklist of the
// saved ports (live forwards ticked), a Custom field for one-off ports, and
// Apply, which makes the ticked set s's forwards — unticking a live forward
// cancels it.
func showForwardDialog(s Server) {
	choices := forwardChoices(ports, activeForwards(s))
	nameW := 0
	for _, c := range choices {
		nameW = max(nameW, utf8.RuneCountInString(c.label))
	}

	list := newMacScrollList(tview.NewList().ShowSecondaryText(false), 1)
	list.setRowStyles(rowStyle, selectedRowStyle)
	list.SetSelectedFocusOnly(true).SetHighlightFullLine(true)
	list.SetBorderPadding(0, 0, 1, 1)
	render := func() {
		cur := list.GetCurrentItem()
		list.Clear()
		for _, c := range choices {
			box := "[ ]"
			if c.on {
				box = "[x]"
			}
			line := fmt.Sprintf("%s %-*s  %s", box, nameW, c.label, c.spec.describe())
			if c.active {
				line += "  (active)"
			}
			// Brackets would read as style tags.
			list.AddItem(tview.Escape(line), "", 0, nil)
		}
		if len(choices) == 0 {
			list.AddItem("(no saved ports — add them in the sidebar)", "", 0, nil)
		}
		list.SetCurrentItem(cur)
	}
	render()
	toggle := func(i int) {
		if i >= 0 && i < len(choices) {
			choices[i].on = !choices[i].on
			render()
		}
	}
	list.SetSelectedFunc(func(i int, _, _ string, _ rune) { toggle(i) })

	// All interfaces only shows up once a port below 1024 is typed — the one
	// case it is for.
	checks := newCheckRow("Save to list", "All interfaces")
	save, open := checks.boxes[0], checks.boxes[1]
	checks.setShown(1, false)
	form := tview.NewForm().
		AddInputField("Custom", "", 0, nil, nil).
		AddTextView("", "", 0, 1, false, false).
		AddFormItem(checks).
		SetButtonsAlign(tview.AlignCenter).
		SetItemPadding(0)
	form.SetBorderPadding(1, 0, 1, 1)
	styleForm(form)
	custom := form.GetFormItemByLabel("Custom").(*tview.InputField)
	custom.SetPlaceholder("e.g. 8080, 13000:3000, 443").SetPlaceholderStyle(placeholderStyle)
	preview := form.GetFormItem(1).(*tview.TextView)

	// customPorts is what the Custom field asks for. All interfaces opens
	// only the ports typed there that are below 1024, which can't be opened
	// any other way; higher ones stay on localhost unless written as "*:…".
	customPorts := func() (specs []PortSpec, bad []string) {
		specs, bad = parseCustomPorts(custom.GetText())
		if open.IsChecked() {
			for i := range specs {
				if n, _ := strconv.Atoi(specs[i].Local); n < 1024 {
					specs[i].Bind = allInterfaces
				}
			}
		}
		return specs, bad
	}
	// The line under Custom says, as you type, what will be forwarded — or
	// which entry isn't a port — instead of an error after Apply.
	showPreview := func() {
		specs, bad := customPorts()
		switch {
		case len(bad) > 0:
			preview.SetTextColor(tcell.ColorMaroon).
				SetText("✗ not a port: " + strings.Join(bad, ", ") + " — try 8080 or 13000:3000")
		case len(specs) > 0:
			d := make([]string, len(specs))
			for i, p := range specs {
				d[i] = p.describe()
			}
			preview.SetTextColor(tcell.ColorDimGray).SetText("→ " + strings.Join(d, ", "))
		default:
			preview.SetText("")
		}
	}
	// All interfaces follows the ports typed — ticked once one of them is
	// below 1024, where nothing else can open it — until the user sets it.
	picked, auto := false, false
	open.SetChangedFunc(func(bool) {
		picked = picked || !auto
		showPreview()
	})
	custom.SetChangedFunc(func(string) {
		specs, _ := parseCustomPorts(custom.GetText())
		need := false
		for _, p := range specs {
			need = need || (!p.open() && needsAllInterfaces(p.Local, runtime.GOOS))
		}
		checks.setShown(1, need)
		if !picked {
			auto = true
			open.SetChecked(need)
			auto = false
		}
		showPreview()
	})

	form.AddButton("Apply", func() {
		extra, bad := customPorts()
		if len(bad) > 0 {
			errModal(fmt.Sprintf("Not a port: %s\nUse 8080, 13000:3000 or 9092:broker.internal:9092.",
				strings.Join(bad, ", ")))
			return
		}
		var want []PortSpec
		byLocal := map[string]PortSpec{}
		addWant := func(p PortSpec) bool {
			if q, dup := byLocal[p.Local]; dup {
				if q != p {
					errModal(fmt.Sprintf("Local port %s is picked twice:\n%s and %s.", p.Local, q.describe(), p.describe()))
					return false
				}
				return true
			}
			byLocal[p.Local] = p
			want = append(want, p)
			return true
		}
		for _, c := range choices {
			if c.on && !addWant(c.spec) {
				return
			}
		}
		for _, p := range extra {
			if !addWant(p) {
				return
			}
		}
		if save.IsChecked() && len(extra) > 0 {
			err := writePorts(func(data string) string {
				saved := append([]Port(nil), ports...)
				for _, p := range extra {
					known := false
					for _, q := range saved {
						known = known || q.Spec == p
					}
					if known {
						continue
					}
					np := Port{Name: uniquePortName("port"+p.Local, saved), Spec: p}
					saved = append(saved, np)
					data = appendPortLine(data, portEntry(np))
				}
				return data
			})
			if err != nil {
				errModal(err.Error())
				return
			}
		}
		// Opening a port to the network is asked about every time it is
		// newly forwarded, not just when it was saved.
		var opening []string
		for _, p := range want {
			was := false
			for _, c := range choices {
				was = was || (c.active && c.spec == p)
			}
			if p.open() && !was {
				opening = append(opening, p.Local)
			}
		}
		if len(opening) > 0 {
			dialog("forwardOpen",
				fmt.Sprintf("⚠ Port(s) %s will be open on all interfaces.\n"+
					"Anyone on a network this machine is on can\n"+
					"reach them, and through them “%s”.\nForward anyway?",
					strings.Join(opening, ", "), s.Name),
				[]string{"Forward", "Cancel"},
				func(label string) {
					if label == "Forward" {
						closeModal()
						applyForwards(s, want)
					}
				})
			return
		}
		closeModal()
		applyForwards(s, want)
	})
	if len(activeForwards(s)) > 0 {
		form.AddButton("Stop all", func() {
			closeModal()
			stopForwards(s)
		})
	}
	form.AddButton("Cancel", closeModal)
	form.SetCancelFunc(closeModal)

	// The checklist sits above the form's fields: it is the first Tab stop,
	// and ↓ off its last row goes on to Custom.
	nav := navigate(form, list)
	list.SetInputCapture(func(e *tcell.EventKey) *tcell.EventKey {
		switch e.Key() {
		case tcell.KeyRune:
			if e.Rune() == ' ' {
				toggle(list.GetCurrentItem())
				return nil
			}
		case tcell.KeyTab, tcell.KeyBacktab:
			nav.tab(e.Key() == tcell.KeyBacktab)
			return nil
		case tcell.KeyDown:
			if list.GetCurrentItem() >= list.GetItemCount()-1 {
				nav.focus(0)
				return nil
			}
		}
		return e
	})

	rows := max(len(choices), 1)
	listH := min(rows, 12)
	f := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(tview.NewTextView().SetText(" Space/Enter ticks a port; unticked active ports stop.").
			SetTextColor(tcell.ColorDimGray), 2, 0, false).
		AddItem(list, listH, 0, true).
		AddItem(form, 6, 0, false)
	f.SetBorderPadding(1, 0, 1, 1)
	f.SetBorder(true).SetTitle(" Forward ports — " + s.Name + " ")
	w := 62
	for _, c := range choices {
		w = max(w, nameW+utf8.RuneCountInString(c.spec.describe())+22)
	}
	_, _, _, sh := pages.GetRect()
	h := 2 + 1 + 2 + listH + 6
	if sh > 0 {
		h = min(h, sh-1)
	}
	showModal("forwardDialog", f, w, h)
}
