// Package config loads profile-based configuration from
// ~/.punt-labs/mcp-proxy/<profile>.toml.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// DefaultURL is the fallback daemon URL when no config or URL is provided.
const DefaultURL = "ws://localhost:8420/mcp"

// Profile holds the resolved configuration for a named profile.
type Profile struct {
	// URL is the WebSocket target for the daemon. Empty means use DefaultURL.
	URL string
	// Headers are additional HTTP headers to send on the WebSocket upgrade.
	Headers map[string]string
}

// InsecurePermissionsError is returned when the config file has permissions
// wider than 0600.
type InsecurePermissionsError struct {
	Path string
}

func (e *InsecurePermissionsError) Error() string {
	return fmt.Sprintf("config file has insecure permissions (expected 0600): %s", e.Path)
}

// Load reads the profile from ~/.punt-labs/mcp-proxy/<profile>.toml.
//
// If the file does not exist, or contains no [<profile>] section, the returned
// Profile has an empty URL (caller should fall back to DefaultURL).
//
// If the file exists but has permissions wider than 0600, an
// InsecurePermissionsError is returned.
func Load(profile string) (Profile, error) {
	path, err := profilePath(profile)
	if err != nil {
		return Profile{}, fmt.Errorf("resolving config path: %w", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// File absent — silent fallback.
			return Profile{}, nil
		}
		return Profile{}, fmt.Errorf("stat %s: %w", path, err)
	}

	if perm := info.Mode().Perm(); perm != 0o600 {
		return Profile{}, &InsecurePermissionsError{Path: tilde(path)}
	}

	var raw map[string]toml.Primitive
	meta, err := toml.DecodeFile(path, &raw)
	if err != nil {
		return Profile{}, fmt.Errorf("parsing %s: %w", path, err)
	}

	prim, ok := raw[profile]
	if !ok {
		// Section absent — silent fallback.
		return Profile{}, nil
	}

	// Decode the section directly into Profile.
	type wireProfile struct {
		URL     string            `toml:"url"`
		Headers map[string]string `toml:"headers"`
	}
	var wp wireProfile
	if err := meta.PrimitiveDecode(prim, &wp); err != nil {
		return Profile{}, fmt.Errorf("decoding [%s] section: %w", profile, err)
	}

	return Profile(wp), nil
}

// profilePath returns the absolute path for a profile's config file.
func profilePath(profile string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".punt-labs", "mcp-proxy", profile+".toml"), nil
}

// tilde replaces the home directory prefix with "~" for display in error
// messages.
func tilde(path string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	rel, err := filepath.Rel(home, path)
	if err != nil {
		return path
	}
	return "~/" + rel
}
