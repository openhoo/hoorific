# Security policy

## Reporting a vulnerability

Please do not disclose suspected vulnerabilities, credentials, exploit details,
or sensitive evidence in a public issue, discussion, or pull request.

When GitHub private vulnerability reporting is enabled for this repository, use
[GitHub's private vulnerability reporting form](https://github.com/openhoo/hoorific/security/advisories/new).
If the form is unavailable, do not substitute a public issue; retain the report
until the maintainers publish a private channel.

Include only the information needed to reproduce and assess the report:

- affected commit, version, image digest, or deployment;
- affected component and configuration;
- impact and prerequisites;
- minimal reproduction steps and safe proof of concept;
- suggested mitigation, if known; and
- whether the issue is already public.

Do not send live secrets, production data, private endpoints, browser storage
state, database files, or unredacted provider responses. Use synthetic data and
redact tokens, account identifiers, cookies, and user content.

## Maintainer handling

Maintainers will keep reports private while validating impact, identify affected
versions, prepare a fix and regression coverage, and coordinate disclosure
through a private GitHub advisory when appropriate. This project has no
published response-time SLA or supported-version promise yet; disclosure timing
and support scope are decided from the validated report.

## Scope

Reports may cover the Hoorific gateway, management console, SDK-facing protocol
surface, configuration and credential handling, qualification tooling, and the
container image in this repository. Provider accounts, third-party services,
and issues requiring live credentials should include a minimal local
reproduction where possible.

## Security boundaries

Hoorific is a source distribution, not a hosted service or a published image.
The gateway's inference and management listeners are separate trust
boundaries. The binary serves plain HTTP; it does not terminate TLS. A
deployment that crosses a host or cluster boundary must provide a trusted TLS
terminating proxy on a protected network, or another private transport for the
gateway hop, set the exact management `public_urls` origin, and restrict
management ingress. The gateway does not trust forwarded-proto headers, so a
TLS-terminating proxy that forwards plain HTTP must be qualified against the
cookie, redirect, and origin checks. The checked-in Compose and Helm examples
are configuration examples, not a guarantee that a deployment is
network-isolated or production-qualified.

### Authentication, tenants, and roles

- Inference requests use tenant-scoped Hoorific API keys, carried through the
  protocol-appropriate credential header (and the Gemini `key` query parameter
  where applicable), or a short-lived authenticated realtime ticket where
  supported. Key secrets are returned only on issue/rotation; the server keeps a
  verifier and rechecks the tenant, key revision, revocation state, role, and
  grants. Reserved identity-override headers, including `X-Hoorific-*` and
  `X-Key-ID`, are rejected.
  Query-string keys can leak through client, proxy, or access logs; prefer
  headers and scrub any query-bearing request logs.
- The management API does not accept inference API keys. Browser access uses
  the loopback-only one-time bootstrap flow or OIDC. OIDC issuers must use
  HTTPS, except for a loopback HTTP issuer; discovery and ID-token validation
  check the configured issuer, signature, audience, and nonce. An OIDC
  identity must be pre-enrolled with an immutable issuer/subject binding;
  successful provider login does not auto-provision an operator.
- `/health/live`, `/health/ready`, and `/metrics` are unauthenticated routes.
  Keep the management listener private even when a readiness or metrics
  system needs to reach it.
- Cookie sessions last 12 hours and are `HttpOnly`/`SameSite=Lax`; HTTPS
  deployments use a `__Host-` cookie. Cookie mutations require the exact
  configured origin and CSRF token. Scoped bearer admin tokens are tenant
  confined, do not support cookie-style tenant switching, and are re-resolved
  against current membership and role on each request. Only owners can issue
  and revoke these tokens; the token secret is shown only at issue.
- Roles are server-managed, not caller claims: `owner` has the wildcard
  administrative policy; `admin` has broad configuration and resource
  management; `operator` has operational connection/catalog/job/playground
  access; `auditor` has usage, audit, and budget read access; and `viewer`
  has catalog/health read access. Actual grants are the current role policy
  and explicit token scopes, and all resources remain tenant-scoped. These
  labels are not a promise of access to a provider account or service.

### Credentials and encryption

The operator-managed JSON keyring contains 32-byte AES-256-GCM keys. It
protects provider credential envelopes, provider OAuth/device-flow state,
native continuation records, and encrypted idempotency responses. Keep the
database and every retained key ID as one recovery unit. There is no general
key-rotation command or complete migration in this source tree; the available
credential rewrap operation is connection-scoped. Do not overwrite a key file
or discard an old key ID without a separately designed, qualified rewrap/
migration that preserves decryptability of every record type.

Provider OAuth credential import is not an assertion that a token belongs to
an account. In the current composition it requires a configured OAuth
registration issuer and successful ID-token verification whose subject matches
the configured connection account; direct import without an account verifier
is unavailable. Provider accounts, OIDC tenants, and third-party control
planes remain outside Hoorific's entitlement guarantees.

### Caching, accounting, and replay

Prompt-cache directives are upstream controls, not a Hoorific response cache
or a promise of a cache hit, saving, or free automatic caching. Missing
provider usage or price evidence remains an explicitly reconcilable unknown,
not zero cost. Admission maximums are reservations/bounds; they do not undo
provider spend that exceeds a maximum.

Response replay is opt-in through `Idempotency-Key`. Completed terminal
responses are retained for 24 hours, scoped to tenant plus authenticated
subject, and capped at a 32 MiB captured body; captured bytes are encrypted.
Pending, failed, partial, oversized, and unreplayable records are durable
negative state and return conflict rather than authorizing a second dispatch.
An idempotency key is not forwarded upstream as a provider retry guarantee,
and no gateway idempotency result is an exactly-once guarantee for provider
execution or billing.

## Qualification and deployment evidence

Local deterministic fixtures, screenshots, browser workflows, helper
containers, and source-level checks demonstrate only the exercised behavior.
They do not prove live-provider entitlements, OIDC enrollment, external
PostgreSQL/Redis availability, TLS/proxy correctness, registry provenance,
cluster admission, backup/restore success, or production performance. Report
the actual deployment and configuration, and state which external
dependencies were not exercised.
