# Sample product documentation — "Orbital Mark 7"

This folder holds standalone documentation for a **fictional** product,
**Orbital Mark 7**, used by the knowledge-base samples as real, ingestible
content. The facts here are intentionally specific (dates, prices, region
lists, limits) so you can ask an agent concrete questions and verify it is
actually retrieving from the knowledge base rather than hallucinating.

## What it's for

The blob-backed KB samples index these files:

- [`samples/knowledge-base-blob-azure/`](../knowledge-base-blob-azure/README.md)
  points a `KnowledgeBase` at an Azure Blob container `product-docs` under the
  `mark7/` prefix.
- [`samples/knowledge-base-blob-s3/`](../knowledge-base-blob-s3/README.md)
  points at an S3 bucket with the same layout.

Upload the Markdown files in this folder to the container/bucket so the
ingester has something real to fetch, chunk, embed, and store in pgvector.

### Upload to Azure Blob (passwordless, via the SP that has Storage Blob Data Contributor)

```powershell
# Create the container (one-time) then upload every doc under mark7/
az storage container create --account-name '<storage-account>' --name product-docs --auth-mode login
az storage blob upload-batch `
  --account-name '<storage-account>' --auth-mode login `
  --destination product-docs --destination-path mark7 `
  --source samples/docs --pattern "*.md"
```

**Bash (Linux/macOS):**

```bash
az storage container create --account-name '<storage-account>' --name product-docs --auth-mode login
az storage blob upload-batch \
  --account-name '<storage-account>' --auth-mode login \
  --destination product-docs --destination-path mark7 \
  --source samples/docs --pattern "*.md"
```

Replace the account name with the approved Storage account in either example.

### Upload to S3

This command works in Bash and PowerShell:

```sh
aws s3 cp samples/docs/ 's3://<your-bucket>/mark7/' --recursive --exclude "*" --include "*.md"
```

## Files

| File | Topic |
| ---- | ----- |
| [`overview.md`](overview.md) | What Orbital Mark 7 is and who it's for |
| [`release-notes.md`](release-notes.md) | Preview and GA schedule, tenant caps |
| [`pricing.md`](pricing.md) | Token pricing, ramps, committed-use terms |
| [`regions.md`](regions.md) | Supported regions and data residency |
| [`limitations.md`](limitations.md) | Day-one limits and roadmap |
| [`quickstart.md`](quickstart.md) | Creating your first deployment |

> These documents describe a made-up product. Any resemblance to a real
> service is coincidental — they exist purely to exercise the KB pipeline.
