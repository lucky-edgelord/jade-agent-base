package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/pocketbase/pocketbase/core"
)

// Scopes an agent can hold. A tool needs one; superusers hold all of them.
const (
	ScopeRead     = "read"     // read collections, records, logs, hooks and site files
	ScopeWrite    = "write"    // create, update and delete records
	ScopeSchema   = "schema"   // create, change and delete collections
	ScopeHooks    = "hooks"    // write server-side JavaScript (pb_hooks)
	ScopeSite     = "site"     // write the static site (pb_public)
	ScopeSettings = "settings" // change the app settings
)

// AllScopes in a stable order. Modules may add their own with AddScope.
var AllScopes = []string{ScopeRead, ScopeWrite, ScopeSchema, ScopeHooks, ScopeSite, ScopeSettings}

// Call is what a tool runs with.
type Call struct {
	App    core.App
	Actor  *core.Record // an agent record, or a superuser
	Scopes map[string]bool
	Args   map[string]any
}

// Tool is something an agent can do. Parameters is a JSON schema object.
type Tool struct {
	Name        string
	Description string
	Scope       string
	Parameters  map[string]any
	// Destructive tools are marked for MCP clients (destructiveHint) and in the tool list.
	Destructive bool
	Run         func(c *Call) (any, error)
}

var (
	mu    sync.RWMutex
	tools = map[string]*Tool{}
)

// AddTool registers a tool (modules call this when they are enabled). A later tool with the same name replaces it.
func AddTool(t *Tool) {
	if t.Parameters == nil {
		t.Parameters = obj(nil)
	}
	mu.Lock()
	defer mu.Unlock()
	tools[t.Name] = t
}

// RemoveTool takes tools away (when their module is switched off).
func RemoveTool(names ...string) {
	mu.Lock()
	defer mu.Unlock()
	for _, n := range names {
		delete(tools, n)
	}
}

// ToolNames lists every registered tool.
func ToolNames() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(tools))
	for n := range tools {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// AddScope makes a scope known (for modules with their own scopes).
func AddScope(s string) {
	mu.Lock()
	defer mu.Unlock()
	for _, x := range AllScopes {
		if x == s {
			return
		}
	}
	AllScopes = append(AllScopes, s)
}

// Tools returns the tools the scopes allow, sorted by name.
func Tools(scopes map[string]bool) []*Tool {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]*Tool, 0, len(tools))
	for _, t := range tools {
		if scopes[t.Scope] {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func getTool(name string) *Tool {
	mu.RLock()
	defer mu.RUnlock()
	return tools[name]
}

// ErrScope is returned when the caller lacks the tool's scope.
var ErrScope = errors.New("not allowed")

// Run runs a tool by name as the caller and records it in the audit log.
func Run(app core.App, actor *core.Record, scopes map[string]bool, name string, args map[string]any) (any, error) {
	t := getTool(name)
	if t == nil {
		return nil, fmt.Errorf("unknown tool %q", name)
	}
	if !scopes[t.Scope] {
		return nil, fmt.Errorf("%w: %s needs the %q scope", ErrScope, name, t.Scope)
	}
	if args == nil {
		args = map[string]any{}
	}
	if err := checkArgs(t, args); err != nil {
		audit(app, actor, name, args, nil, err)
		return nil, err
	}
	var out any
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%s failed: %v", name, r)
			}
		}()
		out, err = t.Run(&Call{App: app, Actor: actor, Scopes: scopes, Args: args})
		return err
	}()
	audit(app, actor, name, args, out, err)
	return out, err
}

// required arguments and basic types, so a model gets a clear message
func checkArgs(t *Tool, args map[string]any) error {
	props, _ := t.Parameters["properties"].(map[string]any)
	req, _ := t.Parameters["required"].([]string)
	for _, r := range req {
		if v, ok := args[r]; !ok || v == nil || v == "" {
			return fmt.Errorf("%s needs %q", t.Name, r)
		}
	}
	for k, v := range args {
		p, ok := props[k].(map[string]any)
		if !ok {
			names := make([]string, 0, len(props))
			for n := range props {
				names = append(names, n)
			}
			sort.Strings(names)
			return fmt.Errorf("%s has no argument %q (arguments: %s)", t.Name, k, strings.Join(names, ", "))
		}
		want, _ := p["type"].(string)
		if !typeOK(want, v) {
			return fmt.Errorf("%s: %q must be %s", t.Name, k, want)
		}
	}
	return nil
}

func typeOK(want string, v any) bool {
	switch want {
	case "string":
		_, ok := v.(string)
		return ok
	case "integer", "number":
		_, ok := v.(float64)
		if !ok {
			_, ok = v.(json.Number)
		}
		if !ok {
			_, ok = v.(int)
		}
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	}
	return true
}

// ---- small helpers for tool definitions and arguments ----

func obj(props map[string]any, required ...string) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	o := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		o["required"] = required
	}
	return o
}
func str(desc string) map[string]any  { return map[string]any{"type": "string", "description": desc} }
func num(desc string) map[string]any  { return map[string]any{"type": "integer", "description": desc} }
func boo(desc string) map[string]any  { return map[string]any{"type": "boolean", "description": desc} }
func objp(desc string) map[string]any { return map[string]any{"type": "object", "description": desc} }
func arr(desc string, items map[string]any) map[string]any {
	return map[string]any{"type": "array", "description": desc, "items": items}
}

func (c *Call) Str(k string) string {
	s, _ := c.Args[k].(string)
	return s
}
func (c *Call) Int(k string, def int) int {
	switch v := c.Args[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		n, err := v.Int64()
		if err == nil {
			return int(n)
		}
	}
	return def
}
func (c *Call) Bool(k string) bool {
	b, _ := c.Args[k].(bool)
	return b
}
func (c *Call) Map(k string) map[string]any {
	m, _ := c.Args[k].(map[string]any)
	return m
}
func (c *Call) List(k string) []any {
	l, _ := c.Args[k].([]any)
	return l
}
