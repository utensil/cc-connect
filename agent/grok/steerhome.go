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
	// Never point the overlay at the real home: that would rewrite the user's
	// config (the one thing this design promises not to touch) and the stale
	// entry replacement below could remove real entries.
	realAbs, err := filepath.Abs(realHome)
	if err != nil {
		return "", fmt.Errorf("grok: resolve %q: %w", realHome, err)
	}
	overlayAbs, err := filepath.Abs(overlayDir)
	if err != nil {
		return "", fmt.Errorf("grok: resolve %q: %w", overlayDir, err)
	}
	if realAbs == overlayAbs {
		return "", fmt.Errorf("grok: steer home must differ from GROK_HOME (%q)", overlayAbs)
	}
	overlayDir = overlayAbs
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
				// A stale symlink: drop it and re-link below.
				if rerr := os.Remove(link); rerr != nil {
					return fmt.Errorf("grok: replace %q: %w", link, rerr)
				}
			} else if existing.IsDir() {
				entries, derr := os.ReadDir(link)
				if derr != nil {
					return fmt.Errorf("grok: inspect %q: %w", link, derr)
				}
				if len(entries) > 0 {
					// The CLI wrote state into the overlay before this entry
					// existed in the real home. Refuse to destroy it: the operator
					// has to look.
					return fmt.Errorf("grok: overlay entry %q is a non-empty directory; move or remove it before reusing this steer home", link)
				}
				if rerr := os.Remove(link); rerr != nil {
					return fmt.Errorf("grok: replace %q: %w", link, rerr)
				}
			} else {
				if rerr := os.Remove(link); rerr != nil {
					return fmt.Errorf("grok: replace %q: %w", link, rerr)
				}
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
	// Write through a temp file so a concurrent session never reads a half
	// written config.
	tmp, err := os.CreateTemp(filepath.Dir(dstPath), ".config-*.toml")
	if err != nil {
		return fmt.Errorf("grok: create temp config for %q: %w", dstPath, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(desired); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("grok: write %q: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("grok: close %q: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("grok: chmod %q: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, dstPath); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("grok: install %q: %w", dstPath, err)
	}
	return nil
}

// injectFollowUpSteer returns the config text with mid-turn follow-ups set to
// steer. Only `[ui] follow_up_behavior` is touched: the rest of the file,
// including comments, keeps its original text, and a same-named key in another
// table is left alone.
func injectFollowUpSteer(config string) string {
	const key = "follow_up_behavior"
	const want = `follow_up_behavior = "steer"`

	lines := strings.Split(config, "\n")
	table := ""
	uiSection := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if name, ok := tomlTableName(trimmed); ok {
			table = name
			if name == "ui" && uiSection < 0 {
				uiSection = i
			}
			continue
		}
		if table != "ui" {
			continue
		}
		indent, comment, ok := matchFollowUpLine(line, key)
		if !ok {
			continue
		}
		lines[i] = indent + want + comment
		return strings.Join(lines, "\n")
	}

	if uiSection >= 0 {
		lines = append(lines[:uiSection+1], append([]string{want}, lines[uiSection+1:]...)...)
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

// tomlTableName reports the table declared by a header line such as `[ui]` or
// `[[projects]]`.
func tomlTableName(trimmed string) (string, bool) {
	if !strings.HasPrefix(trimmed, "[") {
		return "", false
	}
	end := strings.IndexByte(trimmed, ']')
	if end < 0 {
		return "", false
	}
	name := strings.Trim(trimmed[1:end], "[] ")
	if name == "" {
		return "", false
	}
	return name, true
}

// matchFollowUpLine splits one `follow_up_behavior = <value>` assignment into
// its indentation and trailing comment, so the value can be replaced without
// dropping either.
func matchFollowUpLine(line, key string) (indent, comment string, ok bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, key) {
		return "", "", false
	}
	rest := trimmed[len(key):]
	if !strings.HasPrefix(strings.TrimSpace(rest), "=") {
		return "", "", false
	}
	eq := strings.IndexByte(line, '=')
	if eq < 0 {
		return "", "", false
	}
	indent = line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	if hash := strings.IndexByte(line[eq:], '#'); hash >= 0 {
		// Keep the original whitespace and text of the trailing comment.
		start := eq + hash
		for start > eq && (line[start-1] == ' ' || line[start-1] == '\t') {
			start--
		}
		comment = line[start:]
	}
	return indent, comment, true
}
