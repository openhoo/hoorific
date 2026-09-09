# Deployment

Hoorific is distributed as source and build recipes. The `publish-image` job in
`.github/workflows/ci.yml` is configured to publish
`ghcr.io/openhoo/hoorific` after `verify` and
`publish-console-screenshots` succeed for a push to `main`; pull requests,
forks, manual runs, and non-`main` pushes do not publish. Local `hoorific:local`
Docker and Podman builds remain supported.

The Dockerfile produces a `scratch` image containing the statically linked,
CGO-free gateway and its sibling native `hoorific-codex-wire` helper. The
gateway exposes two executable commands:

- `hoorific migrate --config /etc/hoorific/config.json` applies pending database migrations and exits.
- `hoorific serve --config /etc/hoorific/config.json` starts the inference and management listeners.

The final image contains only those executables, the CA trust bundle, full
timezone data, minimal user/group records, and the required directories. It
runs as UID/GID `10001`; there is no shell, package manager, or debugging
utility. For the paths used by the supplied Compose and chart examples,
writable state is confined to the mounted data directory and `/tmp`;
`data_dir`, the SQLite path, and secret paths are configuration inputs and must
point to readable locations that are mounted into the container. A read-only
root filesystem still needs writable mounts for the data directory and `/tmp`.

All paths beginning with `.artifacts/` in this document are private operator-local output paths. They are not shipped proof files or public repository assets.
Once the runtime is configured, use the [Console guide](console.md) for the embedded operator workflow, including model collection and editing, routing setup, and the playground. Its screenshots use isolated synthetic fixtures rather than live-provider data.
## Published image and releases

The CI workflow's `publish-image` job runs after its `verify` and
`publish-console-screenshots` jobs succeed for a push to `main`. Pull requests,
forks, manual runs, and non-`main` pushes do not publish. Hooversion reads the
root `VERSION` file and follows Conventional Commits: `feat` makes a minor
release, `fix` and `perf` make patch releases, and `!` or a `BREAKING CHANGE:`
footer makes a major release. Release commits and merge/revert noise are
ignored.

The workflow uses the repository-default `GITHUB_TOKEN`; no PAT is needed when
repository policy permits the required Actions permissions. The initial GHCR
package may be private. Set its visibility to Public in the GitHub package
settings when anonymous pulls are required; private packages need registry
authentication.

When Hooversion reports a release, the job creates a `v<version>` Git tag and
GitHub Release and publishes the corresponding image at
`ghcr.io/openhoo/hoorific` with unprefixed `<version>` and `<major>.<minor>`
semver tags, plus `latest` and `sha-<7-char-commit>`. When no release is due,
`VERSION` stays unchanged and publication emits only `latest` and the actual
commit's `sha-<7-char-commit>` tag; no semver tag is invented. Prefer a full
semver or SHA tag for reproducible deployments; `latest` is mutable.

To use a published image in the chart, select the repository and a pinned tag
or digest explicitly as shown below.

## Configuration

The application accepts one strict JSON object and rejects unknown keys. A minimum standalone configuration is:

```json
{
  "schema_version": 1,
  "mode": "standalone",
  "data_dir": "/var/lib/hoorific",
  "listeners": {"inference": ":8080", "management": ":8081"},
  "storage": {
    "sqlite": {"path": "/var/lib/hoorific/hoorific.sqlite"},
    "postgres": {"dsn_file": ""}
  },
  "coordination": {"redis": {"url_file": ""}},
  "encryption": {"key_file": "/run/secrets/hoorific-master-key"},
  "oidc": {"issuer": "", "client_id": "", "client_secret_file": ""},
  "public_urls": {},
  "subscription_connectors": {"enabled": false}
}
```

`schema_version` must be `1`. `data_dir` and `encryption.key_file` are always explicit. Standalone mode requires an explicit SQLite path and an empty PostgreSQL DSN path. Cluster mode requires an explicit PostgreSQL DSN file and an empty SQLite path:

```json
{
  "schema_version": 1,
  "mode": "cluster",
  "data_dir": "/var/lib/hoorific",
  "listeners": {"inference": ":8080", "management": ":8081"},
  "storage": {
    "sqlite": {"path": ""},
    "postgres": {"dsn_file": "/etc/hoorific/secrets/postgres-dsn"}
  },
  "coordination": {"redis": {"url_file": "/etc/hoorific/secrets/redis-url"}},
  "encryption": {"key_file": "/etc/hoorific/secrets/encryption-master-key"},
  "oidc": {"issuer": "", "client_id": "", "client_secret_file": ""},
  "public_urls": {},
  "subscription_connectors": {"enabled": false}
}
```

Cluster mode requires Redis coordination at startup; it is not optional. `serve` requires a non-empty `coordination.redis.url_file`, reads it, and parses a Redis URL before it starts. Mount the file shown above and qualify Redis availability in the target environment; a missing, unreadable, or malformed file fails startup. `config validate` checks the JSON/schema contract but does not open the file or test the Redis service. OIDC issuer and client ID must be supplied together; when a client secret is required, keep it in a separate mounted file rather than embedding it. Provider OAuth registrations, when needed, are configured under the `oauth` object and reference secret files rather than embedding secret values. An OAuth credential import also requires a configured registration issuer and successful ID-token account evidence matching the configured connection account; there is no generic unverified-token import path.
The `:8081` management address in these container/cluster examples binds all interfaces in that network namespace. A native host deployment should use a loopback or otherwise protected management address unless a deliberate TLS/proxy and origin design is in place.

Validate a configuration without opening storage:

```sh
.artifacts/hoorific config validate --config /path/to/config.json
```

The ordinary startup sequence is migration followed by serving:

```sh
.artifacts/hoorific migrate --config /path/to/config.json
.artifacts/hoorific serve --config /path/to/config.json
```
`schema_version` in the configuration remains `1`; the durable database has
its own version and is currently at version `3`. Run `migrate` before
`serve` for an existing database as well as a new one. The version-3
migration adds the encrypted idempotency-record table and its expiry index;
it is additive and does not replace the database or the operator keyring.

### Authentication and trust boundaries

The inference and management listeners have different trust boundaries. Inference requests authenticate with Hoorific API keys (or a short-lived authenticated realtime ticket where that protocol supports it), not with management-console sessions. API-key secrets are returned only when issued or rotated; the server stores a verifier and re-resolves the active tenant, key revision, revocation state, role, and grants on request. Do not invent tenant, role, scope, or key claims in request headers; caller-supplied claims are rejected.

The management API does not accept inference API keys. It uses the loopback-only, one-time bootstrap flow or OIDC browser sessions, and it can accept scoped bearer admin tokens. OIDC accepts HTTPS issuers (HTTP is permitted only for a loopback issuer), validates the provider metadata, ID-token signature, audience, and nonce, and resolves an already enrolled issuer/subject identity; it does not auto-provision an operator. Admin token issue and revoke are owner-only. Roles and explicit token scopes are intersected with the current server-side role and tenant membership on every request; a bearer token is tenant-confined, while a cookie session may select only another tenant for which that subject is a member.

The gateway serves plain HTTP and does not terminate TLS. Put it behind a trusted TLS-terminating proxy on a protected network, or provide another private transport for the gateway hop, set `public_urls.management` to the exact externally trusted origin, and restrict management ingress. The gateway does not trust forwarded-proto headers; a proxy that terminates TLS and forwards plain HTTP must be qualified against the cookie, redirect, and origin checks rather than assumed compatible. `public_urls` configures origin/cookie behavior; it does not enable TLS.

`/health/live`, `/health/ready`, and `/metrics` are unauthenticated management routes. Treat their status and metrics as information for a restricted network, not as public authorization or readiness proof.
The loopback bootstrap guard is based on the peer address seen by the management process and does not trust forwarded headers. A host-published bridge port normally does not preserve a loopback peer; a proxy or sidecar that appears as loopback must be treated as part of the trusted boundary. Do not weaken the guard to make a port-forwarded bootstrap work.

## Connection client profiles

The management API accepts an optional `client_profile` on a connection. The
server validates the preset and connector matrix and rejects unsupported
combinations instead of silently ignoring profile fields.

| Preset | Compatible connectors | Behavior |
| --- | --- | --- |
| `custom` | `openai`, `anthropic`, `compatible`, `codex-subscription` | Static/header-only identity overrides |
| `codex-cli` | `openai`, `compatible`, `codex-subscription` | Static/header-only Codex CLI identity hint |
| `codex-desktop` | `openai`, `compatible`, `codex-subscription` | Static/header-only desktop identity hint |
| `zcode-desktop` | `openai`, `anthropic`, `compatible` | Static/header-only ZCode identity hint |
| `codex-passthrough` | `openai`, `compatible`, `codex-subscription` | Bounded caller metadata on native Responses |
| `codex-exec` | `openai`, `compatible`, `codex-subscription` | Native Codex CLI `exec` wire emulation |

Static profiles are header-only identity hints. They do not rewrite request
bodies, create session/request fingerprints, or emulate a complete desktop
client. Their default identity behavior is:

| Preset | Default identity headers |
| --- | --- |
| `custom` | Only the explicit `headers` map |
| `codex-cli` | `Originator: codex_cli_rs`; `User-Agent: codex_cli_rs/<version>` when a version is supplied |
| `codex-desktop` | No guessed defaults; explicit `Originator` and `User-Agent` are required |
| `zcode-desktop` | `User-Agent: ZCode/<version>`, `X-ZCode-App-Version: <version>`, `HTTP-Referer: https://zcode.z.ai`, `X-Title: Z Code` |

`codex-passthrough` is not an emulator. It requires real Codex input and
forwards only bounded caller identity and metadata on the native Responses
route. It leaves the caller's request body content intact, does not reproduce
TLS or raw header casing/order, and is unavailable on portable routes, other
protocols, and continuations. It has no static version or header defaults.

### Native Codex exec profile

`codex-exec` is the genuine native wire profile for the measured Codex CLI
`exec` persona. It is fixed to version `0.153.4`; no other Codex release is
promised or selected. The profile has no static identity-header map and
requires a persistent canonical lowercase UUIDv4 `installation_id`. The
installation ID belongs to the connection and must not be reused on another
profile. Omitting the version or sending an empty version is accepted only as
the profile's compatibility encoding; a nonempty value must be exactly
`0.153.4`.

```json
{
  "client_profile": {
    "preset": "codex-exec",
    "version": "0.153.4",
    "installation_id": "e7ccd59d-93e4-4cf4-bab4-4cc65f55e922"
  }
}
```

Generate a distinct installation ID for each configured connection; the value
above is only a configuration example.

The console generates a fresh installation ID with secure browser randomness
when this preset is selected. An operator may explicitly replace it with
another valid canonical UUIDv4. The saved ID remains attached to that
connection, while static profiles and passthrough omit the field. Selecting
passthrough also removes static headers and version; selecting a static preset
removes the installation ID.

Each `codex-exec` invocation is an independent isolated exec session. Hoorific
creates fresh UUIDv7 session, thread, request, turn, and context identities
for every invocation. It does not borrow the incoming client's Codex identity,
installation ID, or session state, and does not synthesize workspace facts.
The caller's actual prompt and tool definitions remain request content; the
gateway does not invent a harness prompt or tool implementation.

The supported persona is `Linux/Arch Linux Unknown/x86_64/dumb`, with native
OpenSSL `3.6.3` and HTTP/1.1 without ALPN. Native and existing portable Responses
generation are supported on the compatible connectors above. Upstream requests
always use Codex SSE; the caller still receives its requested JSON or streaming
response. Portable routes retain their existing strict protocol subset.
Unsupported endpoints, continuations, incompatible body controls, and connectors are
rejected before inference admission. The profile does not execute tools and
does not claim Codex TUI/Desktop, ZCode, full agent-loop, ChatGPT OAuth,
subscription entitlement, or provider-billing parity.

The native helper uses the gateway's validated network policy: Go validates
DNS answers and pins all permitted concrete addresses for each request, while
the helper does not independently resolve targets, proxies, or redirects.
Redirects are disabled; the URL hostname remains the TLS SNI/certificate
identity; certificate verification remains enabled. Native requests do not
forward caller authentication, cookies, proxy credentials, routing/billing
claims, trace headers, or arbitrary headers. The helper is supervised through
a per-process Unix socket in a temporary `0700` directory with a `0600`
socket, and a missing or broken helper fails closed before admission. These
controls do not make the gateway a TLS terminator.
Normalized numeric URL destinations must also match the approved IP pins.
Native TLS requires TLS 1.2 or newer. The helper accepts the configured
per-host limit up to 65536, separately caps active IPC requests at 256,
and retains at most 64 idle connections per cached upstream client.

### Native helper path and build

The scratch image contains `hoorific-codex-wire` beside
`/usr/local/bin/hoorific`. At runtime, an operator may set
`transport.native_engine_path` to an explicit helper path. When it is empty,
the gateway first looks for `hoorific-codex-wire` beside its executable and
then on `PATH`. Keep this path controlled by the service owner; it is an
executable-code trust boundary, not a provider setting.

The helper is built as a static Linux musl binary with vendored OpenSSL. The
Dockerfile uses the pinned
`docker.io/library/rust:1.95.0-alpine3.23@sha256:606fd313a0f49743ee2a7bd49a0914bab7deedb12791f3a846a34a4711db7ed2`
builder and Cargo cache mounts, then copies only the helper into the existing
scratch runtime. The Go gateway remains `CGO_ENABLED=0`. The runtime retains
the CA bundle, full timezone data, UID/GID `10001`, read-only-root
compatibility, and writable `/tmp` and state-directory contracts. The
measured `x86_64` persona does not constitute a qualification claim for other
architectures.

For a native host build, install Rust `1.95.0`, the
`x86_64-unknown-linux-musl` target, and a musl C toolchain, then build the
supplied crate without changing its lock:

```sh
rustup toolchain install 1.95.0 --profile minimal \
  --target x86_64-unknown-linux-musl
CC_x86_64_unknown_linux_musl=musl-gcc \
CARGO_TARGET_X86_64_UNKNOWN_LINUX_MUSL_LINKER=musl-gcc \
OPENSSL_STATIC=1 \
RUSTFLAGS='-C target-feature=+crt-static' \
cargo +1.95.0 build --locked --release \
  --manifest-path native/codex-wire/Cargo.toml \
  --target x86_64-unknown-linux-musl \
  --target-dir .artifacts/codex-wire-target
```

Point a development configuration at the resulting helper when it is not
next to the gateway:

```json
{
  "transport": {
    "native_engine_path": "/absolute/path/to/.artifacts/codex-wire-target/x86_64-unknown-linux-musl/release/hoorific-codex-wire"
  }
}
```

The native profile is a pinned wire-compatibility implementation, not a
general Codex runtime. It does not promise custom-CA rustls behavior, desktop
UI behavior, OAuth account authorization, provider subscription access, tool
execution, or billing equivalence.

For static profiles, the console's **Identity header overrides
(static/header-only)** JSON object accepts only identity headers such as
`User-Agent`, `Originator`, `Version`, `HTTP-Referer`, `X-Title`, ZCode
identity headers, `Anthropic-Beta`, and the X-Stainless runtime headers. For
passthrough, only incoming `Originator`, `User-Agent`, `Session-Id`,
`Thread-Id`, `X-Client-Request-Id`, `X-Codex-Window-Id`,
`X-Codex-Turn-Metadata`, `X-Codex-Beta-Features`, `Accept`, `Content-Type`,
and `Accept-Encoding` are eligible on native Responses. `Accept-Encoding` may
be absent or `identity` only; compressed encodings are rejected because the
gateway has no compressed-response decoder. Duplicate names, multivalues,
controls, invalid UTF-8, edge whitespace, oversized values, oversized
aggregate metadata, unknown `x-codex-*`, and headers nominated by
`Connection` are rejected. Authentication, cookies, host, content length,
forwarding, `X-Hoorific-*`, baggage, account/project/billing routing, and
other caller headers are never forwarded.

Omitting `client_profile` preserves the legacy/native connector behavior. A
profile neither grants provider entitlement nor changes Hoorific gateway
prices. If distinct client identities must be routed separately, give them
distinct connections and route targets. To remove a profile, update the
connection with the same data but omit `client_profile`; the omission is
intentional and is not a request to synthesize native headers.


## Cost, caching, and replay safety

Provider prompt caching is not gateway response caching. Prompt-cache
directives affect upstream token accounting; Hoorific has no general-purpose
response cache. The only response replay is opt-in through `Idempotency-Key`
and replays one authenticated request's stored terminal response. Hoorific
does not invent implicit controls or writes; it may translate explicit caller
controls into protocol markers, and it does not promise a cache hit or saving.
It forwards only caller-supplied controls that the selected wire protocol can represent.

### Prompt-cache controls

The accepted native controls are:

| Wire protocol | Representable controls and limits |
| --- | --- |
| OpenAI Chat and Responses | `prompt_cache_key`; `prompt_cache_retention` of `in_memory` or `24h`; `prompt_cache_options.mode` of `implicit` or `explicit`; `prompt_cache_options.ttl` of `5m`, `30m`, `1h`, or `24h`; and text-only `prompt_cache_breakpoint` with mode `explicit`. |
| Anthropic Messages | Top-level, content-block, and tool `cache_control` with type `ephemeral` and optional TTL `5m` or `1h`. |
| Gemini content | An existing `cachedContent` resource name in the form `cachedContents/{id}` or `projects/{project}/locations/{location}/cachedContents/{id}`. No portable TTL or cache-control directive is synthesized. |
| Bedrock Converse | `cachePoint` with type `default` and optional TTL `5m` or `1h`, placed after cacheable content (or as a tool cache point). OpenAI keys and Gemini references are not Bedrock controls. |
OpenRouter is the OpenAI-shaped exception for per-content/tool
`cache_control` and `session_id`; direct OpenAI Chat and Responses reject
those foreign controls. Do not assume that a JSON shape accepted by one
OpenAI-compatible connector is portable to another connector.

Gemini cached-content references are connection-bound: a request carrying one
must resolve to one connection, account, project, and region rather than
fan-out across route targets. Hoorific forwards the reference but does not
create or manage the Gemini cached-content resource; create it through the
provider's separately authenticated API/control plane and then use its
provider resource name.

A directive with no equivalent for the selected codec is rejected before
upstream dispatch instead of being silently dropped. Native same-protocol
payloads retain their supported semantics; translation is limited to
equivalent forms (for example, an Anthropic cache-control form can represent
a Bedrock default cache point, but an OpenAI cache key cannot).
See the primary provider references for [OpenAI prompt
caching](https://platform.openai.com/docs/guides/prompt-caching),
[Anthropic prompt caching](https://docs.anthropic.com/en/docs/build-with-claude/prompt-caching),
[Gemini context caching](https://ai.google.dev/gemini-api/docs/caching), and
[Bedrock prompt caching](https://docs.aws.amazon.com/bedrock/latest/userguide/prompt-caching.html).

### Price and usage configuration

Model prices are integer USD nanodollars per million tokens. The four
independent cache-rate JSON fields are:

- `cached_input_per_million` — cache-read input;
- `cache_write_input_per_million` — aggregate cache writes;
- `cache_write_5m_per_million` — the 5-minute write subset; and
- `cache_write_1h_per_million` — the 1-hour write subset.

`cache_write_5m_per_million` and `cache_write_1h_per_million` are subsets of
the aggregate write rate, not additional usage categories. A missing or
`null` rate means unknown; an explicit `0` means the configured category is
zero-cost. If a nonzero provider-reported category needs a missing rate, the
actual cost remains unknown rather than becoming zero or a base-rate guess.
When a TTL-specific write rate is absent, an explicitly configured aggregate
write rate may price that write subset.

Provider usage is normalized inclusively: `Usage.Input` already includes
ordinary, cache-read, and cache-write input tokens. `CachedInput`,
`CacheWriteInput`, `CacheWrite5mInput`, and `CacheWrite1hInput` are provider
detail/subset fields and must not be added to `Input` again. `Output` includes
reasoning output; `ToolInput` is informational provider metadata and does not
automatically add a second charge.

Automatic-caching rates are an operator responsibility. Configure a price
revision only from the current provider/model/account terms you have
verified; Hoorific does not fetch pricing or assume that caching is free,
automatic, or beneficial. `maximum_unit_cost` (nanodollars per its declared
`unit_operation`) and a token-derived `MaximumCost` are conservative
admission bounds/reservations, not actual spend. Token reservations use the
highest configured relevant input rate across ordinary, cache-read, and
cache-write categories.

If required usage or pricing evidence is missing, settlement keeps a
reconcilable unknown outcome/hold instead of fabricating a charge. Reconcile
with provider evidence through the management API. If observed actual spend
exceeds a configured maximum, settlement records the incurred amount and
increments accounting atomically; it does not clamp or roll back real spend.
Subsequent admission is blocked by the now-exhausted allowance.

### Idempotency keys

`Idempotency-Key` is opt-in; requests without it are not replay-captured.
The namespace is tenant plus the authenticated session/key subject (the raw
credential is never the subject), and the fingerprint covers the method,
path/query, body, and semantic request headers while excluding credentials,
the idempotency key, and the gateway request ID. A completed terminal response
is retained for 24 hours from completion, with a 32 MiB maximum captured
response body. Captured response bytes are encrypted with a purpose-separated
key derived from the operator's JSON AES-256-GCM keyring.

Keep old key IDs available while encrypted idempotency records can still be replayed. There is no general key-rotation command or API: rotation requires an operator-designed rewrap/migration covering every encrypted record and preserving the old IDs until all required reads and replays are complete. Never overwrite the key file in place. Pending, partial, failed, oversized, or otherwise unreplayable records return a conflict (normally `409` with `idempotency_in_progress`) and are durable negative state, not permission to dispatch again after expiry or restart.
Fingerprint conflicts are also `409`. Hoorific never blindly forwards
`Idempotency-Key` upstream to obtain provider-side HTTP retry behavior.
An idempotency result is not an exactly-once guarantee for provider execution or billing: a stored terminal result replays without a second gateway dispatch, while pending or unreplayable records conflict and do not authorize a second dispatch.

### Provider-specific controls and cancellation

- Replicate's `Prefer` and `Cancel-After` headers are accepted only on the
  prediction-creation operations (`predictions.create`,
  `models.predictions.create`, and `deployments.predictions.create`). One
  `Prefer` value must be `wait` or `wait=N` for `N` from 1 through 60.
  One `Cancel-After` value must be a provider-supported duration from 5
  seconds through 24 hours. They are not global controls for get, list, or
  cancel operations. See the [Replicate HTTP
  reference](https://replicate.com/docs/reference/http#predictions.create).
- A valid upstream `Retry-After` is preserved on portable and native
  rejection responses. It is timing information, not evidence that a retry
  is safe or that the prior request was free.
- Cohere embed-job cancellation can return the documented empty JSON object
  `{}` as a cancellation receipt. That receipt carries no usage or cost and
  does not by itself prove that every provider execution stopped. Hoorific
  reconciles the job's declared terminal status; it does not invent zero cost
  or mark every successful cancel response terminal. See the [Cohere embed-job
  reference](https://docs.cohere.com/reference/create-embed-job).
- For portable OpenAI Chat streaming, Hoorific requests upstream
  `stream_options.include_usage=true` so accounting can observe a terminal
  usage event. It exposes that usage event to the client only when the
  request included `stream_options.include_usage:true`; omitting the option
  hides usage in the client stream but does not disable internal accounting.
  Native requests follow their native wire contract.

## Master-key handling

The master key is an operator-managed JSON keyring; the container never generates it. Its shape is:

```json
{"current":"key-id","keys":{"key-id":"<base64-key>"}}
```

Every decoded key must be exactly 32 bytes for AES-256-GCM. Generate it on a protected host into a new path; the exclusive create refuses to overwrite an existing key:

```sh
umask 077
python3 - /path/to/master.key <<'PY'
import base64
import json
import os
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
key_id = os.urandom(16).hex()
key = base64.b64encode(os.urandom(32)).decode("ascii")
fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o400)
with os.fdopen(fd, "w") as stream:
    json.dump({"current": key_id, "keys": {key_id: key}}, stream)
    stream.write("\n")
PY
```

Keep key IDs and key material unchanged across restarts, upgrades, backups, and restores. The database and all retained key versions are one recovery unit; losing a key makes encrypted records unrecoverable. Do not overwrite the key file to perform rotation without a procedure that can decrypt existing data.
The keyring also protects provider credential envelopes and provider OAuth/device-flow state, native continuation records, and idempotency responses. There is no blanket key-rotation procedure in this source tree; the available credential rewrap operation is connection-scoped and is not a complete migration of these record types. Design and qualify any rewrap/migration before changing the current key, retain decryptable old key IDs throughout, and never overwrite the mounted key file.

## Build the image

Use Docker BuildKit, Podman, or another OCI-compatible builder with support for `RUN --mount=type=cache`:

```sh
podman build --tag hoorific:local .
# or: docker build --tag hoorific:local .
```

The Dockerfile selects Go `1.27`, Bun `1.3.14`, and the pinned Rust
`1.95.0-alpine3.23` builder for the native helper. Go remains
`CGO_ENABLED=0`. The Go and Rust base tags are not immutable supply-chain
pins unless the approved digests are retained; the native Rust builder is
pinned by digest in the Dockerfile. The build graph copies `go.mod` and
`go.sum` before backend sources and uses shared Go module/build caches. The
native stage copies `Cargo.toml` and `Cargo.lock` before helper sources and
uses locked Cargo registry, Git, and target caches. The schema stage copies
only `tools/schema`, `internal/admin`, and `internal/core`; changes elsewhere
do not invalidate that stage. The frontend stage installs from
`web/package.json` and `web/bun.lock`, creates `web/src/generated`, generates
API types from the fresh schema, and builds the console. The Go stage embeds
those compiled assets; generated API types and host-built console output are
not taken from the checkout.

To isolate builder caches, set a distinct namespace:

```sh
podman build \
  --build-arg HOORIFIC_CACHE_NAMESPACE=hoorific-local \
  --tag hoorific:local .
```

Cache mounts use locked sharing within a namespace. Separate namespaces avoid contention at the cost of reuse; they do not reduce total builder-side storage. The runtime-files stage copies certificates and timezone data from the selected Go image instead of running APT in the runtime stage. For debugging, use external tooling or a separate diagnostic container: `exec ... sh` is intentionally unavailable.

The following is a host-side fresh-checkout source sequence, not a guarantee
of the Dockerfile's target OS/architecture or runtime-image provenance:

```sh
mkdir -p .artifacts web/src/generated
go run ./tools/schema --output .artifacts/admin-openapi.json
(cd web && bun install --frozen-lockfile)
bun run --cwd web generate-api
bun run --cwd web build

rustup toolchain install 1.95.0 --profile minimal \
  --target x86_64-unknown-linux-musl
CC_x86_64_unknown_linux_musl=musl-gcc \
CARGO_TARGET_X86_64_UNKNOWN_LINUX_MUSL_LINKER=musl-gcc \
OPENSSL_STATIC=1 \
RUSTFLAGS='-C target-feature=+crt-static' \
cargo +1.95.0 build --locked --release \
  --manifest-path native/codex-wire/Cargo.toml \
  --target x86_64-unknown-linux-musl \
  --target-dir .artifacts/codex-wire-target
install -m 0755 \
  .artifacts/codex-wire-target/x86_64-unknown-linux-musl/release/hoorific-codex-wire \
  .artifacts/hoorific-codex-wire

CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' \
  -o .artifacts/hoorific ./cmd/hoorific
```

The schema must be generated before `bun run generate-api`; the console must
be built before the Go binary is compiled; and the native helper must be
built before a Codex exec request can use it. Source-only generated files and
local helper/build outputs are intentionally not committed. Docker builds use
the builder's native musl target for the image platform; the measured persona
and CI helper target are `x86_64`.

## Standalone Compose

Create a container-specific config and master-key file on the host, make both
readable by container UID/GID `10001`, and export absolute paths. The standalone
example above uses container paths and listens on `:8081`; Compose publishes
that management port only on host loopback. The README's native loopback config
uses different paths and must not be mounted unchanged. Keep the key mode
restrictive. Rootless Podman users can map ownership in the Podman user namespace;
rootful Docker users may use an equivalent host ownership operation:

```sh
umask 077
export HOORIFIC_CONFIG="$PWD/.local/hoorific-container/config.json"
export HOORIFIC_MASTER_KEY="$PWD/.local/hoorific-container/master.key"
export HOORIFIC_IMAGE=hoorific:local

# For rootless Podman, after creating the files:
podman unshare chown 10001:10001 "$HOORIFIC_CONFIG" "$HOORIFIC_MASTER_KEY"

podman compose up -d
curl --fail --retry 30 --retry-connrefused --retry-delay 1 \
  --retry-max-time 30 --max-time 2 http://127.0.0.1:8081/health/ready
```

Use `docker compose` with the image built by the same Docker engine when using Docker. The Compose file mounts the explicit configuration at `/etc/hoorific/config.json`, the explicit key at `/run/secrets/hoorific-master-key`, and a named `data` volume at `/var/lib/hoorific`. With the checked-in `name: hoorific`, that volume is normally named `hoorific_data`; a different Compose project name changes it. The `migrate` service must complete before `gateway` starts. To inspect or repeat migration:

```sh
podman compose run --rm migrate
podman compose logs --no-log-prefix migrate
podman compose up -d gateway
```

Compose publishes the inference port on the host address in `HOORIFIC_BIND_ADDRESS` (default `127.0.0.1`) and port `HOORIFIC_INFERENCE_PORT` (default `8080`). That variable changes the host-side publish address, not the listener address inside the container. Management is published on host loopback at `HOORIFIC_MANAGEMENT_PORT` (default `8081`), but the example listener `:8081` binds management on all container interfaces; peers sharing the container network may still reach it. Do not expose management publicly without an intentional TLS, trusted-origin, authentication, and network-control design.

The local bootstrap endpoint deliberately accepts only a loopback peer. A
request from a host browser or host `curl` through a normal Docker/Podman
published bridge port reaches the container with a non-loopback peer and is
rejected. The CLI `admin bootstrap` command can create a one-time code inside
the container, but that code is not redeemable through the ordinary host
bridge:

```sh
podman compose run --rm --no-deps gateway \
  admin bootstrap --config /etc/hoorific/config.json
```

For a local authenticated console, use the native loopback flow in the README. For a container deployment, configure OIDC or deliberately use a shared network namespace with a loopback connection, such as a sidecar, and separately secure its TLS and trusted-origin boundary. Merely sharing a Kubernetes namespace does not satisfy the loopback check. The binary does not terminate TLS. Do not weaken the bootstrap guard merely to make a published port work.

## Kubernetes / Helm

The chart is a cluster-mode template around external PostgreSQL and mandatory external Redis coordination. It expects externally created Secrets and does not generate or own the PostgreSQL DSN, Redis URL, OIDC client secret, or master key. Provision the referenced Secret names and keys first, then supply cluster configuration through values. The migration Job runs as a Helm pre-install/pre-upgrade hook; the serve Deployment is separate. The chart also creates separate inference and management Services, probes, a PodDisruptionBudget, anti-affinity, and configurable NetworkPolicies.

The checked-in `values.yaml` image (`ghcr.io/example/hoorific:0.1.0`) is
deliberately a placeholder. For the published image, set the repository to
`ghcr.io/openhoo/hoorific` and pin a full semver or SHA tag (or a digest):

```sh
helm upgrade --install hoorific ./charts/hoorific \
  --namespace hoorific --create-namespace \
  --values deploy/hoorific-values.yaml \
  --set image.repository=ghcr.io/openhoo/hoorific \
  --set image.tag=1.2.3 \
  --set config.coordination.redis.enabled=true \
  --set existingSecrets.redis.secretName=hoorific-redis \
  --set existingSecrets.redis.key=url
helm status hoorific --namespace hoorific
kubectl get jobs --namespace hoorific
```

For a digest-pinned deployment, set `image.digest=sha256:...`; the chart renders `repository@digest` when a digest is present. `deploy/hoorific-values.yaml` is an operator-provided file, not a repository path supplied by this source tree. The pre-created `hoorific-redis` Secret in this example must contain the Redis URL under `url`; PostgreSQL and encryption Secrets are also required.

The checked-in chart defaults to two stateless replicas, external PostgreSQL, and ephemeral `emptyDir` mounts for `/var/lib/hoorific` and `/tmp`. It is not a standalone SQLite deployment and its data mount is not a persistence claim. Although the checked-in values file leaves `config.coordination.redis.enabled` false, cluster `serve` still requires a non-empty Redis URL file; a default install therefore is not a runnable cluster until Redis is enabled and its Secret/configuration are supplied. OIDC remains optional, but its issuer, client ID, client-secret file, and referenced Secret must be supplied together when enabled. With `networkPolicy.enabled` (the default), empty ingress is deny-all and egress permits DNS only: explicitly allow the management/inference clients and external PostgreSQL, Redis, OIDC, and provider destinations required by the deployment. NetworkPolicy permits traffic but does not make PostgreSQL, Redis, OIDC, or provider transport confidential; configure TLS/authentication in those external services and their DSNs/URLs as required. The readiness probe checks the database readiness and whether the gateway is draining; it is not a provider, Redis, network-policy, or production-qualification check. Migration pods share the chart's base selector labels, so inspect Service EndpointSlices during hooks and do not treat them as serving replicas until migration has completed. The read-only root filesystem needs writable `/tmp` and data mounts. No Kubernetes installation or production qualification is claimed by the local deterministic verifier.

## Telemetry

Telemetry is opt-in and remains disabled in the checked-in chart defaults. The
existing Prometheus-compatible `/metrics` route is unchanged; enabling OTLP
does not replace it or expose a new public Service. Telemetry is intended for
operator-selected traces, metrics, and logs, not for exporting request or
response content.

The strict bootstrap JSON below shows a complete cluster configuration with
telemetry enabled. `sample_ratio` applies to traces and is between `0` and `1`;
`1` is the default. An empty `signals` array means all three signals. An empty
`exporters` array selects the environment-variable fallback described below.

```json
{
  "schema_version": 1,
  "mode": "cluster",
  "data_dir": "/var/lib/hoorific",
  "listeners": {"inference": ":8080", "management": ":8081"},
  "storage": {
    "sqlite": {"path": ""},
    "postgres": {"dsn_file": "/etc/hoorific/secrets/postgres-dsn"}
  },
  "coordination": {"redis": {"url_file": "/etc/hoorific/secrets/redis-url"}},
  "encryption": {"key_file": "/etc/hoorific/secrets/encryption-master-key"},
  "oidc": {"issuer": "", "client_id": "", "client_secret_file": ""},
  "public_urls": {},
  "subscription_connectors": {"enabled": false},
  "telemetry": {
    "enabled": true,
    "service_name": "hoorific",
    "service_version": "0.1.0",
    "environment": "production",
    "sample_ratio": 0.10,
    "exporters": [
      {
        "name": "observability-collector",
        "protocol": "http/protobuf",
        "endpoint": "http://otel-collector.observability.svc.cluster.local:4318",
        "headers_file": "",
        "insecure": true,
        "signals": []
      }
    ]
  }
}
```

`endpoint` is an OTLP base endpoint, not a signal-specific URL. HTTP
protobuf exporters append the signal path (`/v1/traces`, `/v1/metrics`, or
`/v1/logs`); include an upstream base path when the consumer requires one.
For example, a traces-only HTTP destination whose OTLP base is
`https://langfuse.example.invalid/api/public/otel` receives a request at
`.../api/public/otel/v1/traces`. A gRPC exporter uses the collector's gRPC
endpoint, for example
`otel-collector.observability.svc.cluster.local:4317`, with
`"protocol": "grpc"`. An `http://` endpoint is plaintext regardless of the
`insecure` value; use `https://` for TLS. `insecure: true` is an explicit
plaintext switch and never disables certificate verification on a TLS endpoint.

Each explicit exporter is an independent destination. The same signal may be
listed on several exporters for fan-out, and each exporter may select a
different signal subset. The following is the replacement `telemetry` member
for the complete object above:

```json
{
  "telemetry": {
    "exporters": [
      {
        "name": "tempo",
        "protocol": "grpc",
        "endpoint": "tempo.observability.svc.cluster.local:4317",
        "headers_file": "",
        "insecure": true,
        "signals": ["traces"]
      },
      {
        "name": "prometheus-collector",
        "protocol": "http/protobuf",
        "endpoint": "http://otel-collector.observability.svc.cluster.local:4318",
        "headers_file": "",
        "insecure": true,
        "signals": ["metrics"]
      }
    ]
  }
}
```

### Helm and secret-backed headers

The chart uses camelCase values and renders the strict snake_case JSON above.
For an exporter that needs headers, set `headersFile` to the file path that
will be mounted and set its chart-only `headersSecret` reference:

```yaml
config:
  telemetry:
    enabled: true
    serviceName: hoorific
    serviceVersion: "0.1.0"
    environment: production
    sampleRatio: 0.10
    exporters:
      - name: grafana
        protocol: http/protobuf
        endpoint: https://grafana.example.invalid/otlp
        headersFile: /etc/hoorific/secrets/telemetry-grafana-headers.json
        headersSecret:
          secretName: grafana-otlp-headers
          key: headers.json
        insecure: false
        signals: [traces, metrics, logs]
```

The existing Secret key above must contain one JSON object whose keys and
values are strings, for example an operator-created file at
`/secure/operator-only/grafana-headers.json` (the file contains the
backend-required authorization headers). Create the Secret from that
operator-only file:

```sh
kubectl create secret generic grafana-otlp-headers \
  --namespace hoorific \
  --from-file=headers.json=/secure/operator-only/grafana-headers.json
```

Do not put header contents in `values.yaml`, a Helm `--set` argument, or either
ConfigMap. The
chart mounts the selected key read-only at `headersFile` in both the serving
Deployment and the migration Job, while the ConfigMaps contain only the path.
Header rotation therefore requires the normal pod restart procedure when a
`subPath` mount is used. The chart rejects a missing protocol, endpoint,
unsupported signal, out-of-range sample ratio, or incomplete
`headersFile`/`headersSecret` pair.

The default NetworkPolicy allows DNS only. Add an egress rule for the
collector or OTLP backend (and its port) in `networkPolicy.egress`; the chart
does not infer safe CIDRs from an endpoint URL. Keep OTLP destinations off the
inference and management Services.

### Environment fallback

When telemetry is enabled with no explicit exporters, the runtime supports
this documented subset of the OpenTelemetry environment configuration:

- resource identity: `OTEL_SERVICE_NAME`, `OTEL_SERVICE_VERSION`, and
  `OTEL_RESOURCE_ATTRIBUTES`;
- signal selection: `OTEL_TRACES_EXPORTER`, `OTEL_METRICS_EXPORTER`, and
  `OTEL_LOGS_EXPORTER`;
- generic and signal-specific endpoints:
  `OTEL_EXPORTER_OTLP_ENDPOINT`,
  `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`,
  `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`, and
  `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT`;
- generic and signal-specific protocols:
  `OTEL_EXPORTER_OTLP_PROTOCOL`,
  `OTEL_EXPORTER_OTLP_TRACES_PROTOCOL`,
  `OTEL_EXPORTER_OTLP_METRICS_PROTOCOL`, and
  `OTEL_EXPORTER_OTLP_LOGS_PROTOCOL`;
- generic and signal-specific header maps:
  `OTEL_EXPORTER_OTLP_HEADERS`,
  `OTEL_EXPORTER_OTLP_TRACES_HEADERS`,
  `OTEL_EXPORTER_OTLP_METRICS_HEADERS`, and
  `OTEL_EXPORTER_OTLP_LOGS_HEADERS`;
- generic and signal-specific plaintext switches:
  `OTEL_EXPORTER_OTLP_INSECURE`,
  `OTEL_EXPORTER_OTLP_TRACES_INSECURE`,
  `OTEL_EXPORTER_OTLP_METRICS_INSECURE`, and
  `OTEL_EXPORTER_OTLP_LOGS_INSECURE`;
- trace sampling: `OTEL_TRACES_SAMPLER` and `OTEL_TRACES_SAMPLER_ARG`.

The generic values are used as defaults for a signal when a signal-specific
value is absent. Header environment values use the standard comma-separated
`key=value` form; prefer a mounted header file for credentials. Once one or
more explicit exporters are configured, they do not inherit ambient endpoint,
protocol, header, or insecure settings from these variables. This prevents an
explicit destination from accidentally receiving another signal's credentials.
The chart does not claim support for arbitrary `OTEL_*` variables.

### Collector and consumer routing

A Collector is useful when credentials, retries, batching, and backend
routing should be kept outside the gateway. A minimal routing shape is:

```yaml
receivers:
  otlp:
    protocols:
      grpc: {}
      http: {}

processors:
  batch: {}

exporters:
  otlphttp/grafana:
    endpoint: https://grafana.example.invalid/otlp
  otlphttp/tempo:
    endpoint: https://tempo.example.invalid/otlp
  prometheusremotewrite/prometheus:
    endpoint: https://prometheus.example.invalid/api/v1/write

service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlphttp/tempo]
    metrics:
      receivers: [otlp]
      processors: [batch]
      exporters: [prometheusremotewrite/prometheus]
    logs:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlphttp/grafana]
```

Configure Collector authentication and TLS through its own secret manager or
environment mechanism; the example intentionally has no credentials. A
Grafana OTLP endpoint, Tempo OTLP receiver, and Prometheus remote-write
endpoint are different consumer contracts. Prometheus can instead scrape
Hoorific's existing `/metrics`; an OTLP metrics pipeline is a separate path
and does not make that route authenticated.

Jaeger deployments that enable an OTLP receiver can be reached through an
OTLP exporter or Collector route; Hoorific does not emit a Jaeger-native
protocol. AI-oriented OTLP consumers such as Langfuse can be used as
traces-only destinations when their configured endpoint, authentication, and
accepted OTLP signal are compatible. Hoorific emits generic OTLP spans and
does not promise a consumer's vendor-specific LLM semantic conventions,
prompt/cost schema, or ingestion behavior. In particular, the Langfuse
base-path example above is a compatibility shape, not live vendor
verification. Qualify the selected consumer and version in the target
environment.

### Privacy, cardinality, and lifecycle

Telemetry is disabled by default. Instrumentation does not export prompts,
completions, API keys, credential material, or arbitrary request headers, and
this documentation makes no claim of tool-execution telemetry. Model
identifiers, where useful for a trace, belong on spans rather than metric
dimensions. Default metric dimensions are deliberately bounded: tenant IDs,
request IDs, model identifiers, and raw upstream values are not unbounded
metric labels. Operator-supplied `OTEL_RESOURCE_ATTRIBUTES` can still create
high-cardinality data, so keep those attributes stable and non-sensitive.

`service_name` and `service_version` provide the configured service/resource
identity. `environment` is emitted as `deployment.environment.name`.
`OTEL_RESOURCE_ATTRIBUTES` is merged into resource metadata for every enabled
setup, including explicit exporters. `sample_ratio` controls trace
sampling only; metrics and logs are not sampled by that field. A ratio of `0`
samples no new root traces, while an incoming sampled parent context is
honored. A ratio of `1` samples every eligible trace subject to the runtime's
normal parent-context behavior.

### AI signal fields and boundaries

The gateway emits bounded, generic AI-oriented fields rather than a complete
provider or tool-execution event stream. The exact names and limits are:

| Signal | Exact names and semantics | Boundary |
| --- | --- | --- |
| Lifecycle spans | `gateway.request` (INTERNAL), `gateway.attempt` (CLIENT), and `gateway.resource_reconcile.poll` (CLIENT). | An attempt span exists per admitted `BeginAttempt` permit; reconcile spans represent native durable polls. |
| Request/result span attributes | `gen_ai.operation.name`, `gen_ai.provider.name`, `gen_ai.request.model`, `gen_ai.request.stream`, `gen_ai.response.model`, and `error.type`. | Attempt spans record the concrete upstream request model. The request span retains the caller alias separately as `hoorific.gateway.request.model`. The response model is emitted only after a decoded result or stream-start event; model identifiers are span attributes only. |
| Usage and cache subsets | `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`, `gen_ai.usage.cache_read.input_tokens`, `gen_ai.usage.cache_write.input_tokens`, and `gen_ai.usage.reasoning.output_tokens`. | Only nonnegative known counts are emitted. Cache-read and cache-write values are subsets, not extra input totals. |
| Cost state | `hoorific.gen_ai.cost.known` and `hoorific.gen_ai.cost.usd`, alongside `hoorific.gen_ai.cache.requested`, `hoorific.gen_ai.cache_read.known`, `hoorific.gen_ai.cache_write.known`, `hoorific.gen_ai.reasoning.known`, `hoorific.gen_ai.input_tokens.known`, `hoorific.gen_ai.output_tokens.known`, and `hoorific.gen_ai.total_tokens.known`. | Observed actual cost stays unknown unless authoritative `ActualCost` evidence exists; the gateway does not infer or fabricate a charge. |
| Streaming timing | `gen_ai.response.time_to_first_chunk`, `gen_ai.client.operation.time_to_first_chunk`, and `hoorific.gateway.first_useful_content`. | Measures elapsed time from upstream dispatch to the first useful `TextDelta` or tool-call event, not the first byte and not model-inference TTFT. |
| Metrics | `gen_ai.client.operation.duration` and `gen_ai.client.token.usage` (input/output token types), plus the timing metric above. | Dimensions are limited to operation, provider, outcome, `error.type`, and `gen_ai.token.type`; model and request identifiers are never metric dimensions. |
| Gateway outcome attributes | `hoorific.gateway.outcome`, `hoorific.gateway.error_type`, `hoorific.gateway.retry_count`, `hoorific.gateway.retry`, `hoorific.gateway.protocol`, `hoorific.gateway.framing`, `hoorific.gateway.native`, `hoorific.gateway.cancellation`, and `hoorific.gateway.resource_reconcile`. | Completion records retain bounded outcome metadata, not prompts, responses, raw errors, URLs, credentials, or arbitrary headers. Cancellation evidence is not proof that remote execution stopped. |
| Tool and opaque protocol limits | `hoorific.gen_ai.tool_definitions.count` and `hoorific.gen_ai.tool_calls.count`. | These are counts, not tool names, schemas, arguments, or proof of execution. Opaque native WebSocket, duplex, and media paths cannot observe payload, usage, or first-useful-content fields. |

These fields are generic OTLP data and do not claim complete external-provider
or vendor-specific semantic-convention compatibility. A consumer must qualify
the signals it understands; the gateway does not claim full tool tracing or
provider-side model timing.

The runtime initializes telemetry before gateway construction and flushes
providers during graceful shutdown with bounded export timeouts. Export
failures are sanitized before local logging and do not change the existing
request or streaming contract. Keep the chart's
`terminationGracePeriodSeconds` long enough for the bounded flush (the
default is 30 seconds); a forced termination can still discard queued
telemetry. Disabling telemetry leaves externally installed global providers
untouched and performs no telemetry network export.

## Backups

### Standalone SQLite

Stop the writer before copying the database. The store uses SQLite WAL mode, so do not copy only `hoorific.sqlite` while `hoorific.sqlite-wal` or `hoorific.sqlite-shm` may contain committed state. The following uses a host-native `sqlite3` CLI and absolute source paths. For the Compose named volume, exposing the file to the host requires qualified, ownership-aware tooling; the scratch gateway image has no `sqlite3`, and a host user cannot necessarily read files owned by container UID `10001`. Do not substitute a generic copy helper.

For the checked-in Compose deployment, stop the writer with
`podman compose stop gateway`; stop a native writer with its own supervisor.
Run the following only once every writer is stopped.

```sh
set -eu
umask 077
export HOORIFIC_SQLITE_SOURCE="${HOORIFIC_SQLITE_SOURCE:?Set an absolute path to the stopped SQLite file}"
export HOORIFIC_MASTER_KEY="${HOORIFIC_MASTER_KEY:?Set an absolute path to the master key}"
install -d -m 0700 backups
BACKUP_DIR="$(mktemp -d "$PWD/backups/2026-01-01.XXXXXX")"
chmod 0700 "$BACKUP_DIR"
(
  cd "$BACKUP_DIR"
  sqlite3 -readonly "$HOORIFIC_SQLITE_SOURCE" \
    ".backup 'hoorific.sqlite'"
  install -m 0400 "$HOORIFIC_MASTER_KEY" master.key
  sha256sum hoorific.sqlite master.key > SHA256SUMS
)
printf 'backup: %s\n' "$BACKUP_DIR"
```

The SQLite `.backup` operation reads the main file and WAL into a consistent
new snapshot. If an SQLite-aware backup is unavailable, preserve the main
file and both WAL sidecars as one stopped, consistent set and include every
copied file in the checksum set; do not assume `busybox cp` of the main file
is sufficient. Keep the key in a separate protected, operator-readable path;
rootless Podman ownership mapping can make a container-readable key unreadable
to the host backup user.

If filesystem snapshots are used instead, take an application-consistent snapshot and retain the key in the same protected backup set.

### Cluster PostgreSQL

Do not place a DSN or password in a command-line argument. Configure libpq through a protected service file and password file, then pass only the service name to PostgreSQL tooling:

```sh
set -eu
umask 077
export HOORIFIC_MASTER_KEY="${HOORIFIC_MASTER_KEY:?Set an absolute path to the master key}"
install -d -m 0700 backups
BACKUP_DIR="$(mktemp -d "$PWD/backups/2026-01-01.XXXXXX")"
export PGSERVICEFILE=/path/to/protected/pg_service.conf
export PGPASSFILE=/path/to/protected/pgpass
export PGSERVICE=hoorific-backup   # operator-provided service name
pg_dump --format=custom \
  --file="$BACKUP_DIR/hoorific.dump" \
  --dbname="service=${PGSERVICE:?Set PGSERVICE to the protected service name}"
install -m 0400 "$HOORIFIC_MASTER_KEY" "$BACKUP_DIR/master.key"
(cd "$BACKUP_DIR" && sha256sum hoorific.dump master.key > SHA256SUMS)
printf 'backup: %s\n' "$BACKUP_DIR"
```

Cluster mode requires Redis at startup. Redis holds disposable coordination/cache hints rather than the authoritative database; back it up or restore it only according to the Redis service's own policy, and never treat it as a substitute for the PostgreSQL dump or master-key backup.

## Restore and master-key recovery

Verify checksums from the backup directory, provision a fresh target, and never restore over a live database or old key path. Standalone restoration below is host-native; exposing a container volume to this CLI still requires qualified ownership-aware tooling.

```sh
set -eu
umask 077
export BACKUP_DIR="${BACKUP_DIR:?Set the absolute backup directory}"
(cd "$BACKUP_DIR" && sha256sum --check SHA256SUMS)
case "${HOORIFIC_MODE:?Set HOORIFIC_MODE=standalone or cluster}" in
standalone)
  export RESTORE_DIR="${RESTORE_DIR:?Set a new absolute restore directory}"
  mkdir -m 0700 -- "$RESTORE_DIR"       # fails atomically if the target exists
  install -m 0600 "$BACKUP_DIR/hoorific.sqlite" "$RESTORE_DIR/hoorific.sqlite"
  install -m 0400 "$BACKUP_DIR/master.key" "$RESTORE_DIR/master.key"
  ;;
cluster)
  export PGSERVICEFILE=/path/to/protected/pg_service.conf
  export PGPASSFILE=/path/to/protected/pgpass
  export PGSERVICE=hoorific-restore-empty  # a new, empty target service
  pg_restore --exit-on-error --single-transaction \
    --dbname="service=${PGSERVICE:?Set PGSERVICE to the protected service name}" \
    "$BACKUP_DIR/hoorific.dump"
  # Recreate the external encryption Secret from exactly
  # "$BACKUP_DIR/master.key" in a fresh target without overwriting a
  # differing Secret, then run the separately configured Helm migration.
  ;;
*)
  echo "HOORIFIC_MODE must be standalone or cluster" >&2
  exit 2
  ;;
esac
```

For standalone, create and review a new configuration before starting anything.
Its `data_dir`, `storage.sqlite.path`, and `encryption.key_file` must point to the
restored directory, database, and key—not the old deployment. Then, as a separate
step using the source-built binary:

```sh
set -eu
: "${HOORIFIC_RESTORED_CONFIG:?Set the new reviewed standalone config path}"
.artifacts/hoorific config validate --config "$HOORIFIC_RESTORED_CONFIG"
.artifacts/hoorific migrate --config "$HOORIFIC_RESTORED_CONFIG"
.artifacts/hoorific serve --config "$HOORIFIC_RESTORED_CONFIG"
```

For Kubernetes, restore PostgreSQL into the new empty target service above,
recreate the externally managed master-key Secret from the exact backed-up
bytes without overwriting a differing Secret, and run the Helm upgrade so the
migration hook executes before the Deployment rolls forward. Do not start an
old Compose configuration after a restore. Never create a new key to replace a
lost key, and never rotate the key by overwriting this file; use a separately
designed key-rotation procedure that can decrypt existing data.

## Limits of these instructions

These instructions describe configuration and operator procedures. They do not execute image builds, Compose startup, Helm rendering, Kubernetes admission, migration, backup, or restore. Health behavior, storage performance, external database/Redis availability, and compatibility with a particular cluster or registry must be qualified in the target environment.
