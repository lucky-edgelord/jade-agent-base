// Package agent lets AI agents run a Jade Agent Base: a tool list over REST and MCP, agent identities with scopes,
// and an audit log of everything they do.
//
//	GET  /api/agent/tools            the tools the caller may use (?format=openai|anthropic|mcp)
//	POST /api/agent/tools/{name}     run one: the body is its arguments → {"result": …} or {"error": "…"}
//	POST /api/agent/mcp              Model Context Protocol (streamable HTTP, JSON responses)
//	POST /api/agent/agents           (superusers) create an agent → {"agent", "token"}
//	POST /api/agent/agents/{id}/token   (superusers) a new token; …/revoke ends all of an agent's tokens
//
// Agents authenticate like any PocketBase auth record: Authorization: <token>. Superusers may call everything.
package agent

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/hook"
)

const (
	AgentsCollection = "agents"
	AuditCollection  = "agent_audit"
)

// Config for the agent module.
type Config struct {
	HooksDir  string // server-side JavaScript (default: pb_hooks next to the data dir)
	PublicDir string // the static site (default: pb_public next to the data dir)
	Version   string // reported by server_info
	Modules   []string
	// Audit keeps a record of every tool call in agent_audit (default true).
	NoAudit bool
}

var cfg Config

// Register adds the agent module to an app: its collections, routes and the core tools.
func Register(app core.App, c Config) {
	cfg = c
	app.OnBootstrap().Bind(&hook.Handler[*core.BootstrapEvent]{
		Id: "jab_agent_bootstrap",
		Func: func(e *core.BootstrapEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			if cfg.HooksDir == "" {
				cfg.HooksDir = filepath.Join(e.App.DataDir(), "..", "pb_hooks")
			}
			if cfg.PublicDir == "" {
				cfg.PublicDir = filepath.Join(e.App.DataDir(), "..", "pb_public")
			}
			return ensureCollections(e.App)
		},
	})
	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id:   "jab_agent_routes",
		Func: func(e *core.ServeEvent) error { routes(e); return e.Next() },
	})
	coreTools()
}

// HooksDir and PublicDir as resolved after bootstrap.
func HooksDir() string  { return cfg.HooksDir }
func PublicDir() string { return cfg.PublicDir }

func ensureCollections(app core.App) error {
	if _, err := app.FindCollectionByNameOrId(AgentsCollection); err != nil {
		c := core.NewAuthCollection(AgentsCollection)
		c.Fields.Add(
			&core.TextField{Name: "name", Required: true, Max: 100},
			&core.SelectField{Name: "scopes", Values: AllScopes, MaxSelect: len(AllScopes)},
			&core.TextField{Name: "description", Max: 2000},
		)
		c.PasswordAuth.Enabled = false // agents use tokens a superuser issues
		c.AuthToken.Duration = 60 * 60 * 24 * 30
		if err := app.Save(c); err != nil {
			return fmt.Errorf("create %s: %w", AgentsCollection, err)
		}
	} else if c, err := app.FindCollectionByNameOrId(AgentsCollection); err == nil {
		// modules may have added scopes since the collection was made
		if f, ok := c.Fields.GetByName("scopes").(*core.SelectField); ok && len(f.Values) < len(AllScopes) {
			f.Values = AllScopes
			f.MaxSelect = len(AllScopes)
			if err := app.Save(c); err != nil {
				return err
			}
		}
	}
	if _, err := app.FindCollectionByNameOrId(AuditCollection); err != nil {
		c := core.NewBaseCollection(AuditCollection)
		c.Fields.Add(
			&core.TextField{Name: "actor"},
			&core.TextField{Name: "actor_name"},
			&core.TextField{Name: "tool", Required: true},
			&core.JSONField{Name: "args", MaxSize: 1 << 20},
			&core.BoolField{Name: "ok"},
			&core.TextField{Name: "error", Max: 5000},
			&core.AutodateField{Name: "created", OnCreate: true},
		)
		c.AddIndex("idx_agent_audit_created", false, "created", "")
		if err := app.Save(c); err != nil {
			return fmt.Errorf("create %s: %w", AuditCollection, err)
		}
	}
	return nil
}

// Caller resolves who is calling and what they may do: an agent with its scopes, or a superuser with all of them.
func Caller(e *core.RequestEvent) (*core.Record, map[string]bool, error) {
	if e.Auth == nil {
		return nil, nil, errors.New("sign in as an agent or a superuser (Authorization: <token>)")
	}
	scopes := map[string]bool{}
	if e.Auth.IsSuperuser() {
		for _, s := range AllScopes {
			scopes[s] = true
		}
		return e.Auth, scopes, nil
	}
	if e.Auth.Collection().Name != AgentsCollection {
		return nil, nil, errors.New("only agents and superusers can use the agent API")
	}
	for _, s := range e.Auth.GetStringSlice("scopes") {
		scopes[s] = true
	}
	return e.Auth, scopes, nil
}

var secretKey = regexp.MustCompile(`(?i)pass|secret|token|key|auth`)

func redact(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if secretKey.MatchString(k) {
				out[k] = "[redacted]"
			} else {
				out[k] = redact(val)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = redact(val)
		}
		return out
	case string:
		if len(x) > 2000 {
			return x[:2000] + "…"
		}
	}
	return v
}

func audit(app core.App, actor *core.Record, tool string, args map[string]any, _ any, err error) {
	if cfg.NoAudit || app == nil {
		return
	}
	col, e := app.FindCachedCollectionByNameOrId(AuditCollection)
	if e != nil {
		return
	}
	r := core.NewRecord(col)
	if actor != nil {
		r.Set("actor", actor.Collection().Name+"/"+actor.Id)
		if actor.IsSuperuser() {
			r.Set("actor_name", actor.GetString("email"))
		} else {
			r.Set("actor_name", actor.GetString("name"))
		}
	}
	r.Set("tool", tool)
	r.Set("args", redact(args))
	r.Set("ok", err == nil)
	if err != nil {
		r.Set("error", err.Error())
	}
	if e := app.Save(r); e != nil {
		app.Logger().Warn("agent audit", "error", e)
	}
}

// CreateAgent makes an agent with scopes and returns it with a token valid for `days` (0: 30 days).
func CreateAgent(app core.App, name string, scopes []string, description string, days int) (*core.Record, string, error) {
	col, err := app.FindCollectionByNameOrId(AgentsCollection)
	if err != nil {
		return nil, "", err
	}
	for _, s := range scopes {
		if !knownScope(s) {
			return nil, "", fmt.Errorf("unknown scope %q (scopes: %s)", s, strings.Join(AllScopes, ", "))
		}
	}
	r := core.NewRecord(col)
	r.Set("name", name)
	r.Set("scopes", scopes)
	r.Set("description", description)
	r.SetEmail(slug(name) + "-" + randHex(4) + "@agents.invalid")
	r.SetPassword(randHex(24))
	r.SetVerified(true)
	if err := app.Save(r); err != nil {
		return nil, "", err
	}
	tok, err := Token(r, days)
	return r, tok, err
}

// Token issues a token for an agent valid for `days` (0: 30 days).
func Token(r *core.Record, days int) (string, error) {
	if days <= 0 {
		days = 30
	}
	return r.NewStaticAuthToken(time.Duration(days) * 24 * time.Hour)
}

// Revoke ends every token an agent holds.
func Revoke(app core.App, r *core.Record) error {
	r.RefreshTokenKey()
	return app.Save(r)
}

func knownScope(s string) bool {
	for _, x := range AllScopes {
		if x == s {
			return true
		}
	}
	return false
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	s = strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if s == "" {
		s = "agent"
	}
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

func httpStatus(err error) int {
	if errors.Is(err, ErrScope) {
		return http.StatusForbidden
	}
	return http.StatusBadRequest
}
