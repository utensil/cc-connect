package core

import (
	"bufio"
	"io"
	"log/slog"
	"sync/atomic"
)

// Bounds for reading newline-delimited records from an agent's stdout.
//
// Agents emit one JSON object per line, and a single record can legitimately be
// very large: resuming a long Codex thread replays enough content that one line
// can exceed tens of megabytes. The per-line bound exists to fail closed on a
// pathological stream, not to cap normal operation, so it has to stay well
// above what a large conversation produces.
//
// Crossing the bound aborts the read loop, which kills the session. The engine
// then starts a fresh conversation and the user silently loses the context of
// the previous one (see core/engine.go's resume fallback), so an under-sized
// bound is far more damaging than the memory needed to read the record.
const (
	// AgentStreamMaxLineBytes is the largest single stdout record cc-connect
	// accepts from an agent process.
	AgentStreamMaxLineBytes = 64 << 20 // 64 MiB
	// AgentStreamLineWarnBytes is the record size at which a stream is logged
	// as growing towards the bound, so growth is visible before it becomes a
	// failure.
	AgentStreamLineWarnBytes = AgentStreamMaxLineBytes / 2
)

// NewAgentStreamScanner returns a line scanner for an agent stdout stream with
// the shared per-line bound applied. The scanner grows its buffer on demand, so
// the bound is only a ceiling, not an allocation.
func NewAgentStreamScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), AgentStreamMaxLineBytes)
	return scanner
}

// WarnOnLongAgentStreamLine logs, once per stream, that a record is approaching
// the per-line bound. warned is a per-stream flag owned by the caller.
func WarnOnLongAgentStreamLine(stream string, lineBytes int, warned *atomic.Bool) {
	if warned == nil || lineBytes < AgentStreamLineWarnBytes {
		return
	}
	if !warned.CompareAndSwap(false, true) {
		return
	}
	slog.Warn("agent stream record is large",
		"stream", stream,
		"bytes", lineBytes,
		"max_bytes", AgentStreamMaxLineBytes,
		"hint", "records approaching the bound abort the session when they cross it",
	)
}
