# Deployment

Hoorific is distributed here as source and build recipes. The repository does not publish a container image or release artifact yet; build `hoorific:local` or provide an image from a registry you control before using the Helm chart.

The Dockerfile produces one statically linked gateway image with two executable entrypoints:

- `hoorific migrate --config /etc/hoorific/config.json` applies pending database migrations and exits.
- `hoorific serve --config /etc/hoorific/config.json` starts the inference and management listeners.

The final image is based on `scratch`. It contains only the gateway, the CA trust bundle, full timezone data, minimal user/group records, and the required directories. It runs as UID/GID `10001`; there is no shell, package manager, or debugging utility. Writable state is confined to the mounted data directory and `/tmp`, so a read-only root filesystem still needs writable mounts for both.

All paths beginning with `.artifacts/` in this document are private operator-local output paths. They are not shipped proof files or public repository assets.
Once the runtime is configured, use the [Console guide](console.md) for the embedded operator workflow, including model collection and editing, routing setup, and the playground. Its screenshots use isolated synthetic fixtures rather than live-provider data.

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
  "coordination": {"redis": {"url_file": ""}},
  "encryption": {"key_file": "/etc/hoorific/secrets/encryption-master-key"},
  "oidc": {"issuer": "", "client_id": "", "client_secret_file": ""},
  "public_urls": {},
  "subscription_connectors": {"enabled": false}
}
```

Redis is optional coordination for cluster mode. When enabled, set `coordination.redis.url_file` to a mounted URL file. OIDC issuer and client ID must be supplied together; keep the client secret in a separate mounted file. Provider OAuth registrations, when needed, are configured under the `oauth` object and also reference secret files rather than embedding secret values.

Validate a configuration without opening storage:

```sh
./hoorific config validate --config /path/to/config.json
```

The ordinary startup sequence is migration followed by serving:

```sh
./hoorific migrate --config /path/to/config.json
./hoorific serve --config /path/to/config.json
```
`schema_version` in the configuration remains `1`; the durable database has
its own version and is currently at version `3`. Run `migrate` before
`serve` for an existing database as well as a new one. The version-3
migration adds the encrypted idempotency-record table and its expiry index;
it is additive and does not replace the database or the operator keyring.

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

Keep old key IDs available while encrypted idempotency records can still be
replayed; rotate the JSON keyring through the same retained-key procedure as
other encrypted records and never overwrite the key file in place. Pending,
partial, failed, oversized, or otherwise unreplayable records return a
conflict (normally `409` with `idempotency_in_progress`) and are durable
negative state, not permission to dispatch again after expiry or restart.
Fingerprint conflicts are also `409`. Hoorific never blindly forwards
`Idempotency-Key` upstream to obtain provider-side HTTP retry behavior.

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

## Build the image

Use Docker BuildKit, Podman, or another OCI-compatible builder with support for `RUN --mount=type=cache`:

```sh
podman build --tag hoorific:local .
# or: docker build --tag hoorific:local .
```

The Dockerfile pins Go `1.27` and Bun `1.3.14`. Its build graph copies `go.mod` and `go.sum` before backend sources and uses shared Go module/build caches. The schema stage copies only `tools/schema`, `internal/admin`, and `internal/core`; changes elsewhere do not invalidate that stage. The frontend stage installs from `web/package.json` and `web/bun.lock`, creates `web/src/generated`, generates API types from the fresh schema, and builds the console. The Go stage embeds those compiled assets; generated API types and host-built console output are not taken from the checkout.

To isolate builder caches, set a distinct namespace:

```sh
podman build \
  --build-arg HOORIFIC_CACHE_NAMESPACE=hoorific-local \
  --tag hoorific:local .
```

Cache mounts use locked sharing within a namespace. Separate namespaces avoid contention at the cost of reuse; they do not reduce total builder-side storage. The runtime-files stage copies certificates and timezone data from the pinned Go image instead of running APT in the runtime stage. For debugging, use external tooling or a separate diagnostic container: `exec ... sh` is intentionally unavailable.

The equivalent fresh-checkout source sequence is:

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
curl --fail http://127.0.0.1:8081/health/ready
```

Use `docker compose` with the image built by the same Docker engine when using Docker. The Compose file mounts the explicit configuration at `/etc/hoorific/config.json`, the explicit key at `/run/secrets/hoorific-master-key`, and a named `data` volume at `/var/lib/hoorific`. The `migrate` service must complete before `gateway` starts. To inspect or repeat migration:

```sh
podman compose run --rm migrate
podman compose logs --no-log-prefix migrate
podman compose up -d gateway
```

The inference listener is published on `HOORIFIC_INFERENCE_PORT` (default
`8080`). Management is bound to loopback on `HOORIFIC_MANAGEMENT_PORT`
(default `8081`); override the inference bind address with
`HOORIFIC_BIND_ADDRESS`. Do not expose management publicly without an
intentional TLS, trusted-origin, authentication, and network-control design.

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

For a local authenticated console, use the native loopback flow in the
README. For a container deployment, configure OIDC or provide an intentional
same-namespace/TLS path that preserves the loopback and trusted-origin
contract. Do not weaken the bootstrap guard merely to make a published port
work.

## Kubernetes / Helm

The chart is a cluster-mode template around external PostgreSQL. It expects externally created Secrets and does not generate or own the PostgreSQL DSN, Redis URL, OIDC client secret, or master key. Provision the referenced Secret names and keys first, then supply cluster configuration through values. The migration Job runs as a Helm pre-install/pre-upgrade hook; the serve Deployment is separate. The chart also creates separate inference and management Services, probes, a PodDisruptionBudget, anti-affinity, and configurable NetworkPolicies.

The checked-in `values.yaml` image (`ghcr.io/example/hoorific:0.1.0`) is deliberately a placeholder, not a published image. Build and publish an image you control, then override the repository and pin a tag or digest:

```sh
helm upgrade --install hoorific ./charts/hoorific \
  --namespace hoorific --create-namespace \
  --set image.repository=registry.example/your-team/hoorific \
  --set image.tag=operator-chosen-tag \
  --values deploy/hoorific-values.yaml
helm status hoorific --namespace hoorific
kubectl get jobs --namespace hoorific
```

For a digest-pinned deployment, set `image.digest=sha256:...`; the chart renders `repository@digest` when a digest is present. `deploy/hoorific-values.yaml` is an operator-provided file, not a repository path supplied by this source tree.

The default chart uses two stateless replicas, external PostgreSQL, and ephemeral `emptyDir` mounts for `/var/lib/hoorific` and `/tmp`. It is not a standalone SQLite deployment and its data mount is not a persistence claim. Redis and OIDC remain optional, but their URL/client-secret files and configuration must be supplied together when enabled. The read-only root filesystem needs writable `/tmp` and data mounts. No Kubernetes installation or production qualification is claimed by the local deterministic verifier.

## Backups

### Standalone SQLite

Stop the writer before copying the database. Preserve the master key with the same backup identifier and a restrictive umask:

```sh
umask 077
podman compose stop gateway
install -d -m 0700 backups/2026-01-01
# Replace the volume path and helper image with the tooling approved by your
# backup environment; do not copy a live SQLite file while it is being written.
podman run --rm -v hoorific_data:/data \
  -v "$PWD/backups/2026-01-01:/backup" \
  busybox cp /data/hoorific.sqlite /backup/hoorific.sqlite
install -m 0400 "$HOORIFIC_MASTER_KEY" backups/2026-01-01/master.key
sha256sum backups/2026-01-01/hoorific.sqlite backups/2026-01-01/master.key \
  > backups/2026-01-01/SHA256SUMS
```

If filesystem snapshots are used instead, take an application-consistent snapshot and retain the key in the same protected backup set.

### Cluster PostgreSQL

Do not place a DSN or password in a command-line argument. Configure libpq through a protected service file and password file, then pass only the service name to PostgreSQL tooling:

```sh
umask 077
install -d -m 0700 backups/2026-01-01
export PGSERVICEFILE=/path/to/protected/pg_service.conf
export PGPASSFILE=/path/to/protected/pgpass
export PGSERVICE=hoorific-backup   # operator-provided service name
pg_dump --format=custom \
  --file=backups/2026-01-01/hoorific.dump \
  --dbname="service=${PGSERVICE:?Set PGSERVICE to the protected service name}"
install -m 0400 "$HOORIFIC_MASTER_KEY" backups/2026-01-01/master.key
sha256sum backups/2026-01-01/hoorific.dump backups/2026-01-01/master.key \
  > backups/2026-01-01/SHA256SUMS
```

Back up Redis separately if it is configured. Redis is coordination/cache state and does not replace the database or master-key backup.

## Restore and master-key recovery

Verify checksums, provision an empty target database or volume, and restore the database before starting the server:

```sh
umask 077
sha256sum --check backups/2026-01-01/SHA256SUMS
# Standalone: restore hoorific.sqlite into the mounted data volume while stopped.
# Cluster: configure the same protected PGSERVICEFILE/PGPASSFILE/PGSERVICE
# values used for the target, then restore without putting a DSN on argv.
export PGSERVICEFILE=/path/to/protected/pg_service.conf
export PGPASSFILE=/path/to/protected/pgpass
export PGSERVICE=hoorific-restore
pg_restore --clean --if-exists \
  --dbname="service=${PGSERVICE:?Set PGSERVICE to the protected service name}" \
  backups/2026-01-01/hoorific.dump
install -m 0400 backups/2026-01-01/master.key .local/hoorific-container/master.key
export HOORIFIC_MASTER_KEY="$PWD/.local/hoorific-container/master.key"
podman compose run --rm migrate
podman compose up -d gateway
```

For Kubernetes, restore the database, recreate the externally managed master-key Secret from the exact backed-up bytes, then run the Helm upgrade so the migration hook executes before the Deployment rolls forward. Never create a new key to replace a lost key, and never rotate the key by overwriting this file; use a separately designed key-rotation procedure that can decrypt existing data.

## Limits of these instructions

These instructions describe configuration and operator procedures. They do not execute image builds, Compose startup, Helm rendering, Kubernetes admission, migration, backup, or restore. Health behavior, storage performance, external database/Redis availability, and compatibility with a particular cluster or registry must be qualified in the target environment.
