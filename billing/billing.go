// Package billing charges for plans with Stripe: a plan becomes a Stripe product and monthly price, users subscribe
// through Stripe Checkout and manage it in the customer portal, and Stripe's webhook moves them between plans as
// subscriptions start, change, fail or end. Paid plans of the gateway need an active subscription.
//
//	POST /api/billing/checkout   (signed-in user) { plan } → { url }
//	POST /api/billing/portal     (signed-in user) → { url }
//	POST /api/billing/webhook    Stripe events (signed)
//	GET  /api/billing/me         (signed-in user) the subscription
//
// Stripe's keys come from STRIPE_SECRET_KEY and STRIPE_WEBHOOK_SECRET, or an agent sets them with billing_setup
// (write-only: no tool ever shows them).
package billing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lucky-edgelord/jade-agent-base/agent"
	"github.com/lucky-edgelord/jade-agent-base/gateway"
	"github.com/lucky-edgelord/jade-agent-base/state"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/hook"
)

// Scope agents need to run billing.
const Scope = "billing"

// APIBase is Stripe's API (STRIPE_API_BASE points it at stripe-mock or a test server).
var APIBase = "https://api.stripe.com"

// Active subscription states: the user keeps the paid plan.
var Active = map[string]bool{"active": true, "trialing": true}

// Register binds billing's routes (they answer only while the module is on).
func Register(app core.App) {
	agent.AddScope(Scope)
	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id:   "jab_billing_routes",
		Func: func(e *core.ServeEvent) error { routes(e); return e.Next() },
	})
}

// Enable needs the gateway (plans live there): it switches the gateway's collections on, adds billing's fields and
// tools, and makes paid plans require an active subscription.
func Enable(app core.App) error {
	if v := os.Getenv("STRIPE_API_BASE"); v != "" { // stripe-mock, or a test server
		APIBase = strings.TrimRight(v, "/")
	}
	if err := gateway.Enable(app); err != nil {
		return err
	}
	if err := ensureCollections(app); err != nil {
		return err
	}
	tools()
	gateway.Check = append(gateway.Check[:0:0], filterChecks(gateway.Check)...)
	gateway.Check = append(gateway.Check, subscriptionCheck)
	return nil
}

// Disable takes billing's tools away and stops checking subscriptions.
func Disable(app core.App) error {
	agent.RemoveTool(toolNames...)
	gateway.Check = filterChecks(gateway.Check)
	return nil
}

var checkTag = fmt.Sprintf("%p", subscriptionCheck)

func filterChecks(cs []func(core.App, *gateway.Account, *core.Record) error) []func(core.App, *gateway.Account, *core.Record) error {
	out := cs[:0:0]
	for _, c := range cs {
		if fmt.Sprintf("%p", c) != checkTag {
			out = append(out, c)
		}
	}
	return out
}

func on() bool { return state.Enabled("billing") }

func ensureCollections(app core.App) error {
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		return err
	}
	changed := false
	for _, f := range []core.Field{
		&core.TextField{Name: "stripe_customer", Hidden: true, Max: 100},
		&core.TextField{Name: "stripe_subscription", Hidden: true, Max: 100},
		&core.TextField{Name: "subscription_status", Max: 40},
		&core.DateField{Name: "current_period_end"},
	} {
		if users.Fields.GetByName(f.GetName()) == nil {
			users.Fields.Add(f)
			changed = true
		}
	}
	if changed {
		// people may not set their own billing fields
		for _, rule := range []**string{&users.UpdateRule, &users.CreateRule} {
			if *rule != nil && !strings.Contains(**rule, "subscription_status") {
				r := **rule
				guard := "@request.body.subscription_status:isset = false && @request.body.stripe_customer:isset = false && @request.body.stripe_subscription:isset = false && @request.body.current_period_end:isset = false"
				if r == "" {
					*rule = &guard
				} else {
					s := "(" + r + ") && " + guard
					*rule = &s
				}
			}
		}
		if err := app.Save(users); err != nil {
			return fmt.Errorf("users billing fields: %w", err)
		}
	}
	if _, err := app.FindCollectionByNameOrId("billing_events"); err != nil {
		c := core.NewBaseCollection("billing_events")
		c.Fields.Add(
			&core.TextField{Name: "event", Required: true, Max: 100},
			&core.TextField{Name: "type", Max: 100},
			&core.TextField{Name: "user", Max: 30},
			&core.TextField{Name: "summary", Max: 1000},
			&core.AutodateField{Name: "created", OnCreate: true},
		)
		c.AddIndex("idx_billing_events_event", true, "event", "")
		if err := app.Save(c); err != nil {
			return fmt.Errorf("billing_events: %w", err)
		}
	}
	return nil
}

// ---- configuration ----

func secretKey() string {
	if k := os.Getenv("STRIPE_SECRET_KEY"); k != "" {
		return k
	}
	s, _ := state.Get("billing", "secret_key", "").(string)
	return s
}

func webhookSecret() string {
	if k := os.Getenv("STRIPE_WEBHOOK_SECRET"); k != "" {
		return k
	}
	s, _ := state.Get("billing", "webhook_secret", "").(string)
	return s
}

func setting(k, def string) string {
	s, _ := state.Get("billing", k, def).(string)
	if s == "" {
		return def
	}
	return s
}

// ---- Stripe's API ----

// stripe calls Stripe's REST API with form-encoded parameters.
func stripe(method, path string, params map[string]any, out any) error {
	key := secretKey()
	if key == "" {
		return errors.New("billing is not set up: an agent runs billing_setup with the Stripe secret key (or set STRIPE_SECRET_KEY)")
	}
	form := url.Values{}
	encode(form, "", params)
	var body io.Reader
	u := APIBase + path
	if method == http.MethodGet {
		if len(form) > 0 {
			u += "?" + form.Encode()
		}
	} else {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, u, body)
	req.Header.Set("Authorization", "Bearer "+key)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("stripe unreachable: %w", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode >= 300 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return fmt.Errorf("stripe %d: %s", res.StatusCode, e.Error.Message)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// encode nested params the way Stripe wants: line_items[0][price]=…, metadata[user]=…
func encode(form url.Values, prefix string, v any) {
	key := func(k string) string {
		if prefix == "" {
			return k
		}
		return prefix + "[" + k + "]"
	}
	switch x := v.(type) {
	case map[string]any:
		ks := make([]string, 0, len(x))
		for k := range x {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			encode(form, key(k), x[k])
		}
	case []any:
		for i, item := range x {
			encode(form, prefix+"["+strconv.Itoa(i)+"]", item)
		}
	case nil:
	case bool:
		form.Set(prefix, strconv.FormatBool(x))
	case int:
		form.Set(prefix, strconv.Itoa(x))
	case int64:
		form.Set(prefix, strconv.FormatInt(x, 10))
	case float64:
		form.Set(prefix, strconv.FormatFloat(x, 'f', -1, 64))
	default:
		form.Set(prefix, fmt.Sprint(x))
	}
}

// customer: the user's Stripe customer, made on first use
func customer(app core.App, u *core.Record) (string, error) {
	if c := u.GetString("stripe_customer"); c != "" {
		return c, nil
	}
	var c struct {
		ID string `json:"id"`
	}
	if err := stripe(http.MethodPost, "/v1/customers", map[string]any{"email": u.Email(), "metadata": map[string]any{"user": u.Id}}, &c); err != nil {
		return "", err
	}
	u.Set("stripe_customer", c.ID)
	return c.ID, app.SaveNoValidate(u)
}

// ---- the subscription check for the gateway ----

func subscriptionCheck(app core.App, a *gateway.Account, _ *core.Record) error {
	if !on() || a.User == nil || a.Plan == nil || a.Plan.GetFloat("price_month") <= 0 {
		return nil
	}
	if !Active[a.User.GetString("subscription_status")] {
		return gatewayErr{http.StatusPaymentRequired, fmt.Sprintf("the %s plan needs an active subscription (status: %s): subscribe at /api/billing/checkout", a.Plan.GetString("name"), orStr(a.User.GetString("subscription_status"), "none"))}
	}
	return nil
}

type gatewayErr struct {
	code int
	msg  string
}

func (e gatewayErr) Error() string   { return e.msg }
func (e gatewayErr) StatusCode() int { return e.code }

func orStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// ---- webhook signatures ----

// verify checks a Stripe-Signature header (t=…,v1=…) against the payload, within tolerance.
func verify(payload []byte, header, secret string, tolerance time.Duration) error {
	if secret == "" {
		return errors.New("no webhook secret set")
	}
	var ts int64
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			ts, _ = strconv.ParseInt(v, 10, 64)
		case "v1":
			sigs = append(sigs, v)
		}
	}
	if ts == 0 || len(sigs) == 0 {
		return errors.New("malformed Stripe-Signature")
	}
	if d := time.Since(time.Unix(ts, 0)); d > tolerance || d < -tolerance {
		return errors.New("signature too old")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts, 10) + "."))
	mac.Write(payload)
	want := mac.Sum(nil)
	for _, s := range sigs {
		got, err := hex.DecodeString(s)
		if err == nil && hmac.Equal(got, want) {
			return nil
		}
	}
	return errors.New("signature does not match")
}

// Sign makes a Stripe-Signature header (for tests and for replaying events).
func Sign(payload []byte, secret string, t time.Time) string {
	mac := hmac.New(sha256.New, []byte(secret))
	ts := strconv.FormatInt(t.Unix(), 10)
	mac.Write([]byte(ts + "."))
	mac.Write(payload)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// ---- applying subscriptions ----

type subscription struct {
	ID       string `json:"id"`
	Customer string `json:"customer"`
	Status   string `json:"status"`
	Period   int64  `json:"current_period_end"`
	Metadata struct {
		User string `json:"user"`
		Plan string `json:"plan"`
	} `json:"metadata"`
	Items struct {
		Data []struct {
			Period int64 `json:"current_period_end"`
			Price  struct {
				ID string `json:"id"`
			} `json:"price"`
		} `json:"data"`
	} `json:"items"`
}

func userFor(app core.App, customerID, userID string) (*core.Record, error) {
	if userID != "" {
		if u, err := app.FindRecordById("users", userID); err == nil {
			return u, nil
		}
	}
	if customerID != "" {
		if u, err := app.FindFirstRecordByFilter("users", "stripe_customer = {:c}", dbx.Params{"c": customerID}); err == nil {
			return u, nil
		}
	}
	return nil, fmt.Errorf("no user for customer %q", customerID)
}

// apply puts the user on the plan the subscription pays for, or back on the default plan when it is not active
func apply(app core.App, s *subscription) (*core.Record, string, error) {
	u, err := userFor(app, s.Customer, s.Metadata.User)
	if err != nil {
		return nil, "", err
	}
	period := s.Period
	priceID := ""
	if len(s.Items.Data) > 0 {
		priceID = s.Items.Data[0].Price.ID
		if period == 0 {
			period = s.Items.Data[0].Period
		}
	}
	u.Set("stripe_customer", s.Customer)
	u.Set("stripe_subscription", s.ID)
	u.Set("subscription_status", s.Status)
	if period > 0 {
		u.Set("current_period_end", time.Unix(period, 0).UTC())
	}
	planName := ""
	if Active[s.Status] || s.Status == "past_due" {
		p, err := app.FindFirstRecordByFilter("plans", "stripe_price = {:p}", dbx.Params{"p": priceID})
		if err != nil && s.Metadata.Plan != "" {
			p, err = app.FindRecordById("plans", s.Metadata.Plan)
		}
		if err == nil {
			u.Set("plan", p.Id)
			planName = p.GetString("name")
		}
	} else {
		if d, err := app.FindFirstRecordByFilter("plans", "default = true && active = true"); err == nil {
			u.Set("plan", d.Id)
			planName = d.GetString("name")
		} else {
			u.Set("plan", "")
		}
	}
	return u, planName, app.SaveNoValidate(u)
}
