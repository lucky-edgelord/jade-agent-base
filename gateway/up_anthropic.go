package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// ---- Anthropic (Messages API) ----

type anthropicUp struct{}

func toAnthropic(msgs []Msg) (system string, out []map[string]any) {
	var sys []string
	for _, m := range msgs {
		var role string
		var blocks []map[string]any
		switch m.Role {
		case "system":
			sys = append(sys, m.Content)
			continue
		case "tool":
			role = "user"
			blocks = []map[string]any{{"type": "tool_result", "tool_use_id": m.ToolCallID, "content": m.Content}}
		case "assistant":
			role = "assistant"
			if m.Content != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
			}
			for _, c := range m.ToolCalls {
				args := c.Args
				if args == nil {
					args = map[string]any{}
				}
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": c.ID, "name": c.Name, "input": args})
			}
		default:
			role = "user"
			for _, img := range m.Images {
				blocks = append(blocks, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": imageMime(img), "data": img}})
			}
			if m.Content != "" || len(blocks) == 0 {
				blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
			}
		}
		if len(blocks) == 0 {
			continue
		}
		// Anthropic wants user and assistant turns to alternate: merge consecutive turns of one role
		if n := len(out); n > 0 && out[n-1]["role"] == role {
			out[n-1]["content"] = append(out[n-1]["content"].([]map[string]any), blocks...)
			continue
		}
		out = append(out, map[string]any{"role": role, "content": blocks})
	}
	return strings.Join(sys, "\n\n"), out
}

func (anthropicUp) Chat(ctx context.Context, p *Provider, model string, r *ChatReq, emit func(Event) error) error {
	msgs := append([]Msg(nil), r.Messages...)
	fillIDs(msgs)
	system, messages := toAnthropic(msgs)
	max := r.MaxTokens
	if max <= 0 {
		max = 4096
	}
	body := map[string]any{"model": model, "messages": messages, "max_tokens": max, "stream": true}
	if system != "" {
		body["system"] = system
	}
	if b := thinkBudget(r.Think); b > 0 {
		if max <= b {
			body["max_tokens"] = b + 4096
		}
		body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": b}
	} else {
		if r.Temperature != nil {
			body["temperature"] = *r.Temperature
		}
		if r.TopP != nil {
			body["top_p"] = *r.TopP
		}
	}
	if len(r.Stop) > 0 {
		body["stop_sequences"] = r.Stop
	}
	if len(r.Tools) > 0 {
		ts := []map[string]any{}
		for _, t := range r.Tools {
			s := t.Parameters
			if s == nil {
				s = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			ts = append(ts, map[string]any{"name": t.Name, "description": t.Description, "input_schema": s})
		}
		body["tools"] = ts
	}
	res, err := post(ctx, p, p.BaseURL+"/v1/messages", map[string]string{"x-api-key": p.Key, "anthropic-version": "2023-06-01"}, body)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	type block struct {
		Type, ID, Name string
		JSON           strings.Builder
	}
	blocks := map[int]*block{}
	done := Event{Done: true}
	err = eachSSE(res.Body, func(event string, data []byte) error {
		var j struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Message struct {
				Usage struct {
					In int `json:"input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Block struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				Out int `json:"output_tokens"`
			} `json:"usage"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &j) != nil {
			return nil
		}
		switch j.Type {
		case "error":
			return &UpstreamError{Status: http.StatusBadGateway, Msg: j.Error.Message}
		case "message_start":
			done.In = j.Message.Usage.In
		case "content_block_start":
			blocks[j.Index] = &block{Type: j.Block.Type, ID: j.Block.ID, Name: j.Block.Name}
		case "content_block_delta":
			switch j.Delta.Type {
			case "text_delta":
				return emit(Event{Text: j.Delta.Text})
			case "thinking_delta":
				return emit(Event{Thinking: j.Delta.Thinking})
			case "input_json_delta":
				if b := blocks[j.Index]; b != nil {
					b.JSON.WriteString(j.Delta.PartialJSON)
				}
			}
		case "content_block_stop":
			if b := blocks[j.Index]; b != nil && b.Type == "tool_use" {
				done.ToolCalls = append(done.ToolCalls, ToolCall{ID: b.ID, Name: b.Name, Args: parseArgs(b.JSON.String())})
			}
		case "message_delta":
			done.Out = j.Usage.Out
			done.Finish = map[string]string{"max_tokens": "length", "tool_use": "tool_calls"}[j.Delta.StopReason]
		}
		return nil
	})
	if err != nil {
		return err
	}
	return emit(done)
}

func (anthropicUp) Embed(context.Context, *Provider, string, []string) ([][]float64, int, error) {
	return nil, 0, errors.New("Anthropic has no embeddings API: use another provider for embedding models")
}
