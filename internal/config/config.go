// Package config loads profile-based configuration from
// ~/.punt-labs/mcp-proxy/<profile>.toml.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/BurntSushi/toml"
)

// validProfile matches safe profile names: letters, digits, hyphens, underscores.
// Dotted names and path separators are not allowed.
var validProfile = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// DefaultURL is the fallback daemon URL when no config or URL is provided.
const DefaultURL = "ws://localhost:8420/mcp"

// Profile holds the resolved configuration for a named profile.
type Profile struct {
	// URL is the WebSocket target for the daemon. Empty means use DefaultURL.
	URL string
	// Headers are additional HTTP headers to send on the WebSocket upgrade.
	Headers map[string]string
	// CACert is the path to a PEM-encoded CA certificate used to verify
	// TLS connections. Empty means use the system certificate pool.
	CACert string
}

// InsecurePermissionsError is returned when the config file has permissions
// that allow group or other access (any bit beyond owner read/write is set).
type InsecurePermissionsError struct {
	Path string
	Mode os.FileMode
}

func (e *InsecurePermissionsError) Error() string {
	return fmt.Sprintf("config file has insecure permissions (%04o, permissions must be 0600 or more restrictive): %s", e.Mode, e.Path)
}

// Load reads the profile from ~/.punt-labs/mcp-proxy/<profile>.toml.
//
// If the file does not exist, or contains no [<profile>] section, the returned
// Profile has an empty URL (caller should fall back to DefaultURL).
//
// If the file exists but has permissions wider than 0600, an
// InsecurePermissionsError is returned.
func Load(profile string) (Profile, error) {
	if !validProfile.MatchString(profile) {
		return Profile{}, fmt.Errorf("invalid profile name %q: only letters, digits, hyphens, and underscores are allowed", profile)
	}

	path, err := profilePath(profile)
	if err != nil {
		return Profile{}, fmt.Errorf("resolving config path: %w", err)
	}

	// Open once; stat on the fd to eliminate TOCTOU between permission check
	// and read.
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// File absent — silent fallback.
			return Profile{}, nil
		}
		return Profile{}, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return Profile{}, fmt.Errorf("stat %s: %w", path, err)
	}

	if !info.Mode().IsRegular() {
		return Profile{}, fmt.Errorf("config path is not a regular file: %s", path)
	}

	if perm := info.Mode().Perm(); perm&^0o600 != 0 {
		return Profile{}, &InsecurePermissionsError{Path: tilde(path), Mode: perm}
	}

	var raw map[string]toml.Primitive
	meta, err := toml.NewDecoder(f).Decode(&raw)
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
		CACert  string            `toml:"ca_cert"`
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
