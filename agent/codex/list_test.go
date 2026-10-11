package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A session file whose first record exceeds the legacy 256 KiB scan cap must still be listed. With
// that cap the scan aborted before session_meta, parseCodexSessionFile returned nil, and the
// conversation silently disappeared from /list. Real transcript records can dwarf that cap: the
// thread behind the 2026-10-11 resume failure carried a 2.09 MiB record.
func TestParseCodexSessionFile_AcceptsRecordLargerThanLegacyCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-fixture.jsonl")
	big := `{"type":"event_msg","payload":{"type":"token_count","padding":"` + strings.Repeat("x", 512<<10) + `"}}`
	content := strings.Join([]string{
		big,
		`{"type":"session_meta","payload":{"id":"session-1","cwd":"/work/dir"}}`,
		`{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":"please list the pending reviews in this repository"}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	info := parseCodexSessionFile(path, "/work/dir")
	if info == nil {
		t.Fatal("parseCodexSessionFile() = nil, want the session parsed past a record over the legacy cap")
	}
	if info.ID != "session-1" {
		t.Fatalf("info.ID = %q, want session-1", info.ID)
	}
	if info.MessageCount != 1 {
		t.Fatalf("info.MessageCount = %d, want 1", info.MessageCount)
	}
	if !strings.Contains(info.Summary, "pending reviews") {
		t.Fatalf("info.Summary = %q, want the user prompt recorded after the oversized record", info.Summary)
	}
}
