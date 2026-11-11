package gateway

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func routes(se *core.ServeEvent) {
	r := se.Router
	// OpenAI's API
	r.GET("/v1/models", openaiModels)
	r.POST("/v1/chat/completions", openaiChat)
	r.POST("/v1/embeddings", openaiEmbed)
	r.POST("/v1/systemone", decide)
	r.POST("/v1/search", func(e *core.RequestEvent) error { return passthrough(e, "search", "/api/web_search", "openai_api") })
	r.POST("/v1/fetch", func(e *core.RequestEvent) error { return passthrough(e, "fetch", "/api/web_fetch", "openai_api") })
	// Ollama's API (the kit's Ollama client works against the gateway as it is)
	r.GET("/api/tags", ollamaTags)
	r.POST("/api/chat", ollamaChat)
	r.POST("/api/embed", ollamaEmbed)
	r.POST("/api/embeddings", ollamaEmbedLegacy)
	r.POST("/api/web_search", func(e *core.RequestEvent) error { return passthrough(e, "search", "/api/web_search", "ollama_api") })
	r.POST("/api/web_fetch", func(e *core.RequestEvent) error { return passthrough(e, "fetch", "/api/web_fetch", "ollama_api") })
	// accounts
	r.POST("/api/gateway/keys", createKeyRoute)
	r.POST("/api/gateway/keys/{id}/revoke", revokeKeyRoute)
	r.GET("/api/gateway/me", meRoute)
}

// ---- helpers ----

func readJSON(e *core.RequestEvent, v any) error {
	b, err := io.ReadAll(io.LimitReader(e.Request.Body, 64<<20))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fail(http.StatusBadRequest, "the body must be JSON: %v", err)
	}
	return nil
}

func openaiError(e *core.RequestEvent, err error) error {
	code, msg := statusOf(err)
	typ := map[int]string{400: "invalid_request_error", 401: "authentication_error", 402: "insufficient_quota", 403: "permission_error", 404: "not_found_error", 429: "rate_limit_error"}[code]
	if typ == "" {
		typ = "api_error"
	}
	return e.JSON(code, map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": code}})
}

func ollamaError(e *core.RequestEvent, err error) error {
	code, msg := statusOf(err)
	return e.JSON(code, map[string]any{"error": msg})
}

func off(e *core.RequestEvent, openai bool) error {
	err := fail(http.StatusNotFound, "this server does not offer that")
	if openai {
		return openaiError(e, err)
	}
	return ollamaError(e, err)
}

// models the caller may use (all enabled ones, filtered by plan when signed in)
func visibleModels(e *core.RequestEvent) ([]*core.Record, error) {
	a, err := authenticate(e)
	if err != nil {
		return nil, err
	}
	ms, err := e.App.FindRecordsByFilter("models", "enabled = true", "name", 0, 0)
	if err != nil {
		return nil, err
	}
	out := []*core.Record{}
	for _, m := range ms {
		if !On(m.GetString("kind")) {
			continue
		}
		if a.Plan != nil {
			if ps := m.GetStringSlice("plans"); len(ps) > 0 && !contains(ps, a.Plan.Id) {
				continue
			}
		}
		out = append(out, m)
	}
	return out, nil
}

// run a chat through its upstream; emit gets each event; returns the final usage
func runChat(e *core.RequestEvent, req *ChatReq, emit func(Event) error) (*Account, *core.Record, error) {
	a, m, p, err := gate(e, "chat", req.Model)
	if err != nil {
		return nil, nil, err
	}
	up := upstreams[p.Type]
	if up == nil {
		return a, m, fail(http.StatusBadRequest, "%s providers do not serve chat", p.Type)
	}
	start := time.Now()
	in, out := 0, 0
	var text strings.Builder
	err = up.Chat(e.Request.Context(), p, upstreamModel(m), req, func(ev Event) error {
		text.WriteString(ev.Text)
		if ev.Done {
			in, out = ev.In, ev.Out
			if in == 0 {
				for _, msg := range req.Messages {
					in += estimate(msg.Content)
				}
			}
			if out == 0 {
				out = estimate(text.String())
			}
		}
		return emit(ev)
	})
	status, msg := 200, ""
	if err != nil {
		status, msg = statusOf(err)
	}
	meter(e.App, a, m, "chat", in, out, start, status, msg)
	return a, m, err
}

// ---- OpenAI ----

type oaMsg struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name"`
	ToolCallID string          `json:"tool_call_id"`
	ToolCalls  []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
	Reasoning string `json:"reasoning_content"`
}

func fromOpenAI(ms []oaMsg) ([]Msg, error) {
	out := []Msg{}
	for _, m := range ms {
		x := Msg{Role: m.Role, ToolCallID: m.ToolCallID, Thinking: m.Reasoning}
		if x.Role == "developer" {
			x.Role = "system"
		}
		if x.Role == "tool" {
			x.ToolName = m.Name
		}
		var s string
		if len(m.Content) > 0 && json.Unmarshal(m.Content, &s) == nil {
			x.Content = s
		} else if len(m.Content) > 0 && string(m.Content) != "null" {
			var parts []struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				ImageURL struct {
					URL string `json:"url"`
				} `json:"image_url"`
			}
			if err := json.Unmarshal(m.Content, &parts); err != nil {
				return nil, fail(http.StatusBadRequest, "message content must be a string or a list of parts")
			}
			var tb []string
			for _, p := range parts {
				switch p.Type {
				case "text", "input_text":
					tb = append(tb, p.Text)
				case "image_url":
					u := p.ImageURL.URL
					i := strings.Index(u, ";base64,")
					if !strings.HasPrefix(u, "data:image/") || i < 0 {
						return nil, fail(http.StatusBadRequest, "images must be data: URLs (data:image/png;base64,…)")
					}
					x.Images = append(x.Images, u[i+8:])
				}
			}
			x.Content = strings.Join(tb, "\n")
		}
		for _, c := range m.ToolCalls {
			x.ToolCalls = append(x.ToolCalls, ToolCall{ID: c.ID, Name: c.Function.Name, Args: parseArgs(c.Function.Arguments)})
		}
		out = append(out, x)
	}
	return out, nil
}

func openaiChat(e *core.RequestEvent) error {
	if !On("openai_api") || !On("chat") {
		return off(e, true)
	}
	var b struct {
		Model     string   `json:"model"`
		Messages  []oaMsg  `json:"messages"`
		Stream    bool     `json:"stream"`
		Temp      *float64 `json:"temperature"`
		TopP      *float64 `json:"top_p"`
		Max       int      `json:"max_tokens"`
		MaxC      int      `json:"max_completion_tokens"`
		Stop      any      `json:"stop"`
		Effort    string   `json:"reasoning_effort"`
		StreamOpt *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
		Tools []struct {
			Function struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				Parameters  map[string]any `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
		RespFormat *struct {
			Type       string `json:"type"`
			JSONSchema struct {
				Schema map[string]any `json:"schema"`
			} `json:"json_schema"`
		} `json:"response_format"`
	}
	if err := readJSON(e, &b); err != nil {
		return openaiError(e, err)
	}
	msgs, err := fromOpenAI(b.Messages)
	if err != nil {
		return openaiError(e, err)
	}
	req := &ChatReq{Model: b.Model, Messages: msgs, Temperature: b.Temp, TopP: b.TopP, MaxTokens: max(b.Max, b.MaxC), Stream: b.Stream}
	switch s := b.Stop.(type) {
	case string:
		req.Stop = []string{s}
	case []any:
		for _, x := range s {
			req.Stop = append(req.Stop, fmt.Sprint(x))
		}
	}
	if b.Effort != "" {
		req.Think = b.Effort
	}
	for _, t := range b.Tools {
		req.Tools = append(req.Tools, ToolDef{Name: t.Function.Name, Description: t.Function.Description, Parameters: t.Function.Parameters})
	}
	if b.RespFormat != nil {
		switch b.RespFormat.Type {
		case "json_object":
			req.Format = "json"
		case "json_schema":
			req.Format = b.RespFormat.JSONSchema.Schema
		}
	}
	id := "chatcmpl-" + randID()
	created := time.Now().Unix()
	if !b.Stream {
		var text, think strings.Builder
		var done Event
		var calls []ToolCall
		_, _, err := runChat(e, req, func(ev Event) error {
			text.WriteString(ev.Text)
			think.WriteString(ev.Thinking)
			calls = append(calls, ev.ToolCalls...)
			if ev.Done {
				done = ev
			}
			return nil
		})
		if err != nil {
			return openaiError(e, err)
		}
		done.ToolCalls = calls
		msg := map[string]any{"role": "assistant", "content": text.String()}
		if think.Len() > 0 {
			msg["reasoning_content"] = think.String()
		}
		finish := orStr(done.Finish, "stop")
		if len(done.ToolCalls) > 0 {
			msg["tool_calls"] = oaCalls(done.ToolCalls, false)
			finish = "tool_calls"
		}
		return e.JSON(http.StatusOK, map[string]any{"id": id, "object": "chat.completion", "created": created, "model": b.Model,
			"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
			"usage":   map[string]any{"prompt_tokens": done.In, "completion_tokens": done.Out, "total_tokens": done.In + done.Out}})
	}
	started := false
	chunk := func(delta map[string]any, finish any) error {
		if !started {
			e.Response.Header().Set("Content-Type", "text/event-stream")
			e.Response.Header().Set("Cache-Control", "no-cache")
			e.Response.WriteHeader(http.StatusOK)
			started = true
		}
		b, _ := json.Marshal(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
		fmt.Fprintf(e.Response, "data: %s\n\n", b)
		return e.Flush()
	}
	first, sawTools, nCalls := true, false, 0
	_, _, err = runChat(e, req, func(ev Event) error {
		d := map[string]any{}
		if first {
			d["role"] = "assistant"
			first = false
		}
		if ev.Thinking != "" {
			d["reasoning_content"] = ev.Thinking
		}
		if ev.Text != "" {
			d["content"] = ev.Text
		}
		if len(ev.ToolCalls) > 0 {
			cs := oaCalls(ev.ToolCalls, true)
			for _, c := range cs {
				c["index"] = nCalls
				nCalls++
			}
			d["tool_calls"] = cs
			sawTools = true
		}
		if !ev.Done {
			return chunk(d, nil)
		}
		finish := orStr(ev.Finish, "stop")
		if sawTools {
			finish = "tool_calls"
		}
		if err := chunk(d, finish); err != nil {
			return err
		}
		if b.StreamOpt != nil && b.StreamOpt.IncludeUsage {
			u, _ := json.Marshal(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model, "choices": []any{},
				"usage": map[string]any{"prompt_tokens": ev.In, "completion_tokens": ev.Out, "total_tokens": ev.In + ev.Out}})
			fmt.Fprintf(e.Response, "data: %s\n\n", u)
		}
		return nil
	})
	if err != nil {
		if !started {
			return openaiError(e, err)
		}
		_, msg := statusOf(err)
		eb, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": "api_error"}})
		fmt.Fprintf(e.Response, "data: %s\n\n", eb)
	}
	fmt.Fprint(e.Response, "data: [DONE]\n\n")
	return e.Flush()
}

func oaCalls(cs []ToolCall, stream bool) []map[string]any {
	out := []map[string]any{}
	for i, c := range cs {
		id := c.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", i+1)
		}
		o := map[string]any{"id": id, "type": "function", "function": map[string]any{"name": c.Name, "arguments": mustJSON(orMap(c.Args))}}
		if stream {
			o["index"] = i
		}
		out = append(out, o)
	}
	return out
}

func orMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func orStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func openaiModels(e *core.RequestEvent) error {
	if !On("openai_api") {
		return off(e, true)
	}
	ms, err := visibleModels(e)
	if err != nil {
		return openaiError(e, err)
	}
	data := []any{}
	for _, m := range ms {
		data = append(data, map[string]any{"id": m.GetString("name"), "object": "model", "created": m.GetDateTime("created").Time().Unix(), "owned_by": "jade-agent-base", "kind": m.GetString("kind")})
	}
	return e.JSON(http.StatusOK, map[string]any{"object": "list", "data": data})
}

func embedInputs(v any) ([]string, error) {
	switch x := v.(type) {
	case string:
		return []string{x}, nil
	case []any:
		out := []string{}
		for _, s := range x {
			str, ok := s.(string)
			if !ok {
				return nil, fail(http.StatusBadRequest, "input must be text (token arrays are not supported)")
			}
			out = append(out, str)
		}
		return out, nil
	}
	return nil, fail(http.StatusBadRequest, "input must be a string or a list of strings")
}

func runEmbed(e *core.RequestEvent, model string, input []string) ([][]float64, int, error) {
	a, m, p, err := gate(e, "embed", model)
	if err != nil {
		return nil, 0, err
	}
	if len(input) > 2048 {
		return nil, 0, fail(http.StatusBadRequest, "at most 2048 inputs a request")
	}
	start := time.Now()
	up := upstreams[p.Type]
	if up == nil {
		return nil, 0, fail(http.StatusBadRequest, "%s providers do not serve embeddings", p.Type)
	}
	vecs, tokens, err := up.Embed(e.Request.Context(), p, upstreamModel(m), input)
	status, msg := 200, ""
	if err != nil {
		status, msg = statusOf(err)
	}
	meter(e.App, a, m, "embed", tokens, 0, start, status, msg)
	return vecs, tokens, err
}

func openaiEmbed(e *core.RequestEvent) error {
	if !On("openai_api") || !On("embed") {
		return off(e, true)
	}
	var b struct {
		Model  string `json:"model"`
		Input  any    `json:"input"`
		Format string `json:"encoding_format"`
	}
	if err := readJSON(e, &b); err != nil {
		return openaiError(e, err)
	}
	in, err := embedInputs(b.Input)
	if err != nil {
		return openaiError(e, err)
	}
	vecs, tokens, err := runEmbed(e, b.Model, in)
	if err != nil {
		return openaiError(e, err)
	}
	data := []any{}
	for i, v := range vecs {
		var emb any = v
		if b.Format == "base64" {
			emb = floatsBase64(v)
		}
		data = append(data, map[string]any{"object": "embedding", "index": i, "embedding": emb})
	}
	return e.JSON(http.StatusOK, map[string]any{"object": "list", "data": data, "model": b.Model, "usage": map[string]any{"prompt_tokens": tokens, "total_tokens": tokens}})
}

// ---- Ollama ----

type olMsg struct {
	Role      string   `json:"role"`
	Content   string   `json:"content"`
	Images    []string `json:"images"`
	Thinking  string   `json:"thinking"`
	ToolName  string   `json:"tool_name"`
	ToolCalls []struct {
		Function struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

func ollamaChat(e *core.RequestEvent) error {
	if !On("ollama_api") || !On("chat") {
		return off(e, false)
	}
	var b struct {
		Model    string         `json:"model"`
		Messages []olMsg        `json:"messages"`
		Stream   *bool          `json:"stream"`
		Think    any            `json:"think"`
		Format   any            `json:"format"`
		Options  map[string]any `json:"options"`
		Tools    []struct {
			Function struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				Parameters  map[string]any `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := readJSON(e, &b); err != nil {
		return ollamaError(e, err)
	}
	req := &ChatReq{Model: b.Model, Think: b.Think, Format: b.Format, Options: map[string]any{}, Stream: b.Stream == nil || *b.Stream}
	for _, m := range b.Messages {
		x := Msg{Role: m.Role, Content: m.Content, Images: m.Images, Thinking: m.Thinking, ToolName: m.ToolName}
		for _, c := range m.ToolCalls {
			x.ToolCalls = append(x.ToolCalls, ToolCall{Name: c.Function.Name, Args: c.Function.Arguments})
		}
		req.Messages = append(req.Messages, x)
	}
	for _, t := range b.Tools {
		req.Tools = append(req.Tools, ToolDef{Name: t.Function.Name, Description: t.Function.Description, Parameters: t.Function.Parameters})
	}
	for k, v := range b.Options {
		switch k {
		case "temperature":
			f, _ := v.(float64)
			req.Temperature = &f
		case "top_p":
			f, _ := v.(float64)
			req.TopP = &f
		case "num_predict":
			f, _ := v.(float64)
			req.MaxTokens = int(f)
		case "stop":
			if l, ok := v.([]any); ok {
				for _, s := range l {
					req.Stop = append(req.Stop, fmt.Sprint(s))
				}
			}
		default:
			req.Options[k] = v
		}
	}
	now := func() string { return time.Now().UTC().Format(time.RFC3339Nano) }
	msgOf := func(ev Event) map[string]any {
		m := map[string]any{"role": "assistant", "content": ev.Text}
		if ev.Thinking != "" {
			m["thinking"] = ev.Thinking
		}
		if len(ev.ToolCalls) > 0 {
			tc := []any{}
			for _, c := range ev.ToolCalls {
				tc = append(tc, map[string]any{"function": map[string]any{"name": c.Name, "arguments": orMap(c.Args)}})
			}
			m["tool_calls"] = tc
		}
		return m
	}
	start := time.Now()
	final := func(ev Event, msg map[string]any) map[string]any {
		return map[string]any{"model": b.Model, "created_at": now(), "message": msg, "done": true, "done_reason": orStr(map[string]string{"length": "length"}[ev.Finish], "stop"),
			"prompt_eval_count": ev.In, "eval_count": ev.Out, "total_duration": time.Since(start).Nanoseconds()}
	}
	if !req.Stream {
		var text, think strings.Builder
		var done Event
		var calls []ToolCall
		_, _, err := runChat(e, req, func(ev Event) error {
			text.WriteString(ev.Text)
			think.WriteString(ev.Thinking)
			calls = append(calls, ev.ToolCalls...)
			if ev.Done {
				done = ev
			}
			return nil
		})
		if err != nil {
			return ollamaError(e, err)
		}
		all := done
		all.Text, all.Thinking, all.ToolCalls = text.String(), think.String(), calls
		return e.JSON(http.StatusOK, final(done, msgOf(all)))
	}
	started := false
	line := func(v any) error {
		if !started {
			e.Response.Header().Set("Content-Type", "application/x-ndjson")
			e.Response.WriteHeader(http.StatusOK)
			started = true
		}
		b, _ := json.Marshal(v)
		e.Response.Write(append(b, '\n'))
		return e.Flush()
	}
	_, _, err := runChat(e, req, func(ev Event) error {
		if ev.Done {
			return line(final(ev, msgOf(Event{ToolCalls: ev.ToolCalls, Text: ev.Text, Thinking: ev.Thinking})))
		}
		return line(map[string]any{"model": b.Model, "created_at": now(), "message": msgOf(ev), "done": false})
	})
	if err != nil {
		if !started {
			return ollamaError(e, err)
		}
		_, msg := statusOf(err)
		return line(map[string]any{"error": msg})
	}
	return nil
}

func ollamaEmbed(e *core.RequestEvent) error {
	if !On("ollama_api") || !On("embed") {
		return off(e, false)
	}
	var b struct {
		Model string `json:"model"`
		Input any    `json:"input"`
	}
	if err := readJSON(e, &b); err != nil {
		return ollamaError(e, err)
	}
	in, err := embedInputs(b.Input)
	if err != nil {
		return ollamaError(e, err)
	}
	vecs, tokens, err := runEmbed(e, b.Model, in)
	if err != nil {
		return ollamaError(e, err)
	}
	return e.JSON(http.StatusOK, map[string]any{"model": b.Model, "embeddings": vecs, "prompt_eval_count": tokens})
}

func ollamaEmbedLegacy(e *core.RequestEvent) error {
	if !On("ollama_api") || !On("embed") {
		return off(e, false)
	}
	var b struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
	}
	if err := readJSON(e, &b); err != nil {
		return ollamaError(e, err)
	}
	vecs, _, err := runEmbed(e, b.Model, []string{b.Prompt})
	if err != nil {
		return ollamaError(e, err)
	}
	var v []float64
	if len(vecs) > 0 {
		v = vecs[0]
	}
	return e.JSON(http.StatusOK, map[string]any{"embedding": v})
}

func ollamaTags(e *core.RequestEvent) error {
	if !On("ollama_api") {
		return off(e, false)
	}
	ms, err := visibleModels(e)
	if err != nil {
		return ollamaError(e, err)
	}
	out := []any{}
	for _, m := range ms {
		out = append(out, map[string]any{"name": m.GetString("name"), "model": m.GetString("name"), "modified_at": m.GetDateTime("updated").Time().UTC().Format(time.RFC3339),
			"size": 0, "details": map[string]any{"family": m.GetString("kind")}})
	}
	return e.JSON(http.StatusOK, map[string]any{"models": out})
}

// ---- decisions, web search and fetch: passed to the provider as they are ----

// the model for a request that may not name one: the named one, else the first enabled model of the kind
func modelFor(e *core.RequestEvent, kind, name string) string {
	if name != "" {
		return name
	}
	m, err := e.App.FindFirstRecordByFilter("models", "kind = {:k} && enabled = true", dbx.Params{"k": kind})
	if err != nil {
		return ""
	}
	return m.GetString("name")
}

func passthrough(e *core.RequestEvent, kind, path, api string) error {
	openai := api == "openai_api"
	errOut := func(err error) error {
		if openai {
			return openaiError(e, err)
		}
		return ollamaError(e, err)
	}
	if !On(api) || !On(kind) {
		return off(e, openai)
	}
	var body map[string]any
	if err := readJSON(e, &body); err != nil {
		return errOut(err)
	}
	name, _ := body["model"].(string)
	name = modelFor(e, kind, name)
	if name == "" {
		return errOut(fail(http.StatusNotFound, "this server has no %s model yet", kind))
	}
	a, m, p, err := gate(e, kind, name)
	if err != nil {
		return errOut(err)
	}
	if p.Type != "ollama-cloud" && p.Type != "ollama" {
		return errOut(fail(http.StatusBadRequest, "web %s needs an Ollama Cloud provider", kind))
	}
	delete(body, "model")
	start := time.Now()
	return forward(e, a, m, p, kind, path, body, start, errOut)
}

func decide(e *core.RequestEvent) error {
	if !On("decide") {
		return off(e, true)
	}
	var body map[string]any
	if err := readJSON(e, &body); err != nil {
		return openaiError(e, err)
	}
	name, _ := body["model"].(string)
	name = modelFor(e, "decide", name)
	a, m, p, err := gate(e, "decide", name)
	if err != nil {
		return openaiError(e, err)
	}
	if p.Type != "ollaya" && p.Type != "ollama" && p.Type != "ollama-cloud" {
		return openaiError(e, fail(http.StatusBadRequest, "decisions need an Ollaya or Ollama provider"))
	}
	body["model"] = upstreamModel(m)
	return forward(e, a, m, p, "decide", "/v1/systemone", body, time.Now(), func(err error) error { return openaiError(e, err) })
}

func forward(e *core.RequestEvent, a *Account, m *core.Record, p *Provider, kind, path string, body map[string]any, start time.Time, errOut func(error) error) error {
	h := map[string]string{}
	if p.Key != "" {
		h["Authorization"] = "Bearer " + p.Key
	}
	res, err := post(e.Request.Context(), p, p.BaseURL+path, h, body)
	if err != nil {
		status, msg := statusOf(err)
		meter(e.App, a, m, kind, 0, 0, start, status, msg)
		return errOut(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return errOut(err)
	}
	var usage struct {
		Usage struct {
			In  int `json:"prompt_tokens"`
			Out int `json:"completion_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(raw, &usage)
	meter(e.App, a, m, kind, usage.Usage.In, usage.Usage.Out, start, 200, "")
	return e.Blob(http.StatusOK, "application/json", raw)
}

// ---- accounts ----

func createKeyRoute(e *core.RequestEvent) error {
	if !anyOn() {
		return off(e, false)
	}
	if e.Auth == nil || e.Auth.Collection().Name != "users" {
		return e.UnauthorizedError("sign in as a user to make an API key", nil)
	}
	var b struct {
		Name string `json:"name"`
	}
	_ = e.BindBody(&b)
	if n, _ := e.App.CountRecords("api_keys", dbx.HashExp{"user": e.Auth.Id, "revoked": false}); n >= 20 {
		return e.BadRequestError("you have 20 keys; revoke one first", nil)
	}
	key, r, err := NewKey(e.App, e.Auth, b.Name)
	if err != nil {
		return e.InternalServerError("", err)
	}
	return e.JSON(http.StatusOK, map[string]any{"key": key, "id": r.Id, "prefix": r.GetString("prefix"), "name": r.GetString("name"), "note": "Keep the key now: it is not shown again."})
}

func revokeKeyRoute(e *core.RequestEvent) error {
	if !anyOn() {
		return off(e, false)
	}
	if e.Auth == nil {
		return e.UnauthorizedError("sign in first", nil)
	}
	k, err := e.App.FindRecordById("api_keys", e.Request.PathValue("id"))
	if err != nil || (!e.Auth.IsSuperuser() && k.GetString("user") != e.Auth.Id) {
		return e.NotFoundError("no such key", nil)
	}
	k.Set("revoked", true)
	if err := e.App.Save(k); err != nil {
		return e.InternalServerError("", err)
	}
	return e.NoContent(http.StatusNoContent)
}

func meRoute(e *core.RequestEvent) error {
	if !anyOn() {
		return off(e, false)
	}
	a, err := authenticate(e)
	if err != nil || a.User == nil {
		return ollamaError(e, fail(http.StatusUnauthorized, "sign in or send an API key"))
	}
	tokens, cost := MonthUsage(e.App, a.User.Id)
	plan := map[string]any{"name": a.Plan.GetString("name"), "monthly_tokens": a.Plan.GetInt("monthly_tokens"), "monthly_budget": a.Plan.GetFloat("monthly_budget"), "rpm": a.Plan.GetInt("rpm")}
	return e.JSON(http.StatusOK, map[string]any{"user": a.User.Id, "plan": plan, "month": map[string]any{"tokens": tokens, "cost": cost}})
}

func anyOn() bool { return On("chat") || On("embed") || On("decide") || On("search") || On("fetch") }

func floatsBase64(v []float64) string {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		u := float32bits(float32(f))
		b[4*i], b[4*i+1], b[4*i+2], b[4*i+3] = byte(u), byte(u>>8), byte(u>>16), byte(u>>24)
	}
	return base64.StdEncoding.EncodeToString(b)
}
