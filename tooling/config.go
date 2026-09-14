package tooling

import (
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
)

// Config contains offline-only tooling inputs. Network schema discovery is not
// implied by a URL-shaped schema identifier.
type Config struct {
	Schema      string       `json:"schema,omitempty"`
	Diagnostics string       `json:"diagnostics,omitempty"`
	Plugin      PluginConfig `json:"plugin,omitempty"`
}

// PluginConfig describes an explicitly trusted executable. The core tooling
// package validates this boundary but never discovers or executes plugins.
type PluginConfig struct {
	Path    string `json:"path,omitempty"`
	ID      string `json:"id,omitempty"`
	Version string `json:"version,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
	Trusted bool   `json:"trusted,omitempty"`
}

// ResolveConfig applies the stable precedence flags > environment > project
// file > defaults. Empty fields do not erase lower-precedence values.
func ResolveConfig(defaults, file, environment, flags Config) Config {
	resolved := defaults
	mergeConfig(&resolved, file)
	mergeConfig(&resolved, environment)
	mergeConfig(&resolved, flags)
	return resolved
}

func mergeConfig(target *Config, source Config) {
	if source.Schema != "" {
		target.Schema = source.Schema
	}
	if source.Diagnostics != "" {
		target.Diagnostics = source.Diagnostics
	}
	if source.Plugin.Path != "" {
		target.Plugin = source.Plugin
	}
}

func (p PluginConfig) Validate() error {
	if p.Path == "" {
		return nil
	}
	if !p.Trusted {
		return errors.New("plugin execution requires explicit trust")
	}
	if !filepath.IsAbs(p.Path) {
		return errors.New("trusted plugin path must be absolute")
	}
	_, digestErr := hex.DecodeString(p.SHA256)
	if p.ID == "" || p.Version == "" || len(p.SHA256) != 64 || strings.ToLower(p.SHA256) != p.SHA256 || digestErr != nil {
		return errors.New("trusted plugin requires pinned id, version, and lowercase sha256")
	}
	return nil
}
