# Jade Agent Base

A backend that AI agents can run. It is [PocketBase](https://pocketbase.io) (auth, a database with collections and API
rules, realtime, files, an admin dashboard), used as a Go library, plus modules for agents. An agent with a token can
design the data, write server-side code, publish a website, and change settings, through one tool list over REST or
MCP. Every action is in an audit log.

**Where it fits.** [Jade Vault](https://github.com/lucky-edgelord/jade-vault) is the control plane (the agent loop,
schedules, autonomy, approvals, the person). **Jade Agent Base is the resource plane**: identity, the database, the
model gateway, billing and the audit log. [Jade Agent Kit](https://github.com/lucky-edgelord/Jade-agent-kit) is the
embeddable subset of Vault (the agent in any web page), [Jade Agent UI/UX](https://github.com/lucky-edgelord/Jade-agent-ui-ux)
the presentation layer, and [Jade Agent Stack](https://github.com/lucky-edgelord/jade-agent-stack) the client toolkit over
all four. The seam is MCP: Vault and Kit ask, Base answers, Base logs, Base can say no. Base hosts no agent and has no
scheduler of its own.

The rules between Vault and Base (credentials, idempotency, refusals, audit, resuming) are in the stack's
[CONTRACT.md](https://github.com/lucky-edgelord/jade-agent-stack/blob/main/CONTRACT.md).

## Where it is going

Batches, each tested and merged before the next:

1. **Agent control** (0.1): agent identities with scopes, tools over REST and MCP, an audit log.
2. **Model gateway** (0.2): chat, embeddings, decisions, web search and fetch for signed-up users, with API keys,
   plans, rate limits and metered, priced usage. Modules switch on at runtime.
3. **Billing with Stripe** (0.3): plans on sale, Checkout, the customer portal, and webhooks that move users between
   plans.
4. **In-process embeddings** (0.4): model2vec embedding models running inside the server, in pure Go.
5. **The kit and UI**: a kit client and sign-in, account and billing components.
6. **Sign-in and limits** (0.5): OAuth2 providers (Google, Facebook, GitHub, …) and the API rate limiter, for agents.
7. **The seam** (0.6): short-lived task credentials, idempotency keys on tool calls, refusals in the audit log.

## Run it

```sh
go build -o bin/jab ./cmd/jab && cd bin
./jab superuser upsert you@example.com 'a-long-password'
./jab agent create shopkeeper --scopes read,write,schema,hooks,site --days 30   # prints the agent's token
./jab serve                                                                     # http://127.0.0.1:8090, dashboard at /_/
```

Every PocketBase command and flag works too.

## Modules

Everything beyond the agent module is off until something switches it on, and a module that is off adds nothing: no
collections, tools or served routes. An agent switches modules on and off at runtime with `modules_list`,
`module_enable` and `module_disable`, and the choice is kept in `pb_data/jab.json` across restarts. `--modules
agent,gateway` (or `JAB_MODULES`) sets them at start instead.

| Module | What it adds |
| --- | --- |
| `agent` (always on) | Agents, the tool list over REST and MCP, the audit log. |
| `gateway` | Chat, embeddings, decisions, web search and fetch for signed-up users (below). |
| `billing` | Stripe: plans on sale, Checkout, the customer portal, webhooks; paid plans need an active subscription. Switches the gateway on too. |

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

**`schema` is root-equivalent.** An agent that can create collections can create a public auth collection, open API
rules to anyone, or read every record by changing a rule. Give it only to agents you trust as much as a superuser.
`hooks` and `settings` are the same.

### Task credentials

An agent's token is long-lived (30 days). For each task, flow or scheduled job, the agent (Vault) mints a short-lived
credential with only the scopes the task needs:

```sh
curl -X POST …/api/agent/credentials -H "Authorization: Bearer $AGENT_TOKEN" \
  -d '{"task_id": "flow-42", "scopes": ["read", "write"], "ttl": "15m"}'
# → {"token": "…", "id": "…", "task_id": "flow-42", "scopes": ["read","write"], "expires": "…"}
curl -X POST …/api/agent/credentials/<id>/revoke -H "Authorization: Bearer $AGENT_TOKEN"
```

- `ttl`: `"15m"` by default, 10 s to 24 h (`"2h"`, or seconds).
- A credential has at most its maker's scopes. A credential can mint narrower ones that never outlive it, and revoking
  a credential revokes those too.
- Each request looks the credential up. If it is revoked, expired, deleted or cannot be read, the answer is **403**
  `{"code": "credential_refused"}` (MCP: HTTP 403, JSON-RPC error -32003). It fails closed. A client holding a cached
  credential that gets 403 should stop and tell the person, not retry.
- Revoking the agent (`…/agents/{id}/revoke`) ends all its credentials.
- Every tool call made with one is in `agent_audit` with its `task`.

### Idempotency keys

Send a key with a mutating call and Base runs it once. Sending it again returns the first answer, whether a result or an
error, and does not run the call again.

- REST: the `Idempotency-Key: <key>` header. A replay carries `Idempotent-Replayed: true`.
- MCP: `tools/call` with `params._meta.idempotency_key`. A replay has `_meta.replayed: true`.

Keys belong to the caller and last 24 hours. The same key with other arguments gets **422**; the same key while the
first call still runs gets **409**. Read tools ignore keys (reading twice is safe). Clients: generate a UUIDv7 per
mutation, and do not retry a mutation without its key. `agent_audit` records the key, and whether the call was a replay.

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
| `auth_providers` | read | How people sign in: password, which OAuth2 providers are on, every provider that can be added, the redirect URL to register. |
| `auth_provider_set`, `auth_provider_remove` | settings | Sign in with Google, Facebook, GitHub, Apple, Microsoft, Discord, … or any OpenID Connect server (OAuth2). The client secret is write-only (`client_secret`, or `client_secret_env` read on the server). |
| `rate_limits` | settings | The API rate limiter per client IP (off until turned on): rules like `*:auth` 2 per 3 s, `*:create`, `/api/`. The gateway's per-plan limits are separate (`plan_set`). |
| `logs_list`, `audit_list` | read | Server logs; what agents did. |

### Sign in with Google, Facebook, GitHub, …

PocketBase does OAuth2; an agent turns providers on. Make an OAuth app at the provider, set the public URL, register
`https://your.site/api/oauth2-redirect` with the provider, then:

```sh
curl -H "Authorization: $TOKEN" -X POST …/api/agent/tools/settings_update -d '{"set":{"meta":{"appURL":"https://your.site"}}}'
curl -H "Authorization: $TOKEN" -X POST …/api/agent/tools/auth_provider_set \
  -d '{"name":"google","client_id":"….apps.googleusercontent.com","client_secret_env":"GOOGLE_CLIENT_SECRET"}'
```

`<jade-auth>` (Jade Agent UI/UX 1.2+) shows a "Continue with Google" button for each provider that is on.

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

## The model gateway

An agent sets up a business that sells model access: it adds **providers**, offers **models** at its own prices,
defines **plans**, and watches **usage**. Signed-up users make API keys and call one API. Each capability switches on
or off on its own (`gateway_serve`): `chat`, `embed`, `decide`, `search`, `fetch`, `openai_api` and `ollama_api`.

| Provider type | For | Default address |
| --- | --- | --- |
| `ollama` | chat, embeddings, decisions (Ollama 0.35+) | http://localhost:11434 |
| `ollama-cloud` | chat, embeddings, decisions, **web search** and **web fetch** | https://ollama.com |
| `ollaya` | decision models | http://localhost:11435 |
| `openai` | any OpenAI-compatible API: OpenAI, OpenRouter, vLLM, llama.cpp, LM Studio, Groq, Together, … | https://api.openai.com/v1 |
| `anthropic` | chat (Claude) | https://api.anthropic.com |
| `google` | chat and embeddings (Gemini) | https://generativelanguage.googleapis.com/v1beta |
| `local` | embeddings computed **inside this server**: [model2vec](https://github.com/MinishLab/model2vec) static models from the Hugging Face Hub (`minishlab/potion-base-8M`, …), pure Go, downloaded on first use into `pb_data/models` | none |

Local embeddings need no provider account or GPU. They run at about 13 µs a sentence on one core, and their vectors
match the Python model2vec library. Every client format works with every provider: messages, images, tool calls (ids are matched up for providers that
need them), thinking, JSON output, and streaming are translated both ways.

| Endpoint | Format |
| --- | --- |
| `POST /v1/chat/completions`, `/v1/embeddings`, `GET /v1/models` | OpenAI (streaming, tools, `stream_options.include_usage`) |
| `POST /api/chat`, `/api/embed`, `/api/embeddings`, `GET /api/tags` | Ollama (so Ollama clients, and the kit, work as they are) |
| `POST /api/web_search`, `/api/web_fetch` (or `/v1/search`, `/v1/fetch`) | Ollama Cloud's web search and fetch |
| `POST /v1/systemone` | decisions (System One API) |
| `POST /api/gateway/keys`, `POST /api/gateway/keys/{id}/revoke`, `GET /api/gateway/me` | API keys and this month's use |

Users sign up through PocketBase (`users`), make a key with `POST /api/gateway/keys`, and call with
`Authorization: Bearer jab_…`. Keys are stored hashed and shown once. Every request is checked against the user's plan:
which models it includes, requests a minute, and monthly tokens and dollars. Then it is recorded in `usage` with its
tokens, price, time and status. Users see their own usage and keys; provider keys are never shown to anyone.

| Tool | Does |
| --- | --- |
| `gateway_status`, `gateway_serve` | What is served, providers, models, plans, this month; switch capabilities on or off. |
| `provider_set`, `provider_models`, `provider_delete` | Add or change a provider (its key is write-only); ask it which models it has. |
| `model_set`, `model_delete` | Offer a model under your name for it: kind, upstream name, prices (per million tokens, or per call), plans. |
| `plan_set`, `user_plan` | Plans (monthly tokens or budget, requests a minute, price, the default plan); put a user on one. |
| `usage_report`, `keys_list`, `key_revoke` | Usage and charges by user, model or day; keys (never the secrets). |

## Billing

An agent connects Stripe and sells the gateway's plans:

1. `billing_setup` with the Stripe secret key and webhook signing secret. Both are write-only; `STRIPE_SECRET_KEY` and
   `STRIPE_WEBHOOK_SECRET` in the environment win if they are set.
2. In Stripe, point a webhook at `<appURL>/api/billing/webhook` for `checkout.session.completed`,
   `customer.subscription.*` and `invoice.payment_failed`. `billing_status` shows the exact URL.
3. `plan_set` gives a plan its `price_month`, and `plan_publish` puts it on sale (a Stripe product and monthly price).

Users then call `POST /api/billing/checkout {"plan": "pro"}` and open the returned Stripe Checkout URL. They manage the
subscription in the portal (`POST /api/billing/portal`) and see it at `GET /api/billing/me`. Stripe's signed webhook
moves them onto the plan they pay for, keeps a past-due user's plan but stops its paid models until payment, and
returns a cancelled user to the default plan. Each event is applied once. Users cannot set their own plan or
subscription fields.

| Tool | Does |
| --- | --- |
| `billing_setup`, `billing_status` | Connect Stripe; see test or live mode, the webhook URL, plans for sale, subscribers and monthly recurring revenue. |
| `plan_publish` | Make or replace a plan's Stripe price from its `price_month`. |
| `subscribers_list`, `billing_events` | Who pays for what; what Stripe reported. |

## Safety

- Every tool call is recorded in `agent_audit` (who, what, arguments with anything named like a password, secret,
  token or key redacted, success or the error, the task credential's `task`, the `idempotency_key` and whether it
  was `replayed`). Refusals are recorded too: a missing scope, an unknown tool, bad arguments. Agents cannot change or
  delete it.
- Reselling models through the gateway makes you a seller: your terms of service, the providers' terms (some forbid
  resale), sales tax or VAT on what you sell, refunds and chargebacks (Stripe debits disputed payments, plus a fee) are
  yours to handle. Base meters and bills; it does not give legal or tax advice.
- Agents cannot delete the system collections, `agents` or `agent_audit`, and need `confirm` to delete a collection.
- Hooks run with the server's power. Give `hooks` only to agents you trust as much as yourself.
- Settings never show the SMTP password or S3 secrets.

## Development

```sh
go test -race ./...    # starts real servers in temp dirs and drives them over HTTP and MCP
go vet ./...
```

Needs Go 1.27 (PocketBase 0.40). License: MIT, see `LICENSE` and `THIRD_PARTY_NOTICES.md`.
