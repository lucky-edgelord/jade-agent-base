package agent

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/auth"
)

// Sign-in with other accounts (OAuth2: Google, Facebook, GitHub, Apple, Microsoft, Discord, … and any OpenID Connect
// server). PocketBase does the OAuth2 flow; these tools let an agent turn providers on and off for an auth collection.
// Client secrets are write-only: they are stored, never returned.

func authTools() {
	AddTool(&Tool{Name: "auth_providers", Scope: ScopeRead,
		Description: "How people can sign in to an auth collection (default users): password, and which OAuth2 providers (Google, Facebook, GitHub, …) are on. Also lists every provider that can be added and the redirect URL to register with them.",
		Parameters:  obj(map[string]any{"collection": str("auth collection (default users)")}),
		Run: func(c *Call) (any, error) {
			col, err := authCollection(c)
			if err != nil {
				return nil, err
			}
			return authInfo(c.App, col), nil
		}})
	AddTool(&Tool{Name: "auth_provider_set", Scope: ScopeSettings,
		Description: "Let people sign in with an OAuth2 provider (google, facebook, github, apple, microsoft, discord, gitlab, twitter, oidc for any OpenID Connect server, …: auth_providers lists them). " +
			"Create an OAuth app at the provider and register the redirect URL that auth_providers gives (the app URL, set with settings_update meta.appURL, + /api/oauth2-redirect). " +
			"The secret is write-only: pass client_secret, or client_secret_env to read it from this server's environment. Changing a provider keeps its secret when you leave it out.",
		Parameters: obj(map[string]any{
			"name": str("provider: google, facebook, github, apple, microsoft, discord, oidc, …"), "client_id": str("the OAuth app's client id"),
			"client_secret": str("the OAuth app's client secret (write-only)"), "client_secret_env": str("or: an environment variable of this server that holds it"),
			"client_id_env": str("or: an environment variable that holds the client id"),
			"display_name":  str("the button text (default: the provider's name)"), "collection": str("auth collection (default users)"),
			"auth_url": str("oidc and self-hosted providers: the authorize URL"), "token_url": str("oidc: the token URL"), "user_info_url": str("oidc: the userinfo URL"),
			"pkce": boo("override PKCE (rarely needed)"), "extra": objp("provider extras (e.g. apple: {\"jwksURL\": …})"),
			"password": boo("also turn password sign-in on (true) or off (false)"),
		}, "name"),
		Run: func(c *Call) (any, error) {
			col, err := authCollection(c)
			if err != nil {
				return nil, err
			}
			name := strings.ToLower(strings.TrimSpace(c.Str("name")))
			if _, ok := auth.Providers[name]; !ok {
				return nil, fmt.Errorf("unknown provider %q; one of: %s", name, strings.Join(providerNames(), ", "))
			}
			id, secret := c.Str("client_id"), c.Str("client_secret")
			if e := c.Str("client_id_env"); e != "" && id == "" {
				if id = os.Getenv(e); id == "" {
					return nil, fmt.Errorf("the environment variable %s is empty on the server", e)
				}
			}
			if e := c.Str("client_secret_env"); e != "" && secret == "" {
				if secret = os.Getenv(e); secret == "" {
					return nil, fmt.Errorf("the environment variable %s is empty on the server", e)
				}
			}
			i := -1
			for k, p := range col.OAuth2.Providers {
				if p.Name == name {
					i = k
				}
			}
			if i < 0 {
				if id == "" || secret == "" {
					return nil, fmt.Errorf("a new provider needs client_id and client_secret (or client_secret_env)")
				}
				col.OAuth2.Providers = append(col.OAuth2.Providers, core.OAuth2ProviderConfig{Name: name})
				i = len(col.OAuth2.Providers) - 1
			}
			p := &col.OAuth2.Providers[i]
			if id != "" {
				p.ClientId = id
			}
			if secret != "" {
				p.ClientSecret = secret
			}
			for k, dst := range map[string]*string{"display_name": &p.DisplayName, "auth_url": &p.AuthURL, "token_url": &p.TokenURL, "user_info_url": &p.UserInfoURL} {
				if v, ok := c.Args[k].(string); ok {
					*dst = v
				}
			}
			if v, ok := c.Args["pkce"].(bool); ok {
				p.PKCE = &v
			}
			if m := c.Map("extra"); m != nil {
				p.Extra = m
			}
			if v, ok := c.Args["password"].(bool); ok {
				col.PasswordAuth.Enabled = v
			}
			col.OAuth2.Enabled = true
			mapFields(col)
			if err := c.App.Save(col); err != nil {
				return nil, explain(err)
			}
			return authInfo(c.App, col), nil
		}})
	AddTool(&Tool{Name: "auth_provider_remove", Scope: ScopeSettings, Destructive: true,
		Description: "Stop sign-in with an OAuth2 provider (its settings and secret are deleted). Accounts made with it stay; they can reset a password to sign in.",
		Parameters:  obj(map[string]any{"name": str("provider"), "collection": str("auth collection (default users)")}, "name"),
		Run: func(c *Call) (any, error) {
			col, err := authCollection(c)
			if err != nil {
				return nil, err
			}
			name, kept := strings.ToLower(c.Str("name")), []core.OAuth2ProviderConfig{}
			for _, p := range col.OAuth2.Providers {
				if p.Name != name {
					kept = append(kept, p)
				}
			}
			if len(kept) == len(col.OAuth2.Providers) {
				return nil, fmt.Errorf("%s is not on", name)
			}
			col.OAuth2.Providers = kept
			col.OAuth2.Enabled = len(kept) > 0
			if err := c.App.Save(col); err != nil {
				return nil, explain(err)
			}
			return authInfo(c.App, col), nil
		}})
	AddTool(&Tool{Name: "rate_limits", Scope: ScopeSettings,
		Description: "The API rate limiter (per client IP): see it, turn it on or off, add or change rules. With no arguments it only shows the current setup. " +
			"A rule's label is a path prefix (/api/, /api/collections/posts/records) or a tag: *:auth (sign-ins), *:create, posts:create, *:list, …; " +
			"audience is \"\" (everyone), @guest or @auth. A rule replaces any with the same label for the same audience (or for everyone). The AI gateway's per-plan limits (rpm, monthly tokens) are separate: plan_set.",
		Parameters: obj(map[string]any{"enabled": boo("turn the limiter on or off"),
			"rules":    arr("rules to add or change", obj(map[string]any{"label": str("path prefix or tag"), "max_requests": num("requests allowed"), "duration": num("per this many seconds"), "audience": str(`"", "@guest" or "@auth"`)}, "label", "max_requests", "duration")),
			"remove":   arr("labels of rules to remove", str("label")),
			"defaults": boo("start again from the recommended rules")}),
		Run: func(c *Call) (any, error) {
			if len(c.Args) == 0 {
				return rateInfo(c.App), nil
			}
			s, err := c.App.Settings().Clone()
			if err != nil {
				return nil, err
			}
			rl := &s.RateLimits
			if c.Bool("defaults") {
				rl.Rules = []core.RateLimitRule{{Label: "*:auth", MaxRequests: 2, Duration: 3}, {Label: "*:create", MaxRequests: 20, Duration: 5},
					{Label: "/api/batch", MaxRequests: 3, Duration: 1}, {Label: "/api/", MaxRequests: 300, Duration: 10}}
			}
			for _, x := range c.List("remove") {
				kept := rl.Rules[:0]
				for _, r := range rl.Rules {
					if r.Label != fmt.Sprint(x) {
						kept = append(kept, r)
					}
				}
				rl.Rules = kept
			}
			for _, x := range c.List("rules") {
				m, _ := x.(map[string]any)
				sub := &Call{Args: m}
				r := core.RateLimitRule{Label: sub.Str("label"), MaxRequests: sub.Int("max_requests", 0), Duration: int64(sub.Int("duration", 0)), Audience: sub.Str("audience")}
				// a rule replaces those it would clash with: the same label for the same people (or for everyone)
				kept := rl.Rules[:0]
				for _, o := range rl.Rules {
					if o.Label != r.Label || (o.Audience != r.Audience && o.Audience != "" && r.Audience != "") {
						kept = append(kept, o)
					}
				}
				rl.Rules = append(kept, r)
			}
			if v, ok := c.Args["enabled"].(bool); ok {
				rl.Enabled = v
			}
			if err := c.App.Save(s); err != nil {
				return nil, explain(err)
			}
			return rateInfo(c.App), nil
		}})
}

func rateInfo(app core.App) map[string]any {
	rl := app.Settings().RateLimits
	rules := []map[string]any{}
	for _, r := range rl.Rules {
		rules = append(rules, map[string]any{"label": r.Label, "max_requests": r.MaxRequests, "duration": r.Duration, "audience": r.Audience})
	}
	return map[string]any{"enabled": rl.Enabled, "rules": rules}
}

func authCollection(c *Call) (*core.Collection, error) {
	n := c.Str("collection")
	if n == "" {
		n = "users"
	}
	col, err := c.App.FindCollectionByNameOrId(n)
	if err != nil {
		return nil, fmt.Errorf("no collection %q", n)
	}
	if !col.IsAuth() || col.Name == core.CollectionNameSuperusers {
		return nil, fmt.Errorf("%s is not an auth collection people sign in to", n)
	}
	return col, nil
}

// fill the user's name and avatar from the provider when the collection has those fields
func mapFields(col *core.Collection) {
	m := &col.OAuth2.MappedFields
	if m.Name == "" && col.Fields.GetByName("name") != nil {
		m.Name = "name"
	}
	if m.AvatarURL == "" {
		if f, ok := col.Fields.GetByName("avatar").(*core.FileField); ok && f != nil {
			m.AvatarURL = "avatar"
		}
	}
}

func providerNames() []string {
	l := make([]string, 0, len(auth.Providers))
	for k := range auth.Providers {
		l = append(l, k)
	}
	sort.Strings(l)
	return l
}

func authInfo(app core.App, col *core.Collection) map[string]any {
	on := []map[string]any{}
	for _, p := range col.OAuth2.Providers {
		d := map[string]any{"name": p.Name, "client_id": p.ClientId, "has_secret": p.ClientSecret != "", "display_name": p.DisplayName}
		if p.AuthURL != "" {
			d["auth_url"] = p.AuthURL
		}
		on = append(on, d)
	}
	appURL := strings.TrimRight(app.Settings().Meta.AppURL, "/")
	out := map[string]any{"collection": col.Name, "password": col.PasswordAuth.Enabled, "oauth2": col.OAuth2.Enabled, "providers": on,
		"available": providerNames(), "redirect_url": appURL + "/api/oauth2-redirect"}
	if appURL == "" || strings.Contains(appURL, "localhost") || strings.Contains(appURL, "127.0.0.1") {
		out["note"] = "set the public app URL first (settings_update {\"set\": {\"meta\": {\"appURL\": \"https://…\"}}}): providers send people back to appURL/api/oauth2-redirect"
	}
	return out
}
