package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/lucky-edgelord/jade-agent-base/agent"
	"github.com/lucky-edgelord/jade-agent-base/state"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

func randID() string               { b := make([]byte, 12); _, _ = rand.Read(b); return hex.EncodeToString(b) }
func float32bits(f float32) uint32 { return math.Float32bits(f) }

var toolNames []string

func add(t *agent.Tool) { t.Scope = Scope; agent.AddTool(t); toolNames = append(toolNames, t.Name) }

func p(props map[string]any, req ...string) map[string]any {
	o := map[string]any{"type": "object", "properties": props}
	if len(req) > 0 {
		o["required"] = req
	}
	return o
}
func s(d string) map[string]any { return map[string]any{"type": "string", "description": d} }
func n(d string) map[string]any { return map[string]any{"type": "number", "description": d} }
func b(d string) map[string]any { return map[string]any{"type": "boolean", "description": d} }

func agentTools() {
	toolNames = toolNames[:0]
	add(&agent.Tool{Name: "gateway_status",
		Description: "The model gateway at a glance: which capabilities are served (chat, embed, decide, search, fetch, openai_api, ollama_api), providers (keys never shown), models with prices, plans, and this month's totals.",
		Run: func(c *agent.Call) (any, error) {
			feats := map[string]bool{}
			for _, f := range Features {
				feats[f] = state.Bool("gateway", f, true)
			}
			prov, _ := c.App.FindRecordsByFilter("providers", "", "name", 0, 0)
			ps := []map[string]any{}
			for _, r := range prov {
				ps = append(ps, map[string]any{"name": r.GetString("name"), "type": r.GetString("type"), "base_url": providerFrom(r).BaseURL, "has_key": r.GetString("api_key") != "", "enabled": r.GetBool("enabled")})
			}
			models, _ := c.App.FindRecordsByFilter("models", "", "name", 0, 0)
			ms := []map[string]any{}
			for _, m := range models {
				ms = append(ms, modelView(c.App, m))
			}
			plans, _ := c.App.FindRecordsByFilter("plans", "", "name", 0, 0)
			pl := []map[string]any{}
			for _, r := range plans {
				pl = append(pl, planView(r))
			}
			var tot struct {
				N int     `db:"n"`
				T float64 `db:"t"`
				C float64 `db:"c"`
			}
			start := time.Now().UTC()
			_ = c.App.DB().NewQuery("SELECT COUNT(*) AS n, COALESCE(SUM(tokens_in+tokens_out),0) AS t, COALESCE(SUM(cost),0) AS c FROM usage WHERE created >= {:s}").
				Bind(dbx.Params{"s": time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02 15:04:05.000Z")}).One(&tot)
			return map[string]any{"serving": feats, "providers": ps, "models": ms, "plans": pl, "this_month": map[string]any{"requests": tot.N, "tokens": int(tot.T), "revenue_cost": tot.C},
				"endpoints": "OpenAI: /v1/chat/completions, /v1/embeddings, /v1/models, /v1/search, /v1/fetch, /v1/systemone. Ollama: /api/chat, /api/embed, /api/tags, /api/web_search, /api/web_fetch. Keys: POST /api/gateway/keys (signed-in users). Usage: GET /api/gateway/me."}, nil
		}})
	add(&agent.Tool{Name: "gateway_serve",
		Description: "Choose what the gateway serves: switch capabilities on or off, e.g. {\"decide\": false, \"ollama_api\": true}. Capabilities: " + strings.Join(Features, ", ") + ".",
		Parameters:  p(map[string]any{"set": map[string]any{"type": "object", "description": "capability → true/false"}}, "set"),
		Run: func(c *agent.Call) (any, error) {
			set := c.Map("set")
			for k, v := range set {
				if !contains(Features, k) {
					return nil, fmt.Errorf("unknown capability %q (capabilities: %s)", k, strings.Join(Features, ", "))
				}
				if _, ok := v.(bool); !ok {
					return nil, fmt.Errorf("%s must be true or false", k)
				}
			}
			if err := state.Set("gateway", set); err != nil {
				return nil, err
			}
			out := map[string]bool{}
			for _, f := range Features {
				out[f] = state.Bool("gateway", f, true)
			}
			return out, nil
		}})
	add(&agent.Tool{Name: "provider_set",
		Description: "Add or change a provider (by name). Types: ollama (a machine, default http://localhost:11434), ollama-cloud (https://ollama.com, needs api_key; also web search and fetch), ollaya (decision models, default http://localhost:11435), local (embedding models running inside this server: model2vec, e.g. minishlab/potion-base-8M; no key, downloads on first use), openai (any OpenAI-compatible API: OpenAI, OpenRouter, vLLM, llama.cpp, LM Studio, Groq, Together… set base_url), anthropic, google. The api_key is stored and never shown again.",
		Parameters: p(map[string]any{"name": s("a name you choose"), "type": s("ollama | ollama-cloud | ollaya | openai | anthropic | google"), "base_url": s("leave empty for the type's default"),
			"api_key": s("the provider's key (write-only)"), "headers": map[string]any{"type": "object", "description": "extra headers"}, "timeout": n("seconds (default 300)"), "enabled": b("default true")}, "name"),
		Run: func(c *agent.Call) (any, error) {
			col, err := c.App.FindCollectionByNameOrId("providers")
			if err != nil {
				return nil, err
			}
			r, err := c.App.FindFirstRecordByData("providers", "name", c.Str("name"))
			if err != nil {
				if c.Str("type") == "" {
					return nil, fmt.Errorf("a new provider needs a type: %s", strings.Join(keys(Defaults), ", "))
				}
				r = core.NewRecord(col)
				r.Set("name", c.Str("name"))
				r.Set("enabled", true)
			}
			for _, k := range []string{"type", "base_url", "api_key", "headers", "timeout", "enabled"} {
				if v, ok := c.Args[k]; ok {
					r.Set(k, v)
				}
			}
			if err := c.App.Save(r); err != nil {
				return nil, fmt.Errorf("%v", err)
			}
			return map[string]any{"name": r.GetString("name"), "type": r.GetString("type"), "base_url": providerFrom(r).BaseURL, "has_key": r.GetString("api_key") != "", "enabled": r.GetBool("enabled"),
				"next": "provider_models lists what it offers; model_set offers one to users"}, nil
		}})
	add(&agent.Tool{Name: "provider_models",
		Description: "Ask a provider which models it has (to choose what to offer). Also checks the address and key work.",
		Parameters:  p(map[string]any{"name": s("provider name")}, "name"),
		Run: func(c *agent.Call) (any, error) {
			r, err := c.App.FindFirstRecordByData("providers", "name", c.Str("name"))
			if err != nil {
				return nil, fmt.Errorf("no provider %q", c.Str("name"))
			}
			pv := providerFrom(r)
			if pv.Type == "local" {
				return map[string]any{"provider": pv.Name, "ok": true, "downloaded": localModels(), "suggested": SuggestedLocal,
					"note": "local runs model2vec embedding models from the Hugging Face Hub in the server; offer one with model_set (upstream: owner/name, kind: embed). It downloads on first use."}, nil
			}
			return listUpstream(pv)
		}})
	add(&agent.Tool{Name: "provider_delete", Destructive: true, Description: "Remove a provider (its models must be removed or moved first).",
		Parameters: p(map[string]any{"name": s("provider name")}, "name"),
		Run: func(c *agent.Call) (any, error) {
			r, err := c.App.FindFirstRecordByData("providers", "name", c.Str("name"))
			if err != nil {
				return nil, fmt.Errorf("no provider %q", c.Str("name"))
			}
			if n, _ := c.App.CountRecords("models", dbx.HashExp{"provider": r.Id}); n > 0 {
				return nil, fmt.Errorf("%d models use %s: delete or move them first", n, c.Str("name"))
			}
			return "deleted " + c.Str("name"), c.App.Delete(r)
		}})
	add(&agent.Tool{Name: "model_set",
		Description: "Offer a model to users (or change one, by name). kind: chat, embed, decide, search (web search), fetch (web fetch). Prices are what users are charged, in dollars: price_in and price_out per million tokens, price_call per request. plans: the plan names that may use it (empty: every plan).",
		Parameters: p(map[string]any{"name": s("the name users call it by"), "provider": s("provider name"), "upstream": s("the provider's name for it (default: name)"), "kind": s("chat | embed | decide | search | fetch"),
			"price_in": n("$ per 1M input tokens"), "price_out": n("$ per 1M output tokens"), "price_call": n("$ per request"), "plans": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "plan names"},
			"enabled": b("default true"), "description": s("shown to users"), "context": n("context window in tokens")}, "name"),
		Run: func(c *agent.Call) (any, error) {
			col, err := c.App.FindCollectionByNameOrId("models")
			if err != nil {
				return nil, err
			}
			r, err := c.App.FindFirstRecordByData("models", "name", c.Str("name"))
			if err != nil {
				if c.Str("provider") == "" || c.Str("kind") == "" {
					return nil, fmt.Errorf("a new model needs a provider and a kind (%s)", strings.Join(Kinds, ", "))
				}
				r = core.NewRecord(col)
				r.Set("name", c.Str("name"))
				r.Set("enabled", true)
			}
			if pn := c.Str("provider"); pn != "" {
				pr, err := c.App.FindFirstRecordByData("providers", "name", pn)
				if err != nil {
					return nil, fmt.Errorf("no provider %q (provider_set adds one)", pn)
				}
				r.Set("provider", pr.Id)
			}
			if l, ok := c.Args["plans"].([]any); ok {
				ids := []string{}
				for _, x := range l {
					pl, err := c.App.FindFirstRecordByData("plans", "name", fmt.Sprint(x))
					if err != nil {
						return nil, fmt.Errorf("no plan %q", x)
					}
					ids = append(ids, pl.Id)
				}
				r.Set("plans", ids)
			}
			for _, k := range []string{"upstream", "kind", "price_in", "price_out", "price_call", "enabled", "description", "context"} {
				if v, ok := c.Args[k]; ok {
					r.Set(k, v)
				}
			}
			if pr, err := c.App.FindRecordById("providers", r.GetString("provider")); err == nil && pr.GetString("type") == "local" {
				if r.GetString("kind") != "embed" {
					return nil, fmt.Errorf("local providers run embedding models only (kind: embed)")
				}
				if !isLocalName(upstreamModel(r)) {
					return nil, fmt.Errorf("a local model's upstream is a Hugging Face repo like %s", SuggestedLocal[0])
				}
			}
			if err := c.App.Save(r); err != nil {
				return nil, fmt.Errorf("%v", err)
			}
			return modelView(c.App, r), nil
		}})
	add(&agent.Tool{Name: "model_delete", Destructive: true, Description: "Stop offering a model and remove it.",
		Parameters: p(map[string]any{"name": s("model name")}, "name"),
		Run: func(c *agent.Call) (any, error) {
			r, err := c.App.FindFirstRecordByData("models", "name", c.Str("name"))
			if err != nil {
				return nil, fmt.Errorf("no model %q", c.Str("name"))
			}
			return "deleted " + c.Str("name"), c.App.Delete(r)
		}})
	add(&agent.Tool{Name: "plan_set",
		Description: "Add or change a plan (by name): what users get each month. monthly_tokens and monthly_budget ($) cap use (0: no cap), rpm caps requests a minute (0: none). default: the plan for users without one. price_month is shown to users (billing charges it).",
		Parameters: p(map[string]any{"name": s("plan name"), "description": s("shown to users"), "price_month": n("$ a month"), "monthly_tokens": n("tokens a month (0: no cap)"), "monthly_budget": n("$ of usage a month (0: no cap)"),
			"rpm": n("requests a minute (0: no cap)"), "default": b("for users without a plan"), "active": b("offered (default true)")}, "name"),
		Run: func(c *agent.Call) (any, error) {
			col, err := c.App.FindCollectionByNameOrId("plans")
			if err != nil {
				return nil, err
			}
			r, err := c.App.FindFirstRecordByData("plans", "name", c.Str("name"))
			if err != nil {
				r = core.NewRecord(col)
				r.Set("name", c.Str("name"))
				r.Set("active", true)
			}
			for _, k := range []string{"description", "price_month", "monthly_tokens", "monthly_budget", "rpm", "default", "active"} {
				if v, ok := c.Args[k]; ok {
					r.Set(k, v)
				}
			}
			err = c.App.RunInTransaction(func(tx core.App) error {
				if c.Bool("default") {
					others, _ := tx.FindRecordsByFilter("plans", "default = true && id != {:id}", "", 0, 0, dbx.Params{"id": r.Id})
					for _, o := range others {
						o.Set("default", false)
						if err := tx.Save(o); err != nil {
							return err
						}
					}
				}
				return tx.Save(r)
			})
			if err != nil {
				return nil, fmt.Errorf("%v", err)
			}
			return planView(r), nil
		}})
	add(&agent.Tool{Name: "user_plan", Description: "Put a user on a plan (by the user's email or id).",
		Parameters: p(map[string]any{"user": s("email or id"), "plan": s("plan name")}, "user", "plan"),
		Run: func(c *agent.Call) (any, error) {
			u, err := findUser(c.App, c.Str("user"))
			if err != nil {
				return nil, err
			}
			pl, err := c.App.FindFirstRecordByData("plans", "name", c.Str("plan"))
			if err != nil {
				return nil, fmt.Errorf("no plan %q", c.Str("plan"))
			}
			u.Set("plan", pl.Id)
			if err := c.App.Save(u); err != nil {
				return nil, err
			}
			return fmt.Sprintf("%s is on %s", u.Email(), pl.GetString("name")), nil
		}})
	add(&agent.Tool{Name: "usage_report",
		Description: "Usage and charges over the last N days, grouped by user, model or day.",
		Parameters:  p(map[string]any{"days": n("default 30"), "group": s("user | model | day (default model)"), "user": s("only this user (email or id)")}),
		Run: func(c *agent.Call) (any, error) {
			days := c.Int("days", 30)
			if days < 1 {
				days = 30
			}
			col := map[string]string{"user": "user", "model": "model", "day": "substr(created, 1, 10)"}[orStr(c.Str("group"), "model")]
			if col == "" {
				return nil, fmt.Errorf("group must be user, model or day")
			}
			where := "created >= {:s}"
			params := dbx.Params{"s": time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02 15:04:05.000Z")}
			if u := c.Str("user"); u != "" {
				ur, err := findUser(c.App, u)
				if err != nil {
					return nil, err
				}
				where += " AND user = {:u}"
				params["u"] = ur.Id
			}
			rows := []struct {
				K   string  `db:"k" json:"key"`
				N   int     `db:"n" json:"requests"`
				In  int     `db:"tin" json:"tokens_in"`
				Out int     `db:"tout" json:"tokens_out"`
				C   float64 `db:"c" json:"charged"`
				E   int     `db:"e" json:"errors"`
			}{}
			err := c.App.DB().NewQuery("SELECT " + col + " AS k, COUNT(*) AS n, COALESCE(SUM(tokens_in),0) AS tin, COALESCE(SUM(tokens_out),0) AS tout, COALESCE(SUM(cost),0) AS c, SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END) AS e FROM usage WHERE " + where + " GROUP BY k ORDER BY c DESC, n DESC LIMIT 200").Bind(params).All(&rows)
			if err != nil {
				return nil, err
			}
			if c.Str("group") == "user" {
				for i := range rows {
					if u, err := c.App.FindRecordById("users", rows[i].K); err == nil {
						rows[i].K = u.Email()
					}
				}
			}
			return rows, nil
		}})
	add(&agent.Tool{Name: "keys_list", Description: "API keys (prefix, owner, name, last used, revoked); keys themselves are never shown.",
		Parameters: p(map[string]any{"user": s("only this user (email or id)")}),
		Run: func(c *agent.Call) (any, error) {
			f, params := "", dbx.Params{}
			if u := c.Str("user"); u != "" {
				ur, err := findUser(c.App, u)
				if err != nil {
					return nil, err
				}
				f, params["u"] = "user = {:u}", ur.Id
			}
			rs, err := c.App.FindRecordsByFilter("api_keys", f, "-created", 500, 0, params)
			if err != nil {
				return nil, err
			}
			out := []map[string]any{}
			for _, r := range rs {
				out = append(out, map[string]any{"id": r.Id, "prefix": r.GetString("prefix"), "user": r.GetString("user"), "name": r.GetString("name"), "last_used": r.GetString("last_used"), "revoked": r.GetBool("revoked")})
			}
			return out, nil
		}})
	add(&agent.Tool{Name: "key_revoke", Destructive: true, Description: "Revoke an API key (by id or prefix).",
		Parameters: p(map[string]any{"key": s("id or prefix")}, "key"),
		Run: func(c *agent.Call) (any, error) {
			r, err := c.App.FindRecordById("api_keys", c.Str("key"))
			if err != nil {
				r, err = c.App.FindFirstRecordByData("api_keys", "prefix", c.Str("key"))
			}
			if err != nil {
				return nil, fmt.Errorf("no key %q", c.Str("key"))
			}
			r.Set("revoked", true)
			return "revoked " + r.GetString("prefix"), c.App.Save(r)
		}})
}

func findUser(app core.App, idOrEmail string) (*core.Record, error) {
	if u, err := app.FindRecordById("users", idOrEmail); err == nil {
		return u, nil
	}
	u, err := app.FindAuthRecordByEmail("users", idOrEmail)
	if err != nil {
		return nil, fmt.Errorf("no user %q", idOrEmail)
	}
	return u, nil
}

func modelView(app core.App, m *core.Record) map[string]any {
	prov := ""
	if pr, err := app.FindRecordById("providers", m.GetString("provider")); err == nil {
		prov = pr.GetString("name")
	}
	plans := []string{}
	for _, id := range m.GetStringSlice("plans") {
		if pl, err := app.FindRecordById("plans", id); err == nil {
			plans = append(plans, pl.GetString("name"))
		}
	}
	return map[string]any{"name": m.GetString("name"), "kind": m.GetString("kind"), "provider": prov, "upstream": upstreamModel(m), "price_in": m.GetFloat("price_in"),
		"price_out": m.GetFloat("price_out"), "price_call": m.GetFloat("price_call"), "plans": plans, "enabled": m.GetBool("enabled")}
}

func planView(r *core.Record) map[string]any {
	return map[string]any{"name": r.GetString("name"), "price_month": r.GetFloat("price_month"), "monthly_tokens": r.GetInt("monthly_tokens"), "monthly_budget": r.GetFloat("monthly_budget"),
		"rpm": r.GetInt("rpm"), "default": r.GetBool("default"), "active": r.GetBool("active"), "description": r.GetString("description")}
}

// the models a provider has
func listUpstream(pv *Provider) (any, error) {
	path, hdr := "/api/tags", map[string]string{}
	switch pv.Type {
	case "openai":
		path = "/models"
		if pv.Key != "" {
			hdr["Authorization"] = "Bearer " + pv.Key
		}
	case "anthropic":
		path, hdr = "/v1/models", map[string]string{"x-api-key": pv.Key, "anthropic-version": "2023-06-01"}
	case "google":
		path, hdr = "/models", map[string]string{"x-goog-api-key": pv.Key}
	default:
		if pv.Key != "" {
			hdr["Authorization"] = "Bearer " + pv.Key
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, pv.BaseURL+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s unreachable at %s: %v", pv.Name, pv.BaseURL, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if res.StatusCode >= 300 {
		return nil, fmt.Errorf("%s answered %d: %s", pv.Name, res.StatusCode, upstreamMessage(raw))
	}
	var j struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &j)
	names := []string{}
	for _, m := range j.Models {
		names = append(names, strings.TrimPrefix(orStr(m.Name, m.Model), "models/"))
	}
	for _, d := range j.Data {
		names = append(names, d.ID)
	}
	sort.Strings(names)
	return map[string]any{"provider": pv.Name, "ok": true, "models": names}, nil
}
