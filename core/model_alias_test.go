package core

import "testing"

func TestResolveModelAlias_CaseInsensitive(t *testing.T) {
	models := []ModelOption{{Name: "gpt-5.3-codex", Alias: "Codex"}}

	got := resolveModelAlias(models, "codex")
	if got != "gpt-5.3-codex" {
		t.Fatalf("resolveModelAlias() = %q, want %q", got, "gpt-5.3-codex")
	}
}

func TestResolveModelAlias_NoMatchFallsBackToInput(t *testing.T) {
	models := []ModelOption{{Name: "gpt-5.3-codex", Alias: "codex"}}

	got := resolveModelAlias(models, "gpt-5.4")
	if got != "gpt-5.4" {
		t.Fatalf("resolveModelAlias() = %q, want original input", got)
	}
}

func TestParseModelSwitchArgs(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		want         string
		defaultScope bool
		wantReason   string
		ok           bool
	}{
		{name: "bare session syntax", args: []string{"gpt"}, want: "gpt", ok: true},
		{name: "session syntax", args: []string{"session", "gpt"}, want: "gpt", ok: true},
		{name: "switch syntax", args: []string{"switch", "gpt"}, want: "gpt", ok: true},
		{name: "default syntax", args: []string{"default", "gpt"}, want: "gpt", defaultScope: true, ok: true},
		{name: "missing switch target", args: []string{"switch"}, ok: false},
		{name: "unknown subcommand", args: []string{"list", "gpt"}, want: "list", wantReason: "gpt", ok: true},
		{name: "bare model + reasoning", args: []string{"gpt", "high"}, want: "gpt", wantReason: "high", ok: true},
		{name: "session model + reasoning", args: []string{"session", "gpt", "medium"}, want: "gpt", wantReason: "medium", ok: true},
		{name: "default model + reasoning", args: []string{"default", "gpt", "low"}, want: "gpt", defaultScope: true, wantReason: "low", ok: true},
		{name: "too many args", args: []string{"session", "gpt", "high", "extra"}, ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, defaultScope, reasoning, ok := parseModelSwitchArgs(tt.args)
			if ok != tt.ok {
				t.Fatalf("parseModelSwitchArgs() ok = %v, want %v", ok, tt.ok)
			}
			if got != tt.want {
				t.Fatalf("parseModelSwitchArgs() = %q, want %q", got, tt.want)
			}
			if defaultScope != tt.defaultScope {
				t.Fatalf("parseModelSwitchArgs() defaultScope = %v, want %v", defaultScope, tt.defaultScope)
			}
			if reasoning != tt.wantReason {
				t.Fatalf("parseModelSwitchArgs() reasoning = %q, want %q", reasoning, tt.wantReason)
			}
		})
	}
}
