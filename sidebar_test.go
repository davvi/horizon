package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rivo/tview"
)

func TestSetConfigValue(t *testing.T) {
	data := "# Horizon config\n#   ping  on/off\nping=off\nping_count=3\n"
	if got := setConfigValue(data, "ping", "on"); got != "# Horizon config\n#   ping  on/off\nping=on\nping_count=3\n" {
		t.Fatalf("replace = %q", got)
	}
	if got := setConfigValue(data, "port_check", "on"); got != data+"port_check=on\n" {
		t.Fatalf("append = %q", got)
	}
	// parseConfig honours the last line for a key, so that's the one rewritten.
	dup := "ping = on\nping=off\n"
	if got := setConfigValue(dup, "ping", "on"); got != "ping = on\nping=on\n" || !parseConfig(got).Ping {
		t.Fatalf("duplicate = %q", got)
	}
	if got := setConfigValue("", "ping", "on"); got != "ping=on\n" {
		t.Fatalf("empty = %q", got)
	}
	// The template round-trips every setting.
	c := configTemplate
	c = setConfigValue(c, "ping", "on")
	c = setConfigValue(c, "ping_count", "7")
	c = setConfigValue(c, "port_check", "on")
	if got := parseConfig(c); !got.Ping || got.PingCount != 7 || !got.PortCheck {
		t.Fatalf("template = %+v", got)
	}
	if !strings.HasPrefix(c, "# Horizon config file") {
		t.Fatal("template comments lost")
	}
}

func newMainForTest(t *testing.T) {
	t.Helper()
	baseDir = t.TempDir()
	if err := ensureFiles(); err != nil {
		t.Fatal(err)
	}
	loadConfig()
	app = tview.NewApplication()
	pages = tview.NewPages()
	pages.AddPage("main", buildMain(), true, true)
	pages.SetRect(0, 0, 120, 40)
	// As in main: the root is what lets a newly opened dialog take focus.
	app.SetRoot(pages, true)
	modals = nil
	sidebarShown = true
	refreshData()
	reloadPorts()
}

// Ticking a sidebar checkbox writes config.txt and applies it; syncing the
// controls from a hand-edited file doesn't write anything back.
func TestSidebarSavesConfig(t *testing.T) {
	newMainForTest(t)
	path := filepath.Join(baseDir, configFileName)

	pingBox.SetChecked(true)
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "\nping=on\n") || !config.Ping {
		t.Fatalf("config after tick = %q", data)
	}
	pingBox.SetChecked(false) // stop the probes this started

	os.WriteFile(path, []byte("port_check=on\n"), 0o600)
	loadConfig()
	syncSidebar()
	if !portCheckBox.IsChecked() || pingBox.IsChecked() {
		t.Fatal("controls don't show the file")
	}
	if data, _ := os.ReadFile(path); string(data) != "port_check=on\n" {
		t.Fatalf("sync wrote the file: %q", data)
	}

	pingCountField.SetText("9")
	commitPingCount()
	if config.PingCount != 9 {
		t.Fatalf("ping count = %d", config.PingCount)
	}
	pingCountField.SetText("")
	commitPingCount()
	if pingCountField.GetText() != "9" {
		t.Fatalf("empty field not restored: %q", pingCountField.GetText())
	}
}

func TestSidebarPortList(t *testing.T) {
	newMainForTest(t)
	if err := writePorts(func(d string) string { return appendPortLine(d, "postgres 5432") }); err != nil {
		t.Fatal(err)
	}
	if portList.GetItemCount() != 2 {
		t.Fatalf("rows = %d", portList.GetItemCount())
	}
	if m, _ := portList.GetItemText(0); !strings.Contains(m, "postgres") || !strings.Contains(m, "5432") {
		t.Fatalf("row 0 = %q", m)
	}
	portList.SetCurrentItem(1)
	if _, ok := currentPort(); ok {
		t.Fatal("Add row counted as a port")
	}
}

// The sidebar folds away on a narrow terminal and on s, and Tab skips it
// while it's hidden.
func TestSidebarVisibility(t *testing.T) {
	newMainForTest(t)
	applySidebarWidth(120)
	if !sidebarVisible {
		t.Fatal("hidden on a wide screen")
	}
	applySidebarWidth(80)
	if sidebarVisible {
		t.Fatal("shown on a narrow screen")
	}
	applySidebarWidth(120)
	app.SetFocus(serverList)
	toggleSidebar()
	applySidebarWidth(120)
	if sidebarVisible {
		t.Fatal("s didn't hide it")
	}
	toggleSidebar()
	applySidebarWidth(120)
	if !sidebarVisible {
		t.Fatal("s didn't bring it back")
	}
}

// Closing a dialog opened from the sidebar puts focus back in the sidebar.
func TestDialogReturnsFocusToOpener(t *testing.T) {
	newMainForTest(t)
	sidebarVisible = true
	app.SetFocus(portList)
	portForm(nil)
	closeModal()
	if app.GetFocus() != portList {
		t.Fatalf("focus = %T, want the Ports pane", app.GetFocus())
	}
	app.SetFocus(serverList)
	portForm(nil)
	closeModal()
	if app.GetFocus() != serverList {
		t.Fatalf("focus = %T, want the server list", app.GetFocus())
	}
}

func TestNeedsAllInterfaces(t *testing.T) {
	for local, want := range map[string]bool{"443": true, "80": true, "1023": true, "1024": false, "8443": false, "": false, "0": false} {
		if got := needsAllInterfaces(local, "darwin"); got != want {
			t.Errorf("darwin %q = %v", local, got)
		}
	}
	if needsAllInterfaces("443", "linux") {
		t.Error("linux 443 ticked: all interfaces needs root there too")
	}
}

// In the port form the box follows the local port until the user sets it.
func TestPortFormAutoTicksAllInterfaces(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("only macOS opens low ports on all interfaces")
	}
	newMainForTest(t)
	portForm(nil)
	defer closeModal()
	form := modals[len(modals)-1].prim.(*tview.Form)
	local := form.GetFormItemByLabel("Local port").(*tview.InputField)
	open := form.GetFormItemByLabel("All interfaces").(*tview.Checkbox)

	local.SetText("443")
	if !open.IsChecked() {
		t.Fatal("443 didn't tick All interfaces")
	}
	local.SetText("8443")
	if open.IsChecked() {
		t.Fatal("8443 left All interfaces ticked")
	}
	local.SetText("443")
	open.SetChecked(false) // the user unticks it
	local.SetText("80")
	if open.IsChecked() {
		t.Fatal("the user's choice was overridden")
	}
}
