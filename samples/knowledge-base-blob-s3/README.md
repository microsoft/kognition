# Knowledge Base With A Local S3 Source

The local fixture uses digest-pinned SeaweedFS 4.49 built from a checksummed
maintained upstream commit with Go 1.27.3. Kognition continues to use
the generic `s3` source provider. The Deployment and Service keep the name
`minio` as a compatibility alias; the unavailable MinIO image is not required.

The fixture runs as UID 1000 with a read-only root filesystem, dropped
capabilities, bounded writable volumes, authenticated S3 and a one-CPU/512-MiB
limit. Its NetworkPolicy exposes only the S3 API to the consuming namespace.
Development credentials are public example values, not production credentials.
The static runtime contains no OS packages, CA bundle or timezone database; it
serves the isolated local S3 API rather than making outbound HTTPS calls.

## Prerequisites

1. An approved, activated Kubernetes installation with the knowledge pipeline,
   Foundry configuration and pgvector prerequisites.
2. An explicit kubeconfig for that installation.
3. Python with `boto3` for upload. The installer checks the selected interpreter
   and installs the SDK only if the dependency check fails.

No AWS account required.

## Pipeline

1. [s3-store.yaml](s3-store.yaml) deploys the fixture and compatibility Service.
2. [apply.ps1](apply.ps1) verifies a loopback-only owned port-forward and invokes
   [upload_docs.py](upload_docs.py).
3. The uploader checks the expected object inventory and reads every object
   back to verify its content before the index run is created.
4. [manifests.yaml](manifests.yaml) declares the embedding Model, KB class,
   source, claim, Agent and explicit one-shot index run. Refresh is on demand.

Data is ephemeral. Reapplying the uploader verifies the same keys instead of
creating duplicate objects. Unexpected extra keys or corrupt readback fail
explicitly. The sample is not AWS S3 production conformance.

## Apply

```powershell
$image = '<approved-registry>/s3-fixture@sha256:<reviewed-manifest-digest>'
.\samples\knowledge-base-blob-s3\apply.ps1 -Kubeconfig $env:KUBECONFIG -FixtureImage $image
```

**Bash (Linux/macOS), with PowerShell 7 installed:**

```bash
source .venv/bin/activate
python -c "import boto3"
image='<approved-registry>/s3-fixture@sha256:<reviewed-manifest-digest>'
pwsh -NoProfile -File ./samples/knowledge-base-blob-s3/apply.ps1 \
  -Kubeconfig "$KUBECONFIG" -FixtureImage "$image"
```

Build the [fixture Dockerfile](Dockerfile) and publish it separately before
selecting the immutable reference. A local `docker build` tag alone has no
registry manifest digest in `RepoDigests`. Both environments require
`powershell-yaml`; see [command shells](../../docs/README.md#command-shells).

Pass `-FixtureImage $image` when a rebuilt image has a different reviewed digest.
The installer loads that exact reference only into the kind cluster selected by
the explicit kubeconfig. For another Kubernetes distribution, publish the image
to an approved registry and pass the reviewed immutable reference.

The command requires an existing explicit target file; it never picks the first
kind cluster. It waits for the fixture before uploading. Failed or skipped upload
inputs cannot produce successful installation.

Run the watch command in Bash or PowerShell:

```sh
kubectl -n default get knowledgebase s3-docs-kb -o wide -w
```

Fixture/API upload qualification passed with six documents, pagination and
anonymous/incorrect-credential denial under the stated resource/security limits.
Governed retrieval and deployment qualification are tracked in the
[blocker-resolution plan](../../docs/development-plans/features/knowledge-provider-execution/remaining-sample-blockers-plan.md).
Successful upload or indexing alone does not prove Agent retrieval.

## Using A Real S3 Store

Change the source in [manifests.yaml](manifests.yaml) and supply appropriate
credentials:

- **AWS S3:** set `region`, remove `endpoint`, and use real
  `accessKeyId`/`secretAccessKey` (or, on EKS with IRSA / EC2 instance
  profile, drop `credentialSecretRef` to use the ambient credential chain).
- **Cloudflare R2:** set `endpoint` to its HTTPS URL and the
  region value the provider expects (R2 uses `auto`).

Validate the chosen endpoint's supported S3 operations and authentication
separately. Keep real credentials outside source control and Agent pods.

## Field reference

See [`api/v1alpha1/memory.go`](../../api/v1alpha1/memory.go) for the full
`KBBlobSource` schema. Admission rules are enforced in
[`internal/admission/knowledgebase_validator.go`](../../internal/admission/knowledgebase_validator.go).
