package agent

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
)

const mcpProtocol = "2025-06-18"

func routes(se *core.ServeEvent) {
	g := se.Router.Group("/api/agent")
	g.GET("/tools", listTools)
	g.POST("/tools/{name}", runTool)
	g.POST("/mcp", mcp)
	g.GET("/mcp", func(e *core.RequestEvent) error {
		return e.Error(http.StatusMethodNotAllowed, "this MCP server answers POST requests with JSON (no event stream)", nil)
	})
	g.DELETE("/mcp", func(e *core.RequestEvent) error { return e.NoContent(http.StatusNoContent) })
	g.POST("/agents", createAgentRoute).Bind(apis.RequireSuperuserAuth())
	g.POST("/agents/{id}/token", tokenRoute).Bind(apis.RequireSuperuserAuth())
	g.POST("/agents/{id}/revoke", revokeRoute).Bind(apis.RequireSuperuserAuth())
}

func toolJSON(t *Tool, format string) map[string]any {
	switch format {
	case "anthropic":
		return map[string]any{"name": t.Name, "description": t.Description, "input_schema": t.Parameters}
	case "mcp":
		return map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.Parameters,
			"annotations": map[string]any{"readOnlyHint": t.Scope == ScopeRead, "destructiveHint": t.Destructive}}
	case "openai":
		return map[string]any{"type": "function", "function": map[string]any{"name": t.Name, "description": t.Description, "parameters": t.Parameters}}
	}
	return map[string]any{"name": t.Name, "description": t.Description, "scope": t.Scope, "destructive": t.Destructive, "parameters": t.Parameters}
}

func listTools(e *core.RequestEvent) error {
	_, scopes, err := Caller(e)
	if err != nil {
		return e.UnauthorizedError(err.Error(), nil)
	}
	f := e.Request.URL.Query().Get("format")
	out := []map[string]any{}
	for _, t := range Tools(scopes) {
		out = append(out, toolJSON(t, f))
	}
	return e.JSON(http.StatusOK, out)
}

func readArgs(e *core.RequestEvent) (map[string]any, error) {
	body, err := io.ReadAll(io.LimitReader(e.Request.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	args := map[string]any{}
	if strings.TrimSpace(string(body)) == "" {
		return args, nil
	}
	err = json.Unmarshal(body, &args)
	return args, err
}

func runTool(e *core.RequestEvent) error {
	actor, scopes, err := Caller(e)
	if err != nil {
		return e.UnauthorizedError(err.Error(), nil)
	}
	args, err := readArgs(e)
	if err != nil {
		return e.JSON(http.StatusBadRequest, map[string]any{"error": "the body must be a JSON object of arguments: " + err.Error()})
	}
	out, err := Run(e.App, actor, scopes, e.Request.PathValue("name"), args)
	if err != nil {
		return e.JSON(httpStatus(err), map[string]any{"error": err.Error()})
	}
	return e.JSON(http.StatusOK, map[string]any{"result": out})
}

// ---- MCP (JSON-RPC 2.0 over streamable HTTP, answered with JSON) ----

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func rpcErr(id json.RawMessage, code int, msg string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}}
}

func mcp(e *core.RequestEvent) error {
	actor, scopes, authErr := Caller(e)
	body, err := io.ReadAll(io.LimitReader(e.Request.Body, 32<<20))
	if err != nil {
		return e.JSON(http.StatusBadRequest, rpcErr(nil, -32700, err.Error()))
	}
	if t := strings.TrimSpace(string(body)); strings.HasPrefix(t, "[") {
		var batch []rpcReq
		if err := json.Unmarshal(body, &batch); err != nil {
			return e.JSON(http.StatusBadRequest, rpcErr(nil, -32700, "parse error: "+err.Error()))
		}
		out := []any{}
		for _, r := range batch {
			if res := handleRPC(e, actor, scopes, authErr, r); res != nil {
				out = append(out, res)
			}
		}
		if len(out) == 0 {
			return e.NoContent(http.StatusAccepted)
		}
		return e.JSON(http.StatusOK, out)
	}
	var r rpcReq
	if err := json.Unmarshal(body, &r); err != nil {
		return e.JSON(http.StatusBadRequest, rpcErr(nil, -32700, "parse error: "+err.Error()))
	}
	if authErr != nil && r.Method != "initialize" && !strings.HasPrefix(r.Method, "notifications/") && r.Method != "ping" {
		e.Response.Header().Set("WWW-Authenticate", `Bearer realm="jade-agent-base"`)
		return e.JSON(http.StatusUnauthorized, rpcErr(r.ID, -32001, authErr.Error()))
	}
	res := handleRPC(e, actor, scopes, authErr, r)
	if res == nil {
		return e.NoContent(http.StatusAccepted)
	}
	return e.JSON(http.StatusOK, res)
}

func handleRPC(e *core.RequestEvent, actor *core.Record, scopes map[string]bool, authErr error, r rpcReq) any {
	if len(r.ID) == 0 || string(r.ID) == "null" { // a notification: no answer
		return nil
	}
	ok := func(result any) map[string]any { return map[string]any{"jsonrpc": "2.0", "id": r.ID, "result": result} }
	switch r.Method {
	case "initialize":
		e.Response.Header().Set("Mcp-Session-Id", randHex(16))
		return ok(map[string]any{
			"protocolVersion": mcpProtocol,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "jade-agent-base", "version": cfg.Version},
			"instructions":    "Tools to run this Jade Agent Base: data (collections and records), server-side JavaScript hooks, the static site, settings and logs. Start with server_info.",
		})
	case "ping":
		return ok(map[string]any{})
	case "tools/list":
		if authErr != nil {
			return rpcErr(r.ID, -32001, authErr.Error())
		}
		list := []map[string]any{}
		for _, t := range Tools(scopes) {
			list = append(list, toolJSON(t, "mcp"))
		}
		return ok(map[string]any{"tools": list})
	case "tools/call":
		if authErr != nil {
			return rpcErr(r.ID, -32001, authErr.Error())
		}
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(r.Params, &p); err != nil {
			return rpcErr(r.ID, -32602, "invalid params: "+err.Error())
		}
		out, err := Run(e.App, actor, scopes, p.Name, p.Arguments)
		if err != nil {
			return ok(map[string]any{"content": []any{map[string]any{"type": "text", "text": "Error: " + err.Error()}}, "isError": true})
		}
		text, _ := json.MarshalIndent(out, "", " ")
		if s, isStr := out.(string); isStr {
			text = []byte(s)
		}
		return ok(map[string]any{"content": []any{map[string]any{"type": "text", "text": string(text)}}, "structuredContent": wrap(out)})
	case "resources/list":
		return ok(map[string]any{"resources": []any{}})
	case "prompts/list":
		return ok(map[string]any{"prompts": []any{}})
	}
	return rpcErr(r.ID, -32601, "method not found: "+r.Method)
}

// structuredContent must be an object
func wrap(v any) any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{"result": v}
}

// ---- managing agents (superusers) ----

func createAgentRoute(e *core.RequestEvent) error {
	var b struct {
		Name        string   `json:"name"`
		Scopes      []string `json:"scopes"`
		Description string   `json:"description"`
		Days        int      `json:"days"`
	}
	if err := e.BindBody(&b); err != nil || strings.TrimSpace(b.Name) == "" {
		return e.BadRequestError("send { name, scopes, description?, days? }", err)
	}
	r, tok, err := CreateAgent(e.App, b.Name, b.Scopes, b.Description, b.Days)
	if err != nil {
		return e.BadRequestError(err.Error(), nil)
	}
	return e.JSON(http.StatusOK, map[string]any{"agent": r, "token": tok})
}

func tokenRoute(e *core.RequestEvent) error {
	r, err := e.App.FindRecordById(AgentsCollection, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("no such agent", nil)
	}
	var b struct {
		Days int `json:"days"`
	}
	_ = e.BindBody(&b)
	tok, err := Token(r, b.Days)
	if err != nil {
		return e.InternalServerError("", err)
	}
	return e.JSON(http.StatusOK, map[string]any{"token": tok})
}

func revokeRoute(e *core.RequestEvent) error {
	r, err := e.App.FindRecordById(AgentsCollection, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("no such agent", nil)
	}
	if err := Revoke(e.App, r); err != nil {
		return e.InternalServerError("", err)
	}
	return e.NoContent(http.StatusNoContent)
}
