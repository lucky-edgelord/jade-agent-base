// Package gateway serves models to signed-up users: chat, embeddings, decisions and web search through one API, with
// API keys, plans (quotas and rate limits) and metered usage. Clients speak OpenAI's API (/v1/…) or Ollama's (/api/chat,
// /api/embed, …); upstreams are Ollama (on a machine or Ollama Cloud), Ollaya, anything OpenAI-compatible, Anthropic
// and Google. An agent with the "gateway" scope runs it: providers, models, prices, plans, keys and usage.
//
// Collections: providers (superusers only), models and plans (the public catalog), api_keys and usage (each user sees
// their own), and a plan field on users.
package gateway

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/lucky-edgelord/jade-agent-base/agent"
	"github.com/lucky-edgelord/jade-agent-base/state"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/hook"
	"github.com/pocketbase/pocketbase/tools/types"
)

// Provider types and the address each uses when none is set.
var Defaults = map[string]string{
	"ollama":       "http://localhost:11434",
	"ollama-cloud": "https://ollama.com",
	"ollaya":       "http://localhost:11435",
	"openai":       "https://api.openai.com/v1",
	"anthropic":    "https://api.anthropic.com",
	"google":       "https://generativelanguage.googleapis.com/v1beta",
}

// Kinds of models.
var Kinds = []string{"chat", "embed", "decide", "search", "fetch"}

var upstreams = map[string]Upstream{
	"ollama": ollamaUp{}, "ollama-cloud": ollamaUp{}, "openai": openaiUp{}, "anthropic": anthropicUp{}, "google": googleUp{},
}

// Check runs before every request (after the plan's limits). Billing adds a check for paid credit here.
var Check []func(app core.App, a *Account, model *core.Record) error

// Account is who is calling.
type Account struct {
	User *core.Record // a users record (nil for superusers)
	Key  *core.Record // the API key, when one was used
	Plan *core.Record // the user's plan (nil: no limits, for superusers)
}

// Scope agents need to run the gateway.
const Scope = "gateway"

// Features: each capability can be served or not (state section "gateway"). All are on unless switched off.
var Features = []string{"chat", "embed", "decide", "search", "fetch", "openai_api", "ollama_api"}

// On reports whether the gateway serves a feature right now.
func On(feature string) bool {
	return state.Enabled("gateway") && state.Bool("gateway", feature, true)
}

// Register binds the gateway's routes at start. They answer only while the module (and the feature) is on.
func Register(app core.App) {
	agent.AddScope(Scope)
	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id:   "jab_gateway_routes",
		Func: func(e *core.ServeEvent) error { routes(e); return e.Next() },
	})
}

// Enable makes the gateway's collections and adds its tools.
func Enable(app core.App) error {
	if err := ensureCollections(app); err != nil {
		return err
	}
	agentTools()
	return nil
}

// Disable takes the gateway's tools away (its data stays).
func Disable(app core.App) error {
	agent.RemoveTool(toolNames...)
	return nil
}

func ptr(s string) *string { return types.Pointer(s) }

func ensureCollections(app core.App) error {
	find := func(n string) *core.Collection { c, _ := app.FindCollectionByNameOrId(n); return c }
	if find("providers") == nil {
		c := core.NewBaseCollection("providers")
		c.Fields.Add(
			&core.TextField{Name: "name", Required: true, Max: 60},
			&core.SelectField{Name: "type", Required: true, Values: keys(Defaults), MaxSelect: 1},
			&core.TextField{Name: "base_url", Max: 500},
			&core.TextField{Name: "api_key", Hidden: true, Max: 4000},
			&core.JSONField{Name: "headers", MaxSize: 20000},
			&core.NumberField{Name: "timeout", Min: types.Pointer(0.0)},
			&core.BoolField{Name: "enabled"},
		)
		c.AddIndex("idx_providers_name", true, "name", "")
		if err := app.Save(c); err != nil {
			return fmt.Errorf("providers: %w", err)
		}
	}
	if find("plans") == nil {
		c := core.NewBaseCollection("plans")
		c.ListRule, c.ViewRule = ptr("active = true"), ptr("active = true")
		c.Fields.Add(
			&core.TextField{Name: "name", Required: true, Max: 60},
			&core.TextField{Name: "description", Max: 2000},
			&core.NumberField{Name: "price_month", Min: types.Pointer(0.0)},
			&core.NumberField{Name: "monthly_budget", Min: types.Pointer(0.0)},
			&core.NumberField{Name: "monthly_tokens", Min: types.Pointer(0.0), OnlyInt: true},
			&core.NumberField{Name: "rpm", Min: types.Pointer(0.0), OnlyInt: true},
			&core.BoolField{Name: "default"},
			&core.BoolField{Name: "active"},
			&core.TextField{Name: "stripe_price", Max: 200},
		)
		c.AddIndex("idx_plans_name", true, "name", "")
		if err := app.Save(c); err != nil {
			return fmt.Errorf("plans: %w", err)
		}
		free := core.NewRecord(c)
		free.Load(map[string]any{"name": "free", "description": "Try it out", "monthly_tokens": 200000, "rpm": 20, "default": true, "active": true})
		if err := app.Save(free); err != nil {
			return err
		}
	}
	plans, providers := find("plans"), find("providers")
	if find("models") == nil {
		c := core.NewBaseCollection("models")
		c.ListRule, c.ViewRule = ptr("enabled = true"), ptr("enabled = true")
		c.Fields.Add(
			&core.TextField{Name: "name", Required: true, Max: 200},
			&core.RelationField{Name: "provider", CollectionId: providers.Id, MaxSelect: 1, Required: true},
			&core.TextField{Name: "upstream", Max: 200},
			&core.SelectField{Name: "kind", Required: true, Values: Kinds, MaxSelect: 1},
			&core.NumberField{Name: "price_in", Min: types.Pointer(0.0)},
			&core.NumberField{Name: "price_out", Min: types.Pointer(0.0)},
			&core.NumberField{Name: "price_call", Min: types.Pointer(0.0)},
			&core.RelationField{Name: "plans", CollectionId: plans.Id, MaxSelect: 100},
			&core.BoolField{Name: "enabled"},
			&core.TextField{Name: "description", Max: 2000},
			&core.NumberField{Name: "context", Min: types.Pointer(0.0), OnlyInt: true},
		)
		c.AddIndex("idx_models_name", true, "name", "")
		if err := app.Save(c); err != nil {
			return fmt.Errorf("models: %w", err)
		}
	}
	users := find("users")
	if users == nil {
		users = core.NewAuthCollection("users")
		users.ListRule, users.ViewRule, users.UpdateRule = ptr("id = @request.auth.id"), ptr("id = @request.auth.id"), ptr("id = @request.auth.id")
		users.CreateRule = ptr("")
		if err := app.Save(users); err != nil {
			return fmt.Errorf("users: %w", err)
		}
	}
	if users.Fields.GetByName("plan") == nil {
		users.Fields.Add(&core.RelationField{Name: "plan", CollectionId: plans.Id, MaxSelect: 1})
		// people may not change their own plan (billing and agents do)
		if users.UpdateRule != nil && !strings.Contains(*users.UpdateRule, "@request.body.plan") {
			users.UpdateRule = ptr("(" + *users.UpdateRule + ") && @request.body.plan:isset = false")
		}
		if users.CreateRule != nil && !strings.Contains(*users.CreateRule, "@request.body.plan") {
			r := *users.CreateRule
			if r == "" {
				users.CreateRule = ptr("@request.body.plan:isset = false")
			} else {
				users.CreateRule = ptr("(" + r + ") && @request.body.plan:isset = false")
			}
		}
		if err := app.Save(users); err != nil {
			return fmt.Errorf("users.plan: %w", err)
		}
	}
	if find("api_keys") == nil {
		c := core.NewBaseCollection("api_keys")
		own := ptr("user = @request.auth.id")
		c.ListRule, c.ViewRule, c.DeleteRule = own, own, own
		c.Fields.Add(
			&core.RelationField{Name: "user", CollectionId: users.Id, MaxSelect: 1, Required: true, CascadeDelete: true},
			&core.TextField{Name: "name", Max: 100},
			&core.TextField{Name: "prefix", Max: 20},
			&core.TextField{Name: "hash", Hidden: true, Max: 100},
			&core.DateField{Name: "last_used"},
			&core.BoolField{Name: "revoked"},
			&core.AutodateField{Name: "created", OnCreate: true},
		)
		c.AddIndex("idx_api_keys_prefix", true, "prefix", "")
		if err := app.Save(c); err != nil {
			return fmt.Errorf("api_keys: %w", err)
		}
	}
	if find("usage") == nil {
		c := core.NewBaseCollection("usage")
		c.ListRule, c.ViewRule = ptr("user = @request.auth.id"), ptr("user = @request.auth.id")
		c.Fields.Add(
			&core.RelationField{Name: "user", CollectionId: users.Id, MaxSelect: 1},
			&core.TextField{Name: "key", Max: 20},
			&core.TextField{Name: "model", Max: 200},
			&core.TextField{Name: "kind", Max: 20},
			&core.NumberField{Name: "tokens_in", OnlyInt: true},
			&core.NumberField{Name: "tokens_out", OnlyInt: true},
			&core.NumberField{Name: "cost"},
			&core.NumberField{Name: "ms", OnlyInt: true},
			&core.NumberField{Name: "status", OnlyInt: true},
			&core.TextField{Name: "error", Max: 2000},
			&core.AutodateField{Name: "created", OnCreate: true},
		)
		c.AddIndex("idx_usage_user_created", false, "user, created", "")
		if err := app.Save(c); err != nil {
			return fmt.Errorf("usage: %w", err)
		}
	}
	return nil
}

func keys(m map[string]string) []string {
	out := []string{"ollama", "ollama-cloud", "ollaya", "openai", "anthropic", "google"}
	for k := range m {
		found := false
		for _, o := range out {
			found = found || o == k
		}
		if !found {
			out = append(out, k)
		}
	}
	return out
}

// ---- API keys ----

// NewKey makes an API key for a user → the key (shown once) and its record.
func NewKey(app core.App, user *core.Record, name string) (string, *core.Record, error) {
	col, err := app.FindCollectionByNameOrId("api_keys")
	if err != nil {
		return "", nil, err
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	key := "jab_" + hex.EncodeToString(b)
	r := core.NewRecord(col)
	r.Set("user", user.Id)
	r.Set("name", name)
	r.Set("prefix", key[:16])
	r.Set("hash", hashKey(key))
	if err := app.Save(r); err != nil {
		return "", nil, err
	}
	return key, r, nil
}

func hashKey(k string) string { s := sha256.Sum256([]byte(k)); return hex.EncodeToString(s[:]) }

// errors that carry the HTTP status for clients
type statusErr struct {
	code int
	msg  string
}

func (e *statusErr) Error() string { return e.msg }
func fail(code int, f string, a ...any) error {
	return &statusErr{code, fmt.Sprintf(f, a...)}
}

// authenticate: an API key (Authorization: Bearer jab_…) or a signed-in user's token
func authenticate(e *core.RequestEvent) (*Account, error) {
	h := strings.TrimSpace(e.Request.Header.Get("Authorization"))
	if h == "" {
		h = strings.TrimSpace(e.Request.Header.Get("x-api-key"))
	}
	tok := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(h, "Bearer "), "bearer "))
	app := e.App
	if strings.HasPrefix(tok, "jab_") && len(tok) > 16 {
		k, err := app.FindFirstRecordByFilter("api_keys", "prefix = {:p}", dbx.Params{"p": tok[:16]})
		if err != nil || subtle.ConstantTimeCompare([]byte(k.GetString("hash")), []byte(hashKey(tok))) != 1 {
			return nil, fail(http.StatusUnauthorized, "invalid API key")
		}
		if k.GetBool("revoked") {
			return nil, fail(http.StatusUnauthorized, "this API key was revoked")
		}
		u, err := app.FindRecordById("users", k.GetString("user"))
		if err != nil {
			return nil, fail(http.StatusUnauthorized, "the key's user no longer exists")
		}
		touch(app, k)
		return withPlan(app, &Account{User: u, Key: k})
	}
	if e.Auth != nil {
		if e.Auth.IsSuperuser() {
			return &Account{}, nil
		}
		if e.Auth.Collection().Name == "users" {
			return withPlan(app, &Account{User: e.Auth})
		}
	}
	return nil, fail(http.StatusUnauthorized, "send an API key: Authorization: Bearer jab_… (make one at /api/gateway/keys)")
}

var touched sync.Map // key id → last write, so last_used is written at most once a minute

func touch(app core.App, k *core.Record) {
	now := time.Now()
	if t, ok := touched.Load(k.Id); ok && now.Sub(t.(time.Time)) < time.Minute {
		return
	}
	touched.Store(k.Id, now)
	k.Set("last_used", now)
	_ = app.SaveNoValidate(k)
}

func withPlan(app core.App, a *Account) (*Account, error) {
	if id := a.User.GetString("plan"); id != "" {
		if p, err := app.FindRecordById("plans", id); err == nil {
			a.Plan = p
			return a, nil
		}
	}
	p, err := app.FindFirstRecordByFilter("plans", "default = true && active = true")
	if err != nil {
		return nil, fail(http.StatusPaymentRequired, "no plan: choose one first")
	}
	a.Plan = p
	return a, nil
}

// ---- limits ----

var (
	rpmMu sync.Mutex
	rpm   = map[string][]time.Time{}
)

func allowRate(user string, limit int) bool {
	if limit <= 0 {
		return true
	}
	rpmMu.Lock()
	defer rpmMu.Unlock()
	now := time.Now()
	cut := now.Add(-time.Minute)
	l := rpm[user]
	i := 0
	for i < len(l) && l[i].Before(cut) {
		i++
	}
	l = l[i:]
	if len(l) >= limit {
		rpm[user] = l
		return false
	}
	rpm[user] = append(l, now)
	return true
}

// MonthUsage is a user's use since the first of the month (UTC).
func MonthUsage(app core.App, userID string) (tokens int, cost float64) {
	start := time.Now().UTC()
	start = time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC)
	var row struct {
		T float64 `db:"t"`
		C float64 `db:"c"`
	}
	_ = app.DB().NewQuery("SELECT COALESCE(SUM(tokens_in + tokens_out), 0) AS t, COALESCE(SUM(cost), 0) AS c FROM usage WHERE user = {:u} AND created >= {:s}").
		Bind(dbx.Params{"u": userID, "s": start.Format("2006-01-02 15:04:05.000Z")}).One(&row)
	return int(row.T), row.C
}

// gate: who is calling, which model, and may they use it now
func gate(e *core.RequestEvent, kind, model string) (*Account, *core.Record, *Provider, error) {
	a, err := authenticate(e)
	if err != nil {
		return nil, nil, nil, err
	}
	m, err := e.App.FindFirstRecordByFilter("models", "name = {:n} && enabled = true", dbx.Params{"n": model})
	if err != nil {
		return nil, nil, nil, fail(http.StatusNotFound, "model %q not found; see /v1/models", model)
	}
	if k := m.GetString("kind"); k != kind {
		return nil, nil, nil, fail(http.StatusBadRequest, "%q is a %s model, not a %s model", model, k, kind)
	}
	if a.Plan != nil {
		if ps := m.GetStringSlice("plans"); len(ps) > 0 && !contains(ps, a.Plan.Id) {
			return nil, nil, nil, fail(http.StatusForbidden, "%q is not in your plan (%s)", model, a.Plan.GetString("name"))
		}
		if !allowRate(a.User.Id, a.Plan.GetInt("rpm")) {
			return nil, nil, nil, fail(http.StatusTooManyRequests, "rate limit: %d requests a minute on the %s plan", a.Plan.GetInt("rpm"), a.Plan.GetString("name"))
		}
		tokens, cost := MonthUsage(e.App, a.User.Id)
		if lim := a.Plan.GetInt("monthly_tokens"); lim > 0 && tokens >= lim {
			return nil, nil, nil, fail(http.StatusTooManyRequests, "monthly limit reached: %d of %d tokens on the %s plan", tokens, lim, a.Plan.GetString("name"))
		}
		if lim := a.Plan.GetFloat("monthly_budget"); lim > 0 && cost >= lim {
			return nil, nil, nil, fail(http.StatusPaymentRequired, "monthly budget reached: $%.2f of $%.2f on the %s plan", cost, lim, a.Plan.GetString("name"))
		}
	}
	for _, c := range Check {
		if err := c(e.App, a, m); err != nil {
			return nil, nil, nil, err
		}
	}
	p, err := providerOf(e.App, m)
	if err != nil {
		return nil, nil, nil, err
	}
	return a, m, p, nil
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func providerOf(app core.App, m *core.Record) (*Provider, error) {
	r, err := app.FindRecordById("providers", m.GetString("provider"))
	if err != nil || !r.GetBool("enabled") {
		return nil, fail(http.StatusServiceUnavailable, "the provider of %q is not available", m.GetString("name"))
	}
	return providerFrom(r), nil
}

func providerFrom(r *core.Record) *Provider {
	p := &Provider{ID: r.Id, Name: r.GetString("name"), Type: r.GetString("type"), BaseURL: strings.TrimRight(r.GetString("base_url"), "/"), Key: r.GetString("api_key"), TimeoutSec: r.GetInt("timeout"), Headers: map[string]string{}}
	if p.BaseURL == "" {
		p.BaseURL = Defaults[p.Type]
	}
	if h, ok := r.Get("headers").(types.JSONRaw); ok && len(h) > 0 {
		var m map[string]any
		if err := jsonUnmarshal(h, &m); err == nil {
			for k, v := range m {
				p.Headers[k] = fmt.Sprint(v)
			}
		}
	}
	return p
}

func upstreamModel(m *core.Record) string {
	if u := m.GetString("upstream"); u != "" {
		return u
	}
	return m.GetString("name")
}

// record what a request used (and cost)
func meter(app core.App, a *Account, m *core.Record, kind string, in, out int, start time.Time, status int, errMsg string) {
	if a == nil || a.User == nil {
		return // superusers are not metered
	}
	col, err := app.FindCachedCollectionByNameOrId("usage")
	if err != nil {
		return
	}
	cost := 0.0
	if m != nil {
		cost = float64(in)*m.GetFloat("price_in")/1e6 + float64(out)*m.GetFloat("price_out")/1e6
		if status < 400 {
			cost += m.GetFloat("price_call")
		}
	}
	r := core.NewRecord(col)
	r.Set("user", a.User.Id)
	if a.Key != nil {
		r.Set("key", a.Key.GetString("prefix"))
	}
	if m != nil {
		r.Set("model", m.GetString("name"))
	}
	r.Set("kind", kind)
	r.Set("tokens_in", in)
	r.Set("tokens_out", out)
	r.Set("cost", cost)
	r.Set("ms", time.Since(start).Milliseconds())
	r.Set("status", status)
	if len(errMsg) > 2000 {
		errMsg = errMsg[:2000]
	}
	r.Set("error", errMsg)
	if err := app.Save(r); err != nil {
		app.Logger().Warn("gateway usage", "error", err)
	}
}

func statusOf(err error) (int, string) {
	var se *statusErr
	if errors.As(err, &se) {
		return se.code, se.msg
	}
	var sc interface{ StatusCode() int }
	if errors.As(err, &sc) {
		return sc.StatusCode(), err.Error()
	}
	var ue *UpstreamError
	if errors.As(err, &ue) {
		code := ue.Status
		if code < 400 || code >= 600 {
			code = http.StatusBadGateway
		}
		if code == http.StatusUnauthorized || code == http.StatusForbidden {
			code = http.StatusBadGateway // the provider's credentials, not the caller's
		}
		return code, ue.Msg
	}
	return http.StatusBadGateway, err.Error()
}
