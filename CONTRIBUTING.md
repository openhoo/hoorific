# Contributing to Hoorific

Thanks for taking the time to improve Hoorific. Keep pull requests focused,
explain the user-visible or operational reason for the change, and include the
checks you actually ran. Do not put credentials, production data, databases,
private configuration, browser storage state, or provider responses in a pull
request or its artifacts.

Hoorific does not publish a release or support-policy promise yet. Treat the
current source and qualification commands as the repository contract, not as a
claim of provider entitlement, production availability, or Kubernetes
qualification.

## Prerequisites

- Go 1.27.0, as declared by `go.mod`.
- Bun 1.3.14, matching the frontend image and CI.
- Python 3.13 for the SDK/browser qualification.
- Rust 1.95.0 with the `x86_64-unknown-linux-musl` target and a musl C
  toolchain for the native Codex wire helper.
- Podman (preferred) or a Docker-compatible builder for the image. The full
  deterministic `all` scenario also requires local Podman and a cached
  `docker.io/library/postgres:17` image for its isolated FAL child. Cluster
  qualification additionally requires cached PostgreSQL and Redis images.

The verifier owns its temporary data, keys, loopback fixtures, and evidence
paths. It does not use an operator database or ambient provider credentials.

## Fresh-checkout build order

Generated API types and console assets are source-only build outputs. Start from
an empty generated directory and run the schema stage before the frontend; run
both before Go checks or a runtime build from a fresh checkout:

```sh
mkdir -p .artifacts
go run -trimpath ./tools/schema --output .artifacts/admin-openapi.json
mkdir -p web/src/generated
(
  cd web
  bun install --frozen-lockfile
  bun run generate-api
  bun run typecheck
  bun run build
)

rustup toolchain install 1.95.0 --profile minimal \
  --target x86_64-unknown-linux-musl
cargo +1.95.0 build --locked --release \
  --manifest-path native/codex-wire/Cargo.toml \
  --target x86_64-unknown-linux-musl \
  --target-dir .artifacts/codex-wire-target
install -m 0755 \
  .artifacts/codex-wire-target/x86_64-unknown-linux-musl/release/hoorific-codex-wire \
  .artifacts/hoorific-codex-wire
export HOORIFIC_TEST_CODEX_WIRE="$PWD/.artifacts/hoorific-codex-wire"

CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o .artifacts/hoorific ./cmd/hoorific
CGO_ENABLED=0 go build -trimpath -tags qualification -ldflags='-s -w' \
  -o .artifacts/hoorific-qualification ./cmd/hoorific
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' \
  -o .artifacts/hoorific-verify ./tools/verify

go vet ./...
go test -race ./...
```

The qualification-tagged gateway is test-only and must never be deployed.
Neither `internal/console/assets/` nor `web/src/generated/` should be
committed; both are rebuilt by the commands above and ignored by Git.

## Deterministic qualification

The complete standalone `all` scenario includes the deep management/protocol
checks, credential lifecycle, SDK checks, operation fixtures (including the
isolated FAL namespace), and packaging checks. Install the SDK requirements
before running it; the FAL child uses local Podman and never pulls its image.
Browser requirements and Chromium are needed only for the separate browser
scenario.

```sh
python3 -m venv .artifacts/sdk-venv
.artifacts/sdk-venv/bin/python -m pip install \
  --requirement tools/verify/sdk/requirements.txt

podman pull docker.io/library/postgres:17

HOORIFIC_VERIFY_FAL_IMAGE=docker.io/library/postgres:17 \
HOORIFIC_VERIFY_PYTHON=.artifacts/sdk-venv/bin/python \
  .artifacts/hoorific-verify \
  --binary .artifacts/hoorific \
  --qualification-binary .artifacts/hoorific-qualification \
  --mode standalone \
  --scenario all \
  --output .artifacts/verify-standalone.json
```

`all` does not include the automated Chromium suite; run the browser section
separately when browser coverage is required. `passed`, `failed`, and `not-run`
results are intentional evidence states. Do not turn a focused selector into a
claim of complete coverage.

To exercise only the cost/accounting and provider-control contracts against
owned temporary fixtures:

```sh
go run ./tools/verify \
  --binary .artifacts/hoorific \
  --mode standalone \
  --scenario cost-safety \
  --output .artifacts/verify-cost-safety.json
```

The selector is deterministic and does not contact a paid provider. Record
the exact selector and result state you actually ran; it is not a claim of
provider entitlement, pricing, or cache savings.

The cluster selector is separate and requires both selected images locally:

```sh
podman pull docker.io/library/postgres:17
podman pull docker.io/library/redis:7.4-alpine

HOORIFIC_POSTGRES_IMAGE=docker.io/library/postgres:17 \
HOORIFIC_REDIS_IMAGE=docker.io/library/redis:7.4-alpine \
.artifacts/hoorific-verify \
  --binary .artifacts/hoorific \
  --mode cluster \
  --scenario cluster/governance \
  --output .artifacts/verify-cluster-governance.json
```

This starts fresh, invocation-owned PostgreSQL and Redis containers, checks
both services, and removes only those containers. It does not qualify an
external Kubernetes cluster or a paid provider.

## Browser qualification

Install the browser-only dependency into the same project-local environment
used by the SDK runner, then install the pinned Chromium browser. The
environment variable prevents this pinned Playwright client from garbage
collecting browsers belonging to other clients:

```sh
.artifacts/sdk-venv/bin/python -m pip install \
  --requirement tools/verify/sdk/requirements-browser.txt
PLAYWRIGHT_SKIP_BROWSER_GC=1 \
  .artifacts/sdk-venv/bin/python -m playwright install --with-deps chromium
```

After that setup, run the actual authenticated console scenario:

```sh
HOORIFIC_VERIFY_PYTHON=.artifacts/sdk-venv/bin/python \
  .artifacts/hoorific-verify \
    --binary .artifacts/hoorific \
    --mode standalone \
    --scenario browser \
    --output .artifacts/verify-browser-e2e.json
```

The browser scenario talks to the verifier's fresh local gateway and deterministic
fixture. It does not use an operator browser session or contact a paid provider.
It is separate from `all`; keep its report and sidecar evidence distinct.
Keep screenshots, storage state, temporary keys, databases, and diagnostics
private; only deliberately redacted JSON/log evidence belongs in shared CI
artifacts.

## Maintaining console screenshots

Use the real console against a fresh, isolated verifier fixture/runtime; do not
mock the page or use production data. After the fresh-checkout build and browser
dependency setup, regenerate the public images with the reproducible command:

```sh
.artifacts/sdk-venv/bin/python tools/verify/sdk/capture_docs.py \
  --binary .artifacts/hoorific \
  --verifier .artifacts/hoorific-verify \
  --output-dir docs/assets
```

The capture command owns a fresh real-server deterministic fixture and cleanup,
uses only synthetic data, and exits nonzero on an incomplete capture. It writes
exactly these six public WebP assets:

- `docs/assets/console.webp`
- `docs/assets/console-login.webp`
- `docs/assets/console-models.webp`
- `docs/assets/console-model-editor.webp`
- `docs/assets/console-playground.webp`
- `docs/assets/console-mobile.webp`

The automated `--scenario browser` run is separate: its screenshots, browser
storage state, temporary keys, databases, and diagnostics remain private under
`.artifacts/` and must not be copied into these paths. Read each generated file
back from its committed path before staging; a browser preview alone is not
proof that an asset was saved. Never commit `.artifacts/`, credentials,
unredacted provider responses, or other private evidence.

On pull requests and pushes to `main`, CI runs this capture after the browser
qualification and uploads an artifact named
`hoorific-console-screenshots-<commit-sha>` containing only these six files.
The verification job has `contents: read`; the separate publication job has
`contents: write` only and runs only after a successful trusted `main` push.
It checks out the originating SHA with checkout credentials disabled, validates
regular non-executable files and WebP signatures in a staging directory, and
copies only changed assets. A normal non-force `GITHUB_TOKEN` push by
`github-actions[bot]` avoids CI recursion. Pull requests cannot publish, and
unchanged or stale source revisions do not mutate `main`.


## Container image

Build locally with a Docker-compatible OCI builder after the source build above:

```sh
podman build --tag hoorific:local .
# Docker is equivalent when Podman is unavailable:
# docker build --tag hoorific:local .
```

Pull requests build and inspect the image without publishing it. After successful
qualification on trusted pushes to `main`, CI refreshes console screenshots and
publishes the Linux/amd64 image to GHCR. Hooversion manages release commits, tags,
and GitHub Releases from Conventional Commits; do not create competing manual
release tags. See `.github/workflows/ci.yml` for publication gates and permissions.

## Pull requests

- Keep one coherent change per pull request and describe the risk or migration
  impact.
- Review generated diffs and remove local artifacts before committing.
- Report the exact commands and meaningful result in the pull request; do not
  claim checks that were not run.
- Never include secrets in source, fixtures, logs, screenshots, or CI artifacts.
- Changes to security behavior should include a focused deterministic
  qualification case where a plausible regression would fail.
