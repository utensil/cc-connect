// ACP session: a persistent `grok agent stdio` process driven over JSON-RPC.
//
// One process serves the whole conversation, so the adapter can inject
// steering guidance into the running turn, cancel a turn without tearing the
// session down, and switch model/effort between turns. The headless `-p`
// transport in session.go remains available via backend = "headless".
package grok

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// acpConfig is the start-session snapshot for the ACP transport.
type acpConfig struct {
	cmd             string
	extraArgs       []string
	workDir         string
	model           string
	mode            string
	reasoningEffort string
	resumeID        string
	extraEnv        []string
	timeout         time.Duration
	nativeSteer     bool
	// steerHome, when non-empty, is exported as GROK_HOME for the child so the
	// CLI runs with the mid-turn steering setting cc-connect configured.
	steerHome string
	// autoApprovePermissions answers permission requests inside the adapter with
	// the most permissive option instead of forwarding them to the platform.
	// --always-approve cannot cover the requests Grok raises for
	// Claude-compatible `deny` rules (for example a global `Read(.env)` deny in
	// ~/.claude/settings.json), so without this a bot turn can block on a human
	// reply even in yolo mode.
	autoApprovePermissions bool
}

type acpSession struct {
	cfg acpConfig

	events chan core.Event
	ctx    context.Context
	cancel context.CancelFunc

	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.Writer
	proc   *exec.Cmd

	writeMu sync.Mutex
	nextID  atomic.Int64

	pendingMu     sync.Mutex
	pending       map[int64]chan acpFrame
	transportDead bool

	permissionMu       sync.Mutex
	pendingPermissions map[string]pendingPermission

	sessionID atomic.Value // string
	alive     atomic.Bool
	started   atomic.Bool

	// turnMu guards the in-flight prompt bookkeeping. inflight maps a request
	// id to its kind so a steer can be told apart from the turn's own prompt;
	// cancelReasons records why a specific turn was cancelled, because the
	// reason must be read by that turn's own settle rather than by whichever
	// turn happens to settle next.
	turnMu        sync.Mutex
	inflight      map[int64]promptKind
	primary       int64
	cancelReasons map[int64]cancelReason
	// handoverPending records that the CLI cancelled the primary prompt to
	// serve a pending steer prompt as its own turn, so nobody owns the engine
	// turn yet. If that steer prompt also ends cancelled, its settle emits the
	// terminal result (silently, per the engine's cancelled-stop rule) instead
	// of leaving the engine turn open forever.
	handoverPending bool

	// streamMu guards the coalescing buffers and the last usage payload, which
	// the read loop and the prompt-settle goroutines both touch.
	streamMu       sync.Mutex
	pendingThought strings.Builder
	textStreamed   bool
	lastUsage      map[string]any
	toolNames      map[string]string

	runtimeMu       sync.Mutex
	model           string
	reasoningEffort string

	wg        sync.WaitGroup
	closeOnce sync.Once
}

// newACPSession spawns `grok agent stdio` and performs the ACP handshake.
func newACPSession(ctx context.Context, cfg acpConfig) (*acpSession, error) {
	if cfg.cmd == "" {
		return nil, fmt.Errorf("grok acp: command is empty")
	}
	if cfg.workDir == "" {
		cfg.workDir = "."
	}
	// ACP locates a stored session by its session folder, which Grok keys off
	// the *resolved* cwd: handing it `/tmp/x` on macOS, where the real path is
	// `/private/tmp/x`, makes `session/load` fail with "Path not found" and the
	// adapter would silently start a fresh session instead of resuming.
	cfg.workDir = canonicalWorkDir(cfg.workDir)

	args := append([]string{}, cfg.extraArgs...)
	args = append(args, "agent")
	if cfg.mode != "plan" {
		// Headless parity: non-plan modes auto-approve so an IM turn cannot
		// block on a permission prompt.
		args = append(args, "--always-approve")
	}
	if model := strings.TrimSpace(cfg.model); model != "" {
		args = append(args, "-m", model)
	}
	if effort := strings.TrimSpace(cfg.reasoningEffort); effort != "" {
		args = append(args, "--reasoning-effort", effort)
	}
	args = append(args, "stdio")

	cmd := exec.CommandContext(ctx, cfg.cmd, args...)
	cmd.Dir = cfg.workDir
	env := os.Environ()
	if len(cfg.extraEnv) > 0 {
		env = core.MergeEnv(env, cfg.extraEnv)
	}
	if cfg.steerHome != "" {
		env = core.MergeEnv(env, []string{"GROK_HOME=" + cfg.steerHome})
	}
	cmd.Env = env
	cmd.WaitDelay = time.Second

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("grok acp: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("grok acp: stdout pipe: %w", err)
	}
	stderrBuf := &acpStderrBuffer{}
	cmd.Stderr = stderrBuf
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("grok acp: start: %w", err)
	}

	s, err := newACPSessionWithStream(ctx, cfg, stdin, stdout)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	s.proc = cmd
	s.stderr = stderrBuf
	slog.Info("grok acp: session started",
		"pid", cmd.Process.Pid, "work_dir", cfg.workDir,
		"native_steer", cfg.nativeSteer, "overlay_home", cfg.steerHome != "")
	return s, nil
}

// newACPSessionWithStream builds a session over an already-connected transport
// and runs the handshake. Tests use it directly with pipes.
func newACPSessionWithStream(parent context.Context, cfg acpConfig, stdin io.WriteCloser, stdout io.ReadCloser) (*acpSession, error) {
	if cfg.workDir == "" {
		cfg.workDir = "."
	}
	cfg.workDir = canonicalWorkDir(cfg.workDir)
	sessionCtx, cancel := context.WithCancel(parent)
	s := &acpSession{
		cfg:                cfg,
		events:             make(chan core.Event, 128),
		ctx:                sessionCtx,
		cancel:             cancel,
		stdin:              stdin,
		stdout:             stdout,
		pending:            make(map[int64]chan acpFrame),
		pendingPermissions: make(map[string]pendingPermission),
		inflight:           make(map[int64]promptKind),
		cancelReasons:      make(map[int64]cancelReason),
		toolNames:          make(map[string]string),
		model:              cfg.model,
		reasoningEffort:    cfg.reasoningEffort,
	}
	s.alive.Store(true)
	s.wg.Add(1)
	go s.readLoop(stdout)

	if err := s.handshake(); err != nil {
		cancel()
		s.failPending(err)
		return nil, err
	}
	s.started.Store(true)
	return s, nil
}

// acpStderrBuffer keeps the last chunk of the CLI's stderr for error messages
// without letting a chatty process grow the buffer without bound.
type acpStderrBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *acpStderrBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > 8*1024 {
		b.buf = b.buf[len(b.buf)-8*1024:]
	}
	return len(p), nil
}

func (b *acpStderrBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.buf))
}

// canonicalWorkDir resolves symlinks so session/load and session/new agree on
// the folder Grok stores sessions under. A missing directory is returned as is;
// the CLI reports the real error.
func canonicalWorkDir(dir string) string {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil || resolved == "" {
		return dir
	}
	return resolved
}

func (s *acpSession) handshake() error {
	// clientCapabilities stays empty on purpose: the agent then runs its tools
	// in-process instead of asking this adapter for fs/terminal handlers.
	params := map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{},
		"clientInfo":         map[string]any{"name": "cc-connect", "version": "1"},
	}
	var init struct {
		ProtocolVersion int `json:"protocolVersion"`
	}
	if err := s.request(s.ctx, "initialize", params, &init); err != nil {
		return fmt.Errorf("grok acp: initialize: %w", err)
	}
	if init.ProtocolVersion != 0 && init.ProtocolVersion != 1 {
		// Keep going: ACP is expected to stay backward compatible, but a silent
		// version jump is the first thing to check when frames stop mapping.
		slog.Warn("grok acp: unexpected ACP protocol version", "protocol_version", init.ProtocolVersion)
	}
	return s.openSession()
}

type acpSessionOpenResult struct {
	SessionID     string               `json:"sessionId"`
	Models        acpSessionModelState `json:"models"`
	ConfigOptions []acpConfigOption    `json:"configOptions"`
	Modes         struct {
		CurrentModeID  string `json:"currentModeId"`
		AvailableModes []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"availableModes"`
	} `json:"modes"`
}

// acpConfigOption is one row of the session config catalog reported by
// session/new and session/load. `model` and `reasoning_effort` are the rows this
// adapter writes; currentValue is what the session will actually use.
type acpConfigOption struct {
	ID           string `json:"id"`
	CurrentValue string `json:"currentValue"`
}

func (s *acpSession) openSession() error {
	meta := map[string]any{}
	if s.cfg.mode == "yolo" || s.cfg.mode == "accept_edits" || s.cfg.mode == "dont_ask" {
		meta["yoloMode"] = true
	}

	resumeID := s.cfg.resumeID
	if resumeID != "" && resumeID != core.ContinueSession {
		var loaded acpSessionOpenResult
		err := s.request(s.ctx, "session/load", map[string]any{
			"sessionId":  resumeID,
			"cwd":        s.cfg.workDir,
			"mcpServers": []any{},
			"_meta":      meta,
		}, &loaded)
		if err == nil {
			id := strings.TrimSpace(loaded.SessionID)
			if id == "" {
				id = resumeID
			}
			s.sessionID.Store(id)
			s.applySessionMode(loaded)
			s.applySpawnRuntime(loaded)
			slog.Info("grok acp: session resumed", "session_id", id)
			return nil
		}
		slog.Warn("grok acp: resume failed, starting a new session",
			"session_id", resumeID, "error", err)
	}

	var created acpSessionOpenResult
	if err := s.request(s.ctx, "session/new", map[string]any{
		"cwd":        s.cfg.workDir,
		"mcpServers": []any{},
		"_meta":      meta,
	}, &created); err != nil {
		return fmt.Errorf("grok acp: session/new: %w", err)
	}
	if created.SessionID == "" {
		return fmt.Errorf("grok acp: session/new returned an empty session id")
	}
	s.sessionID.Store(created.SessionID)
	s.applySessionMode(created)
	s.applySpawnRuntime(created)
	slog.Info("grok acp: session created", "session_id", created.SessionID)
	return nil
}

// applySpawnRuntime pushes the configured model and reasoning effort into the
// session.
//
// The `-m` / `--reasoning-effort` flags handed to `grok agent stdio` do not
// change the session runtime on grok 1.0.70: a child spawned with `-m grok-4.6`
// still opens (and uses) grok-4.7, and `--reasoning-effort low` still runs at the
// account default. ACP exposes both as session config options, so the adapter
// applies them explicitly, right after session/new or session/load — the same
// guarantee the codex app-server gets from the per-turn `effort` field, and the
// reason a configured effort also reaches grok's own subagents. Values the
// session already runs at are not re-sent; a failure is logged and the session
// keeps working with the agent's own default.
func (s *acpSession) applySpawnRuntime(result acpSessionOpenResult) {
	current := make(map[string]string, len(result.ConfigOptions))
	for _, opt := range result.ConfigOptions {
		if opt.ID != "" {
			current[opt.ID] = opt.CurrentValue
		}
	}

	for _, want := range []struct{ configID, value string }{
		{"model", s.cfg.model},
		{"reasoning_effort", s.cfg.reasoningEffort},
	} {
		value := strings.TrimSpace(want.value)
		if value == "" || current[want.configID] == value {
			continue
		}
		if err := s.setConfigOption(want.configID, value); err != nil {
			slog.Warn("grok acp: spawn runtime not applied",
				"config_id", want.configID, "value", value, "error", err)
			continue
		}
		s.runtimeMu.Lock()
		if want.configID == "model" {
			s.model = value
		} else {
			s.reasoningEffort = value
		}
		s.runtimeMu.Unlock()
		slog.Info("grok acp: spawn runtime applied", "config_id", want.configID, "value", value)
	}
}

// applySessionMode maps cc-connect's plan mode onto the ACP session mode when
// the agent exposes one.
func (s *acpSession) applySessionMode(result acpSessionOpenResult) {
	if s.cfg.mode != "plan" {
		return
	}
	for _, m := range result.Modes.AvailableModes {
		if m.ID == result.Modes.CurrentModeID {
			continue
		}
		if strings.Contains(strings.ToLower(m.ID+" "+m.Name), "plan") {
			ctx, cancel := context.WithTimeout(s.ctx, acpControlTimeout)
			defer cancel()
			if err := s.request(ctx, "session/set_mode", map[string]any{
				"sessionId": s.CurrentSessionID(),
				"modeId":    m.ID,
			}, nil); err != nil {
				slog.Debug("grok acp: session/set_mode failed", "mode", m.ID, "error", err)
			}
			return
		}
	}
}

func (s *acpSession) Send(prompt string, images []core.ImageAttachment, files []core.FileAttachment) error {
	fullPrompt, cleanup := stageTurnAttachments(s.cfg.workDir, prompt, images, files)
	return s.promptAsync(fullPrompt, promptPrimary, cleanup)
}

// Steer injects guidance into the running turn.
//
// With native steering enabled the agent folds the extra prompt into the
// active turn at its next safe point. When the CLI was not told to steer
// (nativeSteer false) the adapter keeps cc-connect's steer semantics itself:
// it cancels the running turn and immediately continues with the guidance.
func (s *acpSession) Steer(prompt string) error {
	if !s.alive.Load() {
		return fmt.Errorf("grok acp: session is closed")
	}
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("grok acp: steering prompt is empty")
	}

	if s.nativeSteer() {
		return s.promptAsync(prompt, promptSteer, nil)
	}
	return s.steerByCancel(prompt, nil)
}

// steerByCancel delivers guidance by cancelling the running turn and opening a
// replacement one. It backs both `steer_delivery = "cancel"` and the fallback
// taken when a native injection is rejected by the agent. The replacement
// prompt owns the engine turn, so the cancelled turn's settle is suppressed by
// the per-turn cancel reason.
func (s *acpSession) steerByCancel(prompt string, cleanup func()) error {
	s.markCancelReason(cancelForSteer)
	if err := s.notify("session/cancel", map[string]any{"sessionId": s.CurrentSessionID()}); err != nil {
		if cleanup != nil {
			cleanup()
		}
		return err
	}
	if !s.waitForTurn(acpCancelWait) {
		// Starting a second prompt while the first is still running would hand
		// the engine two terminal events; report the failure instead so the
		// caller can keep the guidance in the queue.
		if cleanup != nil {
			cleanup()
		}
		return fmt.Errorf("grok acp: cancelled turn did not settle within %s", acpCancelWait)
	}
	return s.promptAsync(prompt, promptPrimary, cleanup)
}

// CancelTurn stops the running turn and keeps the session (and its
// conversation) alive, so /stop no longer costs the adapter a process restart.
func (s *acpSession) CancelTurn() error {
	if !s.alive.Load() {
		return fmt.Errorf("grok acp: session is closed")
	}
	s.turnMu.Lock()
	busy := len(s.inflight) > 0
	s.turnMu.Unlock()
	if !busy {
		return nil
	}

	s.markCancelReason(cancelForStop)
	if err := s.notify("session/cancel", map[string]any{"sessionId": s.CurrentSessionID()}); err != nil {
		return err
	}
	if !s.waitForTurn(acpCancelWait) {
		// Report failure so the engine falls back to closing the session: /stop
		// must guarantee that the turn stopped, and the stored session id still
		// resumes the conversation on the next message.
		return fmt.Errorf("grok acp: turn did not settle within %s", acpCancelWait)
	}
	return nil
}

// waitForTurn blocks until no prompt is in flight. It reports false on timeout.
func (s *acpSession) waitForTurn(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		s.turnMu.Lock()
		busy := len(s.inflight) > 0
		s.turnMu.Unlock()
		if !busy {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-s.ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

func (s *acpSession) nativeSteer() bool {
	return s.cfg.nativeSteer
}

// SetLiveModel switches the session model between turns (core.LiveModelSwitcher).
func (s *acpSession) SetLiveModel(model string) bool {
	model = strings.TrimSpace(model)
	if model == "" || !s.alive.Load() {
		return false
	}
	if err := s.setConfigOption("model", model); err != nil {
		slog.Debug("grok acp: live model change failed", "model", model, "error", err)
		return false
	}
	s.runtimeMu.Lock()
	s.model = model
	s.runtimeMu.Unlock()
	slog.Info("grok acp: live model changed", "model", model)
	return true
}

// SetLiveReasoningEffort switches the reasoning effort between turns.
func (s *acpSession) SetLiveReasoningEffort(effort string) bool {
	effort = strings.TrimSpace(effort)
	if effort == "" || !s.alive.Load() {
		return false
	}
	if err := s.setConfigOption("reasoning_effort", effort); err != nil {
		slog.Debug("grok acp: live reasoning change failed", "effort", effort, "error", err)
		return false
	}
	s.runtimeMu.Lock()
	s.reasoningEffort = effort
	s.runtimeMu.Unlock()
	slog.Info("grok acp: live reasoning effort changed", "effort", effort)
	return true
}

func (s *acpSession) setConfigOption(configID, value string) error {
	ctx, cancel := context.WithTimeout(s.ctx, acpControlTimeout)
	defer cancel()

	var out json.RawMessage
	// Grok's `SessionConfigOptionValue` is an untagged enum that takes the bare
	// value. The `{"value": …}` wrapper is rejected with -32602 "Invalid
	// params" on 1.0.70, so keep it only as a fallback for builds that expect
	// the wrapped shape.
	err := s.request(ctx, "session/set_config_option", map[string]any{
		"sessionId": s.CurrentSessionID(),
		"configId":  configID,
		"value":     value,
	}, &out)
	if err == nil {
		return nil
	}
	if acpErr, ok := err.(*acpError); !ok || acpErr.Code != -32602 {
		return err
	}
	return s.request(ctx, "session/set_config_option", map[string]any{
		"sessionId": s.CurrentSessionID(),
		"configId":  configID,
		"value":     map[string]any{"value": value},
	}, &out)
}

func (s *acpSession) RespondPermission(requestID string, result core.PermissionResult) error {
	s.permissionMu.Lock()
	pending, ok := s.pendingPermissions[requestID]
	delete(s.pendingPermissions, requestID)
	s.permissionMu.Unlock()
	if !ok {
		return fmt.Errorf("grok acp: unknown permission request %q", requestID)
	}

	allow := strings.EqualFold(strings.TrimSpace(result.Behavior), "allow")
	optionID := pickPermissionOption(pending.options, allow)
	outcome := map[string]any{"outcome": "cancelled"}
	if optionID != "" {
		outcome = map[string]any{"outcome": "selected", "optionId": optionID}
	}
	return s.writeFrame(map[string]any{
		"jsonrpc": "2.0",
		"id":      pending.id,
		"result":  map[string]any{"outcome": outcome},
	})
}

// pickAutoApproveOption chooses the option that grants the request for the rest
// of the session, falling back to any allow-shaped option.
func pickAutoApproveOption(options []acpPermissionOption) string {
	for _, opt := range options {
		blob := strings.ToLower(opt.Kind + " " + opt.OptionID + " " + opt.Name)
		if strings.Contains(blob, "allow") && strings.Contains(blob, "always") {
			return opt.OptionID
		}
	}
	return pickPermissionOption(options, true)
}

// pickPermissionOption chooses the ACP option matching the user's decision.
func pickPermissionOption(options []acpPermissionOption, allow bool) string {
	preferredKinds := []string{"reject_once", "reject_always", "deny"}
	if allow {
		preferredKinds = []string{"allow_once", "allow_always"}
	}
	for _, want := range preferredKinds {
		for _, opt := range options {
			if strings.EqualFold(opt.Kind, want) {
				return opt.OptionID
			}
		}
	}
	for _, opt := range options {
		kind := strings.ToLower(opt.Kind)
		name := strings.ToLower(opt.Name)
		if allow && (strings.Contains(kind, "allow") || strings.Contains(name, "allow")) {
			return opt.OptionID
		}
		if !allow && (strings.Contains(kind, "reject") || strings.Contains(kind, "deny") ||
			strings.Contains(name, "reject") || strings.Contains(name, "deny")) {
			return opt.OptionID
		}
	}
	return ""
}

func (s *acpSession) Events() <-chan core.Event { return s.events }

func (s *acpSession) CurrentSessionID() string {
	v, _ := s.sessionID.Load().(string)
	return v
}

func (s *acpSession) Alive() bool { return s.alive.Load() }

func (s *acpSession) Close() error {
	s.closeOnce.Do(func() {
		s.alive.Store(false)

		if s.started.Load() && s.CurrentSessionID() != "" {
			ctx, cancel := context.WithTimeout(context.Background(), acpCloseGrace)
			_ = s.request(ctx, "session/close", map[string]any{
				"sessionId": s.CurrentSessionID(),
			}, nil)
			cancel()
		}

		if s.stdin != nil {
			_ = s.stdin.Close()
		}
		// Closing the read side unblocks a read loop parked on the pipe even
		// when there is no process to kill (tests, or an already-dead agent).
		if s.stdout != nil {
			_ = s.stdout.Close()
		}
		if s.proc != nil && s.proc.Process != nil {
			_ = s.proc.Process.Kill()
		}
		s.cancel()
		s.failPending(errors.New("grok acp: session closed"))
		s.wg.Wait()
		close(s.events)
	})
	return nil
}

var (
	_ core.AgentSession                = (*acpSession)(nil)
	_ core.AgentSessionCanceller       = (*acpSession)(nil)
	_ core.SessionSteerer              = (*acpSession)(nil)
	_ core.LiveModelSwitcher           = (*acpSession)(nil)
	_ core.LiveReasoningEffortSwitcher = (*acpSession)(nil)
)
