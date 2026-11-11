# Decisions

Architectural decisions and assumptions, so the next person (or agent) can correct them in one pass. Newest first.

## 2026-11 · 0.6: the seam with Vault

- **Base is the resource plane.** It hosts no agent and has no scheduler. Vault (the control plane) owns schedules,
  autonomy and approvals, and talks to Base over MCP (`/api/agent/mcp`) or the same tools over REST. PocketBase's own
  `cronAdd` remains available inside hooks (it is PocketBase's, and the `hooks` scope is root-equivalent anyway); Base
  adds no scheduling tool.
- **Task credentials are PocketBase tokens with a `jab_cred` claim**, signed with the agent's key, and a row in
  `agent_credentials`. Reusing PocketBase's signing means revoking the agent (rotating its token key) ends every
  credential. The row gives per-credential revocation, task identity and narrowed scopes. It is looked up on every
  request and any failure refuses (fails closed). TTL: 15 min by default, at most 24 h.
- **Scope narrowing happens in `CallerTask`**, so REST, MCP and every module that calls `agent.Caller` see the narrowed
  scopes without changes.
- **Idempotency is per caller (agent or superuser), 24 h, keyed by (actor, key)** with a unique index, so concurrent
  duplicates run once. The stored answer is the JSON the client saw, and errors are replayed too (like Stripe). Read
  tools ignore keys. The same key with different arguments gets 422; a key still in flight gets 409.
- **Refusals are audited** (missing scope, unknown tool, bad arguments, key reuse), so the audit log is authoritative
  for "Base can say no".
- Assumption: callers send the agent token as `Authorization: Bearer <token>` (PocketBase also accepts it without
  "Bearer "). Query-string tokens are not accepted anywhere in Base.

## 2026-11 · 0.5: OAuth2 and rate limits

- OAuth2 is PocketBase's. Agents configure providers through tools, and client secrets are write-only.
- PocketBase's per-IP rate limiter is off by default (PocketBase's default). The `rate_limits` tool turns it on.
  Jade Agent Stack turns it on for the sites it builds.

## Earlier

- PocketBase is used as a Go library (v0.40.6), not forked, so upgrades stay cheap.
- Modules (gateway, billing) are off until an agent turns them on. Their state lives in `pb_data/jab.json`.
