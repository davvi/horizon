package main

// The settings sidebar on the right of the server list: config.txt's
// settings as controls that save straight back to the file, and the saved
// ports of ports.txt with add / edit / delete.

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

const (
	sidebarWidth = 34
	// sidebarMinScreen is the narrowest terminal that still shows the
	// sidebar; below it the server list gets the whole width.
	sidebarMinScreen = 90
)

var (
	body           *tview.Flex // server list | sidebar
	sidebar        *tview.Flex
	sidebarShown   = true // the user's choice (s)
	sidebarVisible = true // shown and the terminal is wide enough; set every frame
	pingBox        *tview.Checkbox
	pingCountField *tview.InputField
	portCheckBox   *tview.Checkbox
	portList       *macScrollList
	sidebarItems   []tview.Primitive // every control in the sidebar
	settingsItems  []tview.Primitive // the Settings pane's controls, top to bottom
	settingsIdx    int               // the one the Settings pane was left on
	ports          []Port
	syncing        bool // the controls are being set from config, not by the user
)

// setConfigValue returns config.txt's content with key set to value. The last
// line for the key is the one parseConfig honours, so that one is rewritten;
// a missing key is appended. Comments and every other line stay as they are.
func setConfigValue(data, key, value string) string {
	lines := strings.Split(data, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if k, _, ok := strings.Cut(t, "="); ok && strings.TrimSpace(k) == key {
			lines[i] = key + "=" + value
			return strings.Join(lines, "\n")
		}
	}
	data = strings.TrimRight(data, " \t\n")
	if data != "" {
		data += "\n"
	}
	return data + key + "=" + value + "\n"
}

// saveConfig writes one setting to config.txt and applies it at once.
func saveConfig(key, value string) {
	path := filepath.Join(baseDir, configFileName)
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		errModal(err.Error())
		return
	}
	if err := os.WriteFile(path, []byte(setConfigValue(string(data), key, value)), 0o600); err != nil {
		errModal(err.Error())
		return
	}
	loadConfig()
	syncSidebar()
	startProbes()
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// syncSidebar shows the loaded config in the controls.
func syncSidebar() {
	syncing = true
	defer func() { syncing = false }()
	pingBox.SetChecked(config.Ping)
	portCheckBox.SetChecked(config.PortCheck)
	pingCountField.SetText(strconv.Itoa(config.PingCount))
}

// commitPingCount saves the ping count field, or puts the saved value back
// when the field holds nothing usable.
func commitPingCount() {
	n, err := strconv.Atoi(pingCountField.GetText())
	if err != nil || n < 1 {
		syncSidebar()
		return
	}
	if n != config.PingCount {
		saveConfig("ping_count", strconv.Itoa(n))
	}
}

func reloadPorts() {
	ports = loadPorts()
	rebuildPortList()
}

// rebuildPortList redraws the Ports pane: one row per saved port and an
// "Add port…" row at the end.
func rebuildPortList() {
	cur := portList.GetCurrentItem()
	portList.Clear()
	nameW := 0
	for _, p := range ports {
		nameW = max(nameW, utf8.RuneCountInString(p.Name))
	}
	for _, p := range ports {
		portList.AddItem(fmt.Sprintf(" %-*s  %s", nameW, p.Name, p.Spec.describe()), "", 0, nil)
	}
	portList.AddItem(" + Add port…", "", 0, nil)
	portList.SetCurrentItem(min(max(cur, 0), portList.GetItemCount()-1))
}

// currentPort is the saved port on the highlighted row of the Ports pane; ok
// is false on the Add row.
func currentPort() (Port, bool) {
	i := portList.GetCurrentItem()
	if i < 0 || i >= len(ports) {
		return Port{}, false
	}
	return ports[i], true
}

// applySidebarWidth shows or hides the sidebar for a screen w columns wide.
// Called before every frame, so it only lays out and never moves focus.
func applySidebarWidth(w int) {
	sidebarVisible = sidebarShown && w >= sidebarMinScreen
	width := 0
	if sidebarVisible {
		width = sidebarWidth
	}
	body.ResizeItem(sidebar, width, 0)
}

// inSidebar reports whether p is one of the sidebar's controls.
func inSidebar(p tview.Primitive) bool {
	for _, q := range sidebarItems {
		if p == q {
			return true
		}
	}
	return false
}

// inSettings reports whether p is one of the Settings pane's controls.
func inSettings(p tview.Primitive) bool {
	for _, q := range settingsItems {
		if p == q {
			return true
		}
	}
	return false
}

// toggleSidebar shows or hides the sidebar (s).
func toggleSidebar() {
	sidebarShown = !sidebarShown
	if !sidebarShown && inSidebar(app.GetFocus()) {
		app.SetFocus(serverList)
	}
}

func buildSidebar() *tview.Flex {
	field := tcell.StyleDefault.Background(tcell.ColorWhite).Foreground(tcell.ColorBlack)
	active := tcell.StyleDefault.Background(tcell.ColorBlack).Foreground(tcell.ColorWhite)
	label := tcell.StyleDefault.Background(tcell.ColorSilver).Foreground(tcell.ColorBlack)
	checkbox := func(text string, key string) *tview.Checkbox {
		// Bracketed like the Forward ports checklist, so an unticked box
		// still reads as a box on any terminal.
		c := tview.NewCheckbox().SetLabel(text).SetLabelWidth(18).
			SetCheckedString(tview.Escape("[x]")).SetUncheckedString(tview.Escape("[ ]")).
			SetLabelStyle(label)
		c.SetUncheckedStyle(label).SetCheckedStyle(label).SetActivatedStyle(active)
		c.SetChangedFunc(func(on bool) {
			if !syncing {
				saveConfig(key, onOff(on))
			}
		})
		return c
	}
	pingBox = checkbox("Ping at startup", "ping")
	portCheckBox = checkbox("Port check", "port_check")
	pingCountField = tview.NewInputField().SetLabel("Ping count").SetLabelWidth(18).
		SetFieldWidth(5).SetAcceptanceFunc(tview.InputFieldInteger).
		SetLabelStyle(label).SetFieldStyle(field)
	pingCountField.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEscape {
			syncSidebar()
			return
		}
		commitPingCount()
	})
	// Tab never reaches the field (the global Tab ring takes it), so leaving
	// it by Tab or a click saves it too.
	pingCountField.SetBlurFunc(commitPingCount)

	settings := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(pingBox, 1, 0, false).
		AddItem(pingCountField, 1, 0, false).
		AddItem(portCheckBox, 1, 0, false)
	settings.SetBorderPadding(1, 1, 1, 1)
	settings.SetBorder(true).SetTitle(" Settings ")

	portList = newMacScrollList(tview.NewList().ShowSecondaryText(false), 1)
	portList.setRowStyles(rowStyle, selectedRowStyle)
	portList.SetSelectedFocusOnly(true).SetHighlightFullLine(true)
	portList.SetBackgroundColor(tcell.ColorWhite)
	portList.SetBorderPadding(1, 0, 0, 1)
	portList.SetBorder(true).SetTitle(" Ports ")
	portList.SetSelectedFunc(func(int, string, string, rune) {
		if p, ok := currentPort(); ok {
			portForm(&p)
		} else {
			portForm(nil)
		}
	})
	portList.SetInputCapture(func(e *tcell.EventKey) *tcell.EventKey {
		switch e.Rune() {
		case 'n', 'a':
			portForm(nil)
			return nil
		case 'e':
			if p, ok := currentPort(); ok {
				portForm(&p)
			}
			return nil
		case 'd':
			if p, ok := currentPort(); ok {
				confirmDeletePort(p)
			}
			return nil
		}
		return e
	})

	hint := tview.NewTextView().SetTextColor(tcell.ColorDimGray).SetWordWrap(true).
		SetText(" Enter: edit   n: new   d: delete\n p on a server forwards ports")

	// Tab reaches the Settings pane as a whole; ↑/↓ move between its
	// controls, and the pane remembers the one it was left on.
	settingsItems = []tview.Primitive{pingBox, pingCountField, portCheckBox}
	for i, c := range []*tview.Box{pingBox.Box, pingCountField.Box, portCheckBox.Box} {
		i := i
		c.SetFocusFunc(func() { settingsIdx = i })
		c.SetInputCapture(func(e *tcell.EventKey) *tcell.EventKey {
			switch e.Key() {
			case tcell.KeyUp:
				app.SetFocus(settingsItems[max(i-1, 0)])
				return nil
			case tcell.KeyDown:
				app.SetFocus(settingsItems[min(i+1, len(settingsItems)-1)])
				return nil
			}
			return e
		})
	}
	sidebarItems = append(append([]tview.Primitive{}, settingsItems...), portList)
	sidebar = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(settings, 7, 0, false).
		AddItem(portList, 0, 1, false).
		AddItem(hint, 2, 0, false)
	rebuildPortList()
	syncSidebar()
	return sidebar
}

// needsAllInterfaces reports whether a local port can only be forwarded by a
// normal user when opened on all interfaces: below 1024 on macOS, which
// allows that there but not on localhost. Elsewhere it would not help.
func needsAllInterfaces(local, goos string) bool {
	n, err := strconv.Atoi(local)
	return err == nil && n >= 1 && n < 1024 && goos == "darwin"
}

// portForm opens the dialog for a saved port: blank for a new one, or filled
// in with orig for editing it.
func portForm(orig *Port) {
	p := Port{Spec: PortSpec{Host: "localhost"}}
	title, name := " New port ", "portForm"
	if orig != nil {
		p = *orig
		title, name = fmt.Sprintf(" Edit %s ", orig.Name), "portEdit"
	}
	form := tview.NewForm().
		AddInputField("Name", p.Name, 24, nil, nil).
		AddInputField("Local port", p.Spec.Local, 6, tview.InputFieldInteger, nil).
		AddInputField("Remote host", p.Spec.Host, 24, nil, nil).
		AddInputField("Remote port", p.Spec.Remote, 6, tview.InputFieldInteger, nil).
		AddCheckbox("All interfaces", p.Spec.open(), nil).
		AddTextView("", "", 38, 4, false, false)
	styleForm(form)
	open := form.GetFormItemByLabel("All interfaces").(*tview.Checkbox)
	open.SetCheckedString(tview.Escape("[x]")).SetUncheckedString(tview.Escape("[ ]"))
	warning := form.GetFormItem(form.GetFormItemCount() - 1).(*tview.TextView)
	warning.SetTextColor(tcell.ColorMaroon).SetWordWrap(true)
	showWarning := func(on bool) {
		warning.SetText("")
		if on {
			warning.SetText("⚠ Reachable from any network this machine is on, not just from here. Needed on macOS for local ports below 1024.")
		}
	}
	showWarning(p.Spec.open())
	// All interfaces follows the local port — ticked below 1024 where only
	// that can open it — until the user sets the box themselves.
	picked, auto := false, false
	open.SetChangedFunc(func(on bool) {
		picked = picked || !auto
		showWarning(on)
	})
	text := func(label string) string {
		return strings.TrimSpace(form.GetFormItemByLabel(label).(*tview.InputField).GetText())
	}
	// A new port's remote side follows its local port until it is typed in
	// separately, the common case being the same number on both ends.
	remote := form.GetFormItemByLabel("Remote port").(*tview.InputField)
	typed, following := orig != nil, false
	remote.SetChangedFunc(func(string) { typed = typed || !following })
	form.GetFormItemByLabel("Local port").(*tview.InputField).SetChangedFunc(func(t string) {
		if !typed {
			following = true
			remote.SetText(t)
			following = false
		}
		if !picked {
			auto = true
			open.SetChecked(needsAllInterfaces(t, runtime.GOOS))
			auto = false
		}
	})
	form.AddButton("Save", func() {
		next := Port{Name: text("Name")}
		if !validName(next.Name) {
			errModal("Name is required and may not contain spaces, slashes\nor a leading “-” or “[”.")
			return
		}
		for _, o := range ports {
			if o.Name == next.Name && (orig == nil || o.Name != orig.Name) {
				errModal(fmt.Sprintf("A port named “%s” already exists.", next.Name))
				return
			}
		}
		host := text("Remote host")
		if host == "" {
			host = "localhost"
		}
		spec, ok := parsePortSpec(text("Local port") + ":" + host + ":" + text("Remote port"))
		if !ok {
			errModal("Ports must be numbers between 1 and 65535, and the\nremote host a plain name or address (default localhost).")
			return
		}
		if open.IsChecked() {
			spec.Bind = allInterfaces
		}
		next.Spec = spec
		err := writePorts(func(data string) string {
			if orig == nil {
				return appendPortLine(data, portEntry(next))
			}
			return replacePortLine(data, orig.Name, portEntry(next))
		})
		if err != nil {
			errModal(err.Error())
			return
		}
		closeModal()
	})
	form.AddButton("Cancel", closeModal)
	form.SetCancelFunc(closeModal)
	form.SetBorder(true)
	form.SetTitle(title)
	navigate(form, nil)
	showModal(name, form, 56, 20)
}

func confirmDeletePort(p Port) {
	dialog("portDelete",
		fmt.Sprintf("Delete the saved port “%s” (%s)?\nForwards already running keep running.", p.Name, p.Spec.describe()),
		[]string{"Delete", "Cancel"},
		func(label string) {
			if label != "Delete" {
				return
			}
			if err := writePorts(func(data string) string { return replacePortLine(data, p.Name, "") }); err != nil {
				errModal(err.Error())
			}
		})
}
