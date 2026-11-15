package core

import (
	"sync/atomic"
	"testing"
	"time"
)

// An outage that outlasts the budget fires exactly once, with the measured duration.
func TestDownWatchExpiresOnce(t *testing.T) {
	var calls atomic.Int32
	done := make(chan time.Duration, 2)
	w := NewWatch("test", 50*time.Millisecond, func(down time.Duration) {
		calls.Add(1)
		done <- down
	})

	w.MarkDown()
	w.MarkDown() // retries must not extend or duplicate the deadline
	select {
	case down := <-done:
		if down < 50*time.Millisecond {
			t.Fatalf("down = %v, want >= the 50ms budget", down)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not expire")
	}

	time.Sleep(120 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("watch expired more than once for one outage")
	default:
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("onExpire calls = %d, want 1", got)
	}
	if d := w.Down(); d != 0 {
		t.Fatalf("Down() after expiry = %v, want 0 (outage consumed)", d)
	}
}

// Recovering before the budget cancels the pending exit, and a later outage arms a fresh one.
func TestDownWatchRecoveryCancels(t *testing.T) {
	expired := make(chan struct{}, 1)
	w := NewWatch("test", 80*time.Millisecond, func(time.Duration) { expired <- struct{}{} })

	w.MarkDown()
	if d := w.Down(); d <= 0 {
		t.Fatalf("Down() while unhealthy = %v, want > 0", d)
	}
	w.Up()
	if d := w.Down(); d != 0 {
		t.Fatalf("Down() after Up() = %v, want 0", d)
	}

	select {
	case <-expired:
		t.Fatal("watch expired after recovery")
	case <-time.After(200 * time.Millisecond):
	}

	w.MarkDown()
	select {
	case <-expired:
	case <-time.After(2 * time.Second):
		t.Fatal("second outage did not expire")
	}
}

// The budget must stay far above a transient flap and far below the multi-day silences it prevents.
func TestPlatformDownExitAfterIsProportional(t *testing.T) {
	if PlatformDownExitAfter < 5*time.Minute {
		t.Fatalf("PlatformDownExitAfter = %v, too short to ride out a transient flap", PlatformDownExitAfter)
	}
	if PlatformDownExitAfter > time.Hour {
		t.Fatalf("PlatformDownExitAfter = %v, too long to prevent a silent multi-hour outage", PlatformDownExitAfter)
	}
}

// A non-positive budget falls back to the platform budget instead of exiting instantly.
func TestNewWatchClampsNonPositiveTimeout(t *testing.T) {
	w := NewWatch("test", 0, func(time.Duration) { t.Error("zero budget must not expire immediately") })
	w.MarkDown()
	if d := w.Down(); d <= 0 {
		t.Fatalf("Down() = %v, want an armed outage", d)
	}
	w.Up()
}

// The exit must be non-zero: launchd's KeepAlive{SuccessfulExit=false} only restarts on failure.
func TestExitPlatformDownUsesFailureStatus(t *testing.T) {
	original := exitProcess
	t.Cleanup(func() { exitProcess = original })

	var code atomic.Int32
	code.Store(-1)
	exitProcess = func(c int) { code.Store(int32(c)) }

	ExitPlatformDown("discord", "test", time.Minute)

	if got := code.Load(); got != 75 {
		t.Fatalf("exit status = %d, want 75 (non-zero so the supervisor restarts the job)", got)
	}
}
