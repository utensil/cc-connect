package grok

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewGrokSession(t *testing.T) {
	ctx := context.Background()
	gs, err := newGrokSession(ctx, sessionConfig{
		cmd:      "grok",
		workDir:  "/tmp",
		model:    "grok-4.5",
		mode:     "default",
		resumeID: "resume-abc",
	})
	require.NoError(t, err)
	require.NotNil(t, gs)
	assert.True(t, gs.Alive())
	assert.Equal(t, "resume-abc", gs.CurrentSessionID())

	err = gs.Close()
	assert.NoError(t, err)
	assert.False(t, gs.Alive())
}

func TestBuildArgs_PromptFileAndResume(t *testing.T) {
	gs, err := newGrokSession(context.Background(), sessionConfig{
		cmd:      "grok",
		workDir:  "/proj",
		model:    "grok-4.5",
		mode:     "yolo",
		resumeID: "sid-1",
	})
	require.NoError(t, err)
	defer gs.Close()

	args := gs.buildArgs("", "/tmp/prompt.txt")
	assert.Contains(t, args, "--output-format")
	assert.Contains(t, args, "streaming-json")
	assert.Contains(t, args, "--cwd")
	assert.Contains(t, args, "/proj")
	assert.Contains(t, args, "--resume")
	assert.Contains(t, args, "sid-1")
	assert.Contains(t, args, "-m")
	assert.Contains(t, args, "grok-4.5")
	assert.Contains(t, args, "--permission-mode")
	assert.Contains(t, args, "bypassPermissions")
	assert.Contains(t, args, "--always-approve")
	assert.Contains(t, args, "--prompt-file")
	assert.Contains(t, args, "/tmp/prompt.txt")
	assert.NotContains(t, args, "-p")
}

func TestBuildArgs_PlanModeNoAlwaysApprove(t *testing.T) {
	gs, err := newGrokSession(context.Background(), sessionConfig{
		cmd:     "grok",
		workDir: "/proj",
		mode:    "plan",
	})
	require.NoError(t, err)
	defer gs.Close()

	args := gs.buildArgs("hello", "")
	assert.Contains(t, args, "plan")
	assert.NotContains(t, args, "--always-approve")
	assert.Contains(t, args, "-p")
	assert.Contains(t, args, "hello")
}

func TestBuildArgs_InlinePrompt(t *testing.T) {
	gs, err := newGrokSession(context.Background(), sessionConfig{
		cmd:  "grok",
		mode: "default",
	})
	require.NoError(t, err)
	defer gs.Close()

	args := gs.buildArgs("ping", "")
	// Find -p value
	for i, a := range args {
		if a == "-p" && i+1 < len(args) {
			assert.Equal(t, "ping", args[i+1])
			return
		}
	}
	t.Fatal("expected -p flag")
}

func TestHandleEvent_ThoughtTextEnd(t *testing.T) {
	gs, err := newGrokSession(context.Background(), sessionConfig{cmd: "grok"})
	require.NoError(t, err)
	defer gs.Close()

	// Grok streams thought/text as token deltas — must coalesce.
	gs.handleEvent(map[string]any{"type": "thought", "data": "hmm"})
	gs.handleEvent(map[string]any{"type": "thought", "data": " "})
	gs.handleEvent(map[string]any{"type": "thought", "data": "ok"})
	gs.handleEvent(map[string]any{"type": "text", "data": "po"})
	gs.handleEvent(map[string]any{"type": "text", "data": "ng"})
	terminal := gs.handleEvent(map[string]any{
		"type":      "end",
		"sessionId": "019f-test-session",
		"usage": map[string]any{
			"input_tokens":            float64(100),
			"output_tokens":           float64(5),
			"cache_read_input_tokens": float64(50),
		},
	})
	assert.True(t, terminal)
	assert.Equal(t, "019f-test-session", gs.CurrentSessionID())

	events := drainEvents(gs.events, 3)
	require.Len(t, events, 3)

	assert.Equal(t, core.EventThinking, events[0].Type)
	assert.Equal(t, "hmm ok", events[0].Content)

	assert.Equal(t, core.EventText, events[1].Type)
	assert.Equal(t, "pong", events[1].Content)

	assert.Equal(t, core.EventResult, events[2].Type)
	assert.True(t, events[2].Done)
	assert.Equal(t, "019f-test-session", events[2].SessionID)
	assert.Equal(t, 100, events[2].InputTokens)
	assert.Equal(t, 5, events[2].OutputTokens)
	assert.Equal(t, 50, events[2].CacheReadInputTokens)
}

func TestHandleEvent_ThoughtTokensNotEmittedPerToken(t *testing.T) {
	gs, err := newGrokSession(context.Background(), sessionConfig{cmd: "grok"})
	require.NoError(t, err)
	defer gs.Close()

	// Mimic the WeChat spam case: one EventThinking per word would be wrong.
	for _, w := range []string{"The", " user", " sent", " just", " \"", "1", "\"."} {
		gs.handleEvent(map[string]any{"type": "thought", "data": w})
	}
	// No non-thought event yet → nothing flushed.
	select {
	case e := <-gs.events:
		t.Fatalf("unexpected early event: %+v", e)
	case <-time.After(50 * time.Millisecond):
	}
	gs.flushThought()
	events := drainEvents(gs.events, 1)
	require.Len(t, events, 1)
	assert.Equal(t, core.EventThinking, events[0].Type)
	assert.Equal(t, `The user sent just "1".`, events[0].Content)
}

func TestHandleEvent_ToolUseAndResult(t *testing.T) {
	gs, err := newGrokSession(context.Background(), sessionConfig{cmd: "grok"})
	require.NoError(t, err)
	defer gs.Close()

	gs.handleEvent(map[string]any{
		"type": "tool_use",
		"name": "Shell",
		"id":   "t1",
		"input": map[string]any{
			"command": "echo hi",
		},
	})
	gs.handleEvent(map[string]any{
		"type":         "tool_result",
		"tool_call_id": "t1",
		"data":         "hi\n",
	})

	events := drainEvents(gs.events, 2)
	require.Len(t, events, 2)
	assert.Equal(t, core.EventToolUse, events[0].Type)
	assert.Equal(t, "Shell", events[0].ToolName)
	assert.Contains(t, events[0].ToolInput, "echo hi")
	assert.Equal(t, "t1", events[0].RequestID)

	assert.Equal(t, core.EventToolResult, events[1].Type)
	assert.Equal(t, "hi\n", events[1].ToolResult)
}

func TestHandleEvent_Error(t *testing.T) {
	gs, err := newGrokSession(context.Background(), sessionConfig{cmd: "grok"})
	require.NoError(t, err)
	defer gs.Close()

	gs.handleEvent(map[string]any{"type": "error", "message": "boom"})
	events := drainEvents(gs.events, 1)
	require.Len(t, events, 1)
	assert.Equal(t, core.EventError, events[0].Type)
	require.Error(t, events[0].Error)
	assert.Contains(t, events[0].Error.Error(), "boom")
}

func drainEvents(ch <-chan core.Event, n int) []core.Event {
	var out []core.Event
	for i := 0; i < n; i++ {
		select {
		case e := <-ch:
			out = append(out, e)
		case <-time.After(2 * time.Second):
			return out
		}
	}
	return out
}

// TestHandleEvent_DocumentedSchema exercises the exact event shapes published in
// the Grok CLI headless reference (thought/text/tool_call/tool_call_update/
// usage/plan/available_commands/end), which is what the real CLI emits.
func TestHandleEvent_DocumentedSchema(t *testing.T) {
	gs, err := newGrokSession(context.Background(), sessionConfig{cmd: "grok"})
	require.NoError(t, err)
	defer gs.Close()

	gs.handleEvent(map[string]any{"type": "available_commands", "tools": []any{}, "commands": []any{}})
	gs.handleEvent(map[string]any{"type": "thought", "data": "Analyzing the directory structure..."})
	gs.handleEvent(map[string]any{
		"type":       "tool_call",
		"toolCallId": "call_1",
		"title":      "Read",
		"kind":       "read",
		"status":     "in_progress",
		"toolName":   "read_file",
		"rawInput":   map[string]any{"path": "src/main.rs"},
		"content":    []any{},
		"locations":  []any{},
	})
	// Non-terminal update: must not surface as a tool result.
	gs.handleEvent(map[string]any{
		"type":       "tool_call_update",
		"toolCallId": "call_1",
		"status":     "in_progress",
		"content":    []any{},
		"locations":  []any{},
	})
	gs.handleEvent(map[string]any{
		"type":       "tool_call_update",
		"toolCallId": "call_1",
		"status":     "completed",
		"rawOutput":  map[string]any{"lines": float64(42)},
		"content":    []any{},
		"locations":  []any{},
	})
	gs.handleEvent(map[string]any{"type": "plan", "entries": []any{}})
	gs.handleEvent(map[string]any{
		"type":       "usage",
		"messageId":  "resp_1",
		"stopReason": "end_turn",
		"usage": map[string]any{
			"input_tokens":  float64(812),
			"output_tokens": float64(45),
		},
	})
	gs.handleEvent(map[string]any{"type": "text", "data": "Here's a summary"})
	terminal := gs.handleEvent(map[string]any{
		"type":       "end",
		"stopReason": "end_turn",
		"sessionId":  "abc123",
		"requestId":  "xyz789",
		"usage": map[string]any{
			"input_tokens":            float64(812),
			"output_tokens":           float64(45),
			"cache_read_input_tokens": float64(0),
		},
	})
	assert.True(t, terminal)
	assert.Equal(t, "abc123", gs.CurrentSessionID())

	events := drainEvents(gs.events, 4)
	require.Len(t, events, 4)
	assert.Equal(t, core.EventThinking, events[0].Type)
	assert.Equal(t, "Analyzing the directory structure...", events[0].Content)
	assert.Equal(t, core.EventToolUse, events[1].Type)
	assert.Equal(t, "read_file", events[1].ToolName)
	assert.Equal(t, "call_1", events[1].RequestID)
	assert.Contains(t, events[1].ToolInput, "src/main.rs")
	assert.Equal(t, core.EventToolResult, events[2].Type)
	assert.Equal(t, "read_file", events[2].ToolName)
	assert.Contains(t, events[2].ToolResult, "42")
	assert.Equal(t, core.EventText, events[3].Type)
	assert.Equal(t, "Here's a summary", events[3].Content)

	// The terminal end frame is still queued behind the text flush.
	end := drainEvents(gs.events, 1)
	require.Len(t, end, 1)
	assert.Equal(t, core.EventResult, end[0].Type)
	assert.True(t, end[0].Done)
	assert.Equal(t, "abc123", end[0].SessionID)
	assert.Equal(t, 812, end[0].InputTokens)
	assert.Equal(t, 45, end[0].OutputTokens)
}

// TestLiveModelAndReasoningSwitch proves /model and /reasoning reach the next
// turn of the same conversation instead of replacing it.
func TestLiveModelAndReasoningSwitch(t *testing.T) {
	gs, err := newGrokSession(context.Background(), sessionConfig{
		cmd:             "grok",
		model:           "grok-4.6",
		reasoningEffort: "high",
		resumeID:        "sid-live",
	})
	require.NoError(t, err)

	require.True(t, gs.SetLiveModel("grok-4.5"))
	require.True(t, gs.SetLiveReasoningEffort("low"))

	args := gs.buildArgs("hi", "")
	assert.Contains(t, args, "-m")
	assert.Contains(t, args, "grok-4.5")
	assert.Contains(t, args, "--reasoning-effort")
	assert.Contains(t, args, "low")
	// The conversation is preserved: the resumed id is still passed.
	assert.Contains(t, args, "--resume")
	assert.Contains(t, args, "sid-live")

	require.True(t, gs.SetLiveModel("grok-4.6"))
	require.NoError(t, gs.Close())
	// Close is idempotent and a dead session refuses further live switches.
	require.NoError(t, gs.Close())
	assert.False(t, gs.SetLiveModel("grok-4.6"))
	assert.False(t, gs.SetLiveReasoningEffort("high"))
}

func TestStartSessionWithRuntimeOverrides(t *testing.T) {
	a := &Agent{cmd: "grok", workDir: "/tmp", model: "grok-4.6", reasoningEffort: "high", activeIdx: -1}
	sess, err := a.StartSessionWithRuntime(context.Background(), "", "grok-4.5", "low")
	require.NoError(t, err)
	defer sess.Close()
	args := sess.(*grokSession).buildArgs("hi", "")
	assert.Contains(t, args, "grok-4.5")
	assert.Contains(t, args, "low")

	sess2, err := a.StartSessionWithModel(context.Background(), "", "grok-4.6")
	require.NoError(t, err)
	defer sess2.Close()
	args2 := sess2.(*grokSession).buildArgs("hi", "")
	assert.Contains(t, args2, "grok-4.6")
	// No override → the agent-level effort is kept.
	assert.Contains(t, args2, "high")

	// Agent defaults are untouched by session-scoped overrides.
	assert.Equal(t, "grok-4.6", a.GetModel())
	assert.Equal(t, "high", a.GetReasoningEffort())
}

// TestSendCloseRace_NoPanicOnClosedEvents is the regression guard for the
// Send/Close race: Close closes the events channel, so a turn that passes the
// liveness check must not emit afterwards (a panic in a goroutine would take the
// whole bridge down). Run with -race.
func TestSendCloseRace_NoPanicOnClosedEvents(t *testing.T) {
	for i := 0; i < 60; i++ {
		gs, err := newGrokSession(context.Background(), sessionConfig{
			cmd:     "/nonexistent-grok-binary",
			workDir: t.TempDir(),
			mode:    "yolo",
		})
		require.NoError(t, err)

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// Either it is refused as closed, or it fails to start; it must
			// never emit into a closed channel.
			_ = gs.Send("hi", nil, nil)
		}()
		close(start)
		_ = gs.Close()
		wg.Wait()
	}
}

// TestSendAfterCloseRefused checks the liveness gate itself.
func TestSendAfterCloseRefused(t *testing.T) {
	gs, err := newGrokSession(context.Background(), sessionConfig{cmd: "/nonexistent-grok-binary"})
	require.NoError(t, err)
	require.NoError(t, gs.Close())
	err = gs.Send("hi", nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "closed")
}

// TestHandleEvent_EndDoesNotDuplicateStreamedText guards the engine's
// fullResponse accounting: when text was streamed, the terminal frame must not
// carry it again as EventResult.Content.
func TestHandleEvent_EndDoesNotDuplicateStreamedText(t *testing.T) {
	gs, err := newGrokSession(context.Background(), sessionConfig{cmd: "grok"})
	require.NoError(t, err)
	defer gs.Close()

	gs.handleEvent(map[string]any{"type": "text", "data": "streamed answer"})
	gs.handleEvent(map[string]any{"type": "end", "sessionId": "s1", "data": "streamed answer"})

	events := drainEvents(gs.events, 2)
	require.Len(t, events, 2)
	assert.Equal(t, core.EventText, events[0].Type)
	assert.Equal(t, "streamed answer", events[0].Content)
	assert.Equal(t, core.EventResult, events[1].Type)
	assert.Empty(t, events[1].Content, "terminal frame must not resend streamed text")
}

// TestHandleEvent_HeaderOnlyEndUsesLastUsage covers a stream where usage arrives
// in a standalone frame and the terminal frame has none.
func TestHandleEvent_HeaderOnlyEndUsesLastUsage(t *testing.T) {
	gs, err := newGrokSession(context.Background(), sessionConfig{cmd: "grok"})
	require.NoError(t, err)
	defer gs.Close()

	gs.lastUsage = map[string]any{"input_tokens": float64(11), "output_tokens": float64(7)}
	terminal := gs.handleEvent(map[string]any{"type": "end", "sessionId": "s2"})
	require.True(t, terminal)

	events := drainEvents(gs.events, 1)
	require.Len(t, events, 1)
	assert.Equal(t, 11, events[0].InputTokens)
	assert.Equal(t, 7, events[0].OutputTokens)
}

// TestHandleEvent_ToolFailureStatus ensures a failed tool call is marked so the
// progress card does not render it as a success.
func TestHandleEvent_ToolFailureStatus(t *testing.T) {
	gs, err := newGrokSession(context.Background(), sessionConfig{cmd: "grok"})
	require.NoError(t, err)
	defer gs.Close()

	gs.handleEvent(map[string]any{"type": "tool_call", "toolCallId": "c1", "toolName": "shell"})
	gs.handleEvent(map[string]any{
		"type": "tool_call_update", "toolCallId": "c1", "status": "failed",
		"rawOutput": map[string]any{"error": "boom"},
	})

	events := drainEvents(gs.events, 2)
	require.Len(t, events, 2)
	assert.Equal(t, core.EventToolUse, events[0].Type)
	assert.Equal(t, core.EventToolResult, events[1].Type)
	assert.Equal(t, "shell", events[1].ToolName)
	assert.Equal(t, "failed", events[1].ToolStatus)
	require.NotNil(t, events[1].ToolSuccess)
	assert.False(t, *events[1].ToolSuccess)
}
