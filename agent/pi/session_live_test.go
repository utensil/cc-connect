package pi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// These tests cover the fork's live model / thinking switching for pi, the
// capability the engine needs so /model and /reasoning on a pi session behave
// like they do on codex (applied in place) instead of falling back to
// tearing the interactive state down and — for /reasoning — clearing the
// session history.
//
// The fake RPC process below speaks the shape real pi uses: every command is
// answered with {"type":"response","id":<same id>,...}, which is what makes a
// switch verifiable instead of fire-and-forget.

// fakeRPCOptions configure the fake pi RPC script written by
// newFakeRPCSwitchSession.
type fakeRPCOptions struct {
	sessionID string // session id reported for get_state
	fail      bool   // answer set_model / set_thinking_level with success=false
}

func (o fakeRPCOptions) sessionIDOr() string {
	if o.sessionID == "" {
		return "fake-session-id"
	}
	return o.sessionID
}

// newFakeRPCSwitchSession starts a real piSession against a fake `pi --mode rpc`
// script that records every stdin frame to <dir>/recorded.jsonl and answers the
// commands this feature uses. It returns the session and the recorded-frame
// path so tests can assert what was actually written to pi.
func newFakeRPCSwitchSession(t *testing.T, opts fakeRPCOptions) (*piSession, string) {
	t.Helper()
	dir := t.TempDir()
	record := filepath.Join(dir, "recorded.jsonl")
	script := filepath.Join(dir, "fake-pi-rpc.sh")

	scriptBody := `#!/bin/sh
record="$PI_FAKE_RECORD"
while IFS= read -r line; do
    printf '%s\n' "$line" >> "$record"
    id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
    case "$line" in
        *'"type":"get_state"'*)
            printf '{"id":"%s","type":"response","command":"get_state","success":true,"data":{"sessionId":"%s","sessionFile":"/tmp/fake.jsonl"}}\n' "$id" "$PI_FAKE_SESSION_ID"
            ;;
        *'"type":"get_available_models"'*)
            printf '{"id":"%s","type":"response","command":"get_available_models","success":true,"data":{"models":[{"id":"deepseek-flash","name":"DeepSeek V4.1 Flash","provider":"deepseek"},{"id":"deepseek-v4-pro","name":"DeepSeek V4 Pro","provider":"deepseek"}]}}\n' "$id"
            ;;
        *'"type":"set_model"'*)
            if [ "$PI_FAKE_FAIL" = "1" ]; then
                printf '{"id":"%s","type":"response","command":"set_model","success":false,"error":"unknown model"}\n' "$id"
            else
                printf '{"id":"%s","type":"response","command":"set_model","success":true,"data":{"id":"deepseek-flash","provider":"deepseek"}}\n' "$id"
            fi
            ;;
        *'"type":"set_thinking_level"'*)
            if [ "$PI_FAKE_FAIL" = "1" ]; then
                printf '{"id":"%s","type":"response","command":"set_thinking_level","success":false,"error":"model does not support reasoning"}\n' "$id"
            else
                printf '{"id":"%s","type":"response","command":"set_thinking_level","success":true}\n' "$id"
            fi
            ;;
        *'"type":"never_answered"'*)
            ;;
    esac
done
`
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatalf("write fake pi rpc script: %v", err)
	}

	fail := "0"
	if opts.fail {
		fail = "1"
	}
	extraEnv := []string{
		"PI_FAKE_RECORD=" + record,
		"PI_FAKE_SESSION_ID=" + opts.sessionIDOr(),
		"PI_FAKE_FAIL=" + fail,
	}

	s, err := newPiSession(context.Background(), script, nil, dir, "", "", "", true, "", extraEnv)
	if err != nil {
		t.Fatalf("newPiSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, record
}

// recordedFrames returns the frames the fake pi received so far.
func recordedFrames(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read recorded frames: %v", err)
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

func frameWithType(t *testing.T, frames []string, typ string) string {
	t.Helper()
	for _, f := range frames {
		if strings.Contains(f, `"type":"`+typ+`"`) {
			return f
		}
	}
	return ""
}

func TestPiSession_RPC_SetLiveModel_ResolvesProviderFromCatalog(t *testing.T) {
	// Regression: /model on a pi session used to require the engine to tear
	// the live conversation down (piSession did not implement
	// core.LiveModelSwitcher), and the model override was dropped at spawn.
	// A bare id must now be resolved to pi's set_model provider+modelId pair.
	s, record := newFakeRPCSwitchSession(t, fakeRPCOptions{})

	if ok := s.SetLiveModel("deepseek-flash"); !ok {
		t.Fatalf("SetLiveModel(deepseek-flash) = false, want true")
	}

	frames := recordedFrames(t, record)
	frame := frameWithType(t, frames, "set_model")
	if frame == "" {
		t.Fatalf("no set_model frame written; frames=%v", frames)
	}
	if !strings.Contains(frame, `"provider":"deepseek"`) {
		t.Errorf("set_model frame missing resolved provider: %s", frame)
	}
	if !strings.Contains(frame, `"modelId":"deepseek-flash"`) {
		t.Errorf("set_model frame missing modelId: %s", frame)
	}
	if model, _ := s.modelAndThinking(); model != "deepseek-flash" {
		t.Errorf("session model = %q, want %q (next spawn must reuse the switched model)", model, "deepseek-flash")
	}
}

func TestPiSession_RPC_SetLiveModel_ProviderQualifiedSkipsCatalog(t *testing.T) {
	// A "provider/id" string carries everything set_model needs, so the
	// catalog round trip must be skipped.
	s, record := newFakeRPCSwitchSession(t, fakeRPCOptions{})

	if ok := s.SetLiveModel("deepseek/deepseek-v4-pro"); !ok {
		t.Fatalf("SetLiveModel(deepseek/deepseek-v4-pro) = false, want true")
	}

	frames := recordedFrames(t, record)
	if f := frameWithType(t, frames, "get_available_models"); f != "" {
		t.Errorf("provider-qualified model should not query the catalog, got: %s", f)
	}
	frame := frameWithType(t, frames, "set_model")
	if !strings.Contains(frame, `"provider":"deepseek"`) || !strings.Contains(frame, `"modelId":"deepseek-v4-pro"`) {
		t.Errorf("unexpected set_model frame: %s", frame)
	}
}

func TestPiSession_RPC_SetLiveModel_FailureIsNotReportedAsApplied(t *testing.T) {
	// pi rejecting the switch must return false so the engine falls back to
	// its respawn path instead of telling the user the model changed.
	s, _ := newFakeRPCSwitchSession(t, fakeRPCOptions{fail: true})

	if ok := s.SetLiveModel("deepseek-flash"); ok {
		t.Fatalf("SetLiveModel returned true for a success=false response")
	}
	if model, _ := s.modelAndThinking(); model != "" {
		t.Errorf("session model = %q, want empty (spawn flag must not change on a failed switch)", model)
	}
}

func TestPiSession_SetLiveModel_JSONModeUnsupported(t *testing.T) {
	// One-shot json mode has no persistent process to switch; the engine must
	// keep using the respawn path there.
	s := newTestSession(false)
	if ok := s.SetLiveModel("deepseek-flash"); ok {
		t.Fatalf("json-mode session reported a live model switch")
	}
}

func TestPiSession_RPC_SetLiveReasoningEffort_SendsThinkingLevel(t *testing.T) {
	s, record := newFakeRPCSwitchSession(t, fakeRPCOptions{})

	if ok := s.SetLiveReasoningEffort("high"); !ok {
		t.Fatalf("SetLiveReasoningEffort(high) = false, want true")
	}

	frame := frameWithType(t, recordedFrames(t, record), "set_thinking_level")
	if !strings.Contains(frame, `"level":"high"`) {
		t.Fatalf("unexpected set_thinking_level frame: %s", frame)
	}
	if _, thinking := s.modelAndThinking(); thinking != "high" {
		t.Errorf("session thinking = %q, want %q", thinking, "high")
	}
}

func TestPiSession_RPC_SetLiveReasoningEffort_FailureReturnsFalse(t *testing.T) {
	s, _ := newFakeRPCSwitchSession(t, fakeRPCOptions{fail: true})

	if ok := s.SetLiveReasoningEffort("high"); ok {
		t.Fatalf("SetLiveReasoningEffort returned true for a success=false response")
	}
	if _, thinking := s.modelAndThinking(); thinking != "" {
		t.Errorf("session thinking = %q, want empty after a failed switch", thinking)
	}
}

func TestPiSession_RPC_CallRPC_TimesOutOnSilentPi(t *testing.T) {
	// A pi that never answers must not hang the /model or /reasoning command:
	// callRPC bounds the wait and reports an error so the caller can fall back.
	s, _ := newFakeRPCSwitchSession(t, fakeRPCOptions{})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	start := time.Now()
	_, err := s.callRPC(ctx, "cc-connect-never-answered", map[string]any{"type": "never_answered"}, 150*time.Millisecond)
	if err == nil {
		t.Fatalf("callRPC returned nil error for a command pi never answered")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("callRPC took %v; the switch timeout must bound it", elapsed)
	}
}

func TestPiSession_RPC_UnknownResponseIdIsIgnored(t *testing.T) {
	// Responses that belong to no pending request (a late reply after a
	// timeout, or pi's unsolicited output) must not disturb the session.
	s, record := newFakeRPCSwitchSession(t, fakeRPCOptions{})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := s.callRPC(ctx, "cc-connect-late", map[string]any{"type": "never_answered"}, 100*time.Millisecond); err == nil {
		t.Fatalf("expected timeout")
	}
	// The session is still usable and the get_state probe id is still handled
	// by its own branch.
	if ok := s.SetLiveReasoningEffort("low"); !ok {
		t.Fatalf("session unusable after a timed-out request; frames=%v", recordedFrames(t, record))
	}
}

func TestAgent_PreservesSessionOnReasoningEffortChange(t *testing.T) {
	// Without this, /reasoning on a pi session cleared the session history
	// (core.resetSessionForReasoningChange) even though pi can resume the same
	// conversation with a new thinking level.
	a := &Agent{}
	if !a.PreservesSessionOnReasoningEffortChange() {
		t.Fatalf("pi must preserve the session on a reasoning-effort change")
	}
}

func TestAgent_ValidateSessionRuntime_RejectsUnknownThinkingLevel(t *testing.T) {
	a := &Agent{}
	if err := a.ValidateSessionRuntime("deepseek-flash", "high"); err != nil {
		t.Errorf("ValidateSessionRuntime(high) = %v, want nil", err)
	}
	if err := a.ValidateSessionRuntime("", ""); err != nil {
		t.Errorf("ValidateSessionRuntime(empty) = %v, want nil", err)
	}
	if err := a.ValidateSessionRuntime("", "bogus"); err == nil {
		t.Errorf("ValidateSessionRuntime(bogus) = nil, want error")
	}
}

func TestAgent_StartSessionWithRuntime_AppliesOverridesWithoutChangingDefaults(t *testing.T) {
	// Regression: without core.SessionModelStarter the engine silently dropped
	// a session's model override at spawn (startAgentSessionWithRuntimeOverrides
	// falls through to StartSession), and a reasoning override made it error out
	// with "does not support per-session reasoning".
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv.txt")
	script := filepath.Join(dir, "fake-pi-args.sh")
	scriptBody := `#!/bin/sh
printf '%s\n' "$@" > "$PI_ARGV_FILE"
while IFS= read -r line; do
    id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
    case "$line" in
        *'"type":"get_state"'*)
            printf '{"id":"%s","type":"response","command":"get_state","success":true,"data":{"sessionId":"s-1"}}\n' "$id"
            ;;
    esac
done
`
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatalf("write fake pi script: %v", err)
	}

	a, err := New(map[string]any{
		"cmd":      script,
		"work_dir": dir,
		"model":    "deepseek-v4-pro",
		"thinking": "low",
		"rpc":      true,
		"env":      map[string]any{"PI_ARGV_FILE": argvFile},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	piAgent := a.(*Agent)

	sess, err := piAgent.StartSessionWithRuntime(context.Background(), "", "deepseek-flash", "high")
	if err != nil {
		t.Fatalf("StartSessionWithRuntime: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	data, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("read argv: %v", err)
	}
	args := string(data)
	if !strings.Contains(args, "--model\ndeepseek-flash\n") {
		t.Errorf("spawn args missing model override:\n%s", args)
	}
	if !strings.Contains(args, "--thinking\nhigh\n") {
		t.Errorf("spawn args missing thinking override:\n%s", args)
	}
	if _, ok := sess.(core.LiveModelSwitcher); !ok {
		t.Errorf("piSession must implement core.LiveModelSwitcher")
	}
	if _, ok := sess.(core.LiveReasoningEffortSwitcher); !ok {
		t.Errorf("piSession must implement core.LiveReasoningEffortSwitcher")
	}
	// The agent-wide defaults stay untouched: StartSessionWithRuntime is
	// per-conversation.
	if got := piAgent.GetModel(); got != "deepseek-v4-pro" {
		t.Errorf("agent model = %q, want the configured default", got)
	}
	if got := piAgent.GetReasoningEffort(); got != "low" {
		t.Errorf("agent thinking = %q, want the configured default", got)
	}
}
