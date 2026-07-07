# Fluctio API

Fluctio is a **single-user, multi-agent** runtime: one owner who owns every
agent. This document describes the HTTP API an external client (script, bot,
automation, or the dashboard itself) uses to drive agents.

> Multi-tenant features that previously lived here — `POST /v1/users`
> end-user provisioning, `X-Fluctio-End-User` / chat `user` lazy mint,
> API-key tiers (`admin`/`user`/`agent`), the `apikey_agents` ACL, and
> link-sharing via `agents.is_public` — have been **removed**. There is no
> longer a path to create additional user accounts; the platform only ever
> holds the one owner created via onboarding or `fluctio admin create-user`.

## Authentication

Use an API key (all keys are owner-level):

```http
Authorization: Bearer fcak_...
```

Issue keys from the dashboard (**API Keys**) or the CLI:

```bash
fluctio apikey create --name "my-key"
```

The token is shown once. Keys may be rotated (`fluctio apikey rotate`) or
revoked. Do not expose keys in browsers/mobile clients — call Fluctio
server-side.

## Chat

### `POST /v1/chat/completions`

OpenAI-compatible, streaming-capable, with Fluctio extensions.

Required:

- `messages`: array with at least one `user` message

Agent selection:

- Body field `agent_id` (preferred), or `X-Fluctio-Agent-ID` header
- If omitted, Fluctio resolves the default agent

Session selection:

- `X-Fluctio-Session-Key` header
- If omitted, Fluctio creates a session key for that turn

```http
POST /v1/chat/completions
Authorization: Bearer fcak_...
Content-Type: application/json
X-Fluctio-Session-Key: my-bot:default

{
  "agent_id": "agt_...",
  "stream": true,
  "messages": [
    { "role": "user", "content": "总结今天的订单异常" }
  ],
  "params": { "locale": "zh-CN" },
  "images": ["https://example.com/chart.png"],
  "attachments": [
    { "url": "https://example.com/report.pdf", "name": "report.pdf" }
  ]
}
```

Fluctio extensions:

| Field | Type | Purpose |
|---|---|---|
| `agent_id` | string | Agent to call. Body wins over header. |
| `params` | object | Per-turn structured context shown to the agent. Not persisted. |
| `images` / `imageUrls` | string[] | Image URLs/data URLs shown to vision models and materialized into the workspace. |
| `attachments` | array | General files (PDFs, docs, zips) materialized into the workspace. Each entry: `{ url, name? }`. |

Streaming responses use the OpenAI SSE chunk shape; non-streaming responses use
the OpenAI `chat.completion` shape (with a `usage` block).

### Session key guidance

Choose a deterministic key per conversation, e.g. `<bot>:<conversation-id>`.
Do not reuse one key for unrelated conversations — session keys control chat
history, memory extraction context, and usage grouping.

## Agents

### `GET /v1/agents`

Lists the owner's agents.

```http
GET /v1/agents
Authorization: Bearer fcak_...
```

```json
{
  "agents": [
    { "id": "agt_...", "name": "default", "model": "openai/gpt-4.1-mini" }
  ]
}
```

For creating agents, cloning templates, installing skills, or configuring
providers, use the dashboard (`/api/*`) or the `fluctio` CLI — those are
operator workflows, not the chat API.

## Usage And Quotas

These endpoints remain for token-spend visibility and optional self-imposed
limits. In single-user mode there is exactly one user (the owner), so no
`user_id` parameter is required.

### `GET /v1/usage`

```http
GET /v1/usage?days=30
Authorization: Bearer fcak_...
```

Returns daily token usage broken down per agent/model, plus totals. `days`
defaults to `30`, max `90`.

### `PUT /v1/quota`

Set self-imposed monthly limits (token / request counts, reset day). A value of
`0` means no limit on that dimension. `reset_day` must be `1..28`.

```http
PUT /v1/quota
Authorization: Bearer fcak_...
Content-Type: application/json

{ "monthly_token_limit": 5000000, "monthly_request_limit": 10000, "reset_day": 1 }
```

`GET /v1/quota` reads the configured limit and current status; `DELETE /v1/quota`
removes it (reverts to unlimited). These return `503` when the usage/quota
subsystem is not configured.

## Error Shape

```json
{ "error": { "message": "agent not found", "type": "not_found_error" } }
```

| Status | Meaning |
|---|---|
| `400` | Bad request body, missing messages. |
| `401` | Missing/invalid API key. |
| `404` | Agent not found. |
| `429` | Rate limited or quota exceeded. |
| `503` | Usage/quota subsystem not configured. |

## Coding-agent runtime

If you are integrating a coding agent that needs project/workspace runtime
endpoints, see [`docs/coding-agent-runtime.md`](coding-agent-runtime.md).
