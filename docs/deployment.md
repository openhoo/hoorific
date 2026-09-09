# Deployment

Hoorific is distributed as source and build recipes. The `publish-image` job in
`.github/workflows/ci.yml` is configured to publish
`ghcr.io/openhoo/hoorific` after `verify` and
`publish-console-screenshots` succeed for a push to `main`; pull requests,
forks, manual runs, and non-`main` pushes do not publish. Local `hoorific:local`
Docker and Podman builds remain supported.

The Dockerfile produces one statically linked gateway image with two executable entrypoints:

- `hoorific migrate --config /etc/hoorific/config.json` applies pending database migrations and exits.
- `hoorific serve --config /etc/hoorific/config.json` starts the inference and management listeners.

The final image is based on `scratch`. It contains only the gateway, the CA trust bundle, full timezone data, minimal user/group records, and the required directories. It runs as UID/GID `10001`; there is no shell, package manager, or debugging utility. For the paths used by the supplied Compose and chart examples, writable state is confined to the mounted data directory and `/tmp`; `data_dir`, the SQLite path, and secret paths are configuration inputs and must point to readable locations that are mounted into the container. A read-only root filesystem still needs writable mounts for the data directory and `/tmp`.

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

The Dockerfile selects Go `1.27` and Bun `1.3.14` version tags. Those tags are not immutable supply-chain pins; production builders should pin and verify approved base-image digests. Its build graph copies `go.mod` and `go.sum` before backend sources and uses shared Go module/build caches. The schema stage copies only `tools/schema`, `internal/admin`, and `internal/core`; changes elsewhere do not invalidate that stage. The frontend stage installs from `web/package.json` and `web/bun.lock`, creates `web/src/generated`, generates API types from the fresh schema, and builds the console. The Go stage embeds those compiled assets; generated API types and host-built console output are not taken from the checkout.

To isolate builder caches, set a distinct namespace:

```sh
podman build \
  --build-arg HOORIFIC_CACHE_NAMESPACE=hoorific-local \
  --tag hoorific:local .
```

Cache mounts use locked sharing within a namespace. Separate namespaces avoid contention at the cost of reuse; they do not reduce total builder-side storage. The runtime-files stage copies certificates and timezone data from the selected Go image instead of running APT in the runtime stage. For debugging, use external tooling or a separate diagnostic container: `exec ... sh` is intentionally unavailable.

The following is a host-side fresh-checkout source sequence, not a guarantee of the Dockerfile's target OS/architecture or runtime-image provenance:

```sh
mkdir -p .artifacts web/src/generated
go run ./tools/schema --output .artifacts/admin-openapi.json
(cd web && bun install --frozen-lockfile)
bun run --cwd web generate-api
bun run --cwd web build
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o .artifacts/hoorific ./cmd/hoorific
```

The schema must be generated before `bun run generate-api`; the console must be built before the Go binary is compiled. Source-only generated files are intentionally not committed.

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
