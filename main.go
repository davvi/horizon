// Horizon — a small SSH connection manager TUI for macOS and Linux.
//
// All data lives as plain text files in ~/.horizon (override with -f <folder>).
// Connections are held and reused via OpenSSH ControlMaster/ControlPersist,
// so authentication, keys, agents and 2FA behave exactly like plain `ssh`.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// version is stamped by the release build (goreleaser) via -ldflags.
var version = "dev"

const serversFileName = "list_of_servers.txt"

const serversTemplate = `# Horizon servers — one per line:
#   name  user@host[:port]
# Example:
#   web1  deploy@203.0.113.10
#   db    admin@db.internal:2222
`

const envTemplate = `# Horizon environment file.
# Lines like KEY=value are exported on the server after connecting.
# Every other non-comment line is executed as a command, in order.
# Afterwards you are dropped into an interactive login shell.
# Example:
#   APP_ENV=staging
#   cd /srv/app
`

type Server struct{ Name, Target, Port string }

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
	serverList *tview.List
	envList    *tview.List
	servers    []Server
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
	refreshServers()

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
	sf := filepath.Join(baseDir, serversFileName)
	if _, err := os.Stat(sf); os.IsNotExist(err) {
		if err := os.WriteFile(sf, []byte(serversTemplate), 0o600); err != nil {
			return err
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
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		s := Server{Name: f[0], Target: f[1], Port: "22"}
		if host, port, ok := strings.Cut(s.Target, ":"); ok {
			s.Target, s.Port = host, port
		}
		out = append(out, s)
	}
	return out
}

func envFiles() []string {
	matches, _ := filepath.Glob(filepath.Join(baseDir, "*.txt"))
	var out []string
	for _, m := range matches {
		if filepath.Base(m) != serversFileName {
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

func alive(s Server) bool {
	return exec.Command("ssh", "-o", "ControlPath="+socketPath(s), "-O", "check", s.Target).Run() == nil
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

func buildMain() tview.Primitive {
	serverList = tview.NewList()
	serverList.SetSelectedTextColor(tcell.ColorWhite).
		SetSelectedBackgroundColor(tcell.ColorBlack).
		SetSelectedFocusOnly(true). // no highlight while the pane is unfocused
		SetHighlightFullLine(true)  // selection spans the whole row, Mac style
	serverList.SetBackgroundColor(tcell.ColorWhite) // Finder-style white pane
	serverList.SetBorderPadding(1, 0, 1, 1)
	serverList.SetBorder(true).SetTitle(" Servers ")
	serverList.SetSelectedFunc(func(i int, _, _ string, _ rune) {
		if i < len(servers) {
			selectServer(servers[i])
		}
	})

	envList = tview.NewList().ShowSecondaryText(false)
	envList.SetSelectedTextColor(tcell.ColorWhite).
		SetSelectedBackgroundColor(tcell.ColorBlack).
		SetSelectedFocusOnly(true).
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
	for _, b := range []*tview.Button{newSrv, newEnv, refresh, quit} {
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
	bar.AddItem(tview.NewBox().SetBackgroundColor(tcell.ColorWhite), 0, 1, false)

	help := tview.NewTextView().
		SetText(" Enter: connect   Tab: switch focus   arrows/mouse: navigate").
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

func refreshServers() {
	servers = loadServers()
	serverList.Clear()
	// One full-width block line per server; the empty secondary line acts
	// as spacing between entries.
	for _, s := range servers {
		line := fmt.Sprintf(" %-16s %s:%s", s.Name, s.Target, s.Port)
		if alive(s) {
			line += "   [::b]● connected — reusable[::-]"
		}
		serverList.AddItem(line, "", 0, nil)
	}
	if len(servers) == 0 {
		serverList.AddItem("(no servers yet)", "press n or click New Server to add one", 0, nil)
	}
	refreshEnvList()
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
		[]string{"Edit", "Delete", "Cancel"},
		func(label string) {
			switch label {
			case "Edit":
				showEnvEditForm(path)
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
		AddInputField("Port", "22", 6, nil, nil)
	styleForm(form)
	form.AddButton("Save", func() {
		name := strings.TrimSpace(form.GetFormItemByLabel("Name").(*tview.InputField).GetText())
		target := strings.TrimSpace(form.GetFormItemByLabel("Target (user@host)").(*tview.InputField).GetText())
		port := strings.TrimSpace(form.GetFormItemByLabel("Port").(*tview.InputField).GetText())
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
		f, err := os.OpenFile(filepath.Join(baseDir, serversFileName),
			os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			errModal(err.Error())
			return
		}
		fmt.Fprintf(f, "%s %s:%s\n", name, target, port)
		f.Close()
		closeModal()
		refreshServers()
	})
	form.AddButton("Cancel", closeModal)
	form.SetCancelFunc(closeModal)
	form.SetBorder(true)
	form.SetTitle(" New server ")
	showModal("serverForm", form, 56, 11)
}

func showEnvFileForm() {
	form := tview.NewForm().
		AddInputField("File name", "", 30, nil, nil).
		AddTextArea("Content", envTemplate, 60, 8, 0, nil)
	styleForm(form)
	form.AddButton("Save", func() {
		name := strings.TrimSpace(form.GetFormItemByLabel("File name").(*tview.InputField).GetText())
		name = filepath.Base(name) // keep it inside baseDir
		if name == "" || name == "." || name == serversFileName {
			errModal("Please enter a valid file name.")
			return
		}
		if !strings.HasSuffix(name, ".txt") {
			name += ".txt"
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
	form.SetTitle(" New env file ")
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
