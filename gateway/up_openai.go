package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
)

// ---- OpenAI-compatible (OpenAI, OpenRouter, vLLM, llama.cpp, LM Studio, Groq, Together, …) ----

type openaiUp struct{}

func (openaiUp) headers(p *Provider) map[string]string {
	if p.Key != "" {
		return map[string]string{"Authorization": "Bearer " + p.Key}
	}
	return nil
}

func toOpenAIMessages(msgs []Msg) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		o := map[string]any{"role": m.Role}
		if len(m.Images) > 0 && m.Role == "user" {
			parts := []map[string]any{}
			if m.Content != "" {
				parts = append(parts, map[string]any{"type": "text", "text": m.Content})
			}
			for _, img := range m.Images {
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + imageMime(img) + ";base64," + img}})
			}
			o["content"] = parts
		} else {
			o["content"] = m.Content
		}
		if len(m.ToolCalls) > 0 {
			tc := []map[string]any{}
			for _, c := range m.ToolCalls {
				tc = append(tc, map[string]any{"id": c.ID, "type": "function", "function": map[string]any{"name": c.Name, "arguments": mustJSON(c.Args)}})
			}
			o["tool_calls"] = tc
			if m.Content == "" {
				o["content"] = nil
			}
		}
		if m.Role == "tool" {
			o["tool_call_id"] = m.ToolCallID
		}
		out = append(out, o)
	}
	return out
}

func (u openaiUp) Chat(ctx context.Context, p *Provider, model string, r *ChatReq, emit func(Event) error) error {
	msgs := append([]Msg(nil), r.Messages...)
	fillIDs(msgs)
	body := map[string]any{"model": model, "messages": toOpenAIMessages(msgs), "stream": true, "stream_options": map[string]any{"include_usage": true}}
	if len(r.Tools) > 0 {
		body["tools"] = toolsOpenAIShape(r.Tools)
	}
	if r.Temperature != nil {
		body["temperature"] = *r.Temperature
	}
	if r.TopP != nil {
		body["top_p"] = *r.TopP
	}
	if r.MaxTokens > 0 {
		body["max_tokens"] = r.MaxTokens
	}
	if len(r.Stop) > 0 {
		body["stop"] = r.Stop
	}
	switch f := r.Format.(type) {
	case string:
		if f == "json" {
			body["response_format"] = map[string]any{"type": "json_object"}
		}
	case map[string]any:
		body["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "answer", "schema": f}}
	}
	if s, ok := r.Think.(string); ok && s != "" && s != "true" {
		body["reasoning_effort"] = s
	}
	res, err := post(ctx, p, p.BaseURL+"/chat/completions", u.headers(p), body)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	type part struct {
		ID, Name, Args string
	}
	calls := map[int]*part{}
	finish := ""
	in, out := 0, 0
	err = eachSSE(res.Body, func(_ string, data []byte) error {
		if string(data) == "[DONE]" {
			return nil
		}
		var c struct {
			Choices []struct {
				Delta struct {
					Content          *string `json:"content"`
					ReasoningContent string  `json:"reasoning_content"`
					Reasoning        string  `json:"reasoning"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				In  int `json:"prompt_tokens"`
				Out int `json:"completion_tokens"`
			} `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &c) != nil {
			return nil
		}
		if c.Error != nil {
			return &UpstreamError{Status: http.StatusBadGateway, Msg: c.Error.Message}
		}
		if c.Usage != nil {
			in, out = c.Usage.In, c.Usage.Out
		}
		for _, ch := range c.Choices {
			d := ch.Delta
			ev := Event{Thinking: d.ReasoningContent + d.Reasoning}
			if d.Content != nil {
				ev.Text = *d.Content
			}
			for _, tc := range d.ToolCalls {
				pc := calls[tc.Index]
				if pc == nil {
					pc = &part{}
					calls[tc.Index] = pc
				}
				if tc.ID != "" {
					pc.ID = tc.ID
				}
				pc.Name += tc.Function.Name
				pc.Args += tc.Function.Arguments
			}
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				finish = *ch.FinishReason
			}
			if ev.Text != "" || ev.Thinking != "" {
				if err := emit(ev); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	done := Event{Done: true, In: in, Out: out, Finish: finish}
	idx := make([]int, 0, len(calls))
	for i := range calls {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		c := calls[i]
		done.ToolCalls = append(done.ToolCalls, ToolCall{ID: c.ID, Name: c.Name, Args: parseArgs(c.Args)})
	}
	if len(done.ToolCalls) > 0 {
		done.Finish = "tool_calls"
	}
	return emit(done)
}

func (u openaiUp) Embed(ctx context.Context, p *Provider, model string, input []string) ([][]float64, int, error) {
	res, err := post(ctx, p, p.BaseURL+"/embeddings", u.headers(p), map[string]any{"model": model, "input": input})
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	var j struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
		Usage struct {
			In int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(res.Body).Decode(&j); err != nil {
		return nil, 0, err
	}
	out := make([][]float64, len(input))
	for i, d := range j.Data {
		k := d.Index
		if k < 0 || k >= len(out) {
			k = i
		}
		if k < len(out) {
			out[k] = d.Embedding
		}
	}
	if j.Usage.In == 0 {
		j.Usage.In = estimate(input...)
	}
	return out, j.Usage.In, nil
}
