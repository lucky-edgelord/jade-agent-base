package billing

import (
	"fmt"
	"math"
	"net/http"
	"strings"

	"github.com/lucky-edgelord/jade-agent-base/agent"
	"github.com/lucky-edgelord/jade-agent-base/state"
	"github.com/pocketbase/dbx"
)

var toolNames []string

func add(t *agent.Tool) { t.Scope = Scope; agent.AddTool(t); toolNames = append(toolNames, t.Name) }

func obj(props map[string]any, req ...string) map[string]any {
	o := map[string]any{"type": "object", "properties": props}
	if len(req) > 0 {
		o["required"] = req
	}
	return o
}
func str(d string) map[string]any { return map[string]any{"type": "string", "description": d} }

func tools() {
	toolNames = toolNames[:0]
	add(&agent.Tool{Name: "billing_setup",
		Description: "Connect Stripe: the secret key (sk_test_… or sk_live_…), the webhook signing secret (whsec_…), the currency and where Checkout returns. Keys are stored and never shown again. Then point a Stripe webhook at <appURL>/api/billing/webhook for checkout.session.completed, customer.subscription.* and invoice.payment_failed.",
		Parameters: obj(map[string]any{"secret_key": str("Stripe secret key (write-only)"), "webhook_secret": str("webhook signing secret (write-only)"), "currency": str("e.g. usd (default), eur"),
			"success_url": str("after paying (default <appURL>/?billing=success)"), "cancel_url": str("after cancelling checkout"), "return_url": str("back from the portal")}),
		Run: func(c *agent.Call) (any, error) {
			set := map[string]any{}
			for _, k := range []string{"secret_key", "webhook_secret", "currency", "success_url", "cancel_url", "return_url"} {
				if v := c.Str(k); v != "" {
					set[k] = v
				}
			}
			if k, _ := set["secret_key"].(string); k != "" && !strings.HasPrefix(k, "sk_") && !strings.HasPrefix(k, "rk_") {
				return nil, fmt.Errorf("a Stripe secret key starts with sk_ (or rk_ for a restricted key)")
			}
			if err := state.Set("billing", set); err != nil {
				return nil, err
			}
			return status(c), nil
		}})
	add(&agent.Tool{Name: "billing_status",
		Description: "Billing at a glance: whether Stripe is connected (test or live), the webhook URL, which plans are for sale, subscribers by status, and monthly recurring revenue.",
		Run:         func(c *agent.Call) (any, error) { return status(c), nil }})
	add(&agent.Tool{Name: "plan_publish",
		Description: "Put a plan on sale: makes (or updates) its Stripe product and a monthly price from the plan's price_month. A new price replaces the old one for new subscribers.",
		Parameters:  obj(map[string]any{"plan": str("plan name (plan_set makes plans)")}, "plan"),
		Run: func(c *agent.Call) (any, error) {
			p, err := c.App.FindFirstRecordByData("plans", "name", c.Str("plan"))
			if err != nil {
				return nil, fmt.Errorf("no plan %q", c.Str("plan"))
			}
			amount := int64(math.Round(p.GetFloat("price_month") * 100))
			if amount <= 0 {
				return nil, fmt.Errorf("%s costs nothing: set price_month with plan_set first", p.GetString("name"))
			}
			cur := setting("currency", "usd")
			var prod struct {
				ID string `json:"id"`
			}
			if err := stripe(http.MethodPost, "/v1/products", map[string]any{"name": p.GetString("name"), "description": orStr(p.GetString("description"), p.GetString("name")+" plan"), "metadata": map[string]any{"plan": p.Id}}, &prod); err != nil {
				return nil, err
			}
			var price struct {
				ID string `json:"id"`
			}
			if err := stripe(http.MethodPost, "/v1/prices", map[string]any{"product": prod.ID, "currency": cur, "unit_amount": amount, "recurring": map[string]any{"interval": "month"}, "metadata": map[string]any{"plan": p.Id}}, &price); err != nil {
				return nil, err
			}
			p.Set("stripe_price", price.ID)
			if err := c.App.Save(p); err != nil {
				return nil, err
			}
			return map[string]any{"plan": p.GetString("name"), "price": price.ID, "amount": fmt.Sprintf("%.2f %s a month", float64(amount)/100, strings.ToUpper(cur)), "checkout": "users call POST /api/billing/checkout {\"plan\": \"" + p.GetString("name") + "\"}"}, nil
		}})
	add(&agent.Tool{Name: "subscribers_list", Description: "Users with a subscription: email, plan, status, renewal date.",
		Parameters: obj(map[string]any{"status": str("only this status: active, trialing, past_due, canceled, …")}),
		Run: func(c *agent.Call) (any, error) {
			f, params := "subscription_status != ''", dbx.Params{}
			if s := c.Str("status"); s != "" {
				f, params["s"] = "subscription_status = {:s}", s
			}
			us, err := c.App.FindRecordsByFilter("users", f, "-current_period_end", 500, 0, params)
			if err != nil {
				return nil, err
			}
			out := []map[string]any{}
			for _, u := range us {
				plan := ""
				if p, err := c.App.FindRecordById("plans", u.GetString("plan")); err == nil {
					plan = p.GetString("name")
				}
				out = append(out, map[string]any{"email": u.Email(), "plan": plan, "status": u.GetString("subscription_status"), "renews": u.GetString("current_period_end")})
			}
			return out, nil
		}})
	add(&agent.Tool{Name: "billing_events", Description: "What Stripe told the server lately (subscriptions started, changed, ended; payments failed).",
		Parameters: obj(map[string]any{"limit": map[string]any{"type": "integer", "description": "default 50"}}),
		Run: func(c *agent.Call) (any, error) {
			rs, err := c.App.FindRecordsByFilter("billing_events", "", "-created", c.Int("limit", 50), 0)
			if err != nil {
				return nil, err
			}
			out := []map[string]any{}
			for _, r := range rs {
				out = append(out, map[string]any{"type": r.GetString("type"), "summary": r.GetString("summary"), "at": r.GetString("created")})
			}
			return out, nil
		}})
}

func status(c *agent.Call) map[string]any {
	key := secretKey()
	mode := "not connected"
	if strings.Contains(key, "_live_") {
		mode = "live"
	} else if key != "" {
		mode = "test"
	}
	plans, _ := c.App.FindRecordsByFilter("plans", "active = true", "price_month", 0, 0)
	sale := []map[string]any{}
	prices := map[string]float64{}
	for _, p := range plans {
		prices[p.Id] = p.GetFloat("price_month")
		sale = append(sale, map[string]any{"plan": p.GetString("name"), "price_month": p.GetFloat("price_month"), "for_sale": p.GetString("stripe_price") != ""})
	}
	counts := map[string]int{}
	mrr := 0.0
	us, _ := c.App.FindRecordsByFilter("users", "subscription_status != ''", "", 0, 0)
	for _, u := range us {
		s := u.GetString("subscription_status")
		counts[s]++
		if Active[s] {
			mrr += prices[u.GetString("plan")]
		}
	}
	return map[string]any{"stripe": mode, "webhook_secret_set": webhookSecret() != "", "currency": setting("currency", "usd"),
		"webhook_url": c.App.Settings().Meta.AppURL + "/api/billing/webhook", "plans": sale, "subscribers": counts, "mrr": math.Round(mrr*100) / 100}
}
