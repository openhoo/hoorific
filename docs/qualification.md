# Qualification and benchmark operations

The qualification tools run a built release process against owned fixtures. They are not unit tests, and they do not substitute for a provider account, paid entitlement, or a Kubernetes deployment. No command in this document imports host credentials or contacts a paid provider unless the operator explicitly supplies the live flags and connection IDs.

Paths beginning with `.artifacts/` are private operator-local output paths. Reports, diagnostics, browser storage state, screenshots, databases, and keys under those paths are not shipped with the repository and must not be published.

## Build prerequisites

Build the ordinary release binary using the ordering in [Deployment](deployment.md):
generate the schema, install locked Bun dependencies, generate API types,
typecheck and build the console, then compile the Go binary. For a fresh
checkout:

```sh
mkdir -p .artifacts web/src/generated
go run -trimpath ./tools/schema --output .artifacts/admin-openapi.json
(
  cd web
  bun install --frozen-lockfile
  bun run generate-api
  bun run typecheck
  bun run build
)
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' \
  -o .artifacts/hoorific ./cmd/hoorific
CGO_ENABLED=0 go build -trimpath -tags qualification -ldflags='-s -w' \
  -o .artifacts/hoorific-qualification ./cmd/hoorific
```

The verifier's SDK runner uses Python 3.13. Use one project-local environment
for every verifier invocation. Choose either the `uv` setup or the standard
library fallback; do not install into a different system interpreter:

```sh
# Recommended: uv
uv venv .artifacts/sdk-venv
uv pip install --python .artifacts/sdk-venv/bin/python \
  --requirement tools/verify/sdk/requirements.txt

# Fallback when uv is unavailable (use this instead of the block above)
python3 -m venv .artifacts/sdk-venv
.artifacts/sdk-venv/bin/python -m pip install \
  --requirement tools/verify/sdk/requirements.txt
```

The SDK requirements are needed by `all`; install
`tools/verify/sdk/requirements-browser.txt` and Chromium only for the
separate browser scenario. The browser procedure also needs a
Chromium-capable host.

The verifier requires a built binary and intentionally refuses an
operator-supplied `--config` path. It owns an isolated data directory,
master-key file, loopback listeners, and deterministic upstream fixture.

## Deterministic qualification

Run the isolated standalone qualification. The SDK runner uses the interpreter
named by `HOORIFIC_VERIFY_PYTHON`; point it at the project-local environment
created above so `all` does not fall back to an unrelated system Python:

```sh
podman pull docker.io/library/postgres:17

HOORIFIC_VERIFY_FAL_IMAGE=docker.io/library/postgres:17 \
HOORIFIC_VERIFY_PYTHON=.artifacts/sdk-venv/bin/python \
  go run ./tools/verify \
    --binary .artifacts/hoorific \
    --qualification-binary .artifacts/hoorific-qualification \
    --mode standalone \
    --scenario all \
    --output .artifacts/verify-standalone.json
```

The `all` selector includes the baseline protocol, stream, security, resource,
governance, credential, and unauthenticated-rejection checks; deep management
and protocol checks; cost safety; credential refresh admission; SDK checks;
operation fixtures (including the isolated FAL namespace); and packaging
checks. It does not include the automated browser scenario. Cluster-specific
checks are added only when `--mode cluster` is selected.

The runner executes `migrate --config PATH`, starts `serve --config PATH`, probes
management liveness/readiness, redeems the one-time `admin bootstrap --config
PATH` code through `/admin/api/v1/auth/bootstrap`, obtains a session CSRF token,
and seeds a tenant, connection, credential, model, alias, route policy, and
one-time API key through the real management API. It then exercises the owned
fixture and routed protocol contracts using that issued key.

`--gateway-key-file` (or `HOORIFIC_VERIFY_KEY`) overrides the generated key for
a pre-seeded configuration. The public `--config` flag is intentionally rejected
for qualification; the harness never copies, starts, migrates, or mutates an
operator-supplied database or service.

Fixture fault modes are selected with `--fixture-mode=truncate`, `disconnect`,
`429`, `slow`, `unicode`, `named-error`, or `tool` (the default is `normal`).
These modes exercise the owned loopback fixture only. Exit status is nonzero
for setup or observed behavior failures; `not-run` coverage is visible in the
JSON report and summary counters.

### Cost-safety selector

Run the focused cost/accounting and protocol-control qualification against the
owned loopback fixture:

```text
go run ./tools/verify \
  --binary .artifacts/hoorific \
  --mode standalone \
  --scenario cost-safety \
  --output .artifacts/verify-cost-safety.json
```

This selector exercises cache settlement and representable controls,
cross-protocol rejection, translated cache controls, idempotent replay and
pending/mismatch handling, streaming usage, `Retry-After`, Cohere
cancellation, Replicate creation headers, budget admission, and unknown-usage
reconciliation. It uses temporary provider fixtures and configured fixture
rates only; it does not contact a paid provider or claim provider pricing,
entitlement, or cache savings. `--scenario all` includes this selector.

### Deep regression and automated browser suite

`all` includes the deep management and protocol regression suites. They can
also be selected independently:

```text
go run ./tools/verify --binary .artifacts/hoorific \
  --scenario deep-admin --output .artifacts/verify-deep-admin.json
go run ./tools/verify --binary .artifacts/hoorific \
  --scenario deep-protocol --output .artifacts/verify-deep-protocol.json
```

For automated browser coverage, install the browser-only dependency into the
same project-local environment and install Chromium. If that environment was
created by `uv`, keep using `uv pip` with its explicit interpreter; if you used
the standard-library fallback, use that environment's Python instead. Set
`PLAYWRIGHT_SKIP_BROWSER_GC=1` when installing the pinned browser client so a
shared Playwright cache is not garbage-collected; a project-local
`PLAYWRIGHT_BROWSERS_PATH` is an alternative.

With the `uv`-created environment:

```sh
uv pip install --python .artifacts/sdk-venv/bin/python \
  --requirement tools/verify/sdk/requirements-browser.txt
```

With the standard-library venv fallback instead:

```sh
.artifacts/sdk-venv/bin/python -m pip install \
  --requirement tools/verify/sdk/requirements-browser.txt
```

Then install Chromium in that same environment:

```sh
PLAYWRIGHT_SKIP_BROWSER_GC=1 \
  .artifacts/sdk-venv/bin/python -m playwright install chromium
```

Run the automated suite with the same isolated-process ownership and automatic
cleanup:

```text
HOORIFIC_VERIFY_PYTHON=.artifacts/sdk-venv/bin/python \
go run ./tools/verify --binary .artifacts/hoorific \
  --mode standalone --scenario browser \
  --output .artifacts/verify-browser-e2e.json
```

The selected Python must have both requirement files; Playwright Chromium must
be available. Browser results are included in the verifier report. The Python
runner also writes `browser-results.json` and screenshots beneath
`.artifacts/verify-browser-e2e.json.browser` (override with
`HOORIFIC_VERIFY_EVIDENCE_DIR`). Missing dependencies and failed browser
assertions cause a nonzero exit. This automated browser scenario is separate
from `all` and starts from a fresh seeded fixture rather than inheriting
API-suite mutations.

### Cluster qualification

Cluster qualification owns a fresh PostgreSQL/Redis fixture and requires a
working local Podman runtime (Docker is a fallback) plus the selected images
already present locally; the runner never pulls images. With Podman, cache the
images and select the exact references explicitly:

```sh
podman pull docker.io/library/postgres:17
podman pull docker.io/library/redis:7.4-alpine

HOORIFIC_POSTGRES_IMAGE=docker.io/library/postgres:17 \
HOORIFIC_REDIS_IMAGE=docker.io/library/redis:7.4-alpine \
go run ./tools/verify --binary .artifacts/hoorific \
  --mode cluster --scenario cluster/governance \
  --output .artifacts/verify-cluster-governance.json
```

`--scenario governance --mode cluster` separately exercises ordinary restart
behavior against a cluster-backed process. `cluster/governance` exercises two
gateway processes against shared SQL/Redis. It never infers distributed
behavior from standalone or single-process results. The cluster harness expects
both selected images to be available locally.

### FAL namespace qualification

The FAL qualification uses the isolated namespace child path only:

```sh
podman pull docker.io/library/postgres:17
HOORIFIC_VERIFY_FAL_IMAGE=docker.io/library/postgres:17 \
  go run ./tools/verify --binary .artifacts/hoorific \
    --scenario operations/fal --output .artifacts/verify-fal.json
```

It requires local Podman and the cached image named by
`HOORIFIC_VERIFY_FAL_IMAGE` (default `docker.io/library/postgres:17`). Image
inspection is local and reports a missing cached image without pulling; the
container run uses `--pull=never`, has no network, and removes owned
containers after the run. The runner does not weaken HTTPS or target an
external FAL service.

## Live qualification

Live qualification is opt-in and requires explicit operator approval, a positive aggregate spend ceiling, selected connection IDs, and all authenticated files below:

```text
go run ./tools/verify --scenario live --allow-paid --connections openai,anthropic \
  --live-gateway-url https://gateway.example \
  --live-management-url https://gateway.example \
  --live-inference-key-file /run/user/$UID/hoorific/live-inference-key \
  --live-admin-cookie-file /run/user/$UID/hoorific/live-admin-cookie \
  --live-csrf-file /run/user/$UID/hoorific/live-csrf \
  --live-cases-file /run/user/$UID/hoorific/live-cases.json \
  --live-spend-ceiling-nanodollars 1000000000 \
  --output .artifacts/verify-live.json
```

The approval is for this exact gateway, connection-ID set, case file, and
positive aggregate ceiling. The `--allow-paid` switch is only an explicit
operator opt-in; it is not a provider entitlement or a pricing check. No
fixture, deterministic selector, or prior report authorizes a live run, and
the live procedure must not be described as successful unless that exact
operator-approved run was actually observed.

The example origins are placeholders. The URL inputs must be HTTPS origins, or literal-loopback HTTP origins. Secret files contain raw values, are regular non-symlink files, and have no group or other permissions. The cases file is strict version 1: each selected connection/advertised operation requires one explicit catalog `model_id`, exact production descriptor `action`, a timeout from 1 through 120 seconds, and wire request JSON. The runner reads authenticated management data and an existing hard-cost policy fence; it never creates or mutates that fence.

The runner does not discover credentials, read CLI token stores, log in, enable credits, or infer provider entitlement. Provider credentials must already be configured on the gateway. Unsupported stateful/resource, async, realtime, duplex, opaque native, path-model, continuation, response-storage, and unregistered-codec operations are local implementation gaps: a supplied case is reported `failed` before dispatch, not as provider coverage. Missing operator inputs are reported as external setup failures or `not-run`; no inference is dispatched until all admission and bound checks pass.

## Credential crash-boundary qualification

The ordinary release binary covers credential lifecycle checks. The process-crash proof additionally requires a separate test-only binary built with the `qualification` tag:

```text
go build -tags qualification \
  -o .artifacts/hoorific-qualification ./cmd/hoorific
go run ./tools/verify \
  --binary .artifacts/hoorific \
  --qualification-binary .artifacts/hoorific-qualification \
  --scenario credential-lifecycle \
  --output .artifacts/verify-credentials.json
```

The verifier starts the tagged child with a private `HOORIFIC_QUALIFICATION_REFRESH_COMMIT_GATE`, waits for its `.ready` marker after refresh validation and before `Store.CommitRefresh`, kills that child without creating `.release`, restarts the ordinary release binary, and checks the durable pending fence plus zero post-crash inference dispatch. Without `--qualification-binary`, only this one result is `not-run`; the other credential lifecycle checks still execute. The tagged binary and gate are qualification-only and must never be deployed.

## Browser console proof (manual keep-alive)

The automated `--scenario browser` procedure above runs the browser runner
itself and owns its process lifetime. `--keep-alive` is a different procedure:
it only keeps an isolated gateway and fixture alive and prints endpoints for a
manual browser run. A `keep-alive` result is not browser-console coverage.

Launch the isolated deterministic gateway and fixture for a bounded browser
window:

```sh
go run ./tools/verify \
  --binary .artifacts/hoorific \
  --mode standalone \
  --scenario protocol \
  --keep-alive 15m \
  --output .artifacts/verify-browser.json
```

Copy the `management=` origin and `storage_state=` path from the runner's
`browser-proof endpoints` stderr line while that command remains alive. In a
second terminal, run the actual authenticated console proof with the same
project-local Python/Playwright environment:

```sh
HOORIFIC_VERIFY_CONSOLE=http://127.0.0.1:<management-port> \
HOORIFIC_VERIFY_BROWSER_STORAGE_STATE=/tmp/hoorific-verify-<id>/browser-storage-state.json \
HOORIFIC_VERIFY_EVIDENCE_DIR=.artifacts/sdk-browser \
.artifacts/sdk-venv/bin/python tools/verify/sdk/browser_runner.py
```

The storage state is private, generated by the verifier after bootstrap, and
valid only for that isolated process. The browser runner requires the real
console route `/admin/playground`, authenticated state, Playwright Chromium,
and the deterministic gateway/fixture; it does not contact a paid provider.
Wait for `browser-results.json` and screenshots to be written, then interrupt
the first command. Do not combine this manual flow with the automated browser
scenario or treat the keep-alive report row as an assertion of UI behavior.

## Evidence interpretation

A `passed` result means the actual child process returned the asserted status, body, or stream terminal observed by the runner. It does not prove provider entitlement, account billing behavior, or Kubernetes runtime behavior. `failed` means an observed contract or setup failure and should be investigated before release. `not-run` is an explicit missing prerequisite or unsupported external qualification, not success.

Do not publish generated reports as if they were hosted proof. If a report records a diagnostics path, that path is private and contains only bounded logs and non-secret infrastructure metadata; it excludes keys, sessions, databases, DSNs, and browser storage. Remove diagnostics after review.

## Recovery and cleanup

The runner sends interrupt to the served child, waits up to 35 seconds, then kills only that child if it does not exit. Its owned temporary run directory, including configuration, keys, logs, and browser storage state, is removed after shutdown. When a run has failures, the report's `diagnostics` path points to a private directory (next to `--output` when supplied, otherwise a temporary directory) containing only bounded logs and non-secret infrastructure metadata. Copy any needed browser evidence before interrupting the runner. Do not point `--config` at a production data directory.

Back up the SQL database together with all currently retained encryption-key versions. Restoring a database without its key versions is not valid recovery and must fail readiness rather than regenerate credentials. For cluster qualification, destroy only the dedicated PostgreSQL/Redis fixture resources after both gateway processes have stopped.
