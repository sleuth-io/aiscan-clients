package cli

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/auth"
	"github.com/sleuth-io/aiscan-clients/desktop/internal/autoupdate"
	"github.com/sleuth-io/aiscan-clients/desktop/internal/tray"
)

// updateCheckInterval is how often the resident agent re-runs the update
// check. The check itself is throttled to once a day (autoupdate.shouldCheck),
// so this only bounds how stale a *running* daemon can be after the throttle
// window opens — short-lived CLI runs get the same check at startup.
const updateCheckInterval = 6 * time.Hour

// agent is the daemon's core: it owns the periodic sync and update loops and
// the state the tray renders. All mutation happens on the run loop goroutine
// except the state snapshot, which is mutex-guarded for the tray's reads.
type agent struct {
	instance string
	exe      string // resolved at startup, for Reexec after an update swap
	interval time.Duration
	logger   *log.Logger
	logW     io.Writer // sync progress lines land in the daemon log

	mu sync.Mutex
	st tray.State

	states        chan tray.State
	syncReq       chan struct{} // manual "Sync now": upload what the plan says is missing
	forceReq      chan struct{} // manual "Sync all": re-upload everything, ignoring coverage
	loginInFlight atomic.Bool
	// restartPending: an update was swapped on disk; restart at the next idle
	// point so the running image catches up.
	restartPending bool
	// releaseLock hands the single-instance lock to a respawned successor
	// (the macOS restart path); set by Daemon after acquiring it, only used
	// on the run-loop goroutine.
	releaseLock func()
}

func newAgent(instance, exe string, interval time.Duration, logger *log.Logger, logW io.Writer) *agent {
	return &agent{
		instance: instance,
		exe:      exe,
		interval: interval,
		logger:   logger,
		logW:     logW,
		states:   make(chan tray.State, 1),
		syncReq:  make(chan struct{}, 1),
		forceReq: make(chan struct{}, 1),
	}
}

// States is the channel the tray renders from; it always carries the latest
// snapshot (older undelivered ones are dropped).
func (a *agent) States() <-chan tray.State { return a.states }

// run is the daemon loop. It blocks until ctx is done; callers run it on a
// goroutine when a tray owns the main one.
func (a *agent) run(ctx context.Context) {
	a.refreshIdentity(ctx)

	// One-shot: on the very first run of a fresh install with no login, open
	// the browser approval unprompted. The tray icon can be hidden on a full
	// macOS menu bar, so this may be the only login affordance the user sees.
	if claimFirstRunLogin(a.instance) {
		a.logger.Printf("first run with no login; starting browser login")
		a.LogIn()
	}

	// First sync shortly after startup rather than a full interval later — a
	// freshly installed daemon should visibly do something.
	first := time.NewTimer(15 * time.Second)
	defer first.Stop()
	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()
	updates := time.NewTicker(updateCheckInterval)
	defer updates.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-first.C:
			a.syncOnce(ctx, false, false)
		case <-ticker.C:
			a.syncOnce(ctx, false, false)
		case <-a.syncReq:
			a.syncOnce(ctx, true, false)
		case <-a.forceReq:
			a.syncOnce(ctx, true, true)
		case <-updates.C:
			a.checkUpdate()
		}
		// The loop is single-threaded, so between events we are idle — the
		// safe point to adopt a swapped binary. restart only returns on
		// failure.
		if a.restartPending {
			a.logger.Printf("restarting to adopt update")
			a.restart()
			a.restartPending = false
			a.logger.Printf("restart failed; continuing on old version")
		}
	}
}

// syncOnce runs one capture→redact→upload pass over all sources. Scheduled
// runs respect pause and never prompt for auth; a manual "Sync now" ignores
// pause (the user explicitly asked) but still never prompts — login is its own
// action. With force set (manual "Sync all"), it ignores the server's coverage
// and re-uploads the whole available history, backfilling anything a past
// partial or failed sync left the server missing.
func (a *agent) syncOnce(ctx context.Context, manual, force bool) {
	if !manual && a.snapshot().Paused {
		return
	}
	token, ok := auth.CachedToken(a.instance)
	if !ok {
		a.setState(func(s *tray.State) { s.Username = ""; s.LastErr = "" })
		a.logger.Printf("sync skipped: not logged in")
		return
	}

	a.setState(func(s *tray.State) { s.Syncing = true; s.LastErr = "" })
	a.logger.Printf("sync starting")

	cfg := syncConfig{instance: a.instance, force: force}
	var err error
	for _, r := range recipes {
		if err = syncSource(ctx, cfg, &token, r, nil, a.logW); err != nil {
			break
		}
	}

	a.setState(func(s *tray.State) {
		s.Syncing = false
		if err != nil {
			s.LastErr = err.Error()
		} else {
			s.LastSync = time.Now()
		}
	})
	switch {
	case errors.Is(err, errAuthRequired):
		// The server rejected the token mid-sync; the cache is already
		// cleared. Flip to logged-out so the tray asks the user to log in.
		a.setState(func(s *tray.State) { s.Username = ""; s.LastErr = "session expired — log in again" })
		a.logger.Printf("sync: authorization rejected; logged out")
	case err != nil:
		a.logger.Printf("sync failed: %v", err)
	default:
		a.logger.Printf("sync finished")
	}
}

// restart adopts a swapped binary; it only returns on failure. On macOS,
// exec-in-place would keep the pid but lose the tray icon — the exec'd image
// drops its LaunchServices registration and its status item never appears —
// so the daemon releases the single-instance lock, spawns a successor, and
// exits. Elsewhere exec-in-place is right: the pid survives for supervisors
// and there is nothing graphical to lose.
func (a *agent) restart() {
	if runtime.GOOS != "darwin" {
		autoupdate.Reexec(a.exe)
		return
	}
	// Release before spawning so the successor can't lose the lock race and
	// bail out as "already running". If the spawn then fails we continue
	// unguarded — the same degraded mode as failing to acquire the lock at
	// startup.
	if a.releaseLock != nil {
		a.releaseLock()
		a.releaseLock = nil
	}
	if err := autoupdate.Respawn(a.exe); err != nil {
		a.logger.Printf("respawn: %v", err)
		return
	}
	os.Exit(0)
}

func (a *agent) checkUpdate() {
	updated, err := autoupdate.Check()
	if err != nil {
		a.logger.Printf("update check: %v", err)
		return
	}
	if updated {
		a.logger.Printf("update downloaded; will restart when idle")
		a.restartPending = true
	}
}

// refreshIdentity resolves the username for the cached token, if any, so the
// tray can show who is logged in across daemon restarts.
func (a *agent) refreshIdentity(ctx context.Context) {
	token, ok := auth.CachedToken(a.instance)
	if !ok {
		a.setState(func(s *tray.State) { s.Username = "" })
		return
	}
	user, err := auth.Whoami(ctx, a.instance, token)
	switch {
	case errors.Is(err, auth.ErrNotLoggedIn):
		_ = auth.ClearToken(a.instance)
		a.setState(func(s *tray.State) { s.Username = "" })
		a.logger.Printf("whoami: token rejected; logged out")
	case err != nil:
		// Network trouble — keep the token, leave identity unknown but
		// logged-in-ish; the next sync will surface real errors.
		a.logger.Printf("whoami: %v", err)
	default:
		a.setState(func(s *tray.State) { s.Username = user })
		a.logger.Printf("logged in as %s", user)
	}
}

// --- tray.Actions ----------------------------------------------------------

// SyncNow queues a manual sync; a no-op if one is already queued.
func (a *agent) SyncNow() {
	select {
	case a.syncReq <- struct{}{}:
	default:
	}
}

// SyncAll queues a manual full sync — re-upload everything, ignoring the
// server's coverage. A no-op if one is already queued.
func (a *agent) SyncAll() {
	select {
	case a.forceReq <- struct{}{}:
	default:
	}
}

func (a *agent) SetPaused(p bool) {
	a.setState(func(s *tray.State) { s.Paused = p })
	if p {
		a.logger.Printf("syncing paused")
	} else {
		a.logger.Printf("syncing resumed")
	}
}

// LogIn runs the device-code flow on its own goroutine (approval can take
// minutes) and opens the browser to the approval page. Triggered by the user
// via the menu, plus exactly once by the daemon itself on a fresh install's
// first run (claimFirstRunLogin) — never on any later start.
func (a *agent) LogIn() {
	if !a.loginInFlight.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer a.loginInFlight.Store(false)
		prompt := func(userCode, verifyURL string) {
			a.logger.Printf("login: approve at %s (code %s)", verifyURL, userCode)
			_ = auth.OpenBrowser(verifyURL)
		}
		token, err := auth.EnsureToken(context.Background(), a.instance, prompt)
		if err != nil {
			a.setState(func(s *tray.State) { s.LastErr = "login failed" })
			a.logger.Printf("login failed: %v", err)
			return
		}
		user, err := auth.Whoami(context.Background(), a.instance, token)
		if err != nil {
			a.logger.Printf("whoami after login: %v", err)
			user = "(unknown)"
		}
		a.setState(func(s *tray.State) { s.Username = user; s.LastErr = "" })
		a.logger.Printf("logged in as %s", user)
		a.SyncNow()
	}()
}

func (a *agent) LogOut() {
	_ = auth.ClearToken(a.instance)
	a.setState(func(s *tray.State) { s.Username = "" })
	a.logger.Printf("logged out")
}

// Quit is called by the tray just before it stops the event loop; the daemon
// process exits right after (cleanly, so launchd's crash-only KeepAlive lets
// it stay down).
func (a *agent) Quit() {
	a.logger.Printf("quit requested from tray")
}

// --- state plumbing ---------------------------------------------------------

func (a *agent) snapshot() tray.State {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.st
}

// setState mutates the snapshot under the lock and publishes it, keeping only
// the latest undelivered state (the tray only ever wants "now").
func (a *agent) setState(mutate func(*tray.State)) {
	a.mu.Lock()
	mutate(&a.st)
	st := a.st
	a.mu.Unlock()
	for {
		select {
		case a.states <- st:
			return
		default:
			select {
			case <-a.states:
			default:
			}
		}
	}
}
