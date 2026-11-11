package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
)

// ---- Google (Gemini API) ----

type googleUp struct{}

// Gemini takes a subset of JSON schema (OpenAPI): drop the keys it rejects
func geminiSchema(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range x {
			switch k {
			case "$schema", "additionalProperties", "$id", "$ref", "$defs", "definitions", "examples", "default", "const", "title":
				continue
			}
			out[k] = geminiSchema(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = geminiSchema(val)
		}
		return out
	}
	return v
}

func toGemini(msgs []Msg) (system map[string]any, contents []map[string]any) {
	var sys []map[string]any
	for _, m := range msgs {
		var role string
		var parts []map[string]any
		switch m.Role {
		case "system":
			sys = append(sys, map[string]any{"text": m.Content})
			continue
		case "tool":
			role = "user"
			var resp any
			if json.Unmarshal([]byte(m.Content), &resp) != nil {
				resp = m.Content
			}
			if _, ok := resp.(map[string]any); !ok {
				resp = map[string]any{"content": resp}
			}
			parts = []map[string]any{{"functionResponse": map[string]any{"name": m.ToolName, "response": resp}}}
		case "assistant":
			role = "model"
			if m.Content != "" {
				parts = append(parts, map[string]any{"text": m.Content})
			}
			for _, c := range m.ToolCalls {
				parts = append(parts, map[string]any{"functionCall": map[string]any{"name": c.Name, "args": c.Args}})
			}
		default:
			role = "user"
			for _, img := range m.Images {
				parts = append(parts, map[string]any{"inlineData": map[string]any{"mimeType": imageMime(img), "data": img}})
			}
			if m.Content != "" || len(parts) == 0 {
				parts = append(parts, map[string]any{"text": m.Content})
			}
		}
		if len(parts) == 0 {
			continue
		}
		if n := len(contents); n > 0 && contents[n-1]["role"] == role {
			contents[n-1]["parts"] = append(contents[n-1]["parts"].([]map[string]any), parts...)
			continue
		}
		contents = append(contents, map[string]any{"role": role, "parts": parts})
	}
	if len(sys) > 0 {
		system = map[string]any{"parts": sys}
	}
	return
}

func (googleUp) Chat(ctx context.Context, p *Provider, model string, r *ChatReq, emit func(Event) error) error {
	msgs := append([]Msg(nil), r.Messages...)
	fillIDs(msgs)
	system, contents := toGemini(msgs)
	body := map[string]any{"contents": contents}
	if system != nil {
		body["systemInstruction"] = system
	}
	gc := map[string]any{}
	if r.Temperature != nil {
		gc["temperature"] = *r.Temperature
	}
	if r.TopP != nil {
		gc["topP"] = *r.TopP
	}
	if r.MaxTokens > 0 {
		gc["maxOutputTokens"] = r.MaxTokens
	}
	if len(r.Stop) > 0 {
		gc["stopSequences"] = r.Stop
	}
	switch f := r.Format.(type) {
	case string:
		if f == "json" {
			gc["responseMimeType"] = "application/json"
		}
	case map[string]any:
		gc["responseMimeType"] = "application/json"
		gc["responseSchema"] = geminiSchema(f)
	}
	if b := thinkBudget(r.Think); b > 0 {
		gc["thinkingConfig"] = map[string]any{"includeThoughts": true, "thinkingBudget": b}
	}
	if len(gc) > 0 {
		body["generationConfig"] = gc
	}
	if len(r.Tools) > 0 {
		decls := []map[string]any{}
		for _, t := range r.Tools {
			d := map[string]any{"name": t.Name, "description": t.Description}
			if props, _ := t.Parameters["properties"].(map[string]any); len(props) > 0 {
				d["parameters"] = geminiSchema(t.Parameters)
			}
			decls = append(decls, d)
		}
		body["tools"] = []map[string]any{{"functionDeclarations": decls}}
	}
	u := p.BaseURL + "/models/" + url.PathEscape(model) + ":streamGenerateContent?alt=sse"
	res, err := post(ctx, p, u, map[string]string{"x-goog-api-key": p.Key}, body)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	done := Event{Done: true}
	n := 0
	err = eachSSE(res.Body, func(_ string, data []byte) error {
		var j struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text         string `json:"text"`
						Thought      bool   `json:"thought"`
						FunctionCall *struct {
							Name string         `json:"name"`
							Args map[string]any `json:"args"`
						} `json:"functionCall"`
					} `json:"parts"`
				} `json:"content"`
				FinishReason string `json:"finishReason"`
			} `json:"candidates"`
			Usage struct {
				In       int `json:"promptTokenCount"`
				Out      int `json:"candidatesTokenCount"`
				Thoughts int `json:"thoughtsTokenCount"`
			} `json:"usageMetadata"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &j) != nil {
			return nil
		}
		if j.Error != nil {
			return &UpstreamError{Status: http.StatusBadGateway, Msg: j.Error.Message}
		}
		if j.Usage.In > 0 {
			done.In, done.Out = j.Usage.In, j.Usage.Out+j.Usage.Thoughts
		}
		for _, c := range j.Candidates {
			for _, pt := range c.Content.Parts {
				switch {
				case pt.FunctionCall != nil:
					n++
					done.ToolCalls = append(done.ToolCalls, ToolCall{ID: "call_g" + itoa(n), Name: pt.FunctionCall.Name, Args: pt.FunctionCall.Args})
				case pt.Thought:
					if err := emit(Event{Thinking: pt.Text}); err != nil {
						return err
					}
				case pt.Text != "":
					if err := emit(Event{Text: pt.Text}); err != nil {
						return err
					}
				}
			}
			if c.FinishReason == "MAX_TOKENS" {
				done.Finish = "length"
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(done.ToolCalls) > 0 {
		done.Finish = "tool_calls"
	}
	return emit(done)
}

func (googleUp) Embed(ctx context.Context, p *Provider, model string, input []string) ([][]float64, int, error) {
	reqs := make([]map[string]any, len(input))
	for i, t := range input {
		reqs[i] = map[string]any{"model": "models/" + model, "content": map[string]any{"parts": []map[string]any{{"text": t}}}}
	}
	res, err := post(ctx, p, p.BaseURL+"/models/"+url.PathEscape(model)+":batchEmbedContents", map[string]string{"x-goog-api-key": p.Key}, map[string]any{"requests": reqs})
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	var j struct {
		Embeddings []struct {
			Values []float64 `json:"values"`
		} `json:"embeddings"`
	}
	if err := json.NewDecoder(res.Body).Decode(&j); err != nil {
		return nil, 0, err
	}
	out := make([][]float64, len(j.Embeddings))
	for i, e := range j.Embeddings {
		out[i] = e.Values
	}
	return out, estimate(input...), nil
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
