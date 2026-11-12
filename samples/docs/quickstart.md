# Orbital Mark 7 — Quickstart

Create your first Mark 7 deployment and send a request in about five minutes.

## 1. Prerequisites

- An Orbital account with access to a **launch region** (`westus3`, `eastus2`,
  `northeurope`, or `australiaeast`).
- The `orbitalctl` CLI **v2.7 or newer** (older CLIs cannot target Mark 7).

## 2. Create a deployment

A deployment is pinned to a region for data residency and cannot be moved
later. Pick the region closest to where your data must reside.

```bash
orbitalctl deployment create \
  --name my-agent \
  --model orbital-mark7 \
  --region westus3
```

The command returns an endpoint of the form
`https://my-agent.westus3.orbital.example/v1`.

## 3. Send a chat request

```bash
curl "$ORBITAL_ENDPOINT/chat/completions" \
  -H "Authorization: Bearer $ORBITAL_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
        "model": "orbital-mark7",
        "messages": [{"role": "user", "content": "Summarize the Mark 7 launch regions."}]
      }'
```

## 4. Use function calling

Mark 7 supports parallel function calling. Provide up to **16** tool
definitions per request; the model may return multiple tool calls in a single
turn.

## 5. Defaults to know

- Default `temperature` at GA is **0.7** (it was 0.9 during preview).
- Use API version **`2026-09-01`**. The preview version `2026-08-preview` is
  deprecated 30 days after GA.
- The context window is **32k tokens**; requests that exceed it return
  `ContextLengthExceeded`.

## Next steps

- Review [pricing](pricing.md) before enabling production traffic.
- Read the [limitations](limitations.md) for features that are not yet
  available (fine-tuning, streaming JSON-mode).
