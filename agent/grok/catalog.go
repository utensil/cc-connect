package grok

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// catalogModel is one entry of the authoritative model catalog reported by the
// Grok CLI itself. `grok models` only prints ids and a default marker, while the
// ACP `initialize` handshake of `grok agent stdio` reports display names, the
// per-model context window and the reasoning efforts each model supports. That
// handshake needs no login, so it is the primary probe; `grok models` is the
// fallback and a small static list is the last resort.
type catalogModel struct {
	ID            string
	Name          string
	Description   string
	DefaultEffort string
	Efforts       []string
	ContextTokens int
}

// catalogTTL bounds how often the CLI is probed. Grok's catalog only changes on
// upgrade or provider switch, so a stale list for a few minutes is harmless.
const catalogTTL = 10 * time.Minute

// catalogProbeTimeout bounds the ACP initialize handshake. The CLI answers it in
// well under a second; the ceiling only protects a /model command from hanging
// when the binary is wedged.
const catalogProbeTimeout = 6 * time.Second

// fallbackModels is used when both probes fail (for example a missing or broken
// CLI): it matches the stock Grok Build catalog at the time of writing.
var fallbackModels = []catalogModel{
	{ID: "grok-4.6", Name: "Grok 4.6", Description: "default", DefaultEffort: "high",
		Efforts: []string{"xhigh", "high", "medium", "low"}, ContextTokens: 500000},
	{ID: "grok-4.5", Name: "Grok 4.5", DefaultEffort: "high",
		Efforts: []string{"high", "medium", "low"}, ContextTokens: 500000},
}

// fallbackEfforts is the conservative union used before any probe succeeds.
var fallbackEfforts = []string{"xhigh", "high", "medium", "low"}

// catalog returns the cached model catalog, probing the CLI when the cache is
// empty or stale. probeMu serializes probes so concurrent commands spawn one
// process, while catalogMu is only ever held for the cache read/write — a probe
// can take up to two 6s timeouts and must not block model/effort reads.
func (a *Agent) catalog(ctx context.Context) []catalogModel {
	a.catalogMu.Lock()
	cached := a.catalogCache
	fresh := len(cached) > 0 && time.Since(a.catalogAt) < catalogTTL
	a.catalogMu.Unlock()
	if fresh {
		return cached
	}

	a.probeMu.Lock()
	defer a.probeMu.Unlock()
	// Another goroutine may have refreshed the cache while we waited.
	a.catalogMu.Lock()
	if len(a.catalogCache) > 0 && time.Since(a.catalogAt) < catalogTTL {
		cached = a.catalogCache
		a.catalogMu.Unlock()
		return cached
	}
	previous := a.catalogCache
	a.catalogMu.Unlock()

	a.mu.RLock()
	cmdName := a.cmd
	a.mu.RUnlock()

	models := probeCatalogACP(ctx, cmdName)
	if len(models) == 0 {
		models = probeCatalogText(ctx, cmdName)
	}
	if len(models) == 0 {
		// Keep a previous good result rather than degrading to the static list.
		return previous
	}
	a.catalogMu.Lock()
	a.catalogCache = models
	a.catalogAt = time.Now()
	a.catalogMu.Unlock()
	return models
}

// probeCatalogACP runs the ACP `initialize` handshake of `grok agent stdio` and
// reads `_meta.modelState`. It needs no authentication, so /model can list the
// real catalog before the operator has ever run `grok login`.
func probeCatalogACP(ctx context.Context, cmdName string) []catalogModel {
	if cmdName == "" {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, catalogProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, cmdName, "agent", "stdio")
	cmd.WaitDelay = time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil
	}
	if err := cmd.Start(); err != nil {
		slog.Debug("grok: catalog probe failed to start", "error", err)
		return nil
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": 1,
			"clientCapabilities": map[string]any{
				"fs":       map[string]any{"readTextFile": true, "writeTextFile": true},
				"terminal": true,
			},
			"clientInfo": map[string]any{"name": "cc-connect", "version": "1"},
		},
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil
	}
	if _, err := stdin.Write(append(payload, '\n')); err != nil {
		slog.Debug("grok: catalog probe write failed", "error", err)
		return nil
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		if models := modelsFromACPFrame([]byte(line)); len(models) > 0 {
			return models
		}
	}
	slog.Debug("grok: catalog probe returned no modelState")
	return nil
}

// modelsFromACPFrame decodes one ACP JSON-RPC frame and, when it is the
// `initialize` result, returns the model catalog from `_meta.modelState`.
func modelsFromACPFrame(data []byte) []catalogModel {
	var frame struct {
		Result *struct {
			Meta struct {
				ModelState *struct {
					CurrentModelID  string `json:"currentModelId"`
					AvailableModels []struct {
						ModelID     string `json:"modelId"`
						Name        string `json:"name"`
						Description string `json:"description"`
						Meta        struct {
							TotalContextTokens      int    `json:"totalContextTokens"`
							ReasoningEffort         string `json:"reasoningEffort"`
							SupportsReasoningEffort bool   `json:"supportsReasoningEffort"`
							ReasoningEfforts        []struct {
								Value   string `json:"value"`
								ID      string `json:"id"`
								Default bool   `json:"default"`
							} `json:"reasoningEfforts"`
						} `json:"_meta"`
					} `json:"availableModels"`
				} `json:"modelState"`
			} `json:"_meta"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		return nil
	}
	if frame.Result == nil || frame.Result.Meta.ModelState == nil {
		return nil
	}
	state := frame.Result.Meta.ModelState
	models := make([]catalogModel, 0, len(state.AvailableModels))
	for _, m := range state.AvailableModels {
		if m.ModelID == "" {
			continue
		}
		entry := catalogModel{
			ID:            m.ModelID,
			Name:          m.Name,
			Description:   m.Description,
			DefaultEffort: m.Meta.ReasoningEffort,
			ContextTokens: m.Meta.TotalContextTokens,
		}
		for _, e := range m.Meta.ReasoningEfforts {
			value := e.Value
			if value == "" {
				value = e.ID
			}
			if value == "" {
				continue
			}
			entry.Efforts = append(entry.Efforts, value)
			if e.Default && entry.DefaultEffort == "" {
				entry.DefaultEffort = value
			}
		}
		if m.ModelID == state.CurrentModelID && entry.Description == "" {
			entry.Description = "default"
		}
		models = append(models, entry)
	}
	return models
}

// probeCatalogText falls back to `grok models`, which prints ids only.
func probeCatalogText(ctx context.Context, cmdName string) []catalogModel {
	if cmdName == "" {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, catalogProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, cmdName, "models").CombinedOutput()
	if err != nil {
		slog.Debug("grok: models probe failed", "error", err)
		return nil
	}
	return parseModelsText(string(out))
}

// parseModelsText parses the plain-text `grok models` listing:
//
//	Default model: grok-4.6
//
//	Available models:
//	  * grok-4.6 (default)
//	  - grok-4.5
func parseModelsText(out string) []catalogModel {
	var models []catalogModel
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(strings.ToLower(line), "default model") ||
			strings.HasPrefix(strings.ToLower(line), "available models") ||
			strings.HasPrefix(line, "You are not authenticated") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "*"))
		line = strings.TrimSpace(strings.TrimPrefix(line, "-"))
		if line == "" || !strings.HasPrefix(line, "grok") {
			continue
		}
		name := line
		desc := ""
		if i := strings.IndexAny(name, " \t"); i > 0 {
			desc = strings.TrimSpace(name[i:])
			name = name[:i]
		}
		models = append(models, catalogModel{ID: name, Description: desc})
	}
	return models
}

// staticModels converts catalogModel entries into core model options.
func staticModels(models []catalogModel) []core.ModelOption {
	out := make([]core.ModelOption, 0, len(models))
	for _, m := range models {
		desc := m.Description
		if desc == "" {
			desc = m.Name
		}
		out = append(out, core.ModelOption{Name: m.ID, Desc: desc})
	}
	return out
}

// modelEffortsLocked returns the reasoning efforts advertised across all cached
// catalog models, in catalog order without duplicates. Callers must hold
// catalogMu.
func (a *Agent) modelEffortsLocked() []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range a.catalogCache {
		for _, e := range m.Efforts {
			if e == "" || seen[e] {
				continue
			}
			seen[e] = true
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		return append([]string{}, fallbackEfforts...)
	}
	return out
}

// effortsForModel returns the efforts a specific catalog model supports, or all
// advertised efforts when the model is unknown.
func (a *Agent) effortsForModel(model string) []string {
	a.catalogMu.Lock()
	defer a.catalogMu.Unlock()
	for _, m := range a.catalogCache {
		if strings.EqualFold(m.ID, model) && len(m.Efforts) > 0 {
			return append([]string{}, m.Efforts...)
		}
	}
	return a.modelEffortsLocked()
}

// effectiveModelLocked resolves the model a turn runs with, honouring an active
// provider override. Callers must hold a.mu.
func (a *Agent) effectiveModelLocked() string {
	model := a.model
	if a.activeIdx >= 0 && a.activeIdx < len(a.providers) {
		if m := a.providers[a.activeIdx].Model; m != "" {
			model = m
		}
	}
	return model
}
