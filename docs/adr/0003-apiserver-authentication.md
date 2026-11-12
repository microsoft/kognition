# ADR-0003 — API server authentication (OIDC) and TLS

- **Status:** Accepted
- **Date:** 2026-07-12
- **Deciders:** Kognition core team

## Context

Kognition runs in two modes:

- **kubernetes mode** — resources are CRDs served by the real Kubernetes API
  server, which already provides authentication, authorization, TLS, and audit.
- **standalone mode** — a first-party REST API server (`cmd/kognition-apiserver`)
  serves the `kognition.dev/v1alpha1` resources backed by PostgreSQL.

In standalone mode the API server historically had **no authentication and no
TLS**. The only middleware was request-ID injection, and identity-sensitive
paths (notably approval quorum, where distinct approvers must be counted) trusted
a caller-asserted `approver` field. A code comment acknowledged this and told
operators to "front this endpoint with an authenticating proxy." That is a valid
pattern, but it makes the platform insecure by default and shifts a security
control entirely onto infrastructure the chart does not ship or enforce.

For wide adoption the inbound auth story must:

1. Work with the identity providers organizations already run — Microsoft Entra
   ID, Google, Auth0, Okta, Keycloak/Dex, and any spec-compliant issuer.
2. Be self-contained (secure without requiring a specific gateway), while still
   composing with a gateway for enterprises that centralize SSO there.
3. Give identity-sensitive handlers a **verified** principal instead of a
   caller-asserted claim.
4. Be safe to adopt incrementally (opt-in) so existing dev/demo flows keep
   working.

## Decision

Add native **OIDC bearer-token authentication** and **TLS** to the standalone API
server, both opt-in.

### Authentication

- New `internal/auth` package with an `Authenticator` interface
  (`Authenticate(ctx, token) (*Identity, error)`), an `Identity` carried in the
  request context, and HTTP middleware.
- **OIDC authenticator** (`coreos/go-oidc`): performs issuer discovery
  (`.well-known/openid-configuration`), fetches and caches JWKS (with rotation),
  and validates each bearer JWT's signature, issuer, audience, and expiry. It
  accepts a list of trusted providers, so multiple issuers can be honored.
  Username and groups are mapped from configurable claims (default `email` and
  `groups`).
- **Static bootstrap-token authenticator** for service-to-service / admin use,
  compared in constant time. Combined with OIDC via a `Chain`.
- **Middleware** rejects requests without a valid token (`401`, with
  `WWW-Authenticate: Bearer`), optionally enforces membership in a required
  group (`403`), and bypasses the health endpoints. On success it stores the
  `Identity` in the request context.
- Handlers derive the acting principal from the verified `Identity` — the
  approval-quorum path now uses `Identity.Username` instead of the caller
  asserted `approver` field when the request is authenticated.

### TLS

- The server serves HTTPS via `ListenAndServeTLS` when `Config.TLSCertFile` and
  `Config.TLSKeyFile` are set.

### Configuration

- Environment (consumed by `cmd/kognition-apiserver`): `KOGNITION_AUTH_ENABLED`,
  `KOGNITION_OIDC_ISSUER`, `KOGNITION_OIDC_AUDIENCE`,
  `KOGNITION_OIDC_USERNAME_CLAIM`, `KOGNITION_OIDC_GROUPS_CLAIM`,
  `KOGNITION_OIDC_REQUIRED_GROUPS`, `KOGNITION_BOOTSTRAP_TOKEN`,
  `KOGNITION_TLS_CERT_FILE`, `KOGNITION_TLS_KEY_FILE`. When auth is disabled the
  server logs a prominent warning.
- Helm: `apiserver.auth.*` (OIDC provider + optional bootstrap-token Secret) and
  `apiserver.tls.*` (a `kubernetes.io/tls` Secret mount and HTTPS probe scheme).

### Defaults

Both auth and TLS default to **off** for backward compatibility. In kubernetes
mode, authentication continues to be handled by the Kubernetes API server.

### CLI client

In standalone mode `kognitionctl` authenticates with a bearer token supplied
through `KOGNITION_API_TOKEN` or `--token-file <path>`; the file takes
precedence and keeps the secret out of process arguments. The token is sent only
to the exact `--server` origin, is never added to a request that already carries
credentials, and is refused over plaintext HTTP unless the server is loopback.
Without a token the unauthenticated local workflow is unchanged. Kubernetes mode
ignores both settings and uses kubeconfig credentials.

## Consequences

**Positive.**

- The standalone API can be secured without an external proxy, using the org's
  existing IdP. TLS terminates at the server (or, alternatively, at the Ingress —
  see `ingress.tls`).
- Approval quorum and audit can rely on a verified identity.
- Opt-in defaults mean no disruption to existing dev/demo/test flows; the
  `httptest`-based server tests run unchanged because the authenticator is nil
  unless configured.
- Composes with a gateway: an org can still front the server with oauth2-proxy /
  Envoy and additionally require OIDC at the server for defense in depth.

**Negative / to accept.**

- Authorization is intentionally minimal in this iteration: authentication plus
  an optional required-group gate. Fine-grained, per-resource RBAC is deferred.
- OIDC discovery requires network egress from the API server to the issuer at
  startup and for JWKS refresh; NetworkPolicies must allow it (443).
- Adds dependencies (`coreos/go-oidc/v3`, `go-jose/v4`).
- Because auth is opt-in, an operator who exposes the server without enabling it
  is still insecure; the startup warning and docs mitigate but cannot prevent
  this.

## Alternatives Considered

1. **Gateway/proxy only (no server-side auth).** Rejected as the default because
   it leaves the server open if reached directly and pushes a security control
   onto infra the chart does not ship. Retained as a documented, composable
   overlay for enterprise SSO.
2. **Static bearer tokens as the primary mechanism.** Rejected for human callers
   (no SSO, manual rotation), but kept as a bootstrap/service option.
3. **mTLS-only.** Rejected as the primary mechanism: strong for
   service-to-service but poor UX for humans/CLI and a heavier cert-distribution
   burden. Can be layered later.
4. **Make authentication mandatory (fail-closed by default).** Rejected for this
   iteration to avoid breaking existing zero-config dev/demo usage; revisit once
   auth adoption is broad, potentially flipping the default in a future major.

## Related

- Implementation: `internal/auth/`, `internal/apiserver/server.go`,
  `internal/apiserver/handler.go`, `cmd/kognition-apiserver/main.go`.
- Operations guide: [docs/security.md](../security.md).
- Ingress/TLS at the edge: `deploy/helm/kognition/templates/ingress.yaml`.
