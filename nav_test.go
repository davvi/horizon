package main

import (
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// press sends a key the way the running app does: the global capture first,
// then down the focus chain from the root, each level's own capture included.
func press(k tcell.Key) {
	e := tcell.NewEventKey(k, 0, tcell.ModNone)
	if e = app.GetInputCapture()(e); e != nil {
		pages.InputHandler()(e, func(p tview.Primitive) { app.SetFocus(p) })
	}
}

func wantFocus(t *testing.T, step string, want tview.Primitive) {
	t.Helper()
	if got := app.GetFocus(); got != want {
		t.Fatalf("%s: focus = %T %p, want %T %p", step, got, got, want, want)
	}
}

// Tab goes pane to pane — servers, Settings, Ports — never into the menu
// bar; inside Settings the arrows move, and the pane remembers its control.
func TestTabSwitchesPanes(t *testing.T) {
	newMainForTest(t)
	sidebarVisible = true
	settingsIdx = 0
	app.SetFocus(serverList)

	press(tcell.KeyTab)
	wantFocus(t, "Tab from servers", pingBox)
	press(tcell.KeyDown)
	wantFocus(t, "↓ in Settings", pingCountField)
	press(tcell.KeyDown)
	press(tcell.KeyDown) // stays on the last control
	wantFocus(t, "↓↓ in Settings", portCheckBox)
	press(tcell.KeyUp)
	wantFocus(t, "↑ in Settings", pingCountField)
	press(tcell.KeyTab)
	wantFocus(t, "Tab from Settings", portList)
	press(tcell.KeyTab)
	wantFocus(t, "Tab from Ports", serverList)
	press(tcell.KeyTab)
	wantFocus(t, "back into Settings", pingCountField)
	press(tcell.KeyBacktab)
	wantFocus(t, "Backtab from Settings", serverList)
	press(tcell.KeyBacktab)
	wantFocus(t, "Backtab from servers", portList)

	sidebarVisible = false
	app.SetFocus(serverList)
	press(tcell.KeyTab)
	wantFocus(t, "Tab with the sidebar hidden", serverList)
}

// In a form Tab walks every field and then the buttons, round and round;
// the arrows move within an area — ↑/↓ between fields, ←/→ between
// buttons, ↑ from a button back to the field left.
func TestFormNavigation(t *testing.T) {
	newMainForTest(t)
	app.SetFocus(serverList)
	showServerForm()
	defer closeModal()
	form := modals[len(modals)-1].prim.(*tview.Form)
	field := func(label string) tview.Primitive { return form.GetFormItemByLabel(label) }

	wantFocus(t, "opened", field("Name"))
	for _, want := range []tview.Primitive{field("User (optional)"), field("Host"), field("Port"),
		field("Folder"), form.GetButton(0), form.GetButton(1), field("Name")} {
		press(tcell.KeyTab)
		wantFocus(t, "Tab", want)
	}
	press(tcell.KeyBacktab)
	wantFocus(t, "Backtab wraps", form.GetButton(1))
	press(tcell.KeyLeft)
	wantFocus(t, "←", form.GetButton(0))
	press(tcell.KeyLeft)
	wantFocus(t, "← on the first button", form.GetButton(0))
	press(tcell.KeyBacktab)
	wantFocus(t, "Backtab off the buttons", field("Folder"))

	press(tcell.KeyUp)
	wantFocus(t, "↑", field("Port"))
	press(tcell.KeyTab)
	press(tcell.KeyTab)
	press(tcell.KeyUp)
	wantFocus(t, "↑ from a button", field("Folder"))

	// The arrows step past the folder picker without opening it.
	drop := field("Folder").(*tview.DropDown)
	press(tcell.KeyDown)
	if drop.IsOpen() {
		t.Fatal("↓ opened the folder picker")
	}
	press(tcell.KeyUp)
	wantFocus(t, "↑ off the picker", field("Port"))
	for i := 0; i < 5; i++ {
		press(tcell.KeyUp)
	}
	wantFocus(t, "↑ stops at the first field", field("Name"))
}

// An alert has only buttons: Tab and the arrows step through them.
func TestAlertNavigation(t *testing.T) {
	newMainForTest(t)
	app.SetFocus(serverList)
	dialog("test", "Sure?", []string{"Yes", "No", "Cancel"}, func(string) {})
	defer closeModal()
	form := modals[len(modals)-1].prim.(*tview.Flex).GetItem(1).(*tview.Form)
	wantFocus(t, "opened", form.GetButton(0))
	press(tcell.KeyTab)
	wantFocus(t, "Tab", form.GetButton(1))
	press(tcell.KeyRight)
	wantFocus(t, "→", form.GetButton(2))
	press(tcell.KeyTab)
	wantFocus(t, "Tab wraps", form.GetButton(0))
	press(tcell.KeyLeft)
	wantFocus(t, "← wraps", form.GetButton(2))
}

// In Forward ports the checklist sits above the fields.
func TestForwardDialogNavigation(t *testing.T) {
	newMainForTest(t)
	writePorts(func(d string) string { return appendPortLine(appendPortLine(d, "a 5432"), "b 6379") })
	app.SetFocus(serverList)
	showForwardDialog(Server{Name: "web1", Target: "deploy@h", Port: "22"})
	defer closeModal()
	f := modals[len(modals)-1].prim.(*tview.Flex)
	list, form := f.GetItem(1).(*macScrollList), f.GetItem(2).(*tview.Form)
	custom := form.GetFormItemByLabel("Custom")

	checks := form.GetFormItem(2).(*checkRow)

	// Tab: list, Custom, the shown checkboxes, the buttons, and round.
	wantFocus(t, "opened", list)
	for _, want := range []tview.Primitive{custom, checks.boxes[0], form.GetButton(0), form.GetButton(1), list} {
		press(tcell.KeyTab)
		wantFocus(t, "Tab", want)
	}
	checks.setShown(1, true)
	press(tcell.KeyBacktab)
	press(tcell.KeyBacktab)
	press(tcell.KeyBacktab)
	wantFocus(t, "Backtab reaches the box just shown", checks.boxes[1])
	press(tcell.KeyBacktab)
	wantFocus(t, "Backtab within the row", checks.boxes[0])
	press(tcell.KeyRight)
	wantFocus(t, "→ in the row", checks.boxes[1])
	checks.setShown(1, false)
	wantFocus(t, "All interfaces hidden under the focus", checks.boxes[0])

	// The arrows: ↑ off Custom reaches the list, ↓ off its last row Custom.
	press(tcell.KeyUp)
	press(tcell.KeyUp)
	wantFocus(t, "↑ off Custom", list)
	press(tcell.KeyDown)
	press(tcell.KeyDown)
	wantFocus(t, "↓ off the last row", custom)
}
