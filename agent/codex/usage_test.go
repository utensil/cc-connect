package codex

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestFetchUsage_Success(t *testing.T) {
	agent := &Agent{}
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if got := req.URL.String(); got != "https://chatgpt.com/backend-api/wham/usage" {
				t.Fatalf("request URL = %q", got)
			}
			if got := req.Header.Get("Authorization"); got != "Bearer token-123" {
				t.Fatalf("authorization = %q", got)
			}
			if got := req.Header.Get("ChatGPT-Account-Id"); got != "acct-123" {
				t.Fatalf("account id header = %q", got)
			}
			body := `{
			  "user_id": "user-1",
			  "account_id": "acct-123",
			  "email": "dev@example.com",
			  "plan_type": "team",
			  "rate_limit": {
			    "allowed": true,
			    "limit_reached": false,
			    "primary_window": {
			      "used_percent": 23,
			      "limit_window_seconds": 18000,
			      "reset_after_seconds": 6665,
			      "reset_at": 1773292109
			    },
			    "secondary_window": {
			      "used_percent": 42,
			      "limit_window_seconds": 604800,
			      "reset_after_seconds": 512698,
			      "reset_at": 1773798142
			    }
			  },
			  "code_review_rate_limit": {
			    "allowed": true,
			    "limit_reached": false,
			    "primary_window": {
			      "used_percent": 0,
			      "limit_window_seconds": 604800,
			      "reset_after_seconds": 604800,
			      "reset_at": 1773890244
			    },
			    "secondary_window": null
			  },
			  "credits": {
			    "has_credits": false,
			    "unlimited": false,
			    "balance": null
			  }
			}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		}),
	}

	report, err := agent.fetchUsage(context.Background(), client, codexOAuthTokens{
		AccessToken: "token-123",
		AccountID:   "acct-123",
	})
	if err != nil {
		t.Fatalf("fetchUsage returned error: %v", err)
	}
	if report.Provider != "codex" {
		t.Fatalf("provider = %q, want codex", report.Provider)
	}
	if report.Plan != "team" {
		t.Fatalf("plan = %q, want team", report.Plan)
	}
	if len(report.Buckets) != 2 {
		t.Fatalf("buckets = %d, want 2", len(report.Buckets))
	}
	if got := report.Buckets[0].Windows[0].UsedPercent; got != 23 {
		t.Fatalf("primary used percent = %d, want 23", got)
	}
	if got := report.Buckets[0].Windows[1].UsedPercent; got != 42 {
		t.Fatalf("secondary used percent = %d, want 42", got)
	}
}

func TestFetchUsage_HTTPError(t *testing.T) {
	agent := &Agent{}
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"error":"unauthorized"}`)),
			}, nil
		}),
	}

	_, err := agent.fetchUsage(context.Background(), client, codexOAuthTokens{
		AccessToken: "token-123",
		AccountID:   "acct-123",
	})
	if err == nil || !strings.Contains(err.Error(), "status 401") {
		t.Fatalf("err = %v, want status 401", err)
	}
}

func TestReadOAuthTokens_MissingFields(t *testing.T) {
	agent := &Agent{}
	_, err := agent.readOAuthTokens(func(string) ([]byte, error) {
		return []byte(`{"tokens":{"access_token":"token-only"}}`), nil
	})
	if err == nil || !strings.Contains(err.Error(), "account_id") {
		t.Fatalf("err = %v, want missing account_id", err)
	}
}

func TestReadOAuthTokens_InvalidJSON(t *testing.T) {
	agent := &Agent{}
	_, err := agent.readOAuthTokens(func(string) ([]byte, error) {
		return []byte(`{`), nil
	})
	if err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("err = %v, want parse error", err)
	}
}

// A rollout record larger than the legacy 2 MiB rollout-scan cap must still yield context usage.
// The same large threads that need a bigger app-server per-line bound (see
// TestAppServerSession_ReadLoopAcceptsRecordLargerThanTheLegacyCap in appserver_session_test.go) also
// carry token_count records over 2 MiB: one measured 2.09 MiB on 2026-10-11, and the aborted scan
// reported no context usage at all for that thread.
func TestScanContextUsageFromRollout_AcceptsRecordLargerThanLegacyCap(t *testing.T) {
	line := tokenCountRolloutLine(t, 3<<20)
	usage, err := scanContextUsageFromRollout(strings.NewReader(line))
	if err != nil {
		t.Fatalf("scanContextUsageFromRollout() error = %v, want nil for a record over the legacy cap", err)
	}
	if usage == nil {
		t.Fatal("scanContextUsageFromRollout() = nil, want usage parsed from a record over the legacy cap")
	}
	if usage.UsedTokens != 12345 || usage.ContextWindow != 400000 {
		t.Fatalf("usage = %+v, want UsedTokens=12345 ContextWindow=400000", usage)
	}
}

// tokenCountRolloutLine builds a valid token_count rollout record padded past pad bytes, mirroring a
// real rollout line whose large tool output dwarfs the token_count payload that follows it.
func tokenCountRolloutLine(t *testing.T, pad int) string {
	t.Helper()
	padding := strings.Repeat("x", pad)
	line := fmt.Sprintf(
		`{"type":"event_msg","payload":{"type":"token_count","padding":%q,"info":{"total_token_usage":{"total_tokens":12345},"last_token_usage":{"total_tokens":12345,"input_tokens":12000,"output_tokens":345},"model_context_window":400000}}}`+"\n",
		padding,
	)
	if len(line) <= pad {
		t.Fatalf("fixture record is %d bytes, want more than the %d-byte pad", len(line), pad)
	}
	return line
}
