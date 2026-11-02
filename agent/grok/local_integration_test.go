package grok

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// TestLocalCatalogAgainstInstalledCLI is a live check against a real Grok Build
// install. It never authenticates and never runs a turn: it only reads the
// catalog the CLI advertises through the ACP initialize handshake, which is what
// /model and /reasoning list.
//
// Opt in explicitly so CI and machines without Grok stay green:
//
//	GROK_LOCAL_INTEGRATION=1 go test ./agent/grok -run TestLocalCatalogAgainstInstalledCLI -v
func TestLocalCatalogAgainstInstalledCLI(t *testing.T) {
	if os.Getenv("GROK_LOCAL_INTEGRATION") == "" {
		t.Skip("set GROK_LOCAL_INTEGRATION=1 to probe the installed grok CLI")
	}
	bin := os.Getenv("GROK_BIN")
	if bin == "" {
		bin = "grok"
	}
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("grok CLI not found in PATH: %v", err)
	}

	agent, err := New(map[string]any{
		"work_dir": t.TempDir(),
		"cmd":      bin,
		"mode":     "yolo",
		"model":    "grok-4.6",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer agent.Stop()

	// The adapter must be reachable by its configured type name, which is what
	// /agent grok and `type = "grok"` resolve through.
	if _, err := core.CreateAgent("grok", map[string]any{
		"work_dir": t.TempDir(),
		"cmd":      bin,
		"mode":     "yolo",
	}); err != nil {
		t.Fatalf("CreateAgent(grok): %v", err)
	}

	switcher, ok := agent.(core.ModelSwitcher)
	if !ok {
		t.Fatal("grok agent must implement core.ModelSwitcher for /model")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	models := switcher.AvailableModels(ctx)
	if len(models) == 0 {
		t.Fatal("AvailableModels returned nothing")
	}
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.Name)
		if !strings.HasPrefix(m.Name, "grok") {
			t.Fatalf("unexpected model id %q (all: %v)", m.Name, ids)
		}
	}
	t.Logf("models: %v", ids)

	rs, ok := agent.(core.ReasoningEffortSwitcher)
	if !ok {
		t.Fatal("grok agent must implement core.ReasoningEffortSwitcher for /reasoning")
	}
	efforts := rs.AvailableReasoningEfforts()
	if len(efforts) == 0 {
		t.Fatal("AvailableReasoningEfforts returned nothing")
	}
	t.Logf("reasoning efforts: %v", efforts)

	sess, err := agent.StartSession(ctx, "")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer sess.Close()
	if _, ok := sess.(core.LiveModelSwitcher); !ok {
		t.Fatal("session must implement core.LiveModelSwitcher so /model keeps history")
	}
	if _, ok := sess.(core.LiveReasoningEffortSwitcher); !ok {
		t.Fatal("session must implement core.LiveReasoningEffortSwitcher")
	}
	args := sess.(*grokSession).buildArgs("hi", "")
	if !contains(args, "--permission-mode") || !contains(args, "bypassPermissions") {
		t.Fatalf("yolo turn must bypass permission prompts, args=%v", args)
	}
}

func contains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// TestLocalTurnThroughAdapter is an end-to-end live turn: it starts a real
// `grok -p` process through the adapter and asserts the streaming-json frames we
// depend on arrive as core events (thinking coalesced, text coalesced, a
// terminal result carrying the session id). It costs tokens, so it needs its own
// opt-in:
//
//	GROK_LOCAL_INTEGRATION=1 GROK_LOCAL_TURN=1 go test ./agent/grok -run TestLocalTurnThroughAdapter -v
func TestLocalTurnThroughAdapter(t *testing.T) {
	if os.Getenv("GROK_LOCAL_INTEGRATION") == "" || os.Getenv("GROK_LOCAL_TURN") == "" {
		t.Skip("set GROK_LOCAL_INTEGRATION=1 GROK_LOCAL_TURN=1 to run a real Grok turn")
	}
	bin := os.Getenv("GROK_BIN")
	if bin == "" {
		bin = "grok"
	}
	dir := t.TempDir()
	agent, err := New(map[string]any{
		"work_dir":         dir,
		"cmd":              bin,
		"mode":             "yolo",
		"reasoning_effort": "low",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer agent.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	sess, err := agent.StartSession(ctx, "")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer sess.Close()

	if err := sess.Send("Reply with exactly: PONG", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var (
		thinking, text strings.Builder
		result         *core.Event
	)
	deadline := time.After(3 * time.Minute)
	for result == nil {
		select {
		case evt, ok := <-sess.Events():
			if !ok {
				t.Fatal("events channel closed before a result")
			}
			switch evt.Type {
			case core.EventThinking:
				thinking.WriteString(evt.Content)
			case core.EventText:
				text.WriteString(evt.Content)
			case core.EventError:
				t.Fatalf("turn error: %v", evt.Error)
			case core.EventResult:
				e := evt
				result = &e
			}
		case <-deadline:
			t.Fatal("timed out waiting for a result")
		}
	}

	if !result.Done {
		t.Error("terminal event must be Done")
	}
	if result.SessionID == "" {
		t.Error("terminal event must carry a grok session id for --resume")
	}
	if !strings.Contains(strings.ToUpper(text.String()), "PONG") {
		t.Fatalf("expected PONG in the streamed text, got %q", text.String())
	}
	t.Logf("session=%s thinking=%dB text=%q usage in=%d out=%d",
		result.SessionID, thinking.Len(), text.String(), result.InputTokens, result.OutputTokens)

	// The adapter must be able to resume that session id and keep the model
	// switch live (verified manually against the CLI as well).
	runtimeStarter, ok := agent.(core.SessionRuntimeStarter)
	if !ok {
		t.Fatal("grok agent must implement core.SessionRuntimeStarter")
	}
	sess2, err := runtimeStarter.StartSessionWithRuntime(ctx, result.SessionID, "grok-4.5", "low")
	if err != nil {
		t.Fatalf("StartSessionWithRuntime: %v", err)
	}
	defer sess2.Close()
	args := sess2.(*grokSession).buildArgs("hi", "")
	if !contains(args, "--resume") || !contains(args, result.SessionID) {
		t.Fatalf("resume id missing from args: %v", args)
	}
	if !contains(args, "grok-4.5") {
		t.Fatalf("model override missing from args: %v", args)
	}
}
