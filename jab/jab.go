// Package jab assembles Jade Agent Base: PocketBase plus modules, each switched on by name. A module that is off
// registers nothing (no collections, routes or tools), so a server carries only what it uses.
package jab

import (
	"fmt"
	"sort"
	"strings"

	"github.com/lucky-edgelord/jade-agent-base/agent"
	"github.com/pocketbase/pocketbase/core"
)

// Version of Jade Agent Base.
const Version = "0.1.0"

// Options for Setup.
type Options struct {
	Modules   []string // module names; empty means DefaultModules
	HooksDir  string
	PublicDir string
}

// DefaultModules are on unless --modules says otherwise.
var DefaultModules = []string{"agent"}

// module registers itself on the app
type module func(app core.App, o Options)

var modules = map[string]module{
	"agent": func(app core.App, o Options) {
		agent.Register(app, agent.Config{HooksDir: o.HooksDir, PublicDir: o.PublicDir, Version: Version, Modules: o.Modules})
	},
}

// Register adds a module (for modules in other packages, called before Setup).
func Register(name string, m func(app core.App, o Options)) { modules[name] = m }

// Names of every module this build has.
func Names() []string {
	out := make([]string, 0, len(modules))
	for n := range modules {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ParseModules reads "agent,gateway" (or "all").
func ParseModules(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return DefaultModules, nil
	}
	if s == "all" {
		return Names(), nil
	}
	out := []string{}
	for _, n := range strings.Split(s, ",") {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if _, ok := modules[n]; !ok {
			return nil, fmt.Errorf("unknown module %q (modules: %s)", n, strings.Join(Names(), ", "))
		}
		out = append(out, n)
	}
	return out, nil
}

// Setup switches on the modules in o.Modules.
func Setup(app core.App, o Options) error {
	if len(o.Modules) == 0 {
		o.Modules = DefaultModules
	}
	for _, n := range o.Modules {
		m, ok := modules[n]
		if !ok {
			return fmt.Errorf("unknown module %q", n)
		}
		m(app, o)
	}
	return nil
}
