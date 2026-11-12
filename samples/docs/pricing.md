# Orbital Mark 7 — Pricing

All prices are in USD and billed per **1,000 tokens**. Reasoning tokens and
output tokens are metered together at the reasoning rate; embedding tokens are
metered separately.

## Reasoning token pricing

| Window | Price per 1,000 reasoning tokens |
| ------ | -------------------------------- |
| First **90 days** after GA (2026-09-15 → 2026-12-14) | **$0.0012** |
| After the introductory window | **$0.0018** |

The introductory $0.0012 rate is automatic — no promo code is required.

## Committed-use discount

**Enterprise committed-use** customers keep the launch price of **$0.0012**
per 1,000 reasoning tokens until **2027-01-01**, even after the general step-up
on 2026-12-14. A committed-use agreement requires a minimum spend of
$25,000/month and a 12-month term.

## Embeddings

Embedding calls are billed at **$0.00002** per 1,000 input tokens. Embeddings
are not eligible for the committed-use discount.

## Free monthly grant

Every tenant receives a recurring grant of **1,000,000 reasoning tokens per
calendar month** during preview. The grant drops to **250,000 tokens/month**
at GA and does not roll over.

## Billing notes

- Cached prompt prefixes are billed at **25%** of the reasoning rate.
- Failed requests (HTTP 5xx from the service) are **not** billed.
- Vision image tiles are billed as **170 reasoning tokens per tile**.
