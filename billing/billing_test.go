package billing_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lucky-edgelord/jade-agent-base/billing"
	"github.com/lucky-edgelord/jade-agent-base/jab"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	_ "github.com/pocketbase/pocketbase/migrations"
)

type fakeStripe struct {
	mu    sync.Mutex
	forms map[string]url.Values
	auth  string
	subs  map[string]string // subscription id → JSON
}

func (f *fakeStripe) form(p string) url.Values { f.mu.Lock(); defer f.mu.Unlock(); return f.forms[p] }

func newStripe(t *testing.T) *fakeStripe {
	f := &fakeStripe{forms: map[string]url.Values{}, subs: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.forms[r.URL.Path], f.auth = r.PostForm, r.Header.Get("Authorization")
		f.mu.Unlock()
		switch {
		case r.URL.Path == "/v1/customers":
			fmt.Fprint(w, `{"id":"cus_1"}`)
		case r.URL.Path == "/v1/products":
			fmt.Fprint(w, `{"id":"prod_1"}`)
		case r.URL.Path == "/v1/prices":
			fmt.Fprint(w, `{"id":"price_pro"}`)
		case r.URL.Path == "/v1/checkout/sessions":
			fmt.Fprint(w, `{"id":"cs_1","url":"https://checkout.stripe.test/cs_1"}`)
		case r.URL.Path == "/v1/billing_portal/sessions":
			fmt.Fprint(w, `{"url":"https://billing.stripe.test/p_1"}`)
		case strings.HasPrefix(r.URL.Path, "/v1/subscriptions/"):
			f.mu.Lock()
			s := f.subs[strings.TrimPrefix(r.URL.Path, "/v1/subscriptions/")]
			f.mu.Unlock()
			fmt.Fprint(w, s)
		default:
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":{"message":"unknown"}}`)
		}
	}))
	t.Cleanup(srv.Close)
	billing.APIBase = srv.URL
	return f
}

type env struct {
	t   *testing.T
	app *core.BaseApp
	srv *httptest.Server
	su  string
}

func setup(t *testing.T) *env {
	dir := t.TempDir()
	app := core.NewBaseApp(core.BaseAppConfig{DataDir: filepath.Join(dir, "pb_data")})
	if err := jab.Setup(app, jab.Options{Modules: []string{"billing"}}); err != nil {
		t.Fatal(err)
	}
	if err := app.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	app.Settings().Logs.MaxDays = 0
	app.Settings().Meta.AppURL = "https://shop.test"
	sucol, _ := app.FindCollectionByNameOrId(core.CollectionNameSuperusers)
	su := core.NewRecord(sucol)
	su.SetEmail("root@example.com")
	su.SetPassword("1234567890")
	_ = app.Save(su)
	tok, _ := su.NewAuthToken()
	router, _ := apis.NewRouter(app)
	var mux http.Handler
	_ = app.OnServe().Trigger(&core.ServeEvent{App: app, Router: router}, func(e *core.ServeEvent) error {
		m, err := e.Router.BuildMux()
		mux = m
		return err
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(func() { srv.Close(); app.ResetBootstrapState() })
	return &env{t, app, srv, tok}
}

func (e *env) json(method, path string, hdr map[string]string, body any) (int, map[string]any) {
	e.t.Helper()
	var r io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		r = bytes.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		r = bytes.NewReader(j)
	}
	q, _ := http.NewRequest(method, e.srv.URL+path, r)
	q.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		q.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(q)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	var m map[string]any
	raw, _ := io.ReadAll(res.Body)
	_ = json.Unmarshal(raw, &m)
	return res.StatusCode, m
}

func (e *env) tool(name string, args map[string]any) any {
	e.t.Helper()
	code, m := e.json("POST", "/api/agent/tools/"+name, map[string]string{"Authorization": e.su}, args)
	if code != 200 {
		e.t.Fatalf("%s: %d %v", name, code, m)
	}
	return m["result"]
}

func (e *env) event(id, typ string, obj map[string]any, secret string) (int, map[string]any) {
	payload, _ := json.Marshal(map[string]any{"id": id, "type": typ, "data": map[string]any{"object": obj}})
	return e.json("POST", "/api/billing/webhook", map[string]string{"Stripe-Signature": billing.Sign(payload, secret, time.Now())}, payload)
}

func sub(status string) map[string]any {
	return map[string]any{"id": "sub_1", "customer": "cus_1", "status": status,
		"items": map[string]any{"data": []any{map[string]any{"price": map[string]any{"id": "price_pro"}, "current_period_end": time.Now().Add(30 * 24 * time.Hour).Unix()}}}}
}

func TestStripe(t *testing.T) {
	st := newStripe(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"message":{"content":"ok"},"done":true,"prompt_eval_count":1,"eval_count":1}`)
	}))
	defer up.Close()
	e := setup(t)
	// billing switched the gateway on with it
	if code, _ := e.json("GET", "/v1/models", map[string]string{"Authorization": e.su}, nil); code != 200 {
		t.Fatalf("gateway not on with billing: %d", code)
	}
	if s := e.tool("billing_status", nil).(map[string]any); s["stripe"] != "not connected" {
		t.Fatalf("status before setup: %v", s)
	}
	_, m := e.json("POST", "/api/agent/tools/billing_setup", map[string]string{"Authorization": e.su}, map[string]any{"secret_key": "pk_nope"})
	if !strings.Contains(fmt.Sprint(m), "starts with sk_") {
		t.Fatalf("publishable key accepted: %v", m)
	}
	s := e.tool("billing_setup", map[string]any{"secret_key": "sk_test_123", "webhook_secret": "whsec_abc", "currency": "eur"}).(map[string]any)
	if s["stripe"] != "test" || s["webhook_url"] != "https://shop.test/api/billing/webhook" || strings.Contains(fmt.Sprint(s), "sk_test_123") {
		t.Fatalf("billing_setup: %v", s)
	}

	// an agent prices a plan and puts it on sale
	e.tool("plan_set", map[string]any{"name": "pro", "price_month": 19.99, "monthly_tokens": 1000000})
	e.tool("provider_set", map[string]any{"name": "box", "type": "ollama", "base_url": up.URL})
	e.tool("model_set", map[string]any{"name": "big", "provider": "box", "kind": "chat", "plans": []any{"pro"}})
	e.tool("model_set", map[string]any{"name": "small", "provider": "box", "kind": "chat"})
	pub := e.tool("plan_publish", map[string]any{"plan": "pro"}).(map[string]any)
	if pub["price"] != "price_pro" || st.form("/v1/prices").Get("unit_amount") != "1999" || st.form("/v1/prices").Get("currency") != "eur" || st.form("/v1/prices").Get("recurring[interval]") != "month" || st.auth != "Bearer sk_test_123" {
		t.Fatalf("plan_publish: %v %v", pub, st.form("/v1/prices"))
	}

	// a user subscribes through Checkout
	e.json("POST", "/api/collections/users/records", nil, map[string]any{"email": "ada@example.com", "password": "password123", "passwordConfirm": "password123"})
	_, a := e.json("POST", "/api/collections/users/auth-with-password", nil, map[string]any{"identity": "ada@example.com", "password": "password123"})
	tok := a["token"].(string)
	user := map[string]string{"Authorization": tok}
	code, co := e.json("POST", "/api/billing/checkout", user, map[string]any{"plan": "pro"})
	f := st.form("/v1/checkout/sessions")
	if code != 200 || co["url"] != "https://checkout.stripe.test/cs_1" || f.Get("line_items[0][price]") != "price_pro" || f.Get("customer") != "cus_1" || f.Get("mode") != "subscription" || f.Get("success_url") != "https://shop.test/?billing=success" {
		t.Fatalf("checkout: %d %v %v", code, co, f)
	}
	if code, _ := e.json("PATCH", "/api/collections/users/records/"+a["record"].(map[string]any)["id"].(string), user, map[string]any{"subscription_status": "active"}); code == 200 {
		t.Fatal("a user set their own subscription status")
	}
	_, k := e.json("POST", "/api/gateway/keys", user, map[string]any{})
	key := map[string]string{"Authorization": "Bearer " + k["key"].(string)}
	chat := func(model string) (int, map[string]any) {
		return e.json("POST", "/v1/chat/completions", key, map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	}
	if code, _ := chat("big"); code != 403 {
		t.Fatalf("big before paying: %d", code)
	}

	// Stripe confirms: checkout completed → the server fetches the subscription and moves the user to pro
	st.mu.Lock()
	b, _ := json.Marshal(sub("active"))
	st.subs["sub_1"] = string(b)
	st.mu.Unlock()
	if code, m := e.event("evt_bad", "checkout.session.completed", map[string]any{}, "whsec_wrong"); code != 400 || !strings.Contains(fmt.Sprint(m), "signature") {
		t.Fatalf("bad signature: %d %v", code, m)
	}
	if code, m := e.event("evt_1", "checkout.session.completed", map[string]any{"customer": "cus_1", "subscription": "sub_1", "client_reference_id": a["record"].(map[string]any)["id"]}, "whsec_abc"); code != 200 {
		t.Fatalf("checkout.session.completed: %d %v", code, m)
	}
	_, me := e.json("GET", "/api/billing/me", user, nil)
	if me["plan"] != "pro" || me["status"] != "active" {
		t.Fatalf("after paying: %v", me)
	}
	if code, m := chat("big"); code != 200 {
		t.Fatalf("big after paying: %d %v", code, m)
	}
	if code, m := e.event("evt_1", "checkout.session.completed", map[string]any{}, "whsec_abc"); code != 200 || m["duplicate"] != true {
		t.Fatalf("a repeated event: %d %v", code, m)
	}
	if bs := e.tool("billing_status", nil).(map[string]any); bs["mrr"].(float64) != 19.99 {
		t.Fatalf("mrr: %v", bs)
	}
	code, pm := e.json("POST", "/api/billing/portal", user, nil)
	if code != 200 || pm["url"] != "https://billing.stripe.test/p_1" {
		t.Fatalf("portal: %d %v", code, pm)
	}

	// a payment fails: past_due keeps the plan but paid models stop until it is paid
	e.event("evt_2", "customer.subscription.updated", sub("past_due"), "whsec_abc")
	if code, m := chat("big"); code != 402 || !strings.Contains(fmt.Sprint(m), "active subscription") {
		t.Fatalf("past_due: %d %v", code, m)
	}
	e.event("evt_3", "customer.subscription.updated", sub("active"), "whsec_abc")
	if code, _ := chat("big"); code != 200 {
		t.Fatalf("paid again: %d", code)
	}
	// cancelled: back to the free plan
	e.event("evt_4", "customer.subscription.deleted", sub("active"), "whsec_abc")
	_, me = e.json("GET", "/api/billing/me", user, nil)
	if me["plan"] != "free" || me["status"] != "canceled" {
		t.Fatalf("after cancelling: %v", me)
	}
	if code, _ := chat("small"); code != 200 {
		t.Fatalf("free models after cancelling: %d", code)
	}
	evs := e.tool("billing_events", nil).([]any)
	if len(evs) != 4 || !strings.Contains(fmt.Sprint(evs), "canceled, now on free") {
		t.Fatalf("billing_events: %v", evs)
	}
	subs := e.tool("subscribers_list", nil).([]any)
	if len(subs) != 1 {
		t.Fatalf("subscribers_list: %v", subs)
	}

	// billing off: the subscription check stops, its routes answer 404
	e.tool("module_disable", map[string]any{"name": "billing"})
	if code, _ := e.json("POST", "/api/billing/checkout", user, map[string]any{"plan": "pro"}); code != 404 {
		t.Fatalf("billing routes while off: %d", code)
	}
}
