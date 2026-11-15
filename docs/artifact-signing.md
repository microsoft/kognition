# Artifact Signing

Tool and Model admission verifies real detached signatures when their Workspace
sets `spec.requireSignedArtifacts: true`. The shared hook applies to standalone
and Kubernetes mode. A nonempty string is no longer accepted as proof.

## Operator Trust

Configure exactly one admission-process source:

- `KOGNITION_ARTIFACT_SIGNING_KEYS_FILE`: an operator-owned JSON file, reread for each required-signature write. Mount its directory without subPath so projected updates remain visible.
- `KOGNITION_ARTIFACT_SIGNING_KEYS_JSON`: an operator-provided inline JSON value, read when admission starts. Restart/reload the process after changing inline trust.

The JSON maps exact namespace names to key IDs and PKIX public-key PEM strings:

```json
{
  "team-a": {
    "release-2026": "-----BEGIN PUBLIC KEY-----\n<base64-public-key>\n-----END PUBLIC KEY-----\n"
  }
}
```

Use real public keys, not the placeholder above. Supported verifier types are
ECDSA, Ed25519 and RSA of at least 2048 bits. Wildcard namespace trust, duplicate
JSON keys, unsupported keys, invalid PEM and oversized configurations are rejected.
Required-signature writes fail closed if namespace trust is missing/unavailable.
Private keys never belong in this file, Kubernetes resource specs or reports.

For Helm, provision an operator-owned Secret in the installation namespace with
key `public-keys.json`, then set `artifactSigning.existingSecret` to its name.
The chart mounts it read-only in standalone admission and the Kubernetes
controller/resource-webhook processes. It does not create the Secret or add
Secret API access. `artifactSigning.publicKeysKey` can select another existing
Secret key. Trust rotation remains an administrator action.

Compose forwards `KOGNITION_ARTIFACT_SIGNING_KEYS_JSON` to its API server. For
file-backed refresh, supply an explicit Compose override with a read-only
operator-file directory mount and `KOGNITION_ARTIFACT_SIGNING_KEYS_FILE` instead.
Never configure both sources. Existing unsigned Workspaces remain opt-in.

## Sign An Artifact

The manifest must explicitly declare `kind`, `metadata.namespace` and
`metadata.name`. Only Tool and Model are supported. Produce the canonical payload
offline; the command never contacts Kubernetes or the API server:

```bash
kognitionctl artifact signing-payload -f tool.yaml > tool.payload.json
cosign sign-blob --key '<operator-private-key-path>' --output-signature tool.sig tool.payload.json
```

**PowerShell 7.4 or newer (Windows):**

```powershell
kognitionctl artifact signing-payload -f tool.yaml > tool.payload.json
cosign sign-blob --key '<operator-private-key-path>' --output-signature tool.sig tool.payload.json
```

PowerShell 7.4 or newer preserves native stdout bytes during redirection. Do not
use Windows PowerShell 5.1 or a text pipeline such as `Out-File` for this payload.

The command emits exact bytes without a trailing newline. Use byte-preserving
redirection; do not round-trip through an editor, pretty-printer or shell text
encoding before signing. The private key and any passphrase stay local to the
signing tool and must not appear in logs or this document.

Place the base64 detached signature from `tool.sig` inside a versioned JSON string
in `spec.signature`, using a key ID pinned for that exact namespace:

```yaml
spec:
  signature: '{"version":1,"keyID":"release-2026","signature":"<base64-detached-signature>"}'
```

The payload binds kind, namespace, name and the normalized specification, excluding
the signature field itself. Status and server-owned metadata such as
resourceVersion are not signed. Sign the final specification after resolving
defaults; changing its endpoint, digest, configuration, identity scope or other
spec fields requires a new signature. Public signer keys cannot be supplied by
the artifact or tenant Workspace.

## Rotation And Limits

Allow old/new key IDs to overlap during an approved rollout. Remove the old ID
after artifacts and writers migrate; subsequent writes signed only by that key
fail. File-backed trust has no verifier cache after removal. An in-flight request
uses the trust snapshot it read; filesystem reload is not a transactional
revocation barrier across concurrent admission requests.

Configuration is bounded to 256 KiB, 200 namespace entries and 16 keys per
namespace. Artifact documents/payload preparation are bounded to 4 MiB and
signature envelopes to 16 KiB. Malformed, unknown-field, forged, wrong-key and
cancelled verifications are rejected without reporting signature/key material.

Historical strings such as `sha256:abc123` must be replaced with real signatures
before required-signature writes succeed. Manifest verification proves approved
specification, not the bytes served by a mutable endpoint. Preserve independent
artifact digest/version checks and reviewed deployment provenance.

See [ADR-0017](adr/0017-artifact-signature-trust.md) for trust boundaries.
