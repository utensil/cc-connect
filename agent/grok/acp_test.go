package grok

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testSessionID is the session id the fake agent reports.
const testSessionID = "sess-acp-1"

type fakeRequest struct {
	ID     json.RawMessage
	Method string
	Params map[string]any
}

// fakeACP is a scripted ACP agent behind a pair of pipes. Handshake methods are
// answered automatically; prompts, cancellations and permission flow are driven
// by the test through reqs.
type fakeACP struct {
	t       *testing.T
	reqs    chan fakeRequest
	w       io.Writer
	mu      sync.Mutex
	raw     []fakeRequest
	loadErr bool
	closed  bool
}

func startFakeACP(t *testing.T, cfg acpConfig) (*acpSession, *fakeACP) {
	t.Helper()

	toAgent, toAgentW := io.Pipe()
	fromAgent, fromAgentW := io.Pipe()

	f := &fakeACP{t: t, reqs: make(chan fakeRequest, 64), w: fromAgentW}
	go f.serve(toAgent)

	if cfg.workDir == "" {
		cfg.workDir = t.TempDir()
	}
	s, err := newACPSessionWithStream(context.Background(), cfg, toAgentW, fromAgent)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = s.Close()
		_ = toAgentW.Close()
		_ = fromAgentW.Close()
		f.stop()
	})
	return s, f
}

func (f *fakeACP) stop() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
}

func (f *fakeACP) serve(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var frame struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			continue
		}
		req := fakeRequest{ID: frame.ID, Method: frame.Method, Params: frame.Params}
		f.mu.Lock()
		f.raw = append(f.raw, req)
		loadErr := f.loadErr
		f.mu.Unlock()

		if f.autoAnswer(req, loadErr) {
			continue
		}
		// Notifications (session/cancel) have no id and no reply; they are only
		// recorded for assertions.
		if len(req.ID) == 0 {
			continue
		}
		select {
		case f.reqs <- req:
		default:
			f.t.Logf("fakeACP: request queue full, dropping %s", req.Method)
		}
	}
}

// autoAnswer handles the methods no test needs to script.
func (f *fakeACP) autoAnswer(req fakeRequest, loadErr bool) bool {
	switch req.Method {
	case "initialize":
		f.result(req.ID, map[string]any{
			"protocolVersion":   1,
			"agentCapabilities": map[string]any{"loadSession": true},
		})
	case "session/new":
		f.result(req.ID, map[string]any{
			"sessionId": testSessionID,
			"models":    map[string]any{"currentModelId": "grok-4.7"},
		})
	case "session/load":
		if loadErr {
			f.errorResp(req.ID, -32002, "session not found")
			return true
		}
		f.result(req.ID, map[string]any{"sessionId": testSessionID})
	case "session/set_mode", "session/close":
		f.result(req.ID, map[string]any{"configOptions": []any{}})
	default:
		return false
	}
	return true
}

func (f *fakeACP) write(v map[string]any) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.mu.Unlock()
	b, err := json.Marshal(v)
	if err != nil {
		f.t.Errorf("fakeACP: marshal: %v", err)
		return
	}
	if _, err := f.w.Write(append(b, '\n')); err != nil {
		// The session closes its read side during teardown.
		return
	}
}

func (f *fakeACP) result(id json.RawMessage, result any) {
	f.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (f *fakeACP) errorResp(id json.RawMessage, code int, msg string) {
	f.write(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": msg},
	})
}

func (f *fakeACP) promptResult(id json.RawMessage, stopReason string, in, out int) {
	f.result(id, map[string]any{
		"stopReason": stopReason,
		"_meta": map[string]any{
			"inputTokens":      in,
			"outputTokens":     out,
			"cachedReadTokens": 7,
		},
	})
}

func (f *fakeACP) update(sessionUpdate string, fields map[string]any) {
	update := map[string]any{"sessionUpdate": sessionUpdate}
	for k, v := range fields {
		update[k] = v
	}
	f.write(map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]any{
			"sessionId": testSessionID,
			"update":    update,
		},
	})
}

func (f *fakeACP) text(text string) {
	f.update("agent_message_chunk", map[string]any{
		"content": map[string]any{"type": "text", "text": text},
	})
}

func (f *fakeACP) thought(text string) {
	f.update("agent_thought_chunk", map[string]any{
		"content": map[string]any{"type": "text", "text": text},
	})
}

func (f *fakeACP) requestPermission(id int, options []map[string]any) {
	f.write(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "session/request_permission",
		"params": map[string]any{
			"sessionId": testSessionID,
			"toolCall": map[string]any{
				"toolCallId": "call-1",
				"title":      "run_terminal_command",
				"kind":       "execute",
				"rawInput":   map[string]any{"command": "ls"},
			},
			"options": options,
		},
	})
}

// expect waits for the next scripted request with the given method.
func (f *fakeACP) expect(method string) fakeRequest {
	f.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case req := <-f.reqs:
			require.Equal(f.t, method, req.Method, "unexpected request")
			return req
		case <-deadline:
			f.t.Fatalf("timed out waiting for %s", method)
			return fakeRequest{}
		}
	}
}

// expectNothing fails if any scripted request arrives within the window.
func (f *fakeACP) expectNothing(window time.Duration) {
	f.t.Helper()
	select {
	case req := <-f.reqs:
		f.t.Fatalf("unexpected request %s", req.Method)
	case <-time.After(window):
	}
}

func (f *fakeACP) sawMethod(method string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, req := range f.raw {
		if req.Method == method {
			return true
		}
	}
	return false
}

// drainEvents collects events until a terminal result or a timeout.
func drainSessionEvents(t *testing.T, s *acpSession) []core.Event {
	t.Helper()
	var got []core.Event
	deadline := time.After(5 * time.Second)
	for {
		select {
		case evt, ok := <-s.Events():
			if !ok {
				return got
			}
			got = append(got, evt)
			if evt.Type == core.EventResult || evt.Type == core.EventError {
				return got
			}
		case <-deadline:
			t.Fatalf("timed out waiting for events, got %d", len(got))
			return got
		}
	}
}

func eventTypes(events []core.Event) []core.EventType {
	types := make([]core.EventType, 0, len(events))
	for _, evt := range events {
		types = append(types, evt.Type)
	}
	return types
}

func resultEvent(t *testing.T, events []core.Event) core.Event {
	t.Helper()
	for _, evt := range events {
		if evt.Type == core.EventResult {
			return evt
		}
	}
	t.Fatalf("no EventResult in %v", eventTypes(events))
	return core.Event{}
}

func TestACPSession_HandshakeAndNewSession(t *testing.T) {
	s, _ := startFakeACP(t, acpConfig{mode: "yolo"})
	assert.Equal(t, testSessionID, s.CurrentSessionID())
	assert.True(t, s.Alive())
}

func TestACPSession_ResumeUsesSessionLoad(t *testing.T) {
	s, f := startFakeACP(t, acpConfig{mode: "yolo", resumeID: "sess-existing"})
	assert.True(t, f.sawMethod("session/load"))
	assert.False(t, f.sawMethod("session/new"))
	assert.Equal(t, testSessionID, s.CurrentSessionID())
}

func TestACPSession_ResumeFailureFallsBackToNew(t *testing.T) {
	toAgent, toAgentW := io.Pipe()
	fromAgent, fromAgentW := io.Pipe()
	f := &fakeACP{t: t, reqs: make(chan fakeRequest, 64), w: fromAgentW, loadErr: true}
	go f.serve(toAgent)

	s, err := newACPSessionWithStream(context.Background(), acpConfig{
		mode:     "yolo",
		workDir:  t.TempDir(),
		resumeID: "sess-missing",
	}, toAgentW, fromAgent)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = s.Close()
		_ = toAgentW.Close()
		_ = fromAgentW.Close()
		f.stop()
	})

	assert.True(t, f.sawMethod("session/load"))
	assert.True(t, f.sawMethod("session/new"))
	assert.Equal(t, testSessionID, s.CurrentSessionID())
}

func TestACPSession_SendMapsStreamToEventsAndUsage(t *testing.T) {
	s, f := startFakeACP(t, acpConfig{mode: "yolo"})

	require.NoError(t, s.Send("hello", nil, nil))
	req := f.expect("session/prompt")
	assert.Equal(t, testSessionID, req.Params["sessionId"])

	f.thought("thinking")
	f.text("hello ")
	f.update("tool_call", map[string]any{
		"toolCallId": "call-1",
		"title":      "run_terminal_command",
		"kind":       "execute",
		"rawInput":   map[string]any{"command": "ls"},
	})
	f.update("tool_call_update", map[string]any{
		"toolCallId": "call-1",
		"status":     "completed",
		"rawOutput":  map[string]any{"output": "file.txt"},
	})
	f.text("world")
	f.promptResult(req.ID, "end_turn", 120, 42)

	events := drainSessionEvents(t, s)
	types := eventTypes(events)
	require.Equal(t, []core.EventType{
		core.EventThinking,
		core.EventText,
		core.EventToolUse,
		core.EventToolResult,
		core.EventText,
		core.EventResult,
	}, types)
	assert.Equal(t, "thinking", events[0].Content)
	assert.Equal(t, "hello ", events[1].Content)
	assert.Equal(t, "run_terminal_command", events[2].ToolName)
	assert.Contains(t, events[3].ToolResult, "file.txt")
	assert.Equal(t, "world", events[4].Content)

	result := resultEvent(t, events)
	assert.True(t, result.Done)
	assert.Equal(t, 120, result.InputTokens)
	assert.Equal(t, 42, result.OutputTokens)
	assert.Equal(t, 7, result.CacheReadInputTokens)
}

func TestACPSession_UpdatesOutsideATurnAreIgnored(t *testing.T) {
	s, f := startFakeACP(t, acpConfig{mode: "yolo"})

	// Transcript replay after session/load arrives before the first prompt.
	f.text("replayed history")
	f.thought("replayed thought")

	select {
	case evt := <-s.Events():
		t.Fatalf("unexpected event before a turn: %+v", evt)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestACPSession_SteerNativeFoldsIntoRunningTurn(t *testing.T) {
	s, f := startFakeACP(t, acpConfig{mode: "yolo", nativeSteer: true})

	require.NoError(t, s.Send("do the long thing", nil, nil))
	primary := f.expect("session/prompt")
	f.text("working")

	require.NoError(t, s.Steer("also cover the edge case"))
	steer := f.expect("session/prompt")
	steerText, ok := steer.Params["prompt"].([]any)
	require.True(t, ok)
	block, ok := steerText[0].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, block["text"], "edge case")

	// Native steer: the steer request settles as cancelled because the running
	// turn absorbed it, and the running turn carries the answer.
	f.promptResult(steer.ID, "cancelled", 0, 0)
	f.text("answer covering both")
	f.promptResult(primary.ID, "end_turn", 100, 20)

	events := drainSessionEvents(t, s)
	results := 0
	for _, evt := range events {
		if evt.Type == core.EventResult {
			results++
		}
		if evt.Type == core.EventError {
			t.Fatalf("unexpected error event: %v", evt.Error)
		}
	}
	assert.Equal(t, 1, results, "native steer must not open a second engine turn")
	assert.Equal(t, 20, resultEvent(t, events).OutputTokens)
}

func TestACPSession_SteerFallbackCancelsThenContinues(t *testing.T) {
	s, f := startFakeACP(t, acpConfig{mode: "yolo", nativeSteer: false})

	require.NoError(t, s.Send("do the long thing", nil, nil))
	primary := f.expect("session/prompt")
	f.text("working")

	// The guidance itself is written by Steer; cancellation is a notification.
	steerDone := make(chan error, 1)
	go func() { steerDone <- s.Steer("change of plan") }()

	// The fake sees session/cancel (a notification is never queued by the
	// dispatcher), then answers the in-flight prompt as cancelled, then the
	// adapter opens the replacement turn.
	require.Eventually(t, func() bool { return f.sawMethod("session/cancel") }, 2*time.Second, 10*time.Millisecond)
	f.promptResult(primary.ID, "cancelled", 0, 0)

	replacement := f.expect("session/prompt")
	blocks, ok := replacement.Params["prompt"].([]any)
	require.True(t, ok)
	block, ok := blocks[0].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, block["text"], "change of plan")

	f.text("done with the new plan")
	f.promptResult(replacement.ID, "end_turn", 90, 11)

	require.NoError(t, <-steerDone)
	events := drainSessionEvents(t, s)
	results := 0
	for _, evt := range events {
		if evt.Type == core.EventError {
			t.Fatalf("unexpected error event: %v", evt.Error)
		}
		if evt.Type == core.EventResult {
			results++
		}
	}
	assert.Equal(t, 1, results, "the replacement prompt owns the engine turn")
}

func TestACPSession_CancelTurnKeepsSessionAlive(t *testing.T) {
	s, f := startFakeACP(t, acpConfig{mode: "yolo"})

	require.NoError(t, s.Send("long job", nil, nil))
	primary := f.expect("session/prompt")
	f.text("working")

	cancelDone := make(chan error, 1)
	go func() { cancelDone <- s.CancelTurn() }()

	require.Eventually(t, func() bool { return f.sawMethod("session/cancel") }, 2*time.Second, 10*time.Millisecond)
	f.promptResult(primary.ID, "cancelled", 0, 0)
	require.NoError(t, <-cancelDone)

	events := drainSessionEvents(t, s)
	assert.Equal(t, []core.EventType{core.EventText, core.EventResult}, eventTypes(events))
	assert.True(t, s.Alive(), "/stop must not tear the session down")

	// The same session keeps working afterwards.
	require.NoError(t, s.Send("next question", nil, nil))
	next := f.expect("session/prompt")
	f.text("answer")
	f.promptResult(next.ID, "end_turn", 10, 2)
	nextEvents := drainSessionEvents(t, s)
	assert.Equal(t, "answer", nextEvents[0].Content)
}

func TestACPSession_SetLiveModelAndEffort(t *testing.T) {
	s, f := startFakeACP(t, acpConfig{mode: "yolo"})

	modelDone := make(chan bool, 1)
	go func() { modelDone <- s.SetLiveModel("grok-4.6") }()
	req := f.expect("session/set_config_option")
	assert.Equal(t, "model", req.Params["configId"])
	value, ok := req.Params["value"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "grok-4.6", value["value"])
	f.result(req.ID, map[string]any{"configOptions": []any{}})
	assert.True(t, <-modelDone)

	effortDone := make(chan bool, 1)
	go func() { effortDone <- s.SetLiveReasoningEffort("xhigh") }()
	req = f.expect("session/set_config_option")
	assert.Equal(t, "reasoning_effort", req.Params["configId"])
	f.result(req.ID, map[string]any{"configOptions": []any{}})
	assert.True(t, <-effortDone)
}

func TestACPSession_CloseIsIdempotent(t *testing.T) {
	s, _ := startFakeACP(t, acpConfig{mode: "yolo"})
	require.NoError(t, s.Close())
	require.NoError(t, s.Close())
	assert.False(t, s.Alive())

	_, open := <-s.Events()
	assert.False(t, open, "Close must close the events channel")
}

func TestACPSession_PermissionRequestRoundTrip(t *testing.T) {
	s, f := startFakeACP(t, acpConfig{mode: "default"})

	require.NoError(t, s.Send("touch a file", nil, nil))
	f.expect("session/prompt")

	f.requestPermission(900, []map[string]any{
		{"optionId": "allow", "name": "Allow", "kind": "allow_once"},
		{"optionId": "deny", "name": "Reject", "kind": "reject_once"},
	})

	evt := <-s.Events()
	require.Equal(t, core.EventPermissionRequest, evt.Type)
	assert.Equal(t, "run_terminal_command", evt.ToolName)

	require.NoError(t, s.RespondPermission(evt.RequestID, core.PermissionResult{Behavior: "allow"}))
}

func TestPickPermissionOption(t *testing.T) {
	options := []acpPermissionOption{
		{OptionID: "a", Name: "Allow once", Kind: "allow_once"},
		{OptionID: "r", Name: "Reject", Kind: "reject_once"},
	}
	assert.Equal(t, "a", pickPermissionOption(options, true))
	assert.Equal(t, "r", pickPermissionOption(options, false))
	assert.Equal(t, "", pickPermissionOption(nil, true))

	// Fallback to name matching when kinds use a different spelling.
	assert.Equal(t, "yes", pickPermissionOption([]acpPermissionOption{
		{OptionID: "yes", Name: "Yes, allow"},
	}, true))
}

func TestACPSession_SendRejectsClosedSession(t *testing.T) {
	s, _ := startFakeACP(t, acpConfig{mode: "yolo"})
	require.NoError(t, s.Close())

	err := s.Send("hello", nil, nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "closed"))
}

func TestACPSession_SteerInjectionErrorFallsBackToCancel(t *testing.T) {
	s, f := startFakeACP(t, acpConfig{mode: "yolo", nativeSteer: true})

	require.NoError(t, s.Send("do the long thing", nil, nil))
	primary := f.expect("session/prompt")
	f.text("working")

	steerDone := make(chan error, 1)
	go func() { steerDone <- s.Steer("change of plan") }()
	steer := f.expect("session/prompt")
	f.errorResp(steer.ID, -32600, "cannot inject")

	// The adapter must not surface a turn error; it cancels and continues.
	require.Eventually(t, func() bool { return f.sawMethod("session/cancel") }, 2*time.Second, 10*time.Millisecond)
	f.promptResult(primary.ID, "cancelled", 0, 0)

	replacement := f.expect("session/prompt")
	blocks, ok := replacement.Params["prompt"].([]any)
	require.True(t, ok)
	block, ok := blocks[0].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, block["text"], "change of plan")

	f.text("did the new plan")
	f.promptResult(replacement.ID, "end_turn", 50, 5)

	require.NoError(t, <-steerDone)
	events := drainSessionEvents(t, s)
	for _, evt := range events {
		if evt.Type == core.EventError {
			t.Fatalf("steer injection failure must not surface as a turn error: %v", evt.Error)
		}
	}
	assert.True(t, strings.Contains(collectText(events), "did the new plan"))
	assert.Equal(t, 1, countResults(events))
}

func TestACPSession_TurnTimeoutDoesNotCancelLaterTurns(t *testing.T) {
	s, f := startFakeACP(t, acpConfig{mode: "yolo", timeout: 120 * time.Millisecond})

	require.NoError(t, s.Send("quick question", nil, nil))
	req := f.expect("session/prompt")
	f.promptResult(req.ID, "end_turn", 10, 2)
	drainSessionEvents(t, s)

	// The turn already settled; its timeout timer must not fire into the next
	// turn (which would cancel an unrelated request).
	time.Sleep(300 * time.Millisecond)
	assert.False(t, f.sawMethod("session/cancel"), "settled turn left a stray timeout cancel")
}

func TestACPSession_AttachmentsRemovedWhenTheTurnSettles(t *testing.T) {
	workDir := t.TempDir()
	s, f := startFakeACP(t, acpConfig{mode: "yolo", workDir: workDir})

	require.NoError(t, s.Send("look at this", []core.ImageAttachment{{
		MimeType: "image/png",
		Data:     []byte("png-bytes"),
	}}, nil))
	req := f.expect("session/prompt")
	f.promptResult(req.ID, "end_turn", 10, 2)
	drainSessionEvents(t, s)

	entries, err := os.ReadDir(filepath.Join(workDir, ".cc-connect", "attachments"))
	require.NoError(t, err)
	assert.Empty(t, entries, "staged attachments must be removed after the turn")
}

func collectText(events []core.Event) string {
	var b strings.Builder
	for _, evt := range events {
		if evt.Type == core.EventText {
			b.WriteString(evt.Content)
		}
	}
	return b.String()
}

func countResults(events []core.Event) int {
	n := 0
	for _, evt := range events {
		if evt.Type == core.EventResult {
			n++
		}
	}
	return n
}

func TestACPSession_CancelTurnReportsSettleTimeout(t *testing.T) {
	old := acpCancelWait
	acpCancelWait = 80 * time.Millisecond
	t.Cleanup(func() { acpCancelWait = old })

	s, f := startFakeACP(t, acpConfig{mode: "yolo"})
	require.NoError(t, s.Send("long job", nil, nil))
	f.expect("session/prompt")

	// The agent ignores session/cancel here: /stop must report failure so the
	// engine can fall back to closing the session rather than reporting a stop
	// that never happened.
	err := s.CancelTurn()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not settle")
}

func TestACPSession_SteerFallbackEmitsOneTerminalResultEvenWhenPrimarySettlesLate(t *testing.T) {
	s, f := startFakeACP(t, acpConfig{mode: "yolo", nativeSteer: false})

	require.NoError(t, s.Send("do the long thing", nil, nil))
	primary := f.expect("session/prompt")

	steerDone := make(chan error, 1)
	go func() { steerDone <- s.Steer("new plan") }()
	require.Eventually(t, func() bool { return f.sawMethod("session/cancel") }, 2*time.Second, 10*time.Millisecond)

	// Cancelled primary settles first; the replacement prompt is started after
	// the adapter observed the queue emptied. With a global cancel reason this
	// ordering produced a second Done event.
	f.promptResult(primary.ID, "cancelled", 0, 0)
	replacement := f.expect("session/prompt")
	f.text("answer for the new plan")
	f.promptResult(replacement.ID, "end_turn", 10, 3)

	require.NoError(t, <-steerDone)
	events := drainSessionEvents(t, s)
	assert.Equal(t, 1, countResults(events))

	select {
	case evt := <-s.Events():
		t.Fatalf("stray event after the replacement turn finished: %+v", evt)
	case <-time.After(300 * time.Millisecond):
	}
}
