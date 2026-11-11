package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// a tiny OpenID Connect server: any code gives Ada's account
func fakeOIDC(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=good&state="+q.Get("state"), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("code") != "good" {
			http.Error(w, `{"error":"invalid_grant"}`, 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"at-1","token_type":"Bearer","expires_in":3600}`))
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at-1" {
			http.Error(w, "no", 401)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"sub": "ada-1", "email": "ada@example.com", "email_verified": true, "name": "Ada Lovelace"})
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func TestOAuth2Providers(t *testing.T) {
	e := setup(t)
	idp := fakeOIDC(t)
	_, m, _ := e.req("POST", "/api/agent/agents", e.su, map[string]any{"name": "Builder", "scopes": []string{"read", "write", "schema", "settings"}})
	tok := m["token"].(string)

	info := e.tool(tok, "auth_providers", nil).(map[string]any)
	if info["oauth2"] != false || !strings.Contains(mustJSON(info["available"]), `"google"`) || info["note"] == nil {
		t.Fatalf("auth_providers: %v", info)
	}
	if code, msg := e.toolErr(tok, "auth_provider_set", map[string]any{"name": "myspace", "client_id": "x", "client_secret": "y"}); code == 200 || !strings.Contains(msg, "unknown provider") {
		t.Fatalf("unknown provider: %d %s", code, msg)
	}
	if code, msg := e.toolErr(tok, "auth_provider_set", map[string]any{"name": "google", "client_id": "x"}); code == 200 || !strings.Contains(msg, "client_secret") {
		t.Fatalf("no secret: %d %s", code, msg)
	}
	t.Setenv("TEST_GOOGLE_SECRET", "g-secret")
	e.tool(tok, "settings_update", map[string]any{"set": map[string]any{"meta": map[string]any{"appURL": "https://shop.example.com"}}})
	r := e.tool(tok, "auth_provider_set", map[string]any{"name": "google", "client_id": "g-id", "client_secret_env": "TEST_GOOGLE_SECRET"}).(map[string]any)
	if r["oauth2"] != true || r["redirect_url"] != "https://shop.example.com/api/oauth2-redirect" || r["note"] != nil {
		t.Fatalf("google: %v", r)
	}
	r = e.tool(tok, "auth_provider_set", map[string]any{"name": "oidc", "client_id": "o-id", "client_secret": "o-secret", "display_name": "Acme ID",
		"auth_url": idp.URL + "/authorize", "token_url": idp.URL + "/token", "user_info_url": idp.URL + "/userinfo", "pkce": false}).(map[string]any)
	// secrets never come back, from these tools or the collection tools
	for _, v := range []any{r, e.tool(tok, "collection_get", map[string]any{"name": "users"}), e.tool(tok, "collections_list", nil)} {
		if s := mustJSON(v); strings.Contains(s, "g-secret") || strings.Contains(s, "o-secret") {
			t.Fatalf("a secret leaked: %s", s)
		}
	}
	if !strings.Contains(mustJSON(r["providers"]), `"has_secret":true`) {
		t.Fatalf("providers: %v", r["providers"])
	}
	// the public sign-in methods list both
	_, am, _ := e.req("GET", "/api/collections/users/auth-methods", "", nil)
	if s := mustJSON(am); !strings.Contains(s, `"google"`) || !strings.Contains(s, `"Acme ID"`) || !strings.Contains(s, `"password"`) {
		t.Fatalf("auth-methods: %s", s)
	}
	// sign in with the OIDC server: a new user with the provider's name and email
	code, a, _ := e.req("POST", "/api/collections/users/auth-with-oauth2", "", map[string]any{"provider": "oidc", "code": "good", "codeVerifier": "v", "redirectURL": "https://shop.example.com/api/oauth2-redirect"})
	if code != 200 || a["token"] == "" {
		t.Fatalf("oauth2 sign-in: %d %v", code, a)
	}
	rec := a["record"].(map[string]any)
	if rec["email"] != "ada@example.com" || rec["name"] != "Ada Lovelace" || rec["verified"] != true {
		t.Fatalf("user: %v", rec)
	}
	if code, _, _ := e.req("POST", "/api/collections/users/auth-with-oauth2", "", map[string]any{"provider": "oidc", "code": "bad", "codeVerifier": "v", "redirectURL": "https://x/api/oauth2-redirect"}); code == 200 {
		t.Fatal("a bad code signed in")
	}
	// changing a provider keeps its secret; removing turns OAuth2 off when none are left
	e.tool(tok, "auth_provider_set", map[string]any{"name": "google", "display_name": "Google"})
	col, _ := e.app.FindCollectionByNameOrId("users")
	for _, p := range col.OAuth2.Providers {
		if p.Name == "google" && (p.ClientSecret != "g-secret" || p.DisplayName != "Google") {
			t.Fatalf("google after change: %+v", p)
		}
	}
	e.tool(tok, "auth_provider_remove", map[string]any{"name": "google"})
	if r := e.tool(tok, "auth_provider_remove", map[string]any{"name": "oidc"}).(map[string]any); r["oauth2"] != false {
		t.Fatalf("after removing all: %v", r)
	}
	// a read-only agent can look but not change
	_, m, _ = e.req("POST", "/api/agent/agents", e.su, map[string]any{"name": "Reader", "scopes": []string{"read"}})
	e.tool(m["token"].(string), "auth_providers", nil)
	if code, _ := e.toolErr(m["token"].(string), "auth_provider_set", map[string]any{"name": "github", "client_id": "a", "client_secret": "b"}); code == 200 {
		t.Fatal("a reader added a provider")
	}
}

func TestRateLimits(t *testing.T) {
	e := setup(t)
	_, m, _ := e.req("POST", "/api/agent/agents", e.su, map[string]any{"name": "Builder", "scopes": []string{"read", "write", "schema", "settings"}})
	tok := m["token"].(string)
	if r := e.tool(tok, "rate_limits", nil).(map[string]any); r["enabled"] != false {
		t.Fatalf("default: %v", r)
	}
	r := e.tool(tok, "rate_limits", map[string]any{"enabled": true, "rules": []any{map[string]any{"label": "*:create", "max_requests": 2, "duration": 60, "audience": "@guest"}}}).(map[string]any)
	if r["enabled"] != true || !strings.Contains(mustJSON(r["rules"]), `"max_requests":2`) {
		t.Fatalf("set: %v", r)
	}
	e.tool(tok, "collection_create", map[string]any{"name": "waitlist", "fields": []any{map[string]any{"name": "email", "type": "email"}}, "createRule": ""})
	codes := []int{}
	for i := 0; i < 3; i++ {
		c, _, _ := e.req("POST", "/api/collections/waitlist/records", "", map[string]any{"email": "a@b.co"})
		codes = append(codes, c)
	}
	if codes[0] != 200 || codes[1] != 200 || codes[2] != 429 {
		t.Fatalf("limited creates: %v", codes)
	}
	if code, _ := e.toolErr(tok, "rate_limits", map[string]any{"rules": []any{map[string]any{"label": "bad label!", "max_requests": 1, "duration": 1}}}); code == 200 {
		t.Fatal("a bad rule was saved")
	}
	r = e.tool(tok, "rate_limits", map[string]any{"defaults": true, "remove": []any{"/api/batch"}}).(map[string]any)
	if s := mustJSON(r["rules"]); strings.Contains(s, "/api/batch") || !strings.Contains(s, "*:auth") {
		t.Fatalf("defaults: %s", s)
	}
}
