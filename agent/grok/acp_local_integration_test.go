package grok

// Live checks for the ACP transport against a real Grok Build install.
//
// Opt in explicitly so CI and machines without Grok stay green:
//
//	GROK_LOCAL_INTEGRATION=1 go test ./agent/grok -run TestLocalACP -v
//
// GROK_BIN may point at a non-PATH binary (e.g. ~/.grok/bin/grok).

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func liveGrokBinary(t *testing.T) string {
	t.Helper()
	if os.Getenv("GROK_LOCAL_INTEGRATION") == "" {
		t.Skip("set GROK_LOCAL_INTEGRATION=1 to run live ACP checks")
	}
	bin := os.Getenv("GROK_BIN")
	if bin == "" {
		bin = "grok"
	}
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("grok CLI not found: %v", err)
	}
	return bin
}

func startLiveACPSession(t *testing.T, workDir string) *acpSession {
	t.Helper()
	return startLiveACPSessionResume(t, workDir, "")
}

func startLiveACPSessionResume(t *testing.T, workDir, resumeID string) *acpSession {
	t.Helper()
	bin := liveGrokBinary(t)
	agent, err := New(map[string]any{
		"work_dir": workDir,
		"cmd":      bin,
		"mode":     "yolo",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = agent.Stop() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)

	session, err := agent.StartSession(ctx, resumeID)
	require.NoError(t, err)
	s, ok := session.(*acpSession)
	require.True(t, ok, "grok must default to the ACP transport")
	t.Cleanup(func() { _ = s.Close() })
	require.True(t, s.cfg.nativeSteer, "native steering should be enabled by default")
	return s
}

// collectTurn reads events until the first terminal event and returns the
// concatenated assistant text plus every event seen.
func collectTurn(t *testing.T, s *acpSession, timeout time.Duration) (string, []core.Event) {
	t.Helper()
	var text strings.Builder
	var events []core.Event
	deadline := time.After(timeout)
	for {
		select {
		case evt := <-s.Events():
			events = append(events, evt)
			if evt.Type == core.EventText {
				text.WriteString(evt.Content)
			}
			if evt.Type == core.EventResult {
				return text.String(), events
			}
			if evt.Type == core.EventError {
				t.Fatalf("agent error: %v", evt.Error)
			}
		case <-deadline:
			t.Fatalf("timed out waiting for the turn to finish; text so far: %q", text.String())
		}
	}
}

func TestLocalACP_BasicTurn(t *testing.T) {
	s := startLiveACPSession(t, t.TempDir())

	require.NoError(t, s.Send("Reply with exactly: ACP-OK", nil, nil))
	text, events := collectTurn(t, s, 3*time.Minute)

	assert.Contains(t, text, "ACP-OK")
	result := resultEvent(t, events)
	assert.True(t, result.Done)
	assert.NotEmpty(t, s.CurrentSessionID())
}

// TestLocalACP_NativeSteer proves that a follow-up sent while the turn is
// running is injected into that turn (the cc-connect steer path) rather than
// queued behind it.
func TestLocalACP_NativeSteer(t *testing.T) {
	s := startLiveACPSession(t, t.TempDir())

	require.NoError(t, s.Send("Run `sleep 30; echo READY` with your bash tool, then reply with exactly: ALPHA", nil, nil))

	// Give the agent time to start the slow tool call, then steer.
	waitForToolOrDelay(t, s, 20*time.Second)
	require.NoError(t, s.Steer("Change of plan: reply with exactly: BANANA"))

	text, _ := collectTurn(t, s, 3*time.Minute)
	assert.Contains(t, text, "BANANA", "steering guidance must win over the original instruction")
	assert.NotContains(t, text, "ALPHA")

	// The steered prompt must not open a second engine turn.
	select {
	case evt := <-s.Events():
		if evt.Type == core.EventResult {
			t.Fatalf("steer opened a second turn: %+v", evt)
		}
	case <-time.After(5 * time.Second):
	}
}

// TestLocalACP_CancelTurnKeepsSession proves /stop cancels the turn without
// losing the conversation.
func TestLocalACP_CancelTurnKeepsSession(t *testing.T) {
	s := startLiveACPSession(t, t.TempDir())

	require.NoError(t, s.Send("Remember the codeword PLUM. Reply with exactly: STORED", nil, nil))
	text, _ := collectTurn(t, s, 3*time.Minute)
	assert.Contains(t, text, "STORED")

	require.NoError(t, s.Send("Run `sleep 45; echo READY` with your bash tool, then reply with exactly: GAMMA", nil, nil))
	waitForToolOrDelay(t, s, 20*time.Second)

	started := time.Now()
	require.NoError(t, s.CancelTurn())
	require.Less(t, time.Since(started), 40*time.Second, "cancel should settle quickly")
	assert.True(t, s.Alive(), "the session must survive /stop")

	// Drain the cancelled turn's terminal event.
	deadline := time.After(30 * time.Second)
drain:
	for {
		select {
		case evt := <-s.Events():
			if evt.Type == core.EventResult || evt.Type == core.EventError {
				break drain
			}
		case <-deadline:
			t.Fatal("cancelled turn never settled")
		}
	}

	// Same session, same conversation.
	require.NoError(t, s.Send("What was the codeword? Reply with only that word.", nil, nil))
	text, _ = collectTurn(t, s, 3*time.Minute)
	assert.Contains(t, strings.ToUpper(text), "PLUM")
}

// waitForToolOrDelay returns as soon as the agent reports tool use, or once the
// delay elapses, so a steer lands mid-turn without guessing exact timings.
func waitForToolOrDelay(t *testing.T, s *acpSession, delay time.Duration) {
	t.Helper()
	deadline := time.After(delay)
	for {
		select {
		case evt := <-s.Events():
			if evt.Type == core.EventToolUse {
				return
			}
			if evt.Type == core.EventResult || evt.Type == core.EventError {
				t.Fatalf("turn finished before it could be steered/cancelled: %+v", evt)
			}
		case <-deadline:
			return
		}
	}
}

// TestLocalACP_ResumeKeepsContext proves a restarted process reattaches to the
// same grok session (this is what the daemon does after a restart).
func TestLocalACP_ResumeKeepsContext(t *testing.T) {
	workDir := t.TempDir()

	first := startLiveACPSession(t, workDir)
	require.NoError(t, first.Send("Remember the codeword OAK. Reply with exactly: STORED", nil, nil))
	text, _ := collectTurn(t, first, 3*time.Minute)
	assert.Contains(t, text, "STORED")
	sessionID := first.CurrentSessionID()
	require.NotEmpty(t, sessionID)
	require.NoError(t, first.Close())

	second := startLiveACPSessionResume(t, workDir, sessionID)
	assert.Equal(t, sessionID, second.CurrentSessionID())
	require.NoError(t, second.Send("What was the codeword? Reply with only that word.", nil, nil))
	text, _ = collectTurn(t, second, 3*time.Minute)
	assert.Contains(t, strings.ToUpper(text), "OAK", "resumed session must keep the conversation")
}
