// Package state keeps the server's own configuration in pb_data/jab.json: which modules are on, and each module's
// settings (for example which gateway capabilities are served). Agents change it at runtime; it survives restarts.
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

type file struct {
	Modules  map[string]bool           `json:"modules"`
	Sections map[string]map[string]any `json:"settings"`
}

var (
	mu   sync.RWMutex
	path string
	f    = file{Modules: map[string]bool{}, Sections: map[string]map[string]any{}}
)

// Load reads <dataDir>/jab.json (missing is fine). It reports whether the file existed.
func Load(dataDir string) bool {
	mu.Lock()
	defer mu.Unlock()
	path = filepath.Join(dataDir, "jab.json")
	f = file{Modules: map[string]bool{}, Sections: map[string]map[string]any{}}
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	_ = json.Unmarshal(b, &f)
	if f.Modules == nil {
		f.Modules = map[string]bool{}
	}
	if f.Sections == nil {
		f.Sections = map[string]map[string]any{}
	}
	return true
}

func save() error {
	if path == "" {
		return nil
	}
	b, _ := json.MarshalIndent(f, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Enabled reports whether a module is on.
func Enabled(module string) bool {
	mu.RLock()
	defer mu.RUnlock()
	return f.Modules[module]
}

// SetEnabled turns a module on or off (and saves).
func SetEnabled(module string, on bool) error {
	mu.Lock()
	defer mu.Unlock()
	f.Modules[module] = on
	return save()
}

// Get returns a module setting, or def when it is not set.
func Get(section, key string, def any) any {
	mu.RLock()
	defer mu.RUnlock()
	if v, ok := f.Sections[section][key]; ok {
		return v
	}
	return def
}

// Bool is Get for booleans.
func Bool(section, key string, def bool) bool {
	b, ok := Get(section, key, def).(bool)
	if !ok {
		return def
	}
	return b
}

// Set changes module settings (and saves).
func Set(section string, values map[string]any) error {
	mu.Lock()
	defer mu.Unlock()
	if f.Sections[section] == nil {
		f.Sections[section] = map[string]any{}
	}
	for k, v := range values {
		f.Sections[section][k] = v
	}
	return save()
}

// Section is a copy of a module's settings.
func Section(section string) map[string]any {
	mu.RLock()
	defer mu.RUnlock()
	out := map[string]any{}
	for k, v := range f.Sections[section] {
		out[k] = v
	}
	return out
}
