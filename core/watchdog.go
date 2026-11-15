package core

import (
	"log/slog"
	"os"
	"sync"
	"time"
)

// PlatformDownExitAfter bounds how long a platform may stay continuously unreachable before the
// process exits so its supervisor can restart it with fresh connections.
//
// Platform transports retry forever on their own (discordgo's gateway reconnect attempts, Zulip's
// /events long-poll), so a link that dies mid-flight never comes back by itself. Both cc-connect
// lanes have been observed sitting `launchd state = running` while deaf for days — no failed exit
// code, no alert, just silence. launchd restarts a job only when it exits non-zero
// (`KeepAlive{SuccessfulExit=false}`), so the bot must exit to be healed.
//
// The budget sits far above a transient flap (seconds) and far below the multi-day silences it
// prevents; a genuinely unreachable network then costs one restart per interval rather than a
// permanently dead bot.
const PlatformDownExitAfter = 10 * time.Minute

// exitProcess is a seam so tests can observe the exit without ending the test binary.
var exitProcess = os.Exit

// ExitPlatformDown ends the process with a temporary-failure status for the supervisor to restart.
// Call it only when the platform has already been unreachable for PlatformDownExitAfter.
func ExitPlatformDown(platform, reason string, down time.Duration) {
	slog.Error("platform unreachable for too long; exiting so the supervisor restarts it",
		"platform", platform, "down", down.Round(time.Second), "budget", PlatformDownExitAfter, "reason", reason)
	exitProcess(75) // EX_TEMPFAIL: a non-zero exit is what KeepAlive{SuccessfulExit=false} reacts to
}

// DownWatch tracks one platform's connectivity and fires onExpire once if an outage outlasts its
// budget. Platforms report transitions with MarkDown/Up; the watchdog does the timing so no platform
// needs its own goroutine or threshold arithmetic.
type DownWatch struct {
	mu       sync.Mutex
	timeout  time.Duration
	onExpire func(down time.Duration)
	since    time.Time
	timer    *time.Timer
}

// NewWatch returns a watch that calls onExpire once after the platform has been down for timeout.
func NewWatch(timeout time.Duration, onExpire func(down time.Duration)) *DownWatch {
	return &DownWatch{timeout: timeout, onExpire: onExpire}
}

// MarkDown records the start of an outage. Repeated calls during the same outage are no-ops, so the
// deadline is not extended by retries.
func (w *DownWatch) MarkDown() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.since.IsZero() {
		return
	}
	w.since = time.Now()
	w.timer = time.AfterFunc(w.timeout, func() {
		w.mu.Lock()
		down := time.Duration(0)
		if !w.since.IsZero() {
			down = time.Since(w.since)
		}
		// Clear first: an expired budget must fire exactly once, even if the platform keeps
		// reporting failures while the exit is in flight.
		w.since = time.Time{}
		w.timer = nil
		w.mu.Unlock()
		if down > 0 && w.onExpire != nil {
			w.onExpire(down)
		}
	})
}

// Up records that the platform is reachable again and cancels a pending expiry.
func (w *DownWatch) Up() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.since = time.Time{}
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
}

// Down reports how long the platform has been unreachable, or zero while it is healthy.
func (w *DownWatch) Down() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.since.IsZero() {
		return 0
	}
	return time.Since(w.since)
}
