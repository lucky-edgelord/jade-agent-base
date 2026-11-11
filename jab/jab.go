// Package jab assembles Jade Agent Base: PocketBase plus modules. Every module in the build binds its hooks at
// start but stays dormant (no collections, tools or served routes) until it is switched on: by --modules, or by an
// agent at runtime with module_enable. What is on is kept in pb_data/jab.json.
package jab

import (
	"fmt"
	"sort"
	"strings"

	"github.com/lucky-edgelord/jade-agent-base/agent"
	"github.com/lucky-edgelord/jade-agent-base/billing"
	"github.com/lucky-edgelord/jade-agent-base/gateway"
	"github.com/lucky-edgelord/jade-agent-base/state"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/hook"
)

// Version of Jade Agent Base.
const Version = "0.6.0"

// Options for Setup.
type Options struct {
	Modules   []string // switch these on at start (and remember it); nil: as remembered, or DefaultModules
	HooksDir  string
	PublicDir string
}

// DefaultModules are on for a new server.
var DefaultModules = []string{"agent"}

// Module is a part of the server that can be switched on and off.
type Module struct {
	Name, Description string
	Always            bool                          // cannot be switched off
	Register          func(app core.App, o Options) // at start: bind hooks and routes (they check state.Enabled)
	Enable            func(app core.App) error      // make its collections, add its tools (idempotent)
	Disable           func(app core.App) error      // take its tools away (data is kept)
}

var modules = map[string]*Module{}

// Add makes a module available (modules in other packages call this before Setup).
func Add(m *Module) { modules[m.Name] = m }

func init() {
	Add(&Module{Name: "agent", Always: true, Description: "agents with scopes, the tool list over REST and MCP, the audit log"})
	Add(&Module{Name: "gateway", Description: "serve chat, embeddings, decisions, web search and fetch to signed-up users: providers, models, plans, API keys, metered usage (OpenAI and Ollama APIs)",
		Register: func(app core.App, o Options) { gateway.Register(app) },
		Enable:   gateway.Enable, Disable: gateway.Disable})
	Add(&Module{Name: "billing", Description: "charge for plans with Stripe: checkout, the customer portal, webhooks that move users between plans; paid plans need an active subscription (switches the gateway on too)",
		Register: func(app core.App, o Options) { billing.Register(app) },
		Enable: func(app core.App) error {
			if err := billing.Enable(app); err != nil {
				return err
			}
			return state.SetEnabled("gateway", true)
		},
		Disable: billing.Disable})
}

// Names of every module in this build.
func Names() []string {
	out := make([]string, 0, len(modules))
	for n := range modules {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ParseModules reads "agent,gateway" or "all"; "" gives nil (keep what is remembered).
func ParseModules(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if s == "all" {
		return Names(), nil
	}
	out := []string{}
	for _, n := range strings.Split(s, ",") {
		if n = strings.TrimSpace(n); n == "" {
			continue
		}
		if _, ok := modules[n]; !ok {
			return nil, fmt.Errorf("unknown module %q (modules: %s)", n, strings.Join(Names(), ", "))
		}
		out = append(out, n)
	}
	return out, nil
}

// Setup binds every module and switches on the chosen ones once the app has bootstrapped.
func Setup(app core.App, o Options) error {
	for _, n := range o.Modules {
		if _, ok := modules[n]; !ok {
			return fmt.Errorf("unknown module %q", n)
		}
	}
	agent.Register(app, agent.Config{HooksDir: o.HooksDir, PublicDir: o.PublicDir, Version: Version, Modules: Enabled})
	for _, n := range Names() {
		if m := modules[n]; m.Register != nil {
			m.Register(app, o)
		}
	}
	moduleTools()
	app.OnBootstrap().Bind(&hook.Handler[*core.BootstrapEvent]{
		Id:       "jab_modules",
		Priority: 100, // after the modules' own bootstrap hooks
		Func: func(e *core.BootstrapEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			remembered := state.Load(e.App.DataDir())
			switch {
			case o.Modules != nil:
				for _, n := range Names() {
					_ = state.SetEnabled(n, contains(o.Modules, n) || modules[n].Always)
				}
			case !remembered:
				for _, n := range DefaultModules {
					_ = state.SetEnabled(n, true)
				}
			}
			for _, n := range Names() {
				m := modules[n]
				if m.Always && !state.Enabled(n) {
					_ = state.SetEnabled(n, true)
				}
				if state.Enabled(n) && m.Enable != nil {
					if err := m.Enable(e.App); err != nil {
						return fmt.Errorf("module %s: %w", n, err)
					}
				}
			}
			return nil
		},
	})
	return nil
}

// Enabled lists the modules that are on.
func Enabled() []string {
	out := []string{}
	for _, n := range Names() {
		if state.Enabled(n) {
			out = append(out, n)
		}
	}
	return out
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// the tools that switch modules on and off
func moduleTools() {
	agent.AddTool(&agent.Tool{Name: "modules_list", Scope: agent.ScopeRead,
		Description: "The server's modules: what each does and whether it is on. Switch one on with module_enable when the business needs it.",
		Run: func(c *agent.Call) (any, error) {
			out := []map[string]any{}
			for _, n := range Names() {
				m := modules[n]
				out = append(out, map[string]any{"name": n, "on": state.Enabled(n), "description": m.Description, "always": m.Always})
			}
			return out, nil
		}})
	agent.AddTool(&agent.Tool{Name: "module_enable", Scope: agent.ScopeSettings,
		Description: "Switch a module on: it makes its collections and adds its tools (for you and other agents) right away.",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string", "description": "module name (see modules_list)"}}, "required": []string{"name"}},
		Run: func(c *agent.Call) (any, error) {
			n := c.Str("name")
			m, ok := modules[n]
			if !ok {
				return nil, fmt.Errorf("unknown module %q (modules: %s)", n, strings.Join(Names(), ", "))
			}
			if m.Enable != nil {
				if err := m.Enable(c.App); err != nil {
					return nil, err
				}
			}
			if err := state.SetEnabled(n, true); err != nil {
				return nil, err
			}
			return fmt.Sprintf("%s is on; its tools are available now (list them again)", n), nil
		}})
	agent.AddTool(&agent.Tool{Name: "module_disable", Scope: agent.ScopeSettings, Destructive: true,
		Description: "Switch a module off: its tools and routes stop; its data is kept for when it is switched on again.",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}, "required": []string{"name"}},
		Run: func(c *agent.Call) (any, error) {
			n := c.Str("name")
			m, ok := modules[n]
			if !ok {
				return nil, fmt.Errorf("unknown module %q", n)
			}
			if m.Always {
				return nil, fmt.Errorf("%s cannot be switched off", n)
			}
			if m.Disable != nil {
				if err := m.Disable(c.App); err != nil {
					return nil, err
				}
			}
			if err := state.SetEnabled(n, false); err != nil {
				return nil, err
			}
			return n + " is off (its data is kept)", nil
		}})
}
