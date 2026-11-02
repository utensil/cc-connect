// ACP transport for the Grok Build CLI.
//
// `grok agent stdio` speaks the Agent Client Protocol (JSON-RPC 2.0, one object
// per line) over stdin/stdout and keeps a session alive across prompts, which
// is what makes native steering, turn cancellation and live model switching
// possible. The headless `grok -p` transport in session.go is one process per
// turn and has none of those primitives.
//
// Wire notes verified against grok 1.0.70:
//   - `session/cancel` is a *notification*; sending it as a request fails with
//     -32601 (method not found).
//   - A `session/prompt` sent while a turn is running is queued by the agent
//     unless the agent runs with `[ui] follow_up_behavior = "steer"`, in which
//     case the text is injected into the running turn and the steer request
//     itself settles with `stopReason: "cancelled"`. See steerhome.go for how
//     this adapter passes that setting to the CLI.
//   - With an empty `clientCapabilities` the agent runs tools in-process, so no
//     fs/terminal client handlers are required.
package grok

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

const (
	// acpRequestTimeout bounds handshake and control requests. Model turns are
	// not bounded by it: a prompt settles when the agent answers.
	acpRequestTimeout = 60 * time.Second
	// acpControlTimeout bounds the small bookkeeping requests issued between
	// turns (model switch, config option, graceful close).
	acpControlTimeout = 20 * time.Second
	// acpCloseGrace bounds the graceful `session/close` before the process is
	// killed outright.
	acpCloseGrace = 3 * time.Second
)

// acpCancelWait bounds how long a cancellation waits for the cancelled prompt
// to settle. It is a variable so tests can shorten it.
var acpCancelWait = 30 * time.Second

// promptKind distinguishes the request that opens a user turn from the request
// that carries steering guidance into it.
type promptKind int

const (
	promptPrimary promptKind = iota
	promptSteer
)

// cancelReason records why a cancellation was requested, because the two cases
// end the engine turn differently.
type cancelReason int

const (
	cancelNone cancelReason = iota
	// cancelForStop keeps the session alive but closes the engine turn, so
	// /stop reports "stopped" and the next message starts fresh.
	cancelForStop
	// cancelForSteer is internal to a native steer fallback: the cancelled turn
	// must not close the engine turn, because the replacement prompt does.
	cancelForSteer
)

// acpError is the JSON-RPC error object returned by the agent.
type acpError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *acpError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message == "" {
		return fmt.Sprintf("grok acp: json-rpc error %d", e.Code)
	}
	return fmt.Sprintf("grok acp: %s (code %d)", e.Message, e.Code)
}

// acpFrame is one JSON-RPC object read from the agent.
type acpFrame struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *acpError       `json:"error,omitempty"`
}

// acpPromptResult is the settled reply to session/prompt.
type acpPromptResult struct {
	StopReason    string `json:"stopReason"`
	UserMessageID string `json:"userMessageId"`
	Meta          struct {
		SessionID     string `json:"sessionId"`
		RequestID     string `json:"requestId"`
		InputTokens   int    `json:"inputTokens"`
		OutputTokens  int    `json:"outputTokens"`
		TotalTokens   int    `json:"totalTokens"`
		CachedRead    int    `json:"cachedReadTokens"`
		CachedWrite   int    `json:"cachedWriteTokens"`
		ReasoningToks int    `json:"reasoningTokens"`
	} `json:"_meta"`
}

// acpSessionModelState is the model catalog returned by session/new and
// session/load. It mirrors ACP's SessionModelState.
type acpSessionModelState struct {
	CurrentModelID  string `json:"currentModelId"`
	AvailableModels []struct {
		ModelID string `json:"modelId"`
		Name    string `json:"name"`
	} `json:"availableModels"`
}

// acpPermissionOption is one row of an ACP permission prompt.
type acpPermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// pendingPermission keeps an unanswered session/request_permission so
// RespondPermission can select one of its options.
type pendingPermission struct {
	id      json.RawMessage
	options []acpPermissionOption
}

func (s *acpSession) emit(evt core.Event) {
	select {
	case <-s.ctx.Done():
		return
	default:
	}
	select {
	case s.events <- evt:
	case <-s.ctx.Done():
	}
}

// writeFrame writes one JSON-RPC object. Writes are serialized so a steer can
// never interleave bytes with a prompt or a cancellation.
func (s *acpSession) writeFrame(m map[string]any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("grok acp: encode frame: %w", err)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.stdin == nil {
		return fmt.Errorf("grok acp: stdin is closed")
	}
	if _, err := s.stdin.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("grok acp: write frame: %w", err)
	}
	return nil
}

func (s *acpSession) notify(method string, params any) error {
	frame := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		frame["params"] = params
	}
	return s.writeFrame(frame)
}

// request sends a JSON-RPC request and waits for its reply. Used for the
// handshake and control calls; prompts use promptAsync instead so they can run
// concurrently with the read loop.
func (s *acpSession) request(ctx context.Context, method string, params any, out any) error {
	id := s.nextID.Add(1)
	ch := s.registerPending(id)
	defer s.forgetPending(id)

	frame := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		frame["params"] = params
	}
	if err := s.writeFrame(frame); err != nil {
		return err
	}

	timer := time.NewTimer(acpRequestTimeout)
	defer timer.Stop()
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return resp.Error
		}
		if out != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, out); err != nil {
				return fmt.Errorf("grok acp: decode %s response: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ctx.Done():
		return fmt.Errorf("grok acp: session closed while waiting for %s", method)
	case <-timer.C:
		return fmt.Errorf("grok acp: %s timed out", method)
	}
}

func (s *acpSession) registerPending(id int64) chan acpFrame {
	ch := make(chan acpFrame, 1)
	s.pendingMu.Lock()
	if s.transportDead {
		s.pendingMu.Unlock()
		ch <- acpFrame{Error: &acpError{Code: -32000, Message: "grok acp: agent transport is closed"}}
		return ch
	}
	s.pending[id] = ch
	s.pendingMu.Unlock()
	return ch
}

func (s *acpSession) forgetPending(id int64) {
	s.pendingMu.Lock()
	delete(s.pending, id)
	s.pendingMu.Unlock()
}

// failPending unblocks every waiter (handshake or prompt) once the transport is
// gone, so no caller can hang on a dead process.
func (s *acpSession) failPending(err error) {
	s.pendingMu.Lock()
	s.transportDead = true
	pending := s.pending
	s.pending = make(map[int64]chan acpFrame)
	s.pendingMu.Unlock()

	for _, ch := range pending {
		select {
		case ch <- acpFrame{Error: &acpError{Code: -32000, Message: err.Error()}}:
		default:
		}
	}
}

func (s *acpSession) readLoop(stdout io.Reader) {
	defer s.wg.Done()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), core.AgentStreamMaxLineBytes)
	var longLineWarned atomic.Bool

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "{") {
			slog.Debug("grok acp: non-JSON line", "line", truncate(line, 200))
			continue
		}
		core.WarnOnLongAgentStreamLine("grok-acp", len(line), &longLineWarned)
		s.handleFrame([]byte(line))
	}

	if err := scanner.Err(); err != nil && s.alive.Load() && s.ctx.Err() == nil {
		// The in-flight turn is failed through failPending below; emitting here
		// too would give the engine a second, stale error event.
		slog.Error("grok acp: read stdout", "error", err)
	}

	// The process is gone: fail waiters and mark the session dead so the engine
	// rebuilds an interactive state on the next message.
	wasAlive := s.alive.Swap(false)
	s.failPending(errors.New("grok acp: agent process exited"))
	if wasAlive && s.ctx.Err() == nil {
		stderrTail := ""
		if buf, ok := s.stderr.(*acpStderrBuffer); ok {
			stderrTail = truncate(buf.String(), 800)
		}
		slog.Warn("grok acp: agent process exited", "stderr", stderrTail)
	}
}

func (s *acpSession) handleFrame(line []byte) {
	var frame acpFrame
	if err := json.Unmarshal(line, &frame); err != nil {
		slog.Debug("grok acp: unparsable frame", "error", err, "line", truncate(string(line), 200))
		return
	}

	switch {
	case frame.Method != "" && len(frame.ID) > 0:
		// Agent -> client request (permissions). It needs an answer.
		s.handleAgentRequest(frame)
	case frame.Method != "":
		s.handleNotification(frame)
	case len(frame.ID) > 0:
		s.handleResponse(frame)
	}
}

func (s *acpSession) handleResponse(frame acpFrame) {
	var id int64
	if err := json.Unmarshal(frame.ID, &id); err != nil {
		slog.Debug("grok acp: response with non-numeric id", "id", string(frame.ID))
		return
	}
	s.pendingMu.Lock()
	ch := s.pending[id]
	s.pendingMu.Unlock()
	if ch == nil {
		// A settled prompt keeps its own waiter; anything else is stale.
		slog.Debug("grok acp: response for unknown request", "id", id)
		return
	}
	select {
	case ch <- frame:
	default:
	}
}

func (s *acpSession) handleNotification(frame acpFrame) {
	switch frame.Method {
	case "session/update":
		s.handleSessionUpdate(frame.Params)
	case "_x.ai/session_notification", "x.ai/session_notification":
		s.handleExtensionNotification(frame.Params)
	default:
		// Notifications such as _x.ai/mcp/server_status, x.ai/announcements/*
		// and x.ai/fs/* indexes carry no user-visible turn content.
		slog.Debug("grok acp: notification ignored", "method", frame.Method)
	}
}

// handleAgentRequest answers requests the agent sends to its client. The
// adapter advertises no fs/terminal capabilities, so the only request that can
// arrive is a permission prompt (non always-approve modes).
func (s *acpSession) handleAgentRequest(frame acpFrame) {
	switch frame.Method {
	case "session/request_permission":
		s.handlePermissionRequest(frame)
	default:
		slog.Debug("grok acp: unsupported agent request", "method", frame.Method)
		_ = s.writeFrame(map[string]any{
			"jsonrpc": "2.0",
			"id":      frame.ID,
			"error":   map[string]any{"code": -32601, "message": "method not supported by cc-connect"},
		})
	}
}

func (s *acpSession) handlePermissionRequest(frame acpFrame) {
	var params struct {
		SessionID string `json:"sessionId"`
		ToolCall  struct {
			ToolCallID string          `json:"toolCallId"`
			Title      string          `json:"title"`
			Kind       string          `json:"kind"`
			RawInput   json.RawMessage `json:"rawInput"`
		} `json:"toolCall"`
		Options []acpPermissionOption `json:"options"`
	}
	if err := json.Unmarshal(frame.Params, &params); err != nil {
		slog.Warn("grok acp: undecodable permission request", "error", err)
		_ = s.writeFrame(map[string]any{
			"jsonrpc": "2.0",
			"id":      frame.ID,
			"result": map[string]any{
				"outcome": map[string]any{"outcome": "cancelled"},
			},
		})
		return
	}

	// The request id is echoed back verbatim, so keep it as the opaque digits
	// or string the agent chose.
	requestID := "acp:" + string(frame.ID)

	s.permissionMu.Lock()
	if s.pendingPermissions == nil {
		s.pendingPermissions = make(map[string]pendingPermission)
	}
	s.pendingPermissions[requestID] = pendingPermission{id: frame.ID, options: params.Options}
	s.permissionMu.Unlock()

	toolName := params.ToolCall.Title
	if toolName == "" {
		toolName = params.ToolCall.Kind
	}
	s.emit(core.Event{
		Type:         core.EventPermissionRequest,
		RequestID:    requestID,
		ToolName:     toolName,
		ToolInput:    truncate(string(params.ToolCall.RawInput), 500),
		ToolInputRaw: rawToMap(params.ToolCall.RawInput),
		SessionID:    s.CurrentSessionID(),
	})
}

func (s *acpSession) handleSessionUpdate(raw json.RawMessage) {
	var params struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		slog.Debug("grok acp: undecodable session/update", "error", err)
		return
	}
	if params.SessionID != "" && s.CurrentSessionID() != "" && params.SessionID != s.CurrentSessionID() {
		return
	}

	var update struct {
		SessionUpdate string          `json:"sessionUpdate"`
		Content       json.RawMessage `json:"content"`
		ToolCallID    string          `json:"toolCallId"`
		Title         string          `json:"title"`
		Kind          string          `json:"kind"`
		Status        string          `json:"status"`
		RawInput      json.RawMessage `json:"rawInput"`
		RawOutput     json.RawMessage `json:"rawOutput"`
	}
	if err := json.Unmarshal(params.Update, &update); err != nil {
		slog.Debug("grok acp: undecodable session update body", "error", err)
		return
	}

	// session/load replays the stored transcript before the first prompt. Those
	// frames must not reach the platform as new output.
	if !s.turnActive() {
		slog.Debug("grok acp: update outside a turn ignored", "sessionUpdate", update.SessionUpdate)
		return
	}

	switch update.SessionUpdate {
	case "agent_message_chunk":
		text := acpTextContent(update.Content)
		if text == "" {
			return
		}
		s.flushThought()
		s.streamMu.Lock()
		s.textStreamed = true
		s.streamMu.Unlock()
		s.emit(core.Event{Type: core.EventText, Content: text, SessionID: s.CurrentSessionID()})

	case "agent_thought_chunk":
		if text := acpTextContent(update.Content); text != "" {
			s.streamMu.Lock()
			s.pendingThought.WriteString(text)
			s.streamMu.Unlock()
		}

	case "tool_call":
		s.flushThought()
		name := update.Title
		if name == "" {
			name = update.Kind
		}
		if update.ToolCallID != "" && name != "" {
			s.streamMu.Lock()
			s.toolNames[update.ToolCallID] = name
			s.streamMu.Unlock()
		}
		s.emit(core.Event{
			Type:      core.EventToolUse,
			ToolName:  name,
			ToolInput: truncate(string(update.RawInput), 500),
			RequestID: update.ToolCallID,
			SessionID: s.CurrentSessionID(),
		})

	case "tool_call_update":
		status := strings.ToLower(strings.TrimSpace(update.Status))
		if status != "completed" && status != "failed" {
			return
		}
		s.flushThought()
		result := acpToolResultText(update.RawOutput, update.Content)
		name := s.toolNames[update.ToolCallID]
		delete(s.toolNames, update.ToolCallID)
		if name == "" {
			name = update.Title
		}
		ok := status == "completed"
		s.emit(core.Event{
			Type:        core.EventToolResult,
			ToolName:    name,
			ToolResult:  truncate(result, 500),
			ToolStatus:  status,
			ToolSuccess: &ok,
			SessionID:   s.CurrentSessionID(),
		})

	case "usage_update":
		s.storeUsage(update.Content)

	case "plan", "user_message_chunk", "available_commands_update",
		"config_option_update", "session_info_update", "current_mode_update":
		// No IM representation; the engine renders plans from tool events.

	default:
		slog.Debug("grok acp: session update ignored", "sessionUpdate", update.SessionUpdate)
	}
}

// handleExtensionNotification consumes x.ai/session_notification frames, which
// report model changes and compaction state out of band from session/update.
func (s *acpSession) handleExtensionNotification(raw json.RawMessage) {
	var params struct {
		Update struct {
			SessionUpdate string `json:"sessionUpdate"`
			ModelID       string `json:"model_id"`
			Reasoning     string `json:"reasoning_effort"`
		} `json:"update"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return
	}
	if params.Update.SessionUpdate == "model_changed" && params.Update.ModelID != "" {
		slog.Debug("grok acp: model changed", "model", params.Update.ModelID, "effort", params.Update.Reasoning)
	}
}

func (s *acpSession) turnActive() bool {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	return len(s.inflight) > 0
}

func (s *acpSession) flushThought() {
	s.streamMu.Lock()
	if s.pendingThought.Len() == 0 {
		s.streamMu.Unlock()
		return
	}
	text := s.pendingThought.String()
	s.pendingThought.Reset()
	s.streamMu.Unlock()
	if strings.TrimSpace(text) == "" {
		return
	}
	s.emit(core.Event{Type: core.EventThinking, Content: text, SessionID: s.CurrentSessionID()})
}

// promptTurn is one in-flight session/prompt: the engine turn's own prompt, a
// steering injection, or the replacement prompt of a steer fallback.
type promptTurn struct {
	id      int64
	kind    promptKind
	text    string
	cleanup func()
	timer   *time.Timer
}

// promptAsync starts a turn (promptPrimary) or injects guidance into the
// running turn (promptSteer). The settled response is handled on its own
// goroutine so Send/Steer never block the engine while the model works.
func (s *acpSession) promptAsync(text string, kind promptKind, cleanup func()) error {
	if !s.alive.Load() {
		if cleanup != nil {
			cleanup()
		}
		return fmt.Errorf("grok acp: session is closed")
	}
	if strings.TrimSpace(text) == "" {
		if cleanup != nil {
			cleanup()
		}
		return fmt.Errorf("grok acp: prompt is empty")
	}
	sessionID := s.CurrentSessionID()
	if sessionID == "" {
		if cleanup != nil {
			cleanup()
		}
		return fmt.Errorf("grok acp: session id is not ready")
	}

	turn := &promptTurn{id: s.nextID.Add(1), kind: kind, text: text, cleanup: cleanup}
	ch := s.registerPending(turn.id)

	s.turnMu.Lock()
	s.inflight[turn.id] = kind
	if kind == promptPrimary {
		s.primary = turn.id
	}
	s.turnMu.Unlock()

	frame := map[string]any{
		"jsonrpc": "2.0",
		"id":      turn.id,
		"method":  "session/prompt",
		"params": map[string]any{
			"sessionId": sessionID,
			"prompt":    []map[string]any{{"type": "text", "text": text}},
		},
	}
	if err := s.writeFrame(frame); err != nil {
		s.forgetPending(turn.id)
		s.clearInflight(turn.id)
		if turn.cleanup != nil {
			turn.cleanup()
		}
		return err
	}

	// A configured turn timeout still applies to the ACP transport: cancel the
	// turn when it expires rather than leaving the user waiting forever. The
	// timer is stopped when the turn settles, so it can never cancel a later
	// turn.
	if s.cfg.timeout > 0 {
		turn.timer = time.AfterFunc(s.cfg.timeout, func() {
			s.turnMu.Lock()
			active := s.primary == turn.id
			s.turnMu.Unlock()
			if !active {
				return
			}
			slog.Warn("grok acp: turn timeout reached, cancelling",
				"timeout", s.cfg.timeout, "prompt_id", turn.id)
			_ = s.notify("session/cancel", map[string]any{"sessionId": sessionID})
		})
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		var reply acpFrame
		select {
		case reply = <-ch:
		case <-s.ctx.Done():
			reply = acpFrame{Error: &acpError{Code: -32000, Message: "session closed"}}
		}
		if turn.timer != nil {
			turn.timer.Stop()
		}
		s.forgetPending(turn.id)
		s.clearInflight(turn.id)
		if turn.cleanup != nil {
			// Attachment files must survive the turn (the agent may read them at
			// any point), so they are removed only once it settles.
			turn.cleanup()
			turn.cleanup = nil
		}
		s.settlePrompt(turn, reply)
	}()
	return nil
}

// markCancelReason records why the turn that is running now is being
// cancelled, so its own settle can decide whether to close the engine turn.
func (s *acpSession) markCancelReason(reason cancelReason) int64 {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	if s.primary == 0 {
		return 0
	}
	s.cancelReasons[s.primary] = reason
	return s.primary
}

// takeCancelReason consumes the reason recorded for one turn id.
func (s *acpSession) takeCancelReason(id int64) cancelReason {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	reason := s.cancelReasons[id]
	delete(s.cancelReasons, id)
	return reason
}

func (s *acpSession) clearInflight(id int64) {
	s.turnMu.Lock()
	delete(s.inflight, id)
	if s.primary == id {
		s.primary = 0
	}
	s.turnMu.Unlock()
}

// settlePrompt turns a settled session/prompt reply into engine events.
func (s *acpSession) settlePrompt(turn *promptTurn, frame acpFrame) {
	s.flushThought()

	if frame.Error != nil {
		if turn.kind == promptSteer {
			// A rejected steering injection must not surface as a turn error: the
			// engine would end the primary turn on the spot. Fall back to
			// cancel-and-continue so the guidance is still delivered.
			slog.Warn("grok acp: steering injection failed, falling back to cancel-and-continue",
				"error", frame.Error)
			if err := s.steerByCancel(turn.text, nil); err != nil {
				// The engine already acknowledged this message as steered, so
				// there is no queue to fall back to; surface it loudly instead of
				// ending the running turn with an error event.
				slog.Error("grok acp: steering fallback failed, guidance was not delivered",
					"error", err)
			}
			return
		}
		s.emit(core.Event{
			Type:      core.EventError,
			SessionID: s.CurrentSessionID(),
			Error:     frame.Error,
		})
		return
	}

	var result acpPromptResult
	if len(frame.Result) > 0 {
		if err := json.Unmarshal(frame.Result, &result); err != nil {
			slog.Warn("grok acp: undecodable prompt result", "error", err)
		}
	}
	stopReason := strings.ToLower(strings.TrimSpace(result.StopReason))

	if turn.kind == promptSteer {
		// A steered prompt settles as "cancelled" when the agent folded it into
		// the running turn: the running turn carries the answer, so there is
		// nothing to finish here.
		if stopReason == "cancelled" {
			slog.Debug("grok acp: steer folded into the active turn")
			return
		}
		// Grok delivers steering at its next safe point and deliberately holds
		// it while the agent is blocked on a long tool call, a background task
		// or a subagent. When the turn ends first, the guidance simply runs as
		// its own turn; emit the terminal event so its text is not dropped.
		slog.Info("grok acp: steer ran as its own turn", "stop_reason", stopReason)
		s.emitPromptResult(result)
		return
	}

	reason := s.takeCancelReason(turn.id)

	if stopReason == "cancelled" {
		if reason == cancelForSteer {
			// The steer replacement prompt owns the engine turn; closing it here
			// would end the turn before the guidance runs.
			slog.Debug("grok acp: cancelled turn replaced by a steer prompt")
			return
		}
		slog.Info("grok acp: turn cancelled")
		s.emit(core.Event{Type: core.EventResult, SessionID: s.CurrentSessionID(), Done: true})
		return
	}

	s.emitPromptResult(result)
}

// emitPromptResult emits the terminal EventResult for a finished turn,
// including the token accounting reported by the agent.
func (s *acpSession) emitPromptResult(result acpPromptResult) {
	evt := core.Event{
		Type:      core.EventResult,
		SessionID: s.CurrentSessionID(),
		Done:      true,
	}
	s.applyTokenMeta(&evt, result)
	s.emit(evt)
}

func (s *acpSession) applyTokenMeta(evt *core.Event, result acpPromptResult) {
	s.streamMu.Lock()
	usage := s.lastUsage
	s.streamMu.Unlock()

	if result.Meta.InputTokens > 0 {
		evt.InputTokens = result.Meta.InputTokens
	} else if usage != nil {
		evt.InputTokens = intFromAny(usage["inputTokens"])
		if evt.InputTokens == 0 {
			evt.InputTokens = intFromAny(usage["input_tokens"])
		}
	}
	if result.Meta.OutputTokens > 0 {
		evt.OutputTokens = result.Meta.OutputTokens
	} else if usage != nil {
		evt.OutputTokens = intFromAny(usage["outputTokens"])
		if evt.OutputTokens == 0 {
			evt.OutputTokens = intFromAny(usage["output_tokens"])
		}
	}
	if result.Meta.CachedRead > 0 {
		evt.CacheReadInputTokens = result.Meta.CachedRead
	}
	if result.Meta.CachedWrite > 0 {
		evt.CacheCreationInputTokens = result.Meta.CachedWrite
	}
}

// storeUsage keeps the latest usage_update payload so a settled prompt without
// its own _meta still reports token counts.
func (s *acpSession) storeUsage(raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var usage map[string]any
	if err := json.Unmarshal(raw, &usage); err != nil {
		return
	}
	s.streamMu.Lock()
	s.lastUsage = usage
	s.streamMu.Unlock()
}

// acpTextContent pulls text out of an ACP content block. Chunks carry
// `{"type":"text","text":"..."}`; a content array is flattened as a fallback.
func acpTextContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var block struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &block); err == nil && block.Text != "" {
		return block.Text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Text != "" {
				b.WriteString(blk.Text)
			}
		}
		return b.String()
	}
	return ""
}

// acpToolResultText prefers the raw tool output and falls back to the content
// blocks a tool_call_update may carry instead.
func acpToolResultText(rawOutput, content json.RawMessage) string {
	if len(rawOutput) > 0 {
		var s string
		if err := json.Unmarshal(rawOutput, &s); err == nil && s != "" {
			return s
		}
		var m map[string]any
		if err := json.Unmarshal(rawOutput, &m); err == nil && len(m) > 0 {
			if b, err := json.Marshal(m); err == nil {
				return string(b)
			}
		}
	}
	if text := acpTextContent(content); text != "" {
		return text
	}
	return ""
}

func rawToMap(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}
