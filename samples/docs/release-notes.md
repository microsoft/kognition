# Orbital Mark 7 — Release Notes

## Schedule

- **Public preview:** opens **2026-08-18**, four weeks ahead of GA. The preview
  is capped at a hard limit of **500 tenants**; once the cap is reached, new
  sign-ups are waitlisted until GA.
- **General availability (GA):** ships **2026-09-15** in all launch regions.

Preview tenants are automatically migrated to the GA control plane on the GA
date with no action required, but must re-accept the production terms within
14 days to avoid deployment suspension.

## What's new since Mark 6

- Reasoning depth roughly doubled at flat per-token latency.
- Parallel function calling (multiple tool calls per turn).
- Native vision input.
- Regional-only deployments for stronger data residency guarantees.

## Dot-release roadmap

| Release | Target | Highlights |
| ------- | ------ | ---------- |
| Mark 7.1 | November 2026 | Brazil South region, larger context window |
| Mark 7.2 | Q1 2027 | Fine-tuning endpoint, JSON-mode for streaming |

Dates on the roadmap are targets, not commitments, and may shift.

## Preview-to-GA breaking changes

- The preview `/v1/plan` debug endpoint is **removed** at GA.
- Default temperature changes from `0.9` (preview) to `0.7` (GA).
- API version `2026-08-preview` is deprecated 30 days after GA; move to
  `2026-09-01`.
