// Native mid-turn steering for the ACP transport.
//
// Grok decides what a `session/prompt` arriving mid-turn means by its own
// `[ui] follow_up_behavior` setting: "queue" (the CLI default) runs it as the
// next turn, "steer" injects it into the running turn at the next safe point.
// That setting is user-scoped and is *not* reachable through `GROK_CONFIG` /
// `GROK_CONFIG_PATH` (both overlays are confined to an allowlist that excludes
// `ui`) nor through ACP's `session/set_config_option` (model and
// reasoning_effort only).
//
// So this adapter passes the setting the only way the CLI honours it: it runs
// the agent against a GROK_HOME whose config.toml is the user's config plus
// `[ui] follow_up_behavior = "steer"`, and symlinks every other entry of the
// user's home so auth, sessions, skills, plugins and caches stay shared. The
// user's own config.toml is never modified, and the setting applies only to
// the cc-connect child process.
package grok

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const steerHomeDirName = "grok-steer-home"

// steerHomeConfigName is the one entry the overlay owns; everything else in
// the overlay home is a symlink into the real GROK_HOME.
const steerHomeConfigName = "config.toml"

// resolveGrokHome returns the CLI's config home, honouring GROK_HOME.
func resolveGrokHome() string {
	if home := strings.TrimSpace(os.Getenv("GROK_HOME")); home != "" {
		return home
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(userHome, ".grok")
}

// defaultSteerHomeDir is where the adapter keeps the generated overlay. It
// lives in the user cache directory because it is regenerable, but it must be
// stable across sessions: `grok` keys its session store, plugin state and
// leader socket off GROK_HOME.
func defaultSteerHomeDir() string {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "cc-connect", steerHomeDirName)
}

// ensureSteerHome materialises the overlay home and returns its path.
func ensureSteerHome(realHome, overlayDir string) (string, error) {
	if realHome == "" {
		return "", fmt.Errorf("grok: cannot resolve GROK_HOME")
	}
	info, err := os.Stat(realHome)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("grok: GROK_HOME %q is not a directory", realHome)
	}
	if overlayDir == "" {
		overlayDir = defaultSteerHomeDir()
	}
	if err := os.MkdirAll(overlayDir, 0o700); err != nil {
		return "", fmt.Errorf("grok: create steer home %q: %w", overlayDir, err)
	}

	if err := linkHomeEntries(realHome, overlayDir); err != nil {
		return "", err
	}
	if err := writeSteerConfig(filepath.Join(realHome, steerHomeConfigName), filepath.Join(overlayDir, steerHomeConfigName)); err != nil {
		return "", err
	}
	return overlayDir, nil
}

// linkHomeEntries symlinks every entry of the real home into the overlay,
// skipping the config file the overlay owns.
func linkHomeEntries(realHome, overlayDir string) error {
	entries, err := os.ReadDir(realHome)
	if err != nil {
		return fmt.Errorf("grok: read %q: %w", realHome, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Name() == steerHomeConfigName {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		target := filepath.Join(realHome, name)
		link := filepath.Join(overlayDir, name)

		if existing, lerr := os.Lstat(link); lerr == nil {
			if existing.Mode()&os.ModeSymlink != 0 {
				if resolved, rerr := os.Readlink(link); rerr == nil && resolved == target {
					continue
				}
			}
			// A stale overlay entry: replace it so the overlay cannot shadow a
			// real home entry with stale content.
			if rerr := os.RemoveAll(link); rerr != nil {
				return fmt.Errorf("grok: replace %q: %w", link, rerr)
			}
		}
		if err := os.Symlink(target, link); err != nil {
			return fmt.Errorf("grok: link %q -> %q: %w", link, target, err)
		}
	}
	return nil
}

// writeSteerConfig writes the overlay config: the user's config with
// `[ui] follow_up_behavior = "steer"` enforced. An unchanged result is not
// rewritten, so the overlay keeps a stable mtime.
func writeSteerConfig(srcPath, dstPath string) error {
	src, err := os.ReadFile(srcPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("grok: read %q: %w", srcPath, err)
	}
	desired := injectFollowUpSteer(string(src))

	if current, cerr := os.ReadFile(dstPath); cerr == nil && string(current) == desired {
		return nil
	}
	if err := os.WriteFile(dstPath, []byte(desired), 0o600); err != nil {
		return fmt.Errorf("grok: write %q: %w", dstPath, err)
	}
	return nil
}

// injectFollowUpSteer returns the config text with mid-turn follow-ups set to
// steer, preserving the rest of the file byte for byte.
func injectFollowUpSteer(config string) string {
	const key = "follow_up_behavior"
	const want = `follow_up_behavior = "steer"`

	lines := strings.Split(config, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, key) {
			continue
		}
		rest := strings.TrimPrefix(trimmed, key)
		if rest != "" && !strings.HasPrefix(rest, " ") && !strings.HasPrefix(rest, "=") && !strings.HasPrefix(rest, "\t") {
			continue
		}
		lines[i] = want
		return strings.Join(lines, "\n")
	}

	for i, line := range lines {
		if strings.TrimSpace(line) != "[ui]" {
			continue
		}
		lines = append(lines[:i+1], append([]string{want}, lines[i+1:]...)...)
		return strings.Join(lines, "\n")
	}

	if len(config) > 0 && !strings.HasSuffix(config, "\n") {
		config += "\n"
	}
	if config != "" {
		config += "\n"
	}
	return config + "[ui]\n" + want + "\n"
}
