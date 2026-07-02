// Package tray renders the daemon's system-tray UI — the client's trust
// surface. It shows whether the agent is logged in (and as whom), when it last
// synced, and gives the user direct control: sync now, pause, log in/out,
// quit. It deliberately contains no capture/upload logic; it renders State
// snapshots pushed by the agent and forwards menu clicks to Actions.
//
// fyne.io/systray is pure Go on Windows/Linux and Cgo (Cocoa) on macOS, which
// is why darwin release binaries are built on a macOS runner.
package tray

import (
	_ "embed"
	"time"

	"fyne.io/systray"
)

// icon.png is a black-with-alpha template glyph; macOS recolors it to match
// the menu bar theme (SetTemplateIcon), other platforms use it as-is.
//
//go:embed icon.png
var iconPNG []byte

// State is one renderable snapshot of the agent. The zero value means
// "starting up, not logged in".
type State struct {
	Username string    // empty = not logged in
	Paused   bool      // scheduled syncs are skipped
	Syncing  bool      // a sync pass is running right now
	LastSync time.Time // zero = never synced since start
	LastErr  string    // last sync/login failure, cleared on success
}

// Actions is what the menu can ask the agent to do. Implementations must not
// block: clicks arrive on the tray's event goroutine.
type Actions interface {
	SyncNow()
	SetPaused(bool)
	LogIn()
	LogOut()
	Quit()
}

// Run starts the tray and blocks until Quit is chosen or the actions' owner
// stops it via systray.Quit. It MUST be called on the main goroutine — on
// macOS systray owns the Cocoa main event loop.
func Run(a Actions, states <-chan State, version string) {
	systray.Run(func() { onReady(a, states, version) }, nil)
}

func onReady(a Actions, states <-chan State, version string) {
	systray.SetTemplateIcon(iconPNG, iconPNG)
	systray.SetTooltip("aiscan")

	// Two disabled lines carry the status; everything below them acts.
	account := systray.AddMenuItem("Not logged in", "")
	account.Disable()
	status := systray.AddMenuItem("Starting…", "")
	status.Disable()
	systray.AddSeparator()
	syncNow := systray.AddMenuItem("Sync now", "Capture, redact, and upload now")
	pause := systray.AddMenuItem("Pause syncing", "Skip scheduled syncs until resumed")
	systray.AddSeparator()
	login := systray.AddMenuItem("Log in…", "Authorize this machine in your browser")
	logout := systray.AddMenuItem("Log out", "Forget this machine's authorization")
	logout.Hide()
	systray.AddSeparator()
	ver := systray.AddMenuItem("aiscan "+version, "")
	ver.Disable()
	quit := systray.AddMenuItem("Quit aiscan", "Stop capturing until reopened")

	go func() {
		var st State
		for {
			select {
			case st = <-states:
				render(st, account, status, syncNow, pause, login, logout)
			case <-syncNow.ClickedCh:
				a.SyncNow()
			case <-pause.ClickedCh:
				a.SetPaused(!st.Paused)
			case <-login.ClickedCh:
				a.LogIn()
			case <-logout.ClickedCh:
				a.LogOut()
			case <-quit.ClickedCh:
				a.Quit()
				systray.Quit()
				return
			}
		}
	}()
}

// render maps a State onto the menu items. Called only from the event
// goroutine, so no locking.
func render(st State, account, status, syncNow, pause, login, logout *systray.MenuItem) {
	if st.Username != "" {
		account.SetTitle("Logged in as " + st.Username)
		login.Hide()
		logout.Show()
	} else {
		account.SetTitle("Not logged in")
		login.Show()
		logout.Hide()
	}

	status.SetTitle(statusLine(st))

	if st.Syncing {
		syncNow.Disable()
	} else {
		syncNow.Enable()
	}
	if st.Paused {
		pause.SetTitle("Resume syncing")
	} else {
		pause.SetTitle("Pause syncing")
	}
}

// statusLine is the one-line summary under the account line, in priority
// order: activity beats pause beats errors beats history.
func statusLine(st State) string {
	switch {
	case st.Syncing:
		return "Syncing…"
	case st.Paused:
		return "Paused"
	case st.LastErr != "":
		return "Problem: " + st.LastErr
	case st.Username == "":
		return "Log in to start syncing"
	case st.LastSync.IsZero():
		return "Waiting for first sync"
	default:
		return "Last sync " + st.LastSync.Format("3:04 PM")
	}
}
