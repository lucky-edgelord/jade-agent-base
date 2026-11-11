package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	_ "github.com/pocketbase/pocketbase/migrations"
)

// a real app in a temp dir, served over HTTP with the agent module
type env struct {
	t      *testing.T
	app    *core.BaseApp
	srv    *httptest.Server
	su     string // superuser token
	hooks  string
	public string
}

func setup(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	app := core.NewBaseApp(core.BaseAppConfig{DataDir: filepath.Join(dir, "pb_data")})
	hooks, public := filepath.Join(dir, "pb_hooks"), filepath.Join(dir, "pb_public")
	Register(app, Config{HooksDir: hooks, PublicDir: public, Version: "test"})
	if err := app.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	app.Settings().Logs.MaxDays = 0
	sucol, _ := app.FindCollectionByNameOrId(core.CollectionNameSuperusers)
	su := core.NewRecord(sucol)
	su.SetEmail("root@example.com")
	su.SetPassword("1234567890")
	if err := app.Save(su); err != nil {
		t.Fatal(err)
	}
	tok, _ := su.NewAuthToken()
	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	se := &core.ServeEvent{App: app, Router: router}
	var mux http.Handler
	if err := app.OnServe().Trigger(se, func(e *core.ServeEvent) error {
		m, err := e.Router.BuildMux()
		mux = m
		return err
	}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(func() { srv.Close(); app.ResetBootstrapState() })
	return &env{t: t, app: app, srv: srv, su: tok, hooks: hooks, public: public}
}

func (e *env) req(method, path, token string, body any) (int, map[string]any, []any) {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	q, _ := http.NewRequest(method, e.srv.URL+path, r)
	q.Header.Set("Content-Type", "application/json")
	if token != "" {
		q.Header.Set("Authorization", token)
	}
	res, err := http.DefaultClient.Do(q)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	var m map[string]any
	var a []any
	if json.Unmarshal(b, &m) != nil {
		_ = json.Unmarshal(b, &a)
	}
	return res.StatusCode, m, a
}

// run a tool over REST; fail the test on an error unless wantErr
func (e *env) tool(token, name string, args map[string]any) any {
	e.t.Helper()
	code, m, _ := e.req("POST", "/api/agent/tools/"+name, token, args)
	if code != 200 {
		e.t.Fatalf("%s: %d %v", name, code, m)
	}
	return m["result"]
}
func (e *env) toolErr(token, name string, args map[string]any) (int, string) {
	e.t.Helper()
	code, m, _ := e.req("POST", "/api/agent/tools/"+name, token, args)
	s, _ := m["error"].(string)
	return code, s
}

func TestAgentsAndScopes(t *testing.T) {
	e := setup(t)
	// no token
	if code, _, _ := e.req("GET", "/api/agent/tools", "", nil); code != 401 {
		t.Fatalf("anonymous tools list: %d", code)
	}
	// only superusers create agents
	code, m, _ := e.req("POST", "/api/agent/agents", e.su, map[string]any{"name": "Reader Bot", "scopes": []string{"read"}})
	if code != 200 || m["token"] == "" {
		t.Fatalf("create agent: %d %v", code, m)
	}
	reader := m["token"].(string)
	if code, _, _ := e.req("POST", "/api/agent/agents", reader, map[string]any{"name": "x"}); code != 403 && code != 401 {
		t.Fatalf("an agent created an agent: %d", code)
	}
	if code, m, _ := e.req("POST", "/api/agent/agents", e.su, map[string]any{"name": "x", "scopes": []string{"god"}}); code != 400 || !strings.Contains(strings.ToLower(m["message"].(string)), "unknown scope") {
		t.Fatalf("unknown scope accepted: %d %v", code, m)
	}
	// the reader sees only read tools
	_, _, list := e.req("GET", "/api/agent/tools", reader, nil)
	names := map[string]bool{}
	for _, x := range list {
		names[x.(map[string]any)["name"].(string)] = true
	}
	if !names["records_list"] || !names["server_info"] || names["record_create"] || names["hook_write"] || names["collection_create"] {
		t.Fatalf("reader tools: %v", names)
	}
	// …and cannot run the others
	if code, msg := e.toolErr(reader, "record_create", map[string]any{"collection": "x", "data": map[string]any{}}); code != 403 || !strings.Contains(msg, `needs the "write" scope`) {
		t.Fatalf("scope check: %d %s", code, msg)
	}
	info := e.tool(reader, "server_info", nil).(map[string]any)
	if info["you"] != "agent Reader Bot" || len(info["scopes"].([]any)) != 1 {
		t.Fatalf("server_info: %v", info)
	}
	// openai and anthropic formats
	_, _, oa := e.req("GET", "/api/agent/tools?format=openai", reader, nil)
	if f := oa[0].(map[string]any)["function"].(map[string]any); f["parameters"] == nil {
		t.Fatalf("openai format: %v", oa[0])
	}
	// revoking ends the token
	r, _ := e.app.FindFirstRecordByData(AgentsCollection, "name", "Reader Bot")
	if code, _, _ := e.req("POST", "/api/agent/agents/"+r.Id+"/revoke", e.su, nil); code != 204 {
		t.Fatalf("revoke: %d", code)
	}
	if code, _, _ := e.req("GET", "/api/agent/tools", reader, nil); code != 401 {
		t.Fatalf("revoked token still works: %d", code)
	}
	code, m, _ = e.req("POST", "/api/agent/agents/"+r.Id+"/token", e.su, map[string]any{"days": 1})
	if code != 200 {
		t.Fatalf("new token: %d", code)
	}
	if code, _, _ := e.req("GET", "/api/agent/tools", m["token"].(string), nil); code != 200 {
		t.Fatalf("new token rejected: %d", code)
	}
	// agents cannot sign in with a password, and regular users cannot use the agent API
	if code, _, _ := e.req("POST", "/api/collections/agents/auth-with-password", "", map[string]any{"identity": r.Email(), "password": "x"}); code == 200 {
		t.Fatal("password sign-in should be off for agents")
	}
}

func TestAgentBuildsABusiness(t *testing.T) {
	e := setup(t)
	_, m, _ := e.req("POST", "/api/agent/agents", e.su, map[string]any{"name": "Shopkeeper", "scopes": []string{"read", "write", "schema", "hooks", "site", "settings"}})
	tok := m["token"].(string)

	// schema: products and orders with a relation (by collection name), public read
	e.tool(tok, "collection_create", map[string]any{"name": "products", "listRule": "", "viewRule": "",
		"fields": []any{map[string]any{"name": "title", "type": "text", "required": true}, map[string]any{"name": "price", "type": "number", "min": 0}, map[string]any{"name": "tags", "type": "select", "values": []any{"new", "sale"}, "maxSelect": 2}}})
	e.tool(tok, "collection_create", map[string]any{"name": "orders",
		"fields": []any{map[string]any{"name": "product", "type": "relation", "collectionId": "products", "maxSelect": 1, "required": true}, map[string]any{"name": "qty", "type": "number"}}})
	if _, msg := e.toolErr(tok, "collection_create", map[string]any{"name": "bad", "fields": []any{map[string]any{"name": "x", "type": "relation", "collectionId": "nope"}}}); !strings.Contains(msg, `no collection "nope"`) {
		t.Fatalf("bad relation: %s", msg)
	}
	cols := e.tool(tok, "collections_list", nil).([]any)
	found := ""
	for _, c := range cols {
		if c.(map[string]any)["name"] == "products" {
			b, _ := json.Marshal(c)
			found = string(b)
		}
	}
	if !strings.Contains(found, `"title:text*"`) || !strings.Contains(found, `"price:number"`) {
		t.Fatalf("collections_list: %s", found)
	}

	// records, with validation errors a model can read
	p := e.tool(tok, "record_create", map[string]any{"collection": "products", "data": map[string]any{"title": "Mug", "price": 12, "tags": []any{"new"}}}).(map[string]any)
	if _, msg := e.toolErr(tok, "record_create", map[string]any{"collection": "products", "data": map[string]any{"price": -1}}); !strings.Contains(msg, "title") {
		t.Fatalf("validation message should name the field: %s", msg)
	}
	e.tool(tok, "record_create", map[string]any{"collection": "orders", "data": map[string]any{"product": p["id"], "qty": 2}})
	e.tool(tok, "record_update", map[string]any{"collection": "products", "id": p["id"], "data": map[string]any{"tags+": "sale", "price": 10}})
	got := e.tool(tok, "record_get", map[string]any{"collection": "products", "id": p["id"]}).(map[string]any)
	if got["price"].(float64) != 10 || len(got["tags"].([]any)) != 2 {
		t.Fatalf("record_update: %v", got)
	}
	list := e.tool(tok, "records_list", map[string]any{"collection": "orders", "filter": "qty >= 2", "expand": "product"}).(map[string]any)
	items := list["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["expand"].(map[string]any)["product"].(map[string]any)["title"] != "Mug" {
		t.Fatalf("records_list with expand: %v", list)
	}
	if _, msg := e.toolErr(tok, "records_list", map[string]any{"collection": "orders", "filter": "nope ==="}); msg == "" {
		t.Fatal("a bad filter should explain itself")
	}
	if _, msg := e.toolErr(tok, "records_list", map[string]any{"collection": "orders", "limt": 3}); !strings.Contains(msg, `no argument "limt"`) {
		t.Fatalf("unknown argument: %s", msg)
	}

	// the public API now serves products to anyone (listRule "")
	code, pub, _ := e.req("GET", "/api/collections/products/records", "", nil)
	if code != 200 || pub["totalItems"].(float64) != 1 {
		t.Fatalf("public products: %d %v", code, pub)
	}
	if code, _, _ := e.req("GET", "/api/collections/orders/records", "", nil); code == 200 {
		// orders has no rules: superusers only
		t.Fatalf("orders should not be public")
	}

	// change the schema
	e.tool(tok, "collection_update", map[string]any{"name": "products", "set": map[string]any{"createRule": "@request.auth.id != ''"},
		"addFields": []any{map[string]any{"name": "stock", "type": "number"}}, "updateFields": []any{map[string]any{"name": "title", "max": 80}}})
	col, _ := e.app.FindCollectionByNameOrId("products")
	if col.Fields.GetByName("stock") == nil || col.CreateRule == nil || *col.CreateRule != "@request.auth.id != ''" || col.ListRule == nil || *col.ListRule != "" {
		t.Fatalf("collection_update: %v %v", col.Fields.FieldNames(), col.CreateRule)
	}
	if tf, ok := col.Fields.GetByName("title").(*core.TextField); !ok || tf.Max != 80 || !tf.Required {
		t.Fatalf("updateFields lost the field's other options: %+v", col.Fields.GetByName("title"))
	}
	if _, msg := e.toolErr(tok, "collection_delete", map[string]any{"name": "orders", "confirm": "order"}); !strings.Contains(msg, "confirm must be") {
		t.Fatalf("delete without confirm: %s", msg)
	}
	if _, msg := e.toolErr(tok, "collection_delete", map[string]any{"name": "agents", "confirm": "agents"}); !strings.Contains(msg, "cannot be deleted") {
		t.Fatalf("agents collection deletable: %s", msg)
	}

	// server-side JavaScript: syntax checked, written to pb_hooks
	if _, msg := e.toolErr(tok, "hook_write", map[string]any{"name": "shop.pb.js", "content": "routerAdd('GET', '/x', (e) => {"}); !strings.Contains(msg, "syntax error") {
		t.Fatalf("hook syntax check: %s", msg)
	}
	if _, err := os.Stat(filepath.Join(e.hooks, "shop.pb.js")); err == nil {
		t.Fatal("a broken hook was saved")
	}
	e.tool(tok, "hook_write", map[string]any{"name": "shop.pb.js", "content": "routerAdd('GET', '/api/shop/hello', (e) => e.json(200, { hi: 1 }))"})
	if b, _ := os.ReadFile(filepath.Join(e.hooks, "shop.pb.js")); !strings.Contains(string(b), "routerAdd") {
		t.Fatal("hook not written")
	}
	if _, msg := e.toolErr(tok, "hook_write", map[string]any{"name": "../evil.pb.js", "content": "x"}); msg == "" {
		t.Fatal("hook path escape")
	}
	if l := e.tool(tok, "hooks_list", nil).([]any); len(l) != 1 {
		t.Fatalf("hooks_list: %v", l)
	}

	// the static site
	e.tool(tok, "site_write", map[string]any{"path": "index.html", "content": "<h1>Mugs</h1>"})
	e.tool(tok, "site_write", map[string]any{"path": "img/dot.png", "base64": "iVBORw0KGgo="})
	for _, bad := range []string{"../x.html", "/etc/../../x", ".env", "a/.git/config", ""} {
		if _, msg := e.toolErr(tok, "site_write", map[string]any{"path": bad, "content": "x"}); msg == "" {
			t.Fatalf("site path %q was accepted", bad)
		}
	}
	if s := e.tool(tok, "site_read", map[string]any{"path": "index.html"}); s != "<h1>Mugs</h1>" {
		t.Fatalf("site_read: %v", s)
	}
	if s := e.tool(tok, "site_read", map[string]any{"path": "img/dot.png"}).(map[string]any); s["base64"] != "iVBORw0KGgo=" {
		t.Fatalf("binary site_read: %v", s)
	}
	if l := e.tool(tok, "site_list", nil).([]any); len(l) != 2 {
		t.Fatalf("site_list: %v", l)
	}

	// settings: secrets blanked, changes merged
	e.app.Settings().SMTP.Password = "hunter2"
	s := e.tool(tok, "settings_update", map[string]any{"set": map[string]any{"meta": map[string]any{"appName": "Mug Shop"}}}).(map[string]any)
	if s["meta"].(map[string]any)["appName"] != "Mug Shop" || strings.Contains(mustJSON(s), "hunter2") {
		t.Fatalf("settings_update: %v", s["meta"])
	}
	if e.app.Settings().SMTP.Password != "hunter2" {
		t.Fatal("settings_update wiped a secret it did not set")
	}

	// the audit log saw everything, with secrets redacted
	e.tool(tok, "record_create", map[string]any{"collection": "products", "data": map[string]any{"title": "Key", "price": 1, "api_key": "sk-123"}})
	au := e.tool(tok, "audit_list", map[string]any{"tool": "record_create", "limit": 1}).([]any)
	if a := mustJSON(au); !strings.Contains(a, "[redacted]") || strings.Contains(a, "sk-123") || !strings.Contains(a, "Shopkeeper") {
		t.Fatalf("audit: %s", a)
	}
	all := e.tool(tok, "audit_list", map[string]any{"limit": 500}).([]any)
	if len(all) < 15 {
		t.Fatalf("audit has %d entries", len(all))
	}
}

func TestMCP(t *testing.T) {
	e := setup(t)
	_, m, _ := e.req("POST", "/api/agent/agents", e.su, map[string]any{"name": "mcp", "scopes": []string{"read", "schema"}})
	tok := m["token"].(string)
	rpc := func(token string, body any) (int, map[string]any) {
		code, m, _ := e.req("POST", "/api/agent/mcp", token, body)
		return code, m
	}
	// initialize works before auth (clients discover the server), then everything needs a token
	code, init := rpc("", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-06-18"}})
	if code != 200 || init["result"].(map[string]any)["serverInfo"].(map[string]any)["name"] != "jade-agent-base" {
		t.Fatalf("initialize: %d %v", code, init)
	}
	if code, _ := rpc("", map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"}); code != 401 {
		t.Fatalf("tools/list without a token: %d", code)
	}
	if code, _ := rpc(tok, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); code != 202 {
		t.Fatalf("notification: %d", code)
	}
	_, l := rpc(tok, map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/list"})
	tools := l["result"].(map[string]any)["tools"].([]any)
	var create map[string]any
	for _, x := range tools {
		if x.(map[string]any)["name"] == "collection_create" {
			create = x.(map[string]any)
		}
		if x.(map[string]any)["name"] == "record_create" {
			t.Fatal("tools/list ignored scopes")
		}
	}
	if create == nil || create["inputSchema"] == nil {
		t.Fatalf("tools/list: %v", tools)
	}
	_, c := rpc(tok, map[string]any{"jsonrpc": "2.0", "id": 4, "method": "tools/call", "params": map[string]any{"name": "collection_create", "arguments": map[string]any{"name": "notes", "fields": []any{map[string]any{"name": "body", "type": "text"}}}}})
	res := c["result"].(map[string]any)
	if res["isError"] == true || !strings.Contains(res["content"].([]any)[0].(map[string]any)["text"].(string), `"notes"`) {
		t.Fatalf("tools/call: %v", c)
	}
	_, c = rpc(tok, map[string]any{"jsonrpc": "2.0", "id": 5, "method": "tools/call", "params": map[string]any{"name": "record_create", "arguments": map[string]any{}}})
	if c["result"].(map[string]any)["isError"] != true {
		t.Fatalf("out-of-scope call should be an error result: %v", c)
	}
	_, c = rpc(tok, map[string]any{"jsonrpc": "2.0", "id": 6, "method": "nope"})
	if c["error"].(map[string]any)["code"].(float64) != -32601 {
		t.Fatalf("unknown method: %v", c)
	}
	// batches
	code, _, arr := e.req("POST", "/api/agent/mcp", tok, []any{map[string]any{"jsonrpc": "2.0", "id": 7, "method": "ping"}, map[string]any{"jsonrpc": "2.0", "id": 8, "method": "tools/call", "params": map[string]any{"name": "server_info"}}})
	if code != 200 || len(arr) != 2 {
		t.Fatalf("batch: %d %v", code, arr)
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
