package zulip

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// newTestPlatform builds a Platform bound to a test HTTP server.
func newTestPlatform(t *testing.T, serverURL string, opts map[string]any) *Platform {
	t.Helper()
	o := map[string]any{
		"url":     serverURL,
		"email":   "kepler-bot@example.com",
		"api_key": "test-key",
	}
	for k, v := range opts {
		o[k] = v
	}
	created, err := New(o)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	p, ok := created.(*Platform)
	if !ok {
		t.Fatalf("New() returned %T, want *Platform", created)
	}
	return p
}

// Zulip has no typing indicator, so reaction acks are the default.
func TestNew_AckDefaultsToReactions(t *testing.T) {
	p := newTestPlatform(t, "http://127.0.0.1:1", nil)
	if p.ackStyle != "reaction" || p.steerAckEmoji != "compass" || p.queueAckEmoji != "hourglass" {
		t.Fatalf("ack defaults = (%q, %q, %q), want (reaction, compass, hourglass)",
			p.ackStyle, p.steerAckEmoji, p.queueAckEmoji)
	}
}

func TestNew_AckOptionsOverrideDefaults(t *testing.T) {
	p := newTestPlatform(t, "http://127.0.0.1:1", map[string]any{
		"ack_style":       "message",
		"steer_ack_emoji": "check",
		"queue_ack_emoji": "tada",
	})
	if p.ackStyle != "message" || p.steerAckEmoji != "check" || p.queueAckEmoji != "tada" {
		t.Fatalf("ack overrides = (%q, %q, %q), want (message, check, tada)",
			p.ackStyle, p.steerAckEmoji, p.queueAckEmoji)
	}
}

// Regression: a steered/queued acknowledgement must be delivered as a reaction on the user's
// own message (the Discord behaviour) rather than the localized "Guidance sent..." text.
func TestAcknowledgeMessage_PostsConfiguredReaction(t *testing.T) {
	var mu sync.Mutex
	var method, path, body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		method, path, body = r.Method, r.URL.Path, r.PostForm.Encode()
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":"success","msg":""}`))
	}))
	defer server.Close()

	p := newTestPlatform(t, server.URL, nil)
	rc := replyContext{kind: "stream", stream: "tasks", topic: "cc-connect", messageID: 625570911}

	if !p.AcknowledgeMessage(rc, core.MessageAckSteered) {
		t.Fatal("AcknowledgeMessage(steered) = false, want handled by reaction")
	}
	mu.Lock()
	gotMethod, gotPath, gotBody := method, path, body
	mu.Unlock()
	if gotMethod != http.MethodPost || gotPath != "/api/v1/messages/625570911/reactions" {
		t.Fatalf("reaction request = %s %s, want POST /api/v1/messages/625570911/reactions", gotMethod, gotPath)
	}
	if !strings.Contains(gotBody, "emoji_name=compass") {
		t.Fatalf("steered payload = %q, want emoji_name=compass", gotBody)
	}

	// Pointer reply contexts (as the engine can pass them) must work too.
	if !p.AcknowledgeMessage(&rc, core.MessageAckQueued) {
		t.Fatal("AcknowledgeMessage(queued, *replyContext) = false, want handled")
	}
	mu.Lock()
	gotBody = body
	mu.Unlock()
	if !strings.Contains(gotBody, "emoji_name=hourglass") {
		t.Fatalf("queued payload = %q, want emoji_name=hourglass", gotBody)
	}
}

// Every unsupported or failing case must return false so the engine keeps its text fallback.
func TestAcknowledgeMessage_FallsBackToText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"result":"error","msg":"Emoji does not exist"}`, http.StatusBadRequest)
	}))
	defer server.Close()
	rc := replyContext{kind: "stream", stream: "tasks", topic: "cc-connect", messageID: 625570911}

	if p := newTestPlatform(t, server.URL, map[string]any{"ack_style": "message"}); p.AcknowledgeMessage(rc, core.MessageAckSteered) {
		t.Fatal("ack_style=message must not handle the acknowledgement")
	}
	if p := newTestPlatform(t, server.URL, nil); p.AcknowledgeMessage(replyContext{kind: "stream"}, core.MessageAckSteered) {
		t.Fatal("a reply context without a message id must not handle the acknowledgement")
	}
	if p := newTestPlatform(t, server.URL, nil); p.AcknowledgeMessage(rc, core.MessageAckKind("unknown")) {
		t.Fatal("an unknown ack kind must not be handled")
	}
	if p := newTestPlatform(t, server.URL, nil); p.AcknowledgeMessage("not-a-reply-context", core.MessageAckSteered) {
		t.Fatal("a foreign reply context must not be handled")
	}
	if p := newTestPlatform(t, server.URL, nil); p.AcknowledgeMessage(rc, core.MessageAckSteered) {
		t.Fatal("a rejected reaction must fall back to text")
	}
}

// Regression: the inbound Zulip message id used to be dropped, so the engine logged an empty
// msg_id (and any id-keyed behaviour was blind). It must reach core.Message and the configured
// inbound ack reaction must still be posted.
func TestProcessMessage_SetsMessageIDAndInboundReaction(t *testing.T) {
	reactions := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if strings.HasSuffix(r.URL.Path, "/reactions") {
			select {
			case reactions <- r.URL.Path + " emoji_name=" + r.PostForm.Get("emoji_name"):
			default:
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":"success"}`))
	}))
	defer server.Close()

	p := newTestPlatform(t, server.URL, map[string]any{
		"streams":      []any{"tasks"},
		"ack_reaction": "eyes",
	})
	p.botFullName.Store("🔭 Kepler(开开)")

	var got *core.Message
	p.handler = func(_ core.Platform, m *core.Message) { got = m }

	p.processMessage(context.Background(), &zulipMessage{
		ID:               625570911,
		SenderID:         1054187,
		SenderEmail:      "utensil@example.com",
		SenderFullName:   "Utensil Song",
		Content:          "@**🔭 Kepler(开开)** ack test",
		Type:             "stream",
		Subject:          "cc-connect",
		DisplayRecipient: json.RawMessage(`"tasks"`),
	})

	if got == nil {
		t.Fatal("message handler was not called")
	}
	if got.MessageID != "625570911" {
		t.Fatalf("core.Message.MessageID = %q, want %q", got.MessageID, "625570911")
	}
	select {
	case reaction := <-reactions:
		if !strings.HasSuffix(reaction, "emoji_name=eyes") {
			t.Fatalf("inbound ack reaction = %q, want emoji_name=eyes", reaction)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("inbound ack reaction was not posted")
	}
}
