# Orbital Mark 7 — Regions & Data Residency

## Launch regions

Orbital Mark 7 is available at GA in exactly four regions:

- `westus3`
- `eastus2`
- `northeurope`
- `australiaeast`

No other regions are bookable at launch.

## Planned regions

- **Brazil South** (`brazilsouth`) is targeted for the **Mark 7.1** dot-release
  in **November 2026** and is **not bookable yet**. Attempting to create a
  deployment there returns `RegionNotAvailable`.

## Data residency

- **Customer content** — prompts, completions, uploaded images, and embedding
  inputs — stays **at rest in the region where the deployment was created**.
  It is never moved to another region for processing.
- **Telemetry** (latency metrics, error counts, token counts — no prompt or
  completion content) is replicated to the region's **paired region** for
  disaster-recovery purposes only.

### Region pairings

| Deployment region | Paired region (telemetry DR only) |
| ----------------- | --------------------------------- |
| `westus3` | `eastus2` |
| `eastus2` | `westus3` |
| `northeurope` | `westeurope` |
| `australiaeast` | `australiasoutheast` |

## Failover behavior

Mark 7 does **not** automatically fail a deployment over to its paired region.
If a region has an outage, inference is unavailable for that deployment until
the region recovers; only telemetry is served from the pair. Multi-region
high availability must be built by the customer using two deployments.
