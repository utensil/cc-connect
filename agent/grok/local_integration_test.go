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
