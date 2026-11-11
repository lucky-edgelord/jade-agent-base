package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ---- shared HTTP helpers ----

var httpClient = &http.Client{}

func post(ctx context.Context, p *Provider, url string, headers map[string]string, body any) (*http.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	t := p.TimeoutSec
	if t <= 0 {
		t = 300
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(t)*time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	res, err := httpClient.Do(req)
	if err != nil {
		cancel()
		return nil, &UpstreamError{Status: http.StatusBadGateway, Msg: fmt.Sprintf("%s unreachable: %v", p.Name, err)}
	}
	res.Body = &cancelBody{res.Body, cancel}
	if res.StatusCode >= 300 {
		defer res.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		return nil, &UpstreamError{Status: res.StatusCode, Msg: upstreamMessage(raw)}
	}
	return res, nil
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelBody) Close() error { err := c.ReadCloser.Close(); c.cancel(); return err }

// the message inside a provider's error body, whatever its shape
func upstreamMessage(raw []byte) string {
	var j map[string]any
	if json.Unmarshal(raw, &j) == nil {
		switch e := j["error"].(type) {
		case string:
			return e
		case map[string]any:
			if m, ok := e["message"].(string); ok {
				return m
			}
		}
		if m, ok := j["message"].(string); ok {
			return m
		}
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 500 {
		s = s[:500]
	}
	return s
}

// lines of a response (NDJSON)
func eachLine(r io.Reader, f func([]byte) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if err := f(line); err != nil {
			return err
		}
	}
	return sc.Err()
}

// server-sent events: f(event name, data)
func eachSSE(r io.Reader, f func(event string, data []byte) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	event := ""
	var data bytes.Buffer
	flush := func() error {
		if data.Len() == 0 {
			event = ""
			return nil
		}
		d := bytes.TrimSuffix(data.Bytes(), []byte("\n"))
		err := f(event, d)
		data.Reset()
		event = ""
		return err
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if err := flush(); err != nil {
				return err
			}
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimPrefix(line[5:], " "))
			data.WriteByte('\n')
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return flush()
}

// ---- Ollama (on a machine, or Ollama Cloud) ----

type ollamaUp struct{}

func (ollamaUp) headers(p *Provider) map[string]string {
	if p.Key != "" {
		return map[string]string{"Authorization": "Bearer " + p.Key}
	}
	return nil
}

func toOllamaMessages(msgs []Msg) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		o := map[string]any{"role": m.Role, "content": m.Content}
		if len(m.Images) > 0 {
			o["images"] = m.Images
		}
		if m.Thinking != "" {
			o["thinking"] = m.Thinking
		}
		if len(m.ToolCalls) > 0 {
			tc := []map[string]any{}
			for _, c := range m.ToolCalls {
				tc = append(tc, map[string]any{"function": map[string]any{"name": c.Name, "arguments": c.Args}})
			}
			o["tool_calls"] = tc
		}
		if m.Role == "tool" && m.ToolName != "" {
			o["tool_name"] = m.ToolName
		}
		out = append(out, o)
	}
	return out
}

func toolsOpenAIShape(ts []ToolDef) []map[string]any {
	out := []map[string]any{}
	for _, t := range ts {
		params := t.Parameters
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{"type": "function", "function": map[string]any{"name": t.Name, "description": t.Description, "parameters": params}})
	}
	return out
}

func (u ollamaUp) Chat(ctx context.Context, p *Provider, model string, r *ChatReq, emit func(Event) error) error {
	opts := map[string]any{}
	for k, v := range r.Options {
		opts[k] = v
	}
	if r.Temperature != nil {
		opts["temperature"] = *r.Temperature
	}
	if r.TopP != nil {
		opts["top_p"] = *r.TopP
	}
	if r.MaxTokens > 0 {
		opts["num_predict"] = r.MaxTokens
	}
	if len(r.Stop) > 0 {
		opts["stop"] = r.Stop
	}
	body := map[string]any{"model": model, "messages": toOllamaMessages(r.Messages), "stream": true}
	if len(r.Tools) > 0 {
		body["tools"] = toolsOpenAIShape(r.Tools)
	}
	if len(opts) > 0 {
		body["options"] = opts
	}
	if r.Think != nil {
		body["think"] = r.Think
	}
	if r.Format != nil {
		body["format"] = r.Format
	}
	res, err := post(ctx, p, p.BaseURL+"/api/chat", u.headers(p), body)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	return eachLine(res.Body, func(line []byte) error {
		var c struct {
			Message struct {
				Content   string `json:"content"`
				Thinking  string `json:"thinking"`
				ToolCalls []struct {
					Function struct {
						Name      string         `json:"name"`
						Arguments map[string]any `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			Done       bool   `json:"done"`
			DoneReason string `json:"done_reason"`
			In         int    `json:"prompt_eval_count"`
			Out        int    `json:"eval_count"`
			Error      string `json:"error"`
		}
		if err := json.Unmarshal(line, &c); err != nil {
			return nil
		}
		if c.Error != "" {
			return &UpstreamError{Status: http.StatusBadGateway, Msg: c.Error}
		}
		ev := Event{Text: c.Message.Content, Thinking: c.Message.Thinking}
		for _, tc := range c.Message.ToolCalls {
			ev.ToolCalls = append(ev.ToolCalls, ToolCall{Name: tc.Function.Name, Args: tc.Function.Arguments})
		}
		if c.Done {
			ev.Done, ev.In, ev.Out = true, c.In, c.Out
			ev.Finish = map[string]string{"length": "length"}[c.DoneReason]
		}
		if ev.Text == "" && ev.Thinking == "" && len(ev.ToolCalls) == 0 && !ev.Done {
			return nil
		}
		return emit(ev)
	})
}

func (u ollamaUp) Embed(ctx context.Context, p *Provider, model string, input []string) ([][]float64, int, error) {
	res, err := post(ctx, p, p.BaseURL+"/api/embed", u.headers(p), map[string]any{"model": model, "input": input, "truncate": true})
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	var j struct {
		Embeddings [][]float64 `json:"embeddings"`
		In         int         `json:"prompt_eval_count"`
	}
	if err := json.NewDecoder(res.Body).Decode(&j); err != nil {
		return nil, 0, err
	}
	if j.In == 0 {
		j.In = estimate(input...)
	}
	return j.Embeddings, j.In, nil
}
