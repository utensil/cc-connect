package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
)

func init() {
	core.RegisterAgent("grok", New)
}

// Agent drives the official Grok Build CLI (https://x.ai/cli) in headless mode:
//
//	grok -p <prompt> --output-format streaming-json [--resume <id>] ...
//
// Modes map onto Grok's --permission-mode (+ --always-approve for non-plan):
//   - "default":           permission-mode default + always-approve
//   - "accept_edits":      permission-mode acceptEdits + always-approve
//   - "yolo":              permission-mode bypassPermissions + always-approve
//   - "plan":              permission-mode plan (read-only; no always-approve)
//   - "dont_ask":          permission-mode dontAsk + always-approve
type Agent struct {
	workDir         string
	model           string
	mode            string
	cmd             string
	cliExtraArgs    []string
	configEnv       []string
	timeout         time.Duration
	reasoningEffort string
	maxTurns        int
	providers       []core.ProviderConfig
	activeIdx       int
	sessionEnv      []string
	mu              sync.RWMutex

	// catalogMu guards the cached model/effort catalog probed from the CLI, and
	// probeMu serializes the probes themselves (they spawn processes and last up
	// to two 6s timeouts, so neither lock may be held across a probe).
	catalogMu    sync.Mutex
	probeMu      sync.Mutex
	catalogCache []catalogModel
	catalogAt    time.Time
}

func New(opts map[string]any) (core.Agent, error) {
	workDir, _ := opts["work_dir"].(string)
	if workDir == "" {
		workDir = "."
	}
	model, _ := opts["model"].(string)
	mode, _ := opts["mode"].(string)
	mode = normalizeMode(mode)
	cmd, extraArgs := core.ParseCmdOpts(opts, "grok")

	reasoning, _ := opts["reasoning_effort"].(string)
	if reasoning == "" {
		reasoning, _ = opts["effort"].(string)
	}
	reasoning = strings.TrimSpace(reasoning)

	maxTurns := intFromOpts(opts, "max_turns", 0)
	timeoutMins := intFromOpts(opts, "timeout_mins", 0)
	var timeout time.Duration
	if timeoutMins > 0 {
		timeout = time.Duration(timeoutMins) * time.Minute
	}

	if _, err := exec.LookPath(cmd); err != nil {
		return nil, fmt.Errorf("grok: %q CLI not found in PATH, install with: curl -fsSL https://x.ai/cli/install.sh | bash", cmd)
	}

	return &Agent{
		workDir:         workDir,
		model:           model,
		mode:            mode,
		cmd:             cmd,
		cliExtraArgs:    extraArgs,
		configEnv:       core.ParseConfigEnv(opts),
		timeout:         timeout,
		reasoningEffort: reasoning,
		maxTurns:        maxTurns,
		activeIdx:       -1,
	}, nil
}

func intFromOpts(opts map[string]any, key string, def int) int {
	switch v := opts[key].(type) {
	case int64:
		return int(v)
	case int:
		return v
	case float64:
		return int(v)
	default:
		if v != nil {
			slog.Debug("grok: option has unexpected type", "key", key, "type", fmt.Sprintf("%T", v))
		}
		return def
	}
}

func normalizeMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "yolo", "force", "bypass", "auto", "bypasspermissions":
		return "yolo"
	case "plan":
		return "plan"
	case "accept_edits", "acceptedits", "auto_edit", "autoedit", "edit":
		return "accept_edits"
	case "dont_ask", "dontask", "no_ask":
		return "dont_ask"
	default:
		return "default"
	}
}

// permissionModeFlag maps our mode key to Grok's --permission-mode value.
func permissionModeFlag(mode string) string {
	switch mode {
	case "yolo":
		return "bypassPermissions"
	case "plan":
		return "plan"
	case "accept_edits":
		return "acceptEdits"
	case "dont_ask":
		return "dontAsk"
	default:
		return "default"
	}
}

func (a *Agent) Name() string           { return "grok" }
func (a *Agent) CLIBinaryName() string  { return a.cmd }
func (a *Agent) CLIDisplayName() string { return "Grok Build" }

func (a *Agent) SetWorkDir(dir string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.workDir = dir
	slog.Info("grok: work_dir changed", "work_dir", dir)
}

func (a *Agent) GetWorkDir() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.workDir
}

func (a *Agent) SetModel(model string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.model = model
	slog.Info("grok: model changed", "model", model)
}

func (a *Agent) GetModel() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return core.GetProviderModel(a.providers, a.activeIdx, a.model)
}

func (a *Agent) configuredModels() []core.ModelOption {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return core.GetProviderModels(a.providers, a.activeIdx)
}

// AvailableModels prefers provider-configured models, then the catalog the CLI
// advertises, then the static fallback list.
func (a *Agent) AvailableModels(ctx context.Context) []core.ModelOption {
	if models := a.configuredModels(); len(models) > 0 {
		return models
	}
	if models := a.catalog(ctx); len(models) > 0 {
		return staticModels(models)
	}
	return staticModels(fallbackModels)
}

// ── ReasoningEffortSwitcher ─────────────────────────────────────

func (a *Agent) SetReasoningEffort(effort string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reasoningEffort = strings.TrimSpace(effort)
	slog.Info("grok: reasoning effort changed", "effort", a.reasoningEffort)
}

func (a *Agent) GetReasoningEffort() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.reasoningEffort
}

// AvailableReasoningEfforts reports the union of the efforts the CLI advertises.
// It probes the catalog once when nothing is cached, which is why the caller
// sees the CLI's own levels (xhigh/high/medium/low for grok-4.6) instead of a
// hard-coded guess.
func (a *Agent) AvailableReasoningEfforts() []string {
	ctx, cancel := context.WithTimeout(context.Background(), catalogProbeTimeout)
	defer cancel()
	a.catalog(ctx)
	a.catalogMu.Lock()
	defer a.catalogMu.Unlock()
	return a.modelEffortsLocked()
}

// PreservesSessionOnReasoningEffortChange is true: a resumed Grok session can
// take a different --reasoning-effort, so /reasoning must not clear history.
func (a *Agent) PreservesSessionOnReasoningEffortChange() bool { return true }

func (a *Agent) SetSessionEnv(env []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessionEnv = env
}

func (a *Agent) StartSession(ctx context.Context, sessionID string) (core.AgentSession, error) {
	return a.startSession(ctx, sessionID, "", "")
}

// StartSessionWithModel resumes (or starts) one conversation with a model
// override without changing the agent-wide default.
func (a *Agent) StartSessionWithModel(ctx context.Context, sessionID, model string) (core.AgentSession, error) {
	return a.startSession(ctx, sessionID, model, "")
}

// StartSessionWithRuntime applies both the model and the reasoning-effort
// override for this conversation only.
func (a *Agent) StartSessionWithRuntime(ctx context.Context, sessionID, model, reasoningEffort string) (core.AgentSession, error) {
	return a.startSession(ctx, sessionID, model, reasoningEffort)
}

// ValidateSessionRuntime rejects a model/effort pair the CLI does not advertise.
// Unknown catalogs are accepted: the agent contract prefers a clear CLI error
// over silently refusing a model that a newer Grok build may support.
func (a *Agent) ValidateSessionRuntime(model, reasoningEffort string) error {
	model = strings.TrimSpace(model)
	reasoningEffort = strings.TrimSpace(strings.ToLower(reasoningEffort))
	if model == "" && reasoningEffort == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), catalogProbeTimeout)
	defer cancel()
	models := a.catalog(ctx)
	if len(models) == 0 {
		return nil
	}
	if model != "" {
		known := false
		for _, m := range models {
			if strings.EqualFold(m.ID, model) {
				known = true
				break
			}
		}
		if !known {
			ids := make([]string, 0, len(models))
			for _, m := range models {
				ids = append(ids, m.ID)
			}
			return fmt.Errorf("unknown model %q (known: %s)", model, strings.Join(ids, ", "))
		}
	}
	if reasoningEffort != "" {
		for _, e := range a.effortsForModel(model) {
			if e == reasoningEffort {
				return nil
			}
		}
		return fmt.Errorf("unknown reasoning effort %q for model %q", reasoningEffort, model)
	}
	return nil
}

// startSession snapshots the agent config, applying per-session overrides.
func (a *Agent) startSession(ctx context.Context, sessionID, modelOverride, reasoningOverride string) (core.AgentSession, error) {
	a.mu.Lock()
	model := a.effectiveModelLocked()
	mode := a.mode
	cmd := a.cmd
	extraArgs := append([]string{}, a.cliExtraArgs...)
	workDir := a.workDir
	timeout := a.timeout
	reasoning := a.reasoningEffort
	maxTurns := a.maxTurns
	extraEnv := append([]string(nil), a.configEnv...)
	extraEnv = append(extraEnv, a.providerEnvLocked()...)
	extraEnv = append(extraEnv, a.sessionEnv...)
	a.mu.Unlock()

	if m := strings.TrimSpace(modelOverride); m != "" {
		model = m
	}
	if r := strings.TrimSpace(reasoningOverride); r != "" {
		reasoning = r
	}

	return newGrokSession(ctx, sessionConfig{
		cmd:             cmd,
		extraArgs:       extraArgs,
		workDir:         workDir,
		model:           model,
		mode:            mode,
		resumeID:        sessionID,
		extraEnv:        extraEnv,
		timeout:         timeout,
		reasoningEffort: reasoning,
		maxTurns:        maxTurns,
	})
}

func (a *Agent) ListSessions(_ context.Context) ([]core.AgentSessionInfo, error) {
	return listGrokSessions(a.workDir)
}

func (a *Agent) DeleteSession(_ context.Context, sessionID string) error {
	path := findGrokSessionDir(sessionID)
	if path == "" {
		return fmt.Errorf("session not found: %s", sessionID)
	}
	return os.RemoveAll(path)
}

func (a *Agent) Stop() error { return nil }

// ── ModeSwitcher ────────────────────────────────────────────────

func (a *Agent) SetMode(mode string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mode = normalizeMode(mode)
	slog.Info("grok: mode changed", "mode", a.mode)
}

func (a *Agent) GetMode() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mode
}

func (a *Agent) PermissionModes() []core.PermissionModeInfo {
	return []core.PermissionModeInfo{
		{Key: "default", Name: "Default", NameZh: "默认",
			Desc: "Standard permissions (always-approve in headless)", DescZh: "标准权限（无头模式自动批准）"},
		{Key: "accept_edits", Name: "Accept Edits", NameZh: "自动编辑",
			Desc: "Auto-approve edit tools", DescZh: "自动批准编辑类工具"},
		{Key: "yolo", Name: "YOLO", NameZh: "全自动",
			Desc: "Bypass all permission prompts", DescZh: "跳过所有权限确认"},
		{Key: "plan", Name: "Plan", NameZh: "规划模式",
			Desc: "Read-only plan mode, no execution", DescZh: "只读规划模式，不做修改"},
		{Key: "dont_ask", Name: "Don't Ask", NameZh: "不问确认",
			Desc: "Don't ask for permission", DescZh: "不再询问权限"},
	}
}

// ── SkillProvider ───────────────────────────────────────────────

func (a *Agent) SkillDirs() []string {
	absDir, err := filepath.Abs(a.workDir)
	if err != nil {
		absDir = a.workDir
	}
	dirs := []string{filepath.Join(absDir, ".grok", "skills")}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".grok", "skills"))
	}
	return dirs
}

// ── MemoryFileProvider ──────────────────────────────────────────

func (a *Agent) ProjectMemoryFile() string {
	absDir, err := filepath.Abs(a.workDir)
	if err != nil {
		absDir = a.workDir
	}
	// Grok Build uses AGENTS.md project conventions.
	return filepath.Join(absDir, "AGENTS.md")
}

func (a *Agent) GlobalMemoryFile() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	// Prefer AGENTS.md under ~/.grok if present; otherwise empty.
	p := filepath.Join(homeDir, ".grok", "AGENTS.md")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// ── ProviderSwitcher ────────────────────────────────────────────

func (a *Agent) SetProviders(providers []core.ProviderConfig) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.providers = providers
}

func (a *Agent) SetActiveProvider(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if name == "" {
		a.activeIdx = -1
		slog.Info("grok: provider cleared")
		return true
	}
	for i, p := range a.providers {
		if p.Name == name {
			a.activeIdx = i
			slog.Info("grok: provider switched", "provider", name)
			return true
		}
	}
	return false
}

func (a *Agent) GetActiveProvider() *core.ProviderConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activeIdx < 0 || a.activeIdx >= len(a.providers) {
		return nil
	}
	p := a.providers[a.activeIdx]
	return &p
}

func (a *Agent) ListProviders() []core.ProviderConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	result := make([]core.ProviderConfig, len(a.providers))
	copy(result, a.providers)
	return result
}

func (a *Agent) providerEnvLocked() []string {
	if a.activeIdx < 0 || a.activeIdx >= len(a.providers) {
		return nil
	}
	p := a.providers[a.activeIdx]
	var env []string
	if p.APIKey != "" {
		env = append(env, "XAI_API_KEY="+p.APIKey)
	}
	if p.BaseURL != "" {
		env = append(env, "XAI_API_BASE_URL="+p.BaseURL)
	}
	for k, v := range p.Env {
		env = append(env, k+"="+v)
	}
	return env
}

// ── Session listing ─────────────────────────────────────────────

func grokSessionsBaseDir() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(homeDir, ".grok", "sessions")
}

// grokSessionSlug encodes an absolute work dir the way Grok Build stores it:
// every path separator becomes %2F (url.PathEscape of the whole path).
func grokSessionSlug(absWorkDir string) string {
	return url.PathEscape(absWorkDir)
}

func resolveWorkDir(workDir string) string {
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return workDir
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

func listGrokSessions(workDir string) ([]core.AgentSessionInfo, error) {
	absWorkDir := resolveWorkDir(workDir)
	base := grokSessionsBaseDir()
	if base == "" {
		return nil, nil
	}

	// Prefer the project-scoped directory; fall back to scanning all projects.
	candidates := []string{filepath.Join(base, grokSessionSlug(absWorkDir))}
	// macOS often stores /tmp under /private/tmp — also try unresolved Abs.
	if alt, err := filepath.Abs(workDir); err == nil && alt != absWorkDir {
		candidates = append(candidates, filepath.Join(base, grokSessionSlug(alt)))
	}

	seen := make(map[string]bool)
	var sessions []core.AgentSessionInfo

	scanDir := func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			info := parseGrokSessionDir(filepath.Join(dir, e.Name()), absWorkDir)
			if info == nil || seen[info.ID] {
				continue
			}
			seen[info.ID] = true
			sessions = append(sessions, *info)
		}
	}

	for _, c := range candidates {
		scanDir(c)
	}

	// If nothing matched the slug, scan all project dirs and filter by cwd in summary.
	if len(sessions) == 0 {
		entries, err := os.ReadDir(base)
		if err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				scanDir(filepath.Join(base, e.Name()))
			}
		}
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ModifiedAt.After(sessions[j].ModifiedAt)
	})
	return sessions, nil
}

func parseGrokSessionDir(sessionDir, filterWorkDir string) *core.AgentSessionInfo {
	summaryPath := filepath.Join(sessionDir, "summary.json")
	data, err := os.ReadFile(summaryPath)
	if err != nil {
		return nil
	}
	var summary struct {
		Info struct {
			ID  string `json:"id"`
			Cwd string `json:"cwd"`
		} `json:"info"`
		SessionSummary  string `json:"session_summary"`
		GeneratedTitle  string `json:"generated_title"`
		NumMessages     int    `json:"num_messages"`
		NumChatMessages int    `json:"num_chat_messages"`
		UpdatedAt       string `json:"updated_at"`
		LastActiveAt    string `json:"last_active_at"`
	}
	if json.Unmarshal(data, &summary) != nil {
		return nil
	}

	sessionID := summary.Info.ID
	if sessionID == "" {
		sessionID = filepath.Base(sessionDir)
	}

	if filterWorkDir != "" && summary.Info.Cwd != "" {
		cwdResolved := resolveWorkDir(summary.Info.Cwd)
		cwdAbs, _ := filepath.Abs(summary.Info.Cwd)
		if cwdResolved != filterWorkDir && cwdAbs != filterWorkDir && summary.Info.Cwd != filterWorkDir {
			return nil
		}
	}

	summaryText := strings.TrimSpace(summary.SessionSummary)
	if summaryText == "" {
		summaryText = strings.TrimSpace(summary.GeneratedTitle)
	}
	if utf8.RuneCountInString(summaryText) > 60 {
		summaryText = string([]rune(summaryText)[:60]) + "..."
	}

	msgCount := summary.NumChatMessages
	if msgCount == 0 {
		msgCount = summary.NumMessages
	}

	mod := time.Time{}
	for _, ts := range []string{summary.LastActiveAt, summary.UpdatedAt} {
		if ts == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			mod = t
			break
		}
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			mod = t
			break
		}
	}
	if mod.IsZero() {
		if fi, err := os.Stat(sessionDir); err == nil {
			mod = fi.ModTime()
		}
	}

	return &core.AgentSessionInfo{
		ID:           sessionID,
		Summary:      summaryText,
		MessageCount: msgCount,
		ModifiedAt:   mod,
	}
}

func findGrokSessionDir(sessionID string) string {
	base := grokSessionsBaseDir()
	// Session ids come from /ls output today, but never let a value escape the
	// session store directory when it is deleted recursively.
	sessionID = filepath.Base(strings.TrimSpace(sessionID))
	if base == "" || sessionID == "" || sessionID == "." || sessionID == ".." {
		return ""
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		candidate := filepath.Join(base, e.Name(), sessionID)
		if fi, err := os.Stat(candidate); err == nil && fi.IsDir() {
			return candidate
		}
	}
	return ""
}

// Compile-time interface checks.
var (
	_ core.Agent                           = (*Agent)(nil)
	_ core.AgentDoctorInfo                 = (*Agent)(nil)
	_ core.ModeSwitcher                    = (*Agent)(nil)
	_ core.WorkDirSwitcher                 = (*Agent)(nil)
	_ core.ModelSwitcher                   = (*Agent)(nil)
	_ core.SessionModelStarter             = (*Agent)(nil)
	_ core.SessionRuntimeStarter           = (*Agent)(nil)
	_ core.ReasoningEffortSwitcher         = (*Agent)(nil)
	_ core.ReasoningEffortSessionPreserver = (*Agent)(nil)
	_ core.SessionEnvInjector              = (*Agent)(nil)
	_ core.ProviderSwitcher                = (*Agent)(nil)
	_ core.MemoryFileProvider              = (*Agent)(nil)
	_ core.SkillProvider                   = (*Agent)(nil)
	_ core.SessionDeleter                  = (*Agent)(nil)
)
