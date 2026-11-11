# Jade Agent Base

A backend that AI agents can run. It is [PocketBase](https://pocketbase.io) (auth, a database with collections and API
rules, realtime, files, an admin dashboard), used as a Go library, plus modules for agents. An agent with a token can
design the data, write server-side code, publish a website, and change settings, through one tool list over REST or
MCP. Every action is in an audit log.

It is the server side of the Jade stack: [Jade Agent Kit](https://github.com/lucky-edgelord/Jade-agent-kit) (the agent
in the browser) and [Jade Agent UI/UX](https://github.com/lucky-edgelord/Jade-agent-ui-ux) (its interface).

## Where it is going

Batches, each tested and merged before the next:

1. **Agent control** (this release): agent identities with scopes, tools over REST and MCP, an audit log.
2. **Model gateway**: embeddings, chat and decisions for signed-up users through one API, with API keys, usage metering
   and per-plan limits. Providers: Ollama Cloud and local Ollama, Ollaya (decisions), anything OpenAI-compatible
   (OpenAI, OpenRouter, vLLM, llama.cpp, LM Studio, Groq, Together), Anthropic, Google, and small embedding models
   running in the server.
3. **Billing with Stripe**: plans, subscriptions, metered usage, checkout and the customer portal, with tools so an agent
   can set up and run the business.
4. **The kit and UI**: a kit client and sign-in, account and billing components.

## Run it

```sh
go build -o bin/jab ./cmd/jab && cd bin
./jab superuser upsert you@example.com 'a-long-password'
./jab agent create shopkeeper --scopes read,write,schema,hooks,site --days 30   # prints the agent's token
./jab serve                                                                     # http://127.0.0.1:8090, dashboard at /_/
```

Every PocketBase command and flag works too. `--modules agent` (or `JAB_MODULES`) picks the modules; a module that is
off adds nothing: no collections, routes or tools.

## Agents

An agent is a record in the `agents` auth collection, with **scopes**:

| Scope | Lets the agent |
| --- | --- |
| `read` | see collections, records, logs, hooks, site files and the audit log |
| `write` | create, update and delete records |
| `schema` | create, change and delete collections (fields, API rules, indexes) |
| `hooks` | write server-side JavaScript in `pb_hooks` (routes, record hooks, cron jobs), live on save |
| `site` | write the static site in `pb_public`, served at `/` |
| `settings` | change the app settings (secrets are never shown) |

Agents use tokens a superuser issues: `jab agent create|token|revoke|list`, or `POST /api/agent/agents`,
`/api/agent/agents/{id}/token` and `/api/agent/agents/{id}/revoke`. They cannot sign in with a password, and revoking
ends every token at once. Superusers may call every tool.

## Tools

| Tool | Scope | Does |
| --- | --- | --- |
| `server_info` | read | Version, modules, your scopes, collections, where hooks and the site live. |
| `collections_list`, `collection_get` | read | Collections with fields (`name:type`, `*` required); a full definition. |
| `collection_create`, `collection_update`, `collection_delete` | schema | Create (base, auth or view), change (set properties, add/update/remove fields), delete (with `confirm`). |
| `records_list`, `record_get` | read | PocketBase filters and sorts, `expand` for relations. |
| `record_create`, `record_update`, `record_delete` | write | With validation errors that name the field; `tags+` / `tags-` to add or remove. |
| `hooks_list`, `hook_read` / `hook_write`, `hook_delete` | read / hooks | Server-side JavaScript; the syntax is checked before saving and the server reloads. |
| `site_list`, `site_read` / `site_write`, `site_delete` | read / site | The static site; text or base64; paths cannot leave `pb_public` or touch hidden files. |
| `settings_get`, `settings_update` | settings | Merged in; a secret you leave out is kept. |
| `logs_list`, `audit_list` | read | Server logs; what agents did. |

### Over REST

```sh
curl -H "Authorization: $TOKEN" http://127.0.0.1:8090/api/agent/tools?format=openai     # or anthropic, mcp
curl -H "Authorization: $TOKEN" -X POST http://127.0.0.1:8090/api/agent/tools/collection_create \
  -d '{"name":"products","listRule":"","fields":[{"name":"title","type":"text","required":true},{"name":"price","type":"number"}]}'
# → {"result": {...}}  or  {"error": "…"} (403 when the scope is missing)
```

### Over MCP

`POST /api/agent/mcp` speaks the Model Context Protocol (streamable HTTP, JSON responses; `initialize`,
`tools/list`, `tools/call`, `ping`, batches). Point any MCP client at it with the header
`Authorization: <token>`. With Jade Agent Kit:

```js
const mcp = await kit.use('mcp');
await mcp.add('base', { url: 'https://your.server/api/agent/mcp', headers: { Authorization: '${BASE_TOKEN}' } });
```

### Modules add tools

Other Go packages add tools to the same list, with their own scopes:

```go
agent.AddScope("billing")
agent.AddTool(&agent.Tool{Name: "plans_list", Scope: "billing", Description: "…", Run: func(c *agent.Call) (any, error) { … }})
```

## Safety

- Every tool call is recorded in `agent_audit` (who, what, arguments with anything named like a password, secret,
  token or key redacted, success or the error). Agents cannot change or delete it.
- Agents cannot delete the system collections, `agents` or `agent_audit`, and need `confirm` to delete a collection.
- Hooks run with the server's power. Give `hooks` only to agents you trust as much as yourself.
- Settings never show the SMTP password or S3 secrets.

## Development

```sh
go test -race ./...    # starts real servers in temp dirs and drives them over HTTP and MCP
go vet ./...
```

Needs Go 1.27 (PocketBase 0.40). License: MIT, see `LICENSE` and `THIRD_PARTY_NOTICES.md`.
