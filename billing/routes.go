package billing

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/pocketbase/pocketbase/core"
)

func routes(se *core.ServeEvent) {
	se.Router.POST("/api/billing/checkout", checkout)
	se.Router.POST("/api/billing/portal", portal)
	se.Router.POST("/api/billing/webhook", webhook)
	se.Router.GET("/api/billing/me", me)
}

func signedInUser(e *core.RequestEvent) (*core.Record, error) {
	if !on() {
		return nil, e.NotFoundError("billing is off", nil)
	}
	if e.Auth == nil || e.Auth.Collection().Name != "users" {
		return nil, e.UnauthorizedError("sign in first", nil)
	}
	return e.Auth, nil
}

func checkout(e *core.RequestEvent) error {
	u, err := signedInUser(e)
	if err != nil {
		return err
	}
	var b struct {
		Plan string `json:"plan"`
	}
	if err := e.BindBody(&b); err != nil || b.Plan == "" {
		return e.BadRequestError("send { plan: <name> }", nil)
	}
	p, err := e.App.FindFirstRecordByData("plans", "name", b.Plan)
	if err != nil || !p.GetBool("active") {
		return e.NotFoundError("no such plan", nil)
	}
	price := p.GetString("stripe_price")
	if price == "" {
		return e.BadRequestError(fmt.Sprintf("the %s plan is not for sale yet", b.Plan), nil)
	}
	if Active[u.GetString("subscription_status")] && u.GetString("stripe_subscription") != "" {
		return e.BadRequestError("you already have a subscription: change it in the portal (/api/billing/portal)", nil)
	}
	cust, err := customer(e.App, u)
	if err != nil {
		return e.BadRequestError(err.Error(), nil)
	}
	base := e.App.Settings().Meta.AppURL
	var s struct {
		URL string `json:"url"`
	}
	err = stripe(http.MethodPost, "/v1/checkout/sessions", map[string]any{
		"mode": "subscription", "customer": cust, "client_reference_id": u.Id,
		"line_items":            []any{map[string]any{"price": price, "quantity": 1}},
		"success_url":           setting("success_url", base+"/?billing=success"),
		"cancel_url":            setting("cancel_url", base+"/?billing=cancel"),
		"metadata":              map[string]any{"user": u.Id, "plan": p.Id},
		"subscription_data":     map[string]any{"metadata": map[string]any{"user": u.Id, "plan": p.Id}},
		"allow_promotion_codes": true,
	}, &s)
	if err != nil {
		return e.BadRequestError(err.Error(), nil)
	}
	return e.JSON(http.StatusOK, map[string]any{"url": s.URL})
}

func portal(e *core.RequestEvent) error {
	u, err := signedInUser(e)
	if err != nil {
		return err
	}
	if u.GetString("stripe_customer") == "" {
		return e.BadRequestError("no billing account yet: subscribe first", nil)
	}
	var s struct {
		URL string `json:"url"`
	}
	if err := stripe(http.MethodPost, "/v1/billing_portal/sessions", map[string]any{"customer": u.GetString("stripe_customer"), "return_url": setting("return_url", e.App.Settings().Meta.AppURL+"/")}, &s); err != nil {
		return e.BadRequestError(err.Error(), nil)
	}
	return e.JSON(http.StatusOK, map[string]any{"url": s.URL})
}

func me(e *core.RequestEvent) error {
	u, err := signedInUser(e)
	if err != nil {
		return err
	}
	plan := ""
	if p, err := e.App.FindRecordById("plans", u.GetString("plan")); err == nil {
		plan = p.GetString("name")
	}
	return e.JSON(http.StatusOK, map[string]any{"plan": plan, "status": u.GetString("subscription_status"), "current_period_end": u.GetString("current_period_end"), "has_billing_account": u.GetString("stripe_customer") != ""})
}

// Stripe → us. Each event is applied once.
func webhook(e *core.RequestEvent) error {
	if !on() {
		return e.NotFoundError("billing is off", nil)
	}
	payload, err := io.ReadAll(io.LimitReader(e.Request.Body, 1<<20))
	if err != nil {
		return e.BadRequestError("", nil)
	}
	if err := verify(payload, e.Request.Header.Get("Stripe-Signature"), webhookSecret(), 5*time.Minute); err != nil {
		return e.BadRequestError("signature: "+err.Error(), nil)
	}
	var ev struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Data struct {
			Object json.RawMessage `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil || ev.ID == "" {
		return e.BadRequestError("not a Stripe event", nil)
	}
	col, err := e.App.FindCollectionByNameOrId("billing_events")
	if err != nil {
		return e.InternalServerError("", err)
	}
	if _, err := e.App.FindFirstRecordByData("billing_events", "event", ev.ID); err == nil {
		return e.JSON(http.StatusOK, map[string]any{"received": true, "duplicate": true})
	}
	userID, summary, err := handle(e.App, ev.Type, ev.Data.Object)
	if err != nil {
		e.App.Logger().Warn("billing webhook", "event", ev.ID, "type", ev.Type, "error", err)
		return e.BadRequestError(err.Error(), nil) // Stripe retries
	}
	r := core.NewRecord(col)
	r.Set("event", ev.ID)
	r.Set("type", ev.Type)
	r.Set("user", userID)
	r.Set("summary", summary)
	if err := e.App.Save(r); err != nil {
		return e.InternalServerError("", err)
	}
	return e.JSON(http.StatusOK, map[string]any{"received": true})
}

func handle(app core.App, typ string, obj json.RawMessage) (user, summary string, err error) {
	switch typ {
	case "checkout.session.completed":
		var s struct {
			Customer     string `json:"customer"`
			Subscription string `json:"subscription"`
			ClientRef    string `json:"client_reference_id"`
		}
		if err := json.Unmarshal(obj, &s); err != nil {
			return "", "", err
		}
		u, err := userFor(app, s.Customer, s.ClientRef)
		if err != nil {
			return "", "", err
		}
		u.Set("stripe_customer", s.Customer)
		if s.Subscription != "" {
			var sub subscription
			if err := stripe(http.MethodGet, "/v1/subscriptions/"+s.Subscription, nil, &sub); err != nil {
				return "", "", err
			}
			_, plan, err := apply(app, &sub)
			if err != nil {
				return "", "", err
			}
			return u.Id, fmt.Sprintf("%s subscribed: %s (%s)", u.Email(), plan, sub.Status), nil
		}
		return u.Id, u.Email() + " finished checkout", app.SaveNoValidate(u)
	case "customer.subscription.created", "customer.subscription.updated", "customer.subscription.deleted", "customer.subscription.paused", "customer.subscription.resumed":
		var sub subscription
		if err := json.Unmarshal(obj, &sub); err != nil {
			return "", "", err
		}
		if typ == "customer.subscription.deleted" {
			sub.Status = "canceled"
		}
		u, plan, err := apply(app, &sub)
		if err != nil {
			return "", "", err
		}
		return u.Id, fmt.Sprintf("%s: %s, now on %s", u.Email(), sub.Status, plan), nil
	case "invoice.payment_failed":
		var inv struct {
			Customer string `json:"customer"`
		}
		_ = json.Unmarshal(obj, &inv)
		u, err := userFor(app, inv.Customer, "")
		if err != nil {
			return "", "ignored: " + err.Error(), nil
		}
		return u.Id, u.Email() + ": a payment failed (Stripe retries; the subscription's own event changes the plan)", nil
	}
	return "", "ignored " + typ, nil
}
