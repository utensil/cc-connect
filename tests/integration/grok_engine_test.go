//go:build integration && !no_grok

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/chenhg5/cc-connect/agent/grok"
	"github.com/chenhg5/cc-connect/core"
)

// TestGrok_EngineSwitchAndTurn drives a real Grok turn through the cc-connect
// engine, on the same code path a deployed bot uses: the project hosts an agent,
// a session-scoped `/agent grok` override rebuilds the agent from
// `[projects.agent.options.agents.grok]`, and the next message runs on it.
//
// It needs a logged-in Grok Build CLI and costs tokens, so it is opt-in:
//
//	GROK_LOCAL_INTEGRATION=1 GROK_BIN=$HOME/.grok/bin/grok \
//	  go test -tags=integration ./tests/integration -run TestGrok_EngineSwitchAndTurn -v
func TestGrok_EngineSwitchAndTurn(t *testing.T) {
	if os.Getenv("GROK_LOCAL_INTEGRATION") == "" {
		t.Skip("set GROK_LOCAL_INTEGRATION=1 to run a real Grok turn through the engine")
	}
	bin := os.Getenv("GROK_BIN")
	if bin == "" {
		bin = "grok"
	}
	if resolved, err := exec.LookPath(bin); err == nil {
		bin = resolved
	} else {
		t.Skipf("grok CLI not found: %v", err)
	}

	projectDir := t.TempDir()
	switchDir := t.TempDir()

	base, err := core.CreateAgent("grok", map[string]any{
		"work_dir": projectDir,
		"cmd":      bin,
		"mode":     "yolo",
	})
	if err != nil {
		t.Fatalf("CreateAgent(grok): %v", err)
	}

	mp := &mockPlatform{agent: base}
	e := core.NewEngine("test", base, []core.Platform{mp}, filepath.Join(projectDir, "sessions.json"), core.LangEnglish)
	defer e.Stop()

	// Mirror the deployed Discord config: the project default plus a
	// per-backend block for the switchable grok agent.
	e.SetDefaultAgentConfig("grok", map[string]any{
		"work_dir": projectDir,
		"cmd":      bin,
		"mode":     "yolo",
		"agents": map[string]any{
			"grok": map[string]any{
				"work_dir":         switchDir,
				"cmd":              bin,
				"mode":             "yolo",
				"reasoning_effort": "low",
			},
		},
	}, nil, "")

	sessionKey := "mock:channel-1:user1"
	msg := func(content string) *core.Message {
		return &core.Message{
			SessionKey: sessionKey,
			Platform:   "mock",
			UserID:     "user1",
			UserName:   "testuser",
			Content:    content,
			ReplyCtx:   "ctx1",
		}
	}

	// 1. The catalog the real CLI advertises must reach /model.
	e.ReceiveMessage(mp, msg("/model"))
	models, ok := waitForMessageContaining(mp, "grok-", 30*time.Second)
	if !ok {
		t.Fatalf("/model listed nothing; sent: %v", mp.getSent())
	}
	if !strings.Contains(models, "grok-4") && !strings.Contains(models, "grok-5") {
		t.Fatalf("/model output has no grok model id: %q", models)
	}
	t.Logf("/model → %q", firstLine(models))
	mp.clear()

	// 2. Switch this session onto the per-backend grok agent.
	e.ReceiveMessage(mp, msg("/agent grok"))
	switched, ok := waitForMessageContaining(mp, "grok", 20*time.Second)
	if !ok {
		t.Fatalf("/agent grok produced no reply; sent: %v", mp.getSent())
	}
	t.Logf("/agent grok → %q", firstLine(switched))
	mp.clear()

	// 3. A real turn on the switched agent.
	e.ReceiveMessage(mp, msg("Reply with exactly the word PONG and nothing else."))
	reply, ok := waitForMessageContaining(mp, "PONG", 180*time.Second)
	if !ok {
		t.Fatalf("no PONG from the grok turn; sent: %v", mp.getSent())
	}
	t.Logf("turn → %q", firstLine(reply))
	mp.clear()

	// 4. The conversation is resumable: a second turn keeps the session and the
	//    live model switch does not clear it.
	e.ReceiveMessage(mp, msg("Remember the number 41. Reply with just OK."))
	if _, ok := waitForMessageContaining(mp, "OK", 180*time.Second); !ok {
		t.Fatalf("second turn produced no reply; sent: %v", mp.getSent())
	}
	mp.clear()

	e.ReceiveMessage(mp, msg("/model grok-4.5"))
	changed, ok := waitForMessageContaining(mp, "grok-4.5", 20*time.Second)
	if !ok {
		t.Fatalf("/model grok-4.5 produced no reply; sent: %v", mp.getSent())
	}
	t.Logf("/model grok-4.5 → %q", firstLine(changed))
	mp.clear()

	e.ReceiveMessage(mp, msg("What number did I ask you to remember? Reply with just the number."))
	remembered, ok := waitForMessageContaining(mp, "41", 180*time.Second)
	if !ok {
		t.Fatalf("history was lost across the live model switch; sent: %v", mp.getSent())
	}
	t.Logf("after live /model → %q", firstLine(remembered))

}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// TestGrok_EngineSteerAndStop drives the two engine-level behaviours the ACP
// transport exists for: a message sent while a turn runs must steer that turn
// (queue.busy_behavior = "steer"), and /stop must keep the conversation instead
// of reporting a session reset.
//
// Opt-in like the turn test above:
//
//	GROK_LOCAL_INTEGRATION=1 GROK_BIN=$HOME/.grok/bin/grok \
//	  go test -tags=integration ./tests/integration -run TestGrok_EngineSteerAndStop -v
func TestGrok_EngineSteerAndStop(t *testing.T) {
	if os.Getenv("GROK_LOCAL_INTEGRATION") == "" {
		t.Skip("set GROK_LOCAL_INTEGRATION=1 to run real Grok turns through the engine")
	}
	bin := os.Getenv("GROK_BIN")
	if bin == "" {
		bin = "grok"
	}
	if resolved, err := exec.LookPath(bin); err == nil {
		bin = resolved
	} else {
		t.Skipf("grok CLI not found: %v", err)
	}

	projectDir := t.TempDir()
	agent, err := core.CreateAgent("grok", map[string]any{
		"work_dir": projectDir,
		"cmd":      bin,
		"mode":     "yolo",
	})
	if err != nil {
		t.Fatalf("CreateAgent(grok): %v", err)
	}

	mp := &mockPlatform{agent: agent}
	e := core.NewEngine("test", agent, []core.Platform{mp}, filepath.Join(projectDir, "sessions.json"), core.LangEnglish)
	defer e.Stop()
	e.SetBusyMessageBehavior("steer")

	sessionKey := "mock:channel-1:user1"
	msg := func(content string) *core.Message {
		return &core.Message{
			SessionKey: sessionKey,
			Platform:   "mock",
			UserID:     "user1",
			UserName:   "testuser",
			Content:    content,
			ReplyCtx:   "ctx1",
		}
	}

	// 1. Steering: the second message lands while the first turn is running and
	//    must win, not queue behind it.
	e.ReceiveMessage(mp, msg("Run `sleep 30; echo READY` with your bash tool, then reply with exactly: ALPHA"))
	time.Sleep(12 * time.Second)
	e.ReceiveMessage(mp, msg("Change of plan: reply with exactly: BANANA"))

	// Grok delivers steering at its next safe gap (and holds it while the agent
	// is blocked in a long tool call), so the check is that the guidance reached
	// the agent and was acknowledged as steering, not that it preempted.
	if _, ok := waitForMessageContaining(mp, "Guidance sent", 60*time.Second); !ok {
		t.Fatalf("the follow-up was not acknowledged as a steer; sent: %v", mp.getSent())
	}

	steered, ok := waitForMessageContaining(mp, "BANANA", 240*time.Second)
	if !ok {
		t.Fatalf("steering did not reach the running turn; sent: %v", mp.getSent())
	}
	if strings.Contains(steered, "ALPHA") {
		t.Fatalf("steered turn answered the original instruction: %q", steered)
	}
	for _, sent := range mp.getSent() {
		if strings.Contains(sent, "session reset") {
			t.Fatalf("/steer surfaced a session-reset error: %q", sent)
		}
	}
	mp.clear()

	// 2. /stop keeps the session: no reset error, and the next turn still has
	//    the conversation.
	e.ReceiveMessage(mp, msg("Remember the codeword PLUM. Reply with exactly: STORED"))
	if _, ok := waitForMessageContaining(mp, "STORED", 180*time.Second); !ok {
		t.Fatalf("priming turn produced no reply; sent: %v", mp.getSent())
	}
	mp.clear()

	e.ReceiveMessage(mp, msg("Run `sleep 45; echo READY` with your bash tool, then reply with exactly: GAMMA"))
	time.Sleep(12 * time.Second)
	e.ReceiveMessage(mp, msg("/stop"))

	if _, ok := waitForMessageContaining(mp, "Execution stopped", 60*time.Second); !ok {
		t.Fatalf("/stop produced no confirmation; sent: %v", mp.getSent())
	}
	for _, sent := range mp.getSent() {
		if strings.Contains(sent, "session reset") {
			t.Fatalf("/stop still reports a session reset: %q", sent)
		}
	}
	mp.clear()

	e.ReceiveMessage(mp, msg("What was the codeword? Reply with only that word."))
	recalled, ok := waitForMessageContaining(mp, "PLUM", 180*time.Second)
	if !ok {
		t.Fatalf("conversation was lost across /stop; sent: %v", mp.getSent())
	}
	t.Logf("after /stop → %q", firstLine(recalled))
}
