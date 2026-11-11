package gateway_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/lucky-edgelord/jade-agent-base/jab"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	_ "github.com/pocketbase/pocketbase/migrations"
)

// ---- a fake upstream that speaks Ollama, OpenAI, Anthropic and Gemini, and remembers what it was sent ----

type fake struct {
	mu   sync.Mutex
	last map[string]map[string]any // path → last body
	hdr  map[string]http.Header
}

func (f *fake) body(path string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last[path]
}

func newFake(t *testing.T) (*fake, *httptest.Server) {
	f := &fake{last: map[string]map[string]any{}, hdr: map[string]http.Header{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		key := r.URL.Path
		if strings.Contains(key, ":streamGenerateContent") {
			key = "gemini-chat"
		} else if strings.Contains(key, ":batchEmbedContents") {
			key = "gemini-embed"
		}
		f.mu.Lock()
		f.last[key], f.hdr[key] = b, r.Header.Clone()
		f.mu.Unlock()
		wantTools := b["tools"] != nil && lastRole(b) != "tool"
		fl := w.(http.Flusher)
		switch key {
		case "/api/chat":
			w.Header().Set("Content-Type", "application/x-ndjson")
			for _, c := range []string{`{"message":{"thinking":"hmm"},"done":false}`, `{"message":{"content":"Hello "},"done":false}`, `{"message":{"content":"from Ollama"},"done":false}`} {
				fmt.Fprintln(w, c)
				fl.Flush()
			}
			if wantTools {
				fmt.Fprintln(w, `{"message":{"content":"","tool_calls":[{"function":{"name":"get_weather","arguments":{"city":"Paris"}}}]},"done":false}`)
			}
			fmt.Fprintln(w, `{"message":{"content":""},"done":true,"done_reason":"stop","prompt_eval_count":11,"eval_count":4}`)
		case "/api/embed":
			n := len(b["input"].([]any))
			out := make([][]float64, n)
			for i := range out {
				out[i] = []float64{float64(i), 0.5, 1}
			}
			json.NewEncoder(w).Encode(map[string]any{"embeddings": out, "prompt_eval_count": 6})
		case "/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			for _, c := range []string{`{"choices":[{"delta":{"role":"assistant","content":"Hello "}}]}`, `{"choices":[{"delta":{"content":"from OpenAI"}}]}`} {
				fmt.Fprintf(w, "data: %s\n\n", c)
				fl.Flush()
			}
			if wantTools {
				fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_x","function":{"name":"get_weather","arguments":"{\"ci"}}]}}]}`)
				fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"Rome\"}"}}]},"finish_reason":"tool_calls"}]}`)
			} else {
				fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
			}
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", `{"choices":[],"usage":{"prompt_tokens":20,"completion_tokens":8}}`)
		case "/embeddings":
			json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"index": 0, "embedding": []float64{1, 2}}}, "usage": map[string]any{"prompt_tokens": 3}})
		case "/v1/messages":
			w.Header().Set("Content-Type", "text/event-stream")
			ev := func(name, data string) { fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data); fl.Flush() }
			ev("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":30}}}`)
			ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
			ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello from Claude"}}`)
			ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
			stop := "end_turn"
			if wantTools {
				ev("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather"}}`)
				ev("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`)
				ev("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"Oslo\"}"}}`)
				ev("content_block_stop", `{"type":"content_block_stop","index":1}`)
				stop = "tool_use"
			}
			ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"`+stop+`"},"usage":{"output_tokens":9}}`)
			ev("message_stop", `{"type":"message_stop"}`)
		case "gemini-chat":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: %s\n\n", `{"candidates":[{"content":{"parts":[{"text":"Hello ","thought":false}]}}]}`)
			part := `{"text":"from Gemini"}`
			if wantTools {
				part = `{"functionCall":{"name":"get_weather","args":{"city":"Lima"}}}`
			}
			fmt.Fprintf(w, "data: %s\n\n", `{"candidates":[{"content":{"parts":[`+part+`]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":15,"candidatesTokenCount":5}}`)
		case "gemini-embed":
			json.NewEncoder(w).Encode(map[string]any{"embeddings": []any{map[string]any{"values": []float64{0.1, 0.2, 0.3, 0.4}}}})
		case "/api/web_search":
			json.NewEncoder(w).Encode(map[string]any{"results": []any{map[string]any{"title": "Jade", "url": "https://jade.test", "content": "q=" + fmt.Sprint(b["query"])}}})
		case "/api/web_fetch":
			json.NewEncoder(w).Encode(map[string]any{"title": "Page", "content": "fetched " + fmt.Sprint(b["url"])})
		case "/v1/systemone":
			json.NewEncoder(w).Encode(map[string]any{"model": b["model"], "answers": map[string]any{"q": map[string]any{"answer": "yes", "p": 0.9}}})
		case "/api/tags":
			json.NewEncoder(w).Encode(map[string]any{"models": []any{map[string]any{"name": "llama3:8b"}, map[string]any{"name": "gpt-oss:120b"}}})
		default:
			w.WriteHeader(404)
			fmt.Fprint(w, `{"error":"no route `+r.URL.Path+`"}`)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func lastRole(b map[string]any) string {
	for _, k := range []string{"messages", "contents"} {
		if ms, ok := b[k].([]any); ok && len(ms) > 0 {
			m := ms[len(ms)-1].(map[string]any)
			if r, _ := m["role"].(string); r == "tool" {
				return "tool"
			}
			// Anthropic and Gemini send tool results as user turns
			if cs, ok := m["content"].([]any); ok && len(cs) > 0 {
				if c, ok := cs[0].(map[string]any); ok && c["type"] == "tool_result" {
					return "tool"
				}
			}
			if ps, ok := m["parts"].([]any); ok && len(ps) > 0 {
				if c, ok := ps[0].(map[string]any); ok && c["functionResponse"] != nil {
					return "tool"
				}
			}
		}
	}
	return ""
}

// ---- the server ----

type env struct {
	t   *testing.T
	app *core.BaseApp
	srv *httptest.Server
	su  string
}

func setup(t *testing.T, modules []string) *env {
	dir := t.TempDir()
	app := core.NewBaseApp(core.BaseAppConfig{DataDir: filepath.Join(dir, "pb_data")})
	if err := jab.Setup(app, jab.Options{Modules: modules, HooksDir: filepath.Join(dir, "pb_hooks"), PublicDir: filepath.Join(dir, "pb_public")}); err != nil {
		t.Fatal(err)
	}
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
	router, _ := apis.NewRouter(app)
	var mux http.Handler
	if err := app.OnServe().Trigger(&core.ServeEvent{App: app, Router: router}, func(e *core.ServeEvent) error {
		m, err := e.Router.BuildMux()
		mux = m
		return err
	}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(func() { srv.Close(); app.ResetBootstrapState() })
	return &env{t: t, app: app, srv: srv, su: tok}
}

func (e *env) do(method, path string, hdr map[string]string, body any) (int, []byte) {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
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
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

func (e *env) json(method, path string, hdr map[string]string, body any) (int, map[string]any) {
	code, b := e.do(method, path, hdr, body)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return code, m
}

func (e *env) tool(name string, args map[string]any) any {
	e.t.Helper()
	code, m := e.json("POST", "/api/agent/tools/"+name, map[string]string{"Authorization": e.su}, args)
	if code != 200 {
		e.t.Fatalf("%s: %d %v", name, code, m)
	}
	return m["result"]
}

func (e *env) toolErr(name string, args map[string]any) string {
	_, m := e.json("POST", "/api/agent/tools/"+name, map[string]string{"Authorization": e.su}, args)
	s, _ := m["error"].(string)
	return s
}

// a signed-up user with an API key
func (e *env) user(email string) (string, string) {
	e.t.Helper()
	if code, m := e.json("POST", "/api/collections/users/records", nil, map[string]any{"email": email, "password": "password123", "passwordConfirm": "password123"}); code != 200 {
		e.t.Fatalf("sign up: %d %v", code, m)
	}
	_, a := e.json("POST", "/api/collections/users/auth-with-password", nil, map[string]any{"identity": email, "password": "password123"})
	tok := a["token"].(string)
	code, k := e.json("POST", "/api/gateway/keys", map[string]string{"Authorization": tok}, map[string]any{"name": "laptop"})
	if code != 200 || !strings.HasPrefix(k["key"].(string), "jab_") {
		e.t.Fatalf("make key: %d %v", code, k)
	}
	return tok, k["key"].(string)
}

func bearer(k string) map[string]string { return map[string]string{"Authorization": "Bearer " + k} }

// ---- tests ----

func TestDormantUntilEnabled(t *testing.T) {
	e := setup(t, nil) // a new server: only the agent module
	if code, _ := e.do("GET", "/v1/models", nil, nil); code != 404 {
		t.Fatalf("gateway answers while off: %d", code)
	}
	if _, err := e.app.FindCollectionByNameOrId("models"); err == nil {
		t.Fatal("gateway collections exist while it is off")
	}
	_, list := e.do("GET", "/api/agent/tools", map[string]string{"Authorization": e.su}, nil)
	if strings.Contains(string(list), "model_set") || !strings.Contains(string(list), "module_enable") {
		t.Fatal("gateway tools listed while off, or module tools missing")
	}
	mods := e.tool("modules_list", nil).([]any)
	if len(mods) < 2 {
		t.Fatalf("modules_list: %v", mods)
	}
	e.tool("module_enable", map[string]any{"name": "gateway"})
	if _, err := e.app.FindCollectionByNameOrId("models"); err != nil {
		t.Fatal("enabling did not make the collections")
	}
	_, list = e.do("GET", "/api/agent/tools", map[string]string{"Authorization": e.su}, nil)
	if !strings.Contains(string(list), "model_set") {
		t.Fatal("gateway tools missing after enabling")
	}
	if code, _ := e.do("GET", "/v1/models", map[string]string{"Authorization": e.su}, nil); code != 200 {
		t.Fatalf("gateway off after enabling: %d", code)
	}
	if msg := e.toolErr("module_disable", map[string]any{"name": "agent"}); !strings.Contains(msg, "cannot be switched off") {
		t.Fatalf("agent module disabled: %s", msg)
	}
	e.tool("module_disable", map[string]any{"name": "gateway"})
	if code, _ := e.do("GET", "/v1/models", map[string]string{"Authorization": e.su}, nil); code != 404 {
		t.Fatalf("gateway answers after disabling: %d", code)
	}
	if _, err := e.app.FindCollectionByNameOrId("models"); err != nil {
		t.Fatal("disabling deleted data")
	}
}

func configure(t *testing.T, e *env, upstream string) {
	e.tool("provider_set", map[string]any{"name": "local", "type": "ollama", "base_url": upstream})
	e.tool("provider_set", map[string]any{"name": "cloud", "type": "ollama-cloud", "base_url": upstream, "api_key": "ollama-secret"})
	e.tool("provider_set", map[string]any{"name": "router", "type": "openai", "base_url": upstream, "api_key": "sk-openai"})
	e.tool("provider_set", map[string]any{"name": "claude", "type": "anthropic", "base_url": upstream, "api_key": "sk-ant"})
	e.tool("provider_set", map[string]any{"name": "gemini", "type": "google", "base_url": upstream, "api_key": "g-key"})
	e.tool("provider_set", map[string]any{"name": "decider", "type": "ollaya", "base_url": upstream})
	e.tool("plan_set", map[string]any{"name": "pro", "price_month": 20, "rpm": 1000, "monthly_budget": 50})
	for _, m := range []map[string]any{
		{"name": "llama", "provider": "local", "upstream": "llama3:8b", "kind": "chat", "price_in": 0.1, "price_out": 0.2},
		{"name": "gpt", "provider": "router", "upstream": "openai/gpt-x", "kind": "chat", "price_in": 1, "price_out": 2, "plans": []any{"pro"}},
		{"name": "claude", "provider": "claude", "upstream": "claude-x", "kind": "chat"},
		{"name": "gemini", "provider": "gemini", "upstream": "gemini-x", "kind": "chat"},
		{"name": "embed-small", "provider": "local", "upstream": "embeddinggemma", "kind": "embed", "price_in": 0.02},
		{"name": "embed-g", "provider": "gemini", "upstream": "text-embedding-x", "kind": "embed"},
		{"name": "embed-oa", "provider": "router", "kind": "embed"},
		{"name": "web", "provider": "cloud", "kind": "search", "price_call": 0.005},
		{"name": "reader", "provider": "cloud", "kind": "fetch", "price_call": 0.001},
		{"name": "winnow", "provider": "decider", "upstream": "winnow:e4b", "kind": "decide", "price_call": 0.0001},
	} {
		e.tool("model_set", m)
	}
}

func TestGateway(t *testing.T) {
	f, up := newFake(t)
	e := setup(t, []string{"agent", "gateway"})
	configure(t, e, up.URL)
	if msg := e.toolErr("model_set", map[string]any{"name": "x", "provider": "nope", "kind": "chat"}); !strings.Contains(msg, `no provider "nope"`) {
		t.Fatalf("model_set bad provider: %s", msg)
	}
	if l := e.tool("provider_models", map[string]any{"name": "local"}).(map[string]any); len(l["models"].([]any)) != 2 {
		t.Fatalf("provider_models: %v", l)
	}
	st := e.tool("gateway_status", nil).(map[string]any)
	if b, _ := json.Marshal(st); strings.Contains(string(b), "sk-ant") || !strings.Contains(string(b), `"has_key":true`) {
		t.Fatalf("gateway_status leaks keys or lacks them: %s", b)
	}
	tok, key := e.user("ada@example.com")
	if code, b := e.do("GET", "/api/collections/providers/records", map[string]string{"Authorization": tok}, nil); code == 200 && strings.Contains(string(b), "sk-") {
		t.Fatal("users can read provider keys")
	}

	// models a free user sees: not gpt (pro only)
	_, ms := e.json("GET", "/v1/models", bearer(key), nil)
	names := []string{}
	for _, m := range ms["data"].([]any) {
		names = append(names, m.(map[string]any)["id"].(string))
	}
	if strings.Contains(strings.Join(names, ","), "gpt") || !strings.Contains(strings.Join(names, ","), "llama") {
		t.Fatalf("/v1/models for free: %v", names)
	}
	if code, m := e.json("POST", "/v1/chat/completions", bearer(key), map[string]any{"model": "gpt", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}); code != 403 || !strings.Contains(fmt.Sprint(m), "not in your plan") {
		t.Fatalf("plan check: %d %v", code, m)
	}
	e.tool("user_plan", map[string]any{"user": "ada@example.com", "plan": "pro"})

	// OpenAI clients, every upstream, not streamed
	for model, want := range map[string]string{"llama": "Hello from Ollama", "gpt": "Hello from OpenAI", "claude": "Hello from Claude", "gemini": "Hello from Gemini"} {
		code, m := e.json("POST", "/v1/chat/completions", bearer(key), map[string]any{"model": model, "messages": []any{map[string]any{"role": "system", "content": "Be brief."}, map[string]any{"role": "user", "content": "hi"}}})
		if code != 200 {
			t.Fatalf("%s: %d %v", model, code, m)
		}
		msg := m["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
		if msg["content"] != want || m["usage"].(map[string]any)["prompt_tokens"].(float64) == 0 {
			t.Fatalf("%s: %v", model, m)
		}
	}
	// the providers got their credentials, model names and system prompts in their own shapes
	if h := f.hdr["/v1/messages"]; h.Get("x-api-key") != "sk-ant" || f.body("/v1/messages")["system"] != "Be brief." || f.body("/v1/messages")["model"] != "claude-x" {
		t.Fatalf("anthropic request: %v %v", h, f.body("/v1/messages"))
	}
	if f.hdr["gemini-chat"].Get("x-goog-api-key") != "g-key" || f.body("gemini-chat")["systemInstruction"] == nil {
		t.Fatalf("gemini request: %v", f.body("gemini-chat"))
	}
	if f.hdr["/chat/completions"].Get("Authorization") != "Bearer sk-openai" || f.body("/chat/completions")["model"] != "openai/gpt-x" {
		t.Fatalf("openai request: %v", f.body("/chat/completions"))
	}

	// tool calls come back in OpenAI's shape from every upstream
	tools := []any{map[string]any{"type": "function", "function": map[string]any{"name": "get_weather", "description": "weather", "parameters": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"city": map[string]any{"type": "string"}}}}}}
	for model, city := range map[string]string{"llama": "Paris", "gpt": "Rome", "claude": "Oslo", "gemini": "Lima"} {
		_, m := e.json("POST", "/v1/chat/completions", bearer(key), map[string]any{"model": model, "tools": tools, "messages": []any{map[string]any{"role": "user", "content": "weather?"}}})
		ch := m["choices"].([]any)[0].(map[string]any)
		tc := ch["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
		var args map[string]any
		_ = json.Unmarshal([]byte(tc["function"].(map[string]any)["arguments"].(string)), &args)
		if ch["finish_reason"] != "tool_calls" || args["city"] != city || tc["id"] == "" {
			t.Fatalf("%s tool call: %v", model, ch)
		}
	}
	if _, has := f.body("gemini-chat")["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)[0].(map[string]any)["parameters"].(map[string]any)["additionalProperties"]; has {
		t.Fatal("gemini got schema keys it rejects")
	}
	// a tool round trip from an Ollama client (no ids) to Anthropic (ids required)
	code, b := e.do("POST", "/api/chat", bearer(key), map[string]any{"model": "claude", "stream": false, "messages": []any{
		map[string]any{"role": "user", "content": "weather?", "images": []any{"iVBORw0KGgo="}},
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"function": map[string]any{"name": "get_weather", "arguments": map[string]any{"city": "Oslo"}}}}},
		map[string]any{"role": "tool", "tool_name": "get_weather", "content": "-3C"},
	}})
	sent := f.body("/v1/messages")["messages"].([]any)
	use := sent[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	res := sent[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	img := sent[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if code != 200 || use["type"] != "tool_use" || res["type"] != "tool_result" || res["tool_use_id"] != use["id"] || img["type"] != "image" {
		t.Fatalf("ollama→anthropic tool round trip: %d %s\n%v", code, b, sent)
	}

	// streaming: OpenAI SSE
	code, raw := e.do("POST", "/v1/chat/completions", bearer(key), map[string]any{"model": "claude", "stream": true, "stream_options": map[string]any{"include_usage": true}, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	text, usage, done := "", false, false
	for _, l := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(l, "data: ") {
			continue
		}
		d := strings.TrimPrefix(l, "data: ")
		if d == "[DONE]" {
			done = true
			continue
		}
		var c map[string]any
		_ = json.Unmarshal([]byte(d), &c)
		if u, ok := c["usage"].(map[string]any); ok && u["prompt_tokens"].(float64) == 30 {
			usage = true
		}
		for _, ch := range c["choices"].([]any) {
			if s, ok := ch.(map[string]any)["delta"].(map[string]any)["content"].(string); ok {
				text += s
			}
		}
	}
	if code != 200 || text != "Hello from Claude" || !usage || !done {
		t.Fatalf("openai stream: %d %q usage=%v done=%v\n%s", code, text, usage, done, raw)
	}
	// streaming: Ollama NDJSON (the kit's client), across to an OpenAI upstream, with thinking from Ollama's
	code, raw = e.do("POST", "/api/chat", bearer(key), map[string]any{"model": "gpt", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	sc := bufio.NewScanner(bytes.NewReader(raw))
	text = ""
	var last map[string]any
	for sc.Scan() {
		var c map[string]any
		_ = json.Unmarshal(sc.Bytes(), &c)
		text += c["message"].(map[string]any)["content"].(string)
		last = c
	}
	if code != 200 || text != "Hello from OpenAI" || last["done"] != true || last["eval_count"].(float64) != 8 {
		t.Fatalf("ollama stream: %d %q %v", code, text, last)
	}
	_, raw = e.do("POST", "/api/chat", bearer(key), map[string]any{"model": "llama", "stream": false, "think": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if !strings.Contains(string(raw), `"thinking":"hmm"`) || f.body("/api/chat")["think"] != true || f.body("/api/chat")["model"] != "llama3:8b" {
		t.Fatalf("thinking: %s", raw)
	}

	// embeddings in both shapes, and from Gemini
	_, m := e.json("POST", "/v1/embeddings", bearer(key), map[string]any{"model": "embed-small", "input": []any{"a", "b"}})
	if len(m["data"].([]any)) != 2 || m["usage"].(map[string]any)["prompt_tokens"].(float64) != 6 {
		t.Fatalf("/v1/embeddings: %v", m)
	}
	_, m = e.json("POST", "/api/embed", bearer(key), map[string]any{"model": "embed-g", "input": "a"})
	if len(m["embeddings"].([]any)[0].([]any)) != 4 {
		t.Fatalf("/api/embed via gemini: %v", m)
	}
	if code, m := e.json("POST", "/v1/embeddings", bearer(key), map[string]any{"model": "llama", "input": "a"}); code != 400 || !strings.Contains(fmt.Sprint(m), "chat model") {
		t.Fatalf("kind check: %d %v", code, m)
	}

	// web search and fetch (Ollama Cloud), decisions (Ollaya)
	_, m = e.json("POST", "/api/web_search", bearer(key), map[string]any{"query": "jade", "max_results": 3})
	if !strings.Contains(fmt.Sprint(m), "q=jade") || f.hdr["/api/web_search"].Get("Authorization") != "Bearer ollama-secret" {
		t.Fatalf("web_search: %v", m)
	}
	_, m = e.json("POST", "/v1/fetch", bearer(key), map[string]any{"url": "https://jade.test"})
	if !strings.Contains(fmt.Sprint(m), "fetched https://jade.test") {
		t.Fatalf("fetch: %v", m)
	}
	_, m = e.json("POST", "/v1/systemone", bearer(key), map[string]any{"model": "winnow", "state": "x", "questions": map[string]any{"q": map[string]any{"type": "yesno"}}})
	if m["model"] != "winnow:e4b" {
		t.Fatalf("decide: %v", m)
	}

	// usage is metered and priced
	_, me := e.json("GET", "/api/gateway/me", bearer(key), nil)
	month := me["month"].(map[string]any)
	if month["tokens"].(float64) < 100 || month["cost"].(float64) <= 0.006 {
		t.Fatalf("/api/gateway/me: %v", me)
	}
	rep := e.tool("usage_report", map[string]any{"group": "model"}).([]any)
	found := map[string]float64{}
	for _, r := range rep {
		x := r.(map[string]any)
		found[x["key"].(string)] = x["charged"].(float64)
	}
	if found["web"] != 0.005 || found["gpt"] <= 0 {
		t.Fatalf("usage_report: %v", rep)
	}
	_, own := e.json("GET", "/api/collections/usage/records", map[string]string{"Authorization": tok}, nil)
	if own["totalItems"].(float64) < 10 {
		t.Fatalf("users see their usage: %v", own["totalItems"])
	}
	// people cannot change their own plan
	u, _ := e.app.FindAuthRecordByEmail("users", "ada@example.com")
	free, _ := e.app.FindFirstRecordByData("plans", "name", "free")
	if code, _ := e.json("PATCH", "/api/collections/users/records/"+u.Id, map[string]string{"Authorization": tok}, map[string]any{"plan": free.Id}); code == 200 {
		t.Fatal("a user changed their own plan")
	}
	if code, _ := e.json("PATCH", "/api/collections/users/records/"+u.Id, map[string]string{"Authorization": tok}, map[string]any{"name": "Ada"}); code != 200 {
		t.Fatalf("a user cannot edit their profile any more: %d", code)
	}
}

func TestLimitsAndKeys(t *testing.T) {
	_, up := newFake(t)
	e := setup(t, []string{"gateway"})
	configure(t, e, up.URL)
	e.tool("plan_set", map[string]any{"name": "tiny", "monthly_tokens": 10, "rpm": 2})
	_, key := e.user("bob@example.com")
	e.tool("user_plan", map[string]any{"user": "bob@example.com", "plan": "tiny"})
	chat := func() (int, map[string]any) {
		return e.json("POST", "/v1/chat/completions", bearer(key), map[string]any{"model": "llama", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	}
	if code, m := chat(); code != 200 {
		t.Fatalf("first: %d %v", code, m)
	}
	if code, m := chat(); code != 429 || !strings.Contains(fmt.Sprint(m), "monthly limit") {
		t.Fatalf("token cap: %d %v", code, m)
	}
	e.tool("plan_set", map[string]any{"name": "tiny", "monthly_tokens": 0})
	if code, m := chat(); code != 429 || !strings.Contains(fmt.Sprint(m), "rate limit") {
		t.Fatalf("rpm: %d %v", code, m)
	}
	// bad and revoked keys
	if code, _ := e.json("POST", "/v1/chat/completions", bearer("jab_0000000000000000000000"), map[string]any{"model": "llama"}); code != 401 {
		t.Fatalf("bad key: %d", code)
	}
	keys := e.tool("keys_list", map[string]any{"user": "bob@example.com"}).([]any)
	e.tool("key_revoke", map[string]any{"key": keys[0].(map[string]any)["prefix"]})
	if code, m := chat(); code != 401 || !strings.Contains(fmt.Sprint(m), "revoked") {
		t.Fatalf("revoked key: %d %v", code, m)
	}
	// capabilities switched off answer 404; others keep working
	e.tool("gateway_serve", map[string]any{"set": map[string]any{"chat": false}})
	_, key2 := e.user("cy@example.com")
	if code, _ := e.json("POST", "/v1/chat/completions", bearer(key2), map[string]any{"model": "llama", "messages": []any{}}); code != 404 {
		t.Fatalf("chat off: %d", code)
	}
	if code, _ := e.json("POST", "/v1/embeddings", bearer(key2), map[string]any{"model": "embed-small", "input": "x"}); code != 200 {
		t.Fatalf("embeddings stopped too: %d", code)
	}
	if msg := e.toolErr("gateway_serve", map[string]any{"set": map[string]any{"teleport": true}}); !strings.Contains(msg, "unknown capability") {
		t.Fatalf("gateway_serve: %s", msg)
	}
	// the upstream's failures come back with a useful status, and are metered as errors
	e.tool("provider_set", map[string]any{"name": "local", "base_url": "http://127.0.0.1:1"})
	if code, _ := e.json("POST", "/v1/embeddings", bearer(key2), map[string]any{"model": "embed-small", "input": "x"}); code != 502 {
		t.Fatalf("unreachable upstream: %d", code)
	}
}
