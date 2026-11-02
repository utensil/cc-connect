package grok

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInjectFollowUpSteer_ExistingKey(t *testing.T) {
	in := "[ui]\nfollow_up_behavior = \"queue\"\nmax_thoughts_width = 120\n"
	out := injectFollowUpSteer(in)
	assert.Contains(t, out, "follow_up_behavior = \"steer\"")
	assert.NotContains(t, out, "\"queue\"")
	assert.Contains(t, out, "max_thoughts_width = 120")
}

func TestInjectFollowUpSteer_InsertsIntoExistingUISection(t *testing.T) {
	in := "[cli]\nauto_update = true\n\n[ui]\nmax_thoughts_width = 120\n"
	out := injectFollowUpSteer(in)
	assert.Contains(t, out, "[ui]\nfollow_up_behavior = \"steer\"\nmax_thoughts_width = 120")
}

func TestInjectFollowUpSteer_AppendsUISection(t *testing.T) {
	in := "[cli]\nauto_update = true\n"
	out := injectFollowUpSteer(in)
	assert.Contains(t, out, "[cli]\nauto_update = true\n")
	assert.Contains(t, out, "[ui]\nfollow_up_behavior = \"steer\"")
}

func TestInjectFollowUpSteer_EmptyConfig(t *testing.T) {
	assert.Equal(t, "[ui]\nfollow_up_behavior = \"steer\"\n", injectFollowUpSteer(""))
}

func TestInjectFollowUpSteer_Idempotent(t *testing.T) {
	in := "[ui]\nmax_thoughts_width = 120\n"
	once := injectFollowUpSteer(in)
	twice := injectFollowUpSteer(once)
	assert.Equal(t, once, twice)
}

func TestInjectFollowUpSteer_IgnoresLookalikeKeys(t *testing.T) {
	// A key merely starting with the name must not be rewritten.
	in := "[ui]\nfollow_up_behavior_extra = \"keep\"\n"
	out := injectFollowUpSteer(in)
	assert.Contains(t, out, "follow_up_behavior_extra = \"keep\"")
	assert.Contains(t, out, "[ui]\nfollow_up_behavior = \"steer\"")
}

func TestEnsureSteerHome_LinksEntriesAndInjectsSetting(t *testing.T) {
	realHome := t.TempDir()
	overlay := filepath.Join(t.TempDir(), "overlay")

	require.NoError(t, os.WriteFile(filepath.Join(realHome, "config.toml"),
		[]byte("[ui]\nmax_thoughts_width = 120\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(realHome, "auth.json"), []byte("{}"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(realHome, "sessions"), 0o700))

	got, err := ensureSteerHome(realHome, overlay)
	require.NoError(t, err)
	assert.Equal(t, overlay, got)

	config, err := os.ReadFile(filepath.Join(overlay, "config.toml"))
	require.NoError(t, err)
	assert.Contains(t, string(config), "follow_up_behavior = \"steer\"")
	assert.Contains(t, string(config), "max_thoughts_width = 120")

	// The user's own config must never be modified.
	source, err := os.ReadFile(filepath.Join(realHome, "config.toml"))
	require.NoError(t, err)
	assert.NotContains(t, string(source), "follow_up_behavior")

	// Everything else is shared through symlinks so auth, sessions and skills
	// keep working.
	for _, name := range []string{"auth.json", "sessions"} {
		link := filepath.Join(overlay, name)
		info, err := os.Lstat(link)
		require.NoError(t, err)
		require.NotZero(t, info.Mode()&os.ModeSymlink, "%s should be a symlink", name)
		target, err := os.Readlink(link)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(realHome, name), target)
	}
}

func TestEnsureSteerHome_RefreshesConfigAndReplacesStaleLinks(t *testing.T) {
	realHome := t.TempDir()
	overlay := filepath.Join(t.TempDir(), "overlay")

	configPath := filepath.Join(realHome, "config.toml")
	require.NoError(t, os.WriteFile(configPath, []byte("[cli]\nauto_update = true\n"), 0o600))

	_, err := ensureSteerHome(realHome, overlay)
	require.NoError(t, err)

	// The user edits their config: the overlay must pick the change up.
	require.NoError(t, os.WriteFile(configPath, []byte("[cli]\nauto_update = false\n"), 0o600))
	_, err = ensureSteerHome(realHome, overlay)
	require.NoError(t, err)
	refreshed, err := os.ReadFile(filepath.Join(overlay, "config.toml"))
	require.NoError(t, err)
	assert.Contains(t, string(refreshed), "auto_update = false")

	// A regular file left in the overlay is replaced by the shared symlink.
	stale := filepath.Join(overlay, "auth.json")
	require.NoError(t, os.WriteFile(stale, []byte("stale"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(realHome, "auth.json"), []byte("{}"), 0o600))
	_, err = ensureSteerHome(realHome, overlay)
	require.NoError(t, err)
	info, err := os.Lstat(stale)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink)
}

func TestEnsureSteerHome_RejectsMissingHome(t *testing.T) {
	_, err := ensureSteerHome(filepath.Join(t.TempDir(), "missing"), t.TempDir())
	require.Error(t, err)
}

func TestInjectFollowUpSteer_PreservesTrailingComment(t *testing.T) {
	in := "[ui]\nfollow_up_behavior = \"queue\"   # TUI preference\nmax_thoughts_width = 120\n"
	out := injectFollowUpSteer(in)
	assert.Contains(t, out, "follow_up_behavior = \"steer\"   # TUI preference")
	assert.Contains(t, out, "max_thoughts_width = 120")
}

func TestInjectFollowUpSteer_LeavesOtherTablesAlone(t *testing.T) {
	in := "[models]\nfollow_up_behavior = \"queue\"\n"
	out := injectFollowUpSteer(in)
	// The unrelated key keeps its value; a proper [ui] section is added.
	assert.Contains(t, out, "[models]\nfollow_up_behavior = \"queue\"\n")
	assert.Contains(t, out, "[ui]\nfollow_up_behavior = \"steer\"")
}

func TestInjectFollowUpSteer_ReplacesOnlyTheUISection(t *testing.T) {
	in := "[models]\nfollow_up_behavior = \"queue\"\n\n[ui]\nfollow_up_behavior = \"queue\"\n"
	out := injectFollowUpSteer(in)
	assert.Contains(t, out, "[models]\nfollow_up_behavior = \"queue\"\n")
	assert.Contains(t, out, "[ui]\nfollow_up_behavior = \"steer\"\n")
}

func TestEnsureSteerHome_RefusesToDestroyOverlayState(t *testing.T) {
	realHome := t.TempDir()
	overlay := filepath.Join(t.TempDir(), "overlay")
	require.NoError(t, os.WriteFile(filepath.Join(realHome, "config.toml"), []byte(""), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(overlay, "sessions", "kept"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(overlay, "sessions", "kept", "state"), []byte("x"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(realHome, "sessions"), 0o700))

	_, err := ensureSteerHome(realHome, overlay)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-empty directory")
}

func TestEnsureSteerHome_RejectsOverlayEqualToRealHome(t *testing.T) {
	realHome := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(realHome, "config.toml"), []byte(""), 0o600))

	_, err := ensureSteerHome(realHome, realHome)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must differ")
}
