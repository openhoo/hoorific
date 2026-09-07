# Gateway performance benchmark

The benchmark is an external traffic runner, not a server launcher or a synthetic simulator. It sends requests to the supplied gateway URL and to a deterministic loopback upstream. It never claims a comparison unless both targets complete the same measured cases.

Reports and paths beginning with `.artifacts/` are private operator-local
outputs. They are not hosted proof and are not shipped with the repository.

## Prerequisites

Build the gateway separately, then run:

```sh
go run ./tools/bench \
  --binary .artifacts/hoorific \
  --output .artifacts/bench \
  --url-file /run/user/$UID/hoorific-bench-url \
  --key-file /run/user/$UID/hoorific-bench-key \
  --deployment standalone \
  --route native \
  --gateway-pid 12345
```

The URL file contains the complete HTTP(S) chat-completions endpoint, for example `http://127.0.0.1:8080/v1/chat/completions`. The key file contains only the API key and must not be group/world-readable. `BENCH_GATEWAY_URL` and `BENCH_GATEWAY_KEY` are equivalent environment inputs. Credentials are never written to artifacts or printed. Redirects are rejected so an authorization header cannot be forwarded unexpectedly.

The binary path must exist; the runner does not build, start, migrate, or reconfigure it. `--gateway-pid` is optional. Without it, gateway RSS is explicitly unavailable. If supplied, it must be the process whose `/proc/PID/status` is sampled; a supervisor PID is not a gateway measurement.

The runner starts a fixture on `127.0.0.1:18089` by default. Configure the gateway's model/connection route before starting the runner so the requested model alias reaches that fixture. The fixture route must preserve the benchmark request and return the fixture's deterministic response. The runner performs unary and SSE identity preflight against both direct fixture and gateway URL; a missing or incorrect route is a prerequisite failure, not a successful benchmark. Use `--fixture-listen` only for another loopback address; non-loopback listeners are rejected.

The gateway's runtime entry points remain explicit: `serve --config PATH` starts the gateway and `migrate --config PATH` applies schema migrations. Use the same JSON configuration for the selected deployment, with `schema_version: 1`, explicit `data_dir`, `listeners`, encryption key file, and either SQLite only (`mode: standalone`) or PostgreSQL DSN file only (`mode: cluster`). Do not put a PostgreSQL DSN in a standalone config or SQLite path in a cluster config. Run migration before serving when the deployment requires it, and keep the API key outside the config.

## Deployment and protocol prerequisites

`--deployment standalone|cluster` and `--route native|translated` are required annotations. They are recorded as environment metadata; the runner does not infer topology or protocol translation. Standalone and cluster runs must be collected separately. A native route means the gateway's native request/stream codec is exercised. A translated route means the configured adapter is deliberately exercised. Results from these modes must not be merged or presented as a universal protocol comparison.

## Fixed workload matrix

Every case has three independent measured runs and a separate warmup. Warmup observations are retained but never included in measured percentiles. The runner uses bounded request deadlines and a whole-session deadline; interruption or timeout leaves an incomplete artifact.

* Unary: 1 KiB and 64 KiB input, 1 KiB output, offered 1000 requests/s, concurrency 64.
* SSE: 1 KiB and 64 KiB input, 128 non-empty events, 1 ms event spacing, offered 1000 requests/s, concurrency 256.
* Closed-loop SSE: 1 KiB input, 128 events, concurrency 1 and 64. A new request is admitted only after the previous stream in that slot completes.
* Slow-reader SSE: 1000 simultaneous streams using 1 KiB input, with the response consumed deliberately slowly. This case is intentionally bounded and is not replaced by a smaller load when resources are insufficient.

Input contains a per-session challenge nonce. Successful output must retain that nonce and the exact fixture shape. An incomplete stream, malformed SSE, HTTP rejection, timeout, or content mismatch is an error. Offered arrivals rejected because all configured concurrency slots are occupied are counted as dropped arrivals; the runner never lowers the rate to make a case pass.

## Metrics and artifacts

`results.json` is written incrementally and atomically with mode 0600. `summary.txt` is a human-readable index. Each measured record identifies target, workload, run, offered and achieved rates, attempted/completed/error/dropped counts, total latency p50/p95/p99, streaming TTFT p50/p95/p99, warmup counts, and RSS samples where available. Percentiles are computed from real completed requests. Queueing/dispatch lateness and HTTP rejection are retained as separate counts when available.

Gateway RSS is sampled from `/proc/<gateway-pid>/status` and is unavailable when no PID is supplied or the process exits. Allocation metrics are **unavailable**: this runner does not substitute its own Go runtime allocation counters for gateway allocations. Environment metadata includes OS/architecture, Go version, CPU count, hostname when available, binary sizes, deployment/route annotations, and timestamp. Secrets and the key-bearing URL are excluded.

A result state of `complete_with_failures` is still an honest record of the configured workload, not a pass. `unmet_prerequisite_or_incomplete` means setup, preflight, interruption, or deadline prevented a complete matrix. No native/translated, standalone/cluster, or throughput superiority claim may be made from incomplete or unequal rows.

## Container build time and footprint

`tools/bench/image_build.py` compares a captured baseline Dockerfile and ignore
file with the current candidate. It builds separate temporary source contexts,
does not edit the checkout, and never prunes shared builder state. Both variants
receive identical source mutations. The result images have unique tags and are
retained for inspection unless `--cleanup-images` is explicitly supplied.

```sh
python tools/bench/image_build.py --engine podman \
  --baseline-dockerfile .artifacts/image-review-baseline/Dockerfile \
  --baseline-dockerignore .artifacts/image-review-baseline/.dockerignore \
  --output .artifacts/image-build-comparison.json --timeout 600
```

Capture baseline files before editing them; their hashes are recorded in the
report. The runner also accepts `--engine docker`. The serial matrix covers a
cold build, an unchanged warm build, a backend-source comment, a frontend CSS
asset change, and a verification-tool-only comment. The frontend probe adds an
unused custom property so that it changes the generated asset rather than being
discarded as a comment.

“Cold” means image-layer reuse is disabled and the candidate receives a fresh
`HOORIFIC_CACHE_NAMESPACE`. Base images must already be local; registry/proxy and
OS caches are not cleared. Later candidate builds reuse that same namespace.
The original baseline has no cache mounts. Build duration excludes subsequent
image inspection and export, and builds run serially to avoid contention from
the comparison itself. Do not run other builds or load tests during measurement.

Timed-out builds and interrupted measurements terminate only the owned process
group, with at most ten seconds of bounded cleanup. The report stays incomplete and
retains earlier command evidence; cancellation is never reported as a successful
build. Optional image cleanup removes only the run's tags without forcing the
removal of containers or images still in use.

Image `Size`/`VirtualSize` are reported as engine-provided values, not inferred
from human-readable history. Podman OCI exports additionally record layer blob
bytes and media types; a compressed-layer total is labelled as such only when
compression is verified. Docker's archive size is a different metric and is not
presented as OCI compressed bytes. These are image-size measurements, not
application RSS or throughput benchmarks.

Runtime qualification must accompany a footprint change: migration, readiness,
authenticated console operation, and persisted state after restart, under the
existing non-root/read-only contract. Certificate trust, timezone loading,
identity lookup, and `/tmp` writability must remain available even when the
runtime has no shell.

### Historical local reference measurement

The table below records one Linux/amd64 comparison made with Podman 6.1.0 and
the same pinned Go/Bun base images. Medians are from two serial runs on one
machine. They are workload-specific observations, not CI timing guarantees,
capacity commitments, or a promise about another builder.

| Build case | Previous Dockerfile | Optimized Dockerfile |
| --- | ---: | ---: |
| Cold, base images already present | 54.29 s | 46.91 s |
| Unchanged warm build | 10.92 s | 10.03 s |
| Backend-only source change | 32.59 s | 10.10 s |
| Frontend CSS asset change | 42.38 s | 13.37 s |
| Verification-tool-only change | 33.89 s | 7.23 s |

That local comparison measured 125,752,509 bytes for the previous image and
40,746,582 bytes for the optimized image (67.6% smaller). Verified
gzip-compressed OCI layers measured 47,299,273 versus 13,946,682 bytes (70.5%
smaller). The delivered Go binaries were byte-identical; the difference came
from the runtime filesystem and build graph, not from removing application
features or compressing the executable.

Reproduce the comparison with the private output path below after capturing the
baseline Dockerfile and ignore file. The command creates temporary source
contexts, records hashes, and does not edit the checkout:

```sh
python tools/bench/image_build.py \
  --engine podman \
  --baseline-dockerfile .artifacts/image-review-baseline/Dockerfile \
  --baseline-dockerignore .artifacts/image-review-baseline/.dockerignore \
  --output .artifacts/image-build-comparison.json \
  --timeout 600
```

The baseline files and the output directory in that example are operator
inputs; do not treat them as repository-provided evidence. The comparison
should be run serially with no other builds or load tests. Runtime
qualification must accompany any footprint change: migration, readiness,
authenticated console operation, and persisted state after restart under the
existing non-root/read-only contract. Certificate trust, timezone loading,
identity lookup, and `/tmp` writability must remain available even when the
runtime has no shell.
