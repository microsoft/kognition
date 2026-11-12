# Orbital Mark 7 — Known Limitations

These limitations apply to Orbital Mark 7 on **day one (GA, 2026-09-15)**. Each
notes the release that is expected to address it, where one is planned.

## Not available at launch

- **No fine-tuning endpoint.** Custom fine-tuning is planned for **Mark 7.2**
  (Q1 2027). At GA you can only use the base model with prompting and
  retrieval.
- **No JSON-mode for streaming responses.** Structured JSON output works for
  non-streaming calls only; streaming + strict JSON is planned for
  **Mark 7.2**.
- **32,000-token context window.** A larger context window is targeted for
  **Mark 7.1** (November 2026).

## Supported at launch

The following are fully supported from GA and are **not** limitations:

- **Function calling**, including parallel tool calls.
- **Vision** input (images and rendered pages).
- **Embeddings** endpoint.

## Rate limits

- Default: **240 requests/minute** and **450,000 tokens/minute** per
  deployment.
- Limits can be raised via a quota request; the hard ceiling per deployment is
  **1,000 requests/minute**.
- Exceeding a limit returns HTTP `429` with a `Retry-After` header.

## Other constraints

- Maximum **16 tool definitions** per request.
- Maximum image size for vision input is **20 MB** per image.
- Uploaded files are retained for **48 hours** and then deleted.
