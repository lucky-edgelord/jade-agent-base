package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// The gateway's own shape of a chat. Client formats (OpenAI, Ollama) are decoded into it and upstream adapters
// (Ollama, OpenAI-compatible, Anthropic, Google) encode from it, so every client works with every provider.

// Msg is one message.
type Msg struct {
	Role       string // system, user, assistant, tool
	Content    string
	Images     []string // base64, no data: prefix
	Thinking   string
	ToolCalls  []ToolCall
	ToolCallID string // for role tool: the call it answers
	ToolName   string // for role tool
}

// ToolCall is a model's request to run a tool.
type ToolCall struct {
	ID   string
	Name string
	Args map[string]any
}

// ToolDef is a tool offered to the model.
type ToolDef struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// ChatReq is a chat request.
type ChatReq struct {
	Model       string
	Messages    []Msg
	Tools       []ToolDef
	Temperature *float64
	TopP        *float64
	MaxTokens   int
	Stop        []string
	Think       any // nil, bool, or "low" | "medium" | "high"
	Format      any // nil, "json", or a JSON schema
	Stream      bool
	Options     map[string]any // other Ollama options, passed to Ollama upstreams
}

// Event is a piece of a streamed answer. The last one has Done set, with the token counts.
type Event struct {
	Text      string
	Thinking  string
	ToolCalls []ToolCall
	Done      bool
	Finish    string // stop, length, tool_calls
	In, Out   int
}

// Provider is an upstream, from the providers collection.
type Provider struct {
	ID, Name, Type, BaseURL, Key string
	Headers                      map[string]string
	TimeoutSec                   int
}

// Upstream speaks one provider's API.
type Upstream interface {
	Chat(ctx context.Context, p *Provider, model string, r *ChatReq, emit func(Event) error) error
	Embed(ctx context.Context, p *Provider, model string, input []string) (vecs [][]float64, tokens int, err error)
}

// UpstreamError carries the provider's status, so clients get a meaningful one.
type UpstreamError struct {
	Status int
	Msg    string
}

func (e *UpstreamError) Error() string { return fmt.Sprintf("upstream %d: %s", e.Status, e.Msg) }

// fillIDs gives every tool call an id and every tool result the id of the call it answers (Ollama has no ids:
// results answer the calls of the assistant message before them, in order, matched by name when given).
func fillIDs(msgs []Msg) {
	n := 0
	var pending []*ToolCall
	for i := range msgs {
		m := &msgs[i]
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			pending = pending[:0]
			for j := range m.ToolCalls {
				if m.ToolCalls[j].ID == "" {
					n++
					m.ToolCalls[j].ID = fmt.Sprintf("call_%d", n)
				}
				pending = append(pending, &m.ToolCalls[j])
			}
		}
		if m.Role == "tool" {
			if m.ToolCallID == "" && len(pending) > 0 {
				k := 0
				if m.ToolName != "" {
					for idx, c := range pending {
						if c.Name == m.ToolName {
							k = idx
							break
						}
					}
				}
				m.ToolCallID = pending[k].ID
				if m.ToolName == "" {
					m.ToolName = pending[k].Name
				}
				pending = append(pending[:k], pending[k+1:]...)
			}
			if m.ToolName == "" {
				// find the call this answers to learn its name
				for j := i - 1; j >= 0; j-- {
					for _, c := range msgs[j].ToolCalls {
						if c.ID == m.ToolCallID {
							m.ToolName = c.Name
						}
					}
				}
			}
		}
	}
}

// thinkBudget: a reasoning effort as a token budget (for providers that want one)
func thinkBudget(t any) int {
	switch v := t.(type) {
	case bool:
		if v {
			return 2048
		}
	case string:
		switch strings.ToLower(v) {
		case "low":
			return 1024
		case "medium":
			return 4096
		case "high":
			return 16000
		case "true":
			return 2048
		}
	}
	return 0
}

// mime type of base64 image data, from its first bytes
func imageMime(b64 string) string {
	switch {
	case strings.HasPrefix(b64, "/9j/"):
		return "image/jpeg"
	case strings.HasPrefix(b64, "iVBOR"):
		return "image/png"
	case strings.HasPrefix(b64, "R0lGOD"):
		return "image/gif"
	case strings.HasPrefix(b64, "UklGR"):
		return "image/webp"
	}
	return "image/png"
}

// estimate tokens when a provider does not say (about 4 characters a token)
func estimate(texts ...string) int {
	n := 0
	for _, t := range texts {
		n += len(t)
	}
	return (n + 3) / 4
}

func parseArgs(s string) map[string]any {
	m := map[string]any{}
	if strings.TrimSpace(s) == "" {
		return m
	}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return map[string]any{"_raw": s}
	}
	return m
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
