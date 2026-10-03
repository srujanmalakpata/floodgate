# floodgate verification

- **Measurement date:** 2026-10-03. Results below describe this measurement environment.
- **Environment:** shared 4-vCPU Linux container (x86_64, Intel Xeon @ 2.80 GHz), Ubuntu 24.04, shared with
  other concurrent jobs (load average 9–20 during measurement; disk nearly full).
  Go 1.24.7 (and Go 1.26.8 where stated). Docker 29.6.2.
  golangci-lint 2.5.0, staticcheck, govulncheck 1.8.0, Terraform 1.16.5, kubectl 1.34.1 (kustomize 5.7.1), kubeconform,
  Helm 3.16.2, actionlint 1.7.7, Trivy 0.75.0 (container image pinned by digest), hey.
  Redis: `redis:7-alpine` (Redis 7.4.11) in Docker and a native `redis-server` 7.0.15, both on localhost.
- **Test conditions:** clean build and test caches (`go clean -cache -testcache`), no existing `bin/`, coverage files
  or `deploy/terraform/aws/.terraform/`, and an image build with `--no-cache`.
- **Deployment status:** infrastructure is validated, never deployed to AWS or a Kubernetes cluster. No credentials are used.

## Maintenance verification (2026-10-03)

Environment: macOS arm64, Go 1.27.1, Docker CLI 29.8.1, Compose 5.6.0,
Buildx 0.37.2, cached actionlint 1.7.7. No deployment or GitHub workflow was triggered.
Go module/build caches and generated binaries were isolated under `/private/tmp/floodgate-rt-cache`.
The sandbox denies Docker socket access, TCP listeners and module-proxy DNS resolution.
**The complete updated build is not verified green in this environment.**

Changes under verification: upstream now has an HTTP healthcheck using the existing gateway
`-probe` mode, and both replicas wait for upstream health. The five requested action major
updates and both requested module updates are applied. The minimum Go directive, Docker build
and minimum CI test version now use 1.26.8, replacing the unsupported 1.24 minimum.
CI runs govulncheck on both the minimum and stable toolchains. golangci-lint is updated to
2.14.0 for modern Go support; the action remains responsible for installing/running it.
The shutdown test now waits for upstream entry and proxy listener closure instead of guessing
request arrival with a 50 ms sleep. Assertions and test selection in CI remain intact.

| Command / check | Result | Evidence / limitation |
|---|---|---|
| `go vet ./...` | BLOCKED | Exit 1: uncached updated modules cannot be downloaded; `lookup proxy.golang.org: no such host`. |
| `go test -race -count=1 ./...` | BLOCKED | Exit 1: updated Prometheus, YAML and x/sys sources unavailable. The four dependency-independent test packages pass; this is not a full-suite pass. |
| `go mod tidy -diff` | BLOCKED | Exit 1 for the same module-proxy DNS restriction; combined updates have not been verified tidy. |
| `docker compose up --build --detach --wait --wait-timeout 180 && docker compose down --volumes` | BLOCKED | Exit 1: `permission denied` connecting to the Colima Docker socket. No containers started; the conditional teardown was not reached. |
| `docker compose config -q` | PASS | Exit 0, Compose configuration parses. |
| Assertions on `docker compose config --format json` | PASS | Upstream uses `CMD /usr/local/bin/gateway -probe http://127.0.0.1:9000/`; both gateway dependencies resolve to `service_healthy`. Static validation only. |
| `actionlint .github/workflows/ci.yml` (cached v1.7.7 executable) | PASS | Exit 0, no diagnostics after the action and Go-version changes. |
| New action metadata review | PASS | Read the tagged `action.yml` files for checkout v7, setup-go v7, lint-action v9, setup-buildx v4 and setup-terraform v4. Existing inputs are supported; the actions use Node 24 on hosted runners. This does not execute the actions. |
| `go test -race -count=1 ./internal/breaker ./internal/degrade ./internal/limiter ./internal/limiter/memory` | PASS | All four packages pass with the final go.mod, Go 1.27.1 and race detection. |
| `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...` | BLOCKED | Exit 1 fetching scanner metadata from the module proxy; no current vulnerability result. Historical Go 1.26.8 success below is not a scan of the updated dependencies. |
| `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0 run ./...` | BLOCKED | Exit 1: tool download needs module-proxy DNS. |
| `go test -run '^$' -bench . -benchtime 200x ./...` | BLOCKED | Core and memory benchmarks execute, but the full command exits 1 for uncached updated dependencies. Smoke samples are not substituted for the historical measurements. |
| `gofmt -l .` | PASS | No files listed. |
| `bash -n scripts/demo-shared-limits.sh` | PASS | Exit 0. |
| README command count / local links / documentation content review | PASS | Six Quickstart commands; local link targets exist; content constraints satisfied. |
| `git diff --check` | PASS | No whitespace errors. |
| Tracked-output and ignore review (`git ls-files`, `git check-ignore`) | PASS | No tracked build/test/state output found; bin, build, coverage and CI demo/version outputs are ignored. Added `.editorconfig`; LICENSE unchanged. |
| Terraform fmt/init/validate; Kubernetes rendering/kubeconform; Helm lint/render | NOT_RUN | Terraform, kubectl, kubeconform and Helm executables are absent; historical validation retained below. |
| Trivy image / IaC scans and real-Redis conformance | BLOCKED | Docker daemon access is denied; no new image, scanner container or Redis service could run. |
| Compose demo / native HTTP Quickstart | BLOCKED | Docker startup is denied; native HTTP listeners also fail with `bind: operation not permitted`. Commands and configuration were reviewed, but the current runtime was not exercised. |
| GitHub Actions and tag release / cloud or cluster deployment | NOT_RUN | No push, release or deployment performed. |

### Cached-dependency diagnostics

To check the current source independently of uncached dependency downloads, the original
`go.mod` and `go.sum` were copied to temporary `cached.mod` / `cached.sum` files, with
`GOFLAGS='-modfile=/private/tmp/floodgate-rt-cache/cached.mod -mod=readonly'` and `GOPROXY=off`.
The repository's updated dependency files were not reverted or altered for these checks.
These results **do not validate the dependency upgrades**:

| Command with the temporary original dependency graph | Result | Evidence |
|---|---|---|
| `go vet ./...` | PASS | Exit 0; includes compilation/analysis of the changed shutdown test. |
| `go test -race -count=1 ./...` | BLOCKED | Exit 1: app/gateway httptest servers and miniredis cannot bind TCP sockets (`operation not permitted`). Config, breaker, degradation, limiter and memory tests pass. No tests were skipped or weakened to bypass the restriction. |
| `go build -o /private/tmp/floodgate-rt-cache/bin/ ./cmd/...` | PASS | Both binaries build; `gateway -version` prints `gateway dev`. |
| `gateway -check -config examples/config.yaml` and `examples/compose-config.yaml` | PASS | Both report `config ok` using the diagnostic binary. |

Module versions, required indirect updates and checksums were copied from the existing
[Prometheus update](https://github.com/srujanmalakpata/floodgate/pull/5) and
[YAML update](https://github.com/srujanmalakpata/floodgate/pull/3) patches via read-only access;
upstream module metadata was also inspected. No new direct runtime dependency was added.
Rerun all four acceptance commands, the complete CI checks and the new shutdown test where
network, local sockets and Docker are available before treating this change as fully verified.
The following Linux results remain historical evidence for their original dependency graph.

## Summary

| # | Command | Result | Key output |
|---|---|---|---|
| 1 | `gofmt -l .` | PASS | no files listed |
| 2 | `go vet ./...` | PASS | no findings |
| 3 | `golangci-lint run ./...` (config `.golangci.yml`) | PASS | `0 issues.` |
| 4 | `staticcheck ./...` | PASS | no findings |
| 5 | `RLGW_TEST_REDIS=127.0.0.1:16379 go test -race -count=1 -p 2 -json -coverprofile=… ./...` (Go 1.24.7, Redis 7.4.11 in Docker) | PASS | 48 top-level test functions; 152 test cases including subtests; 152 pass, 0 fail, 0 skip; 8/8 test packages ok |
| 6 | `go test -race -count=1 -p 2 -json ./...` (no real Redis) | PASS | 138 pass, 1 skip (`TestConformanceRealRedis`, needs `RLGW_TEST_REDIS`), 0 fail |
| 7 | `GOTOOLCHAIN=go1.26.8 RLGW_TEST_REDIS=127.0.0.1:16380 go test -race -count=1 -p 2 -json ./...` (native Redis 7.0.15) | PASS | 152 pass, 0 fail, 0 skip; 8/8 test packages ok |
| 8 | `go tool cover -func`; `go test -cover ./internal/...` with `RLGW_TEST_REDIS` | PASS | per tested package: app 89.8 %, breaker 95.2 %, config 94.6 %, degrade 100 %, gateway 97.2 %, limiter 92.3 %, memory 87.5 %, redisstore 93.8 %; total 72.4 % (includes `cmd/` and the helper packages `clock`, `metrics`, `limitertest`, which report 0 % in their own line although other packages' tests exercise them) |
| 9 | Conformance suite against real Redis (`TestConformanceRealRedis`) | PASS | 13/13 subtests on Redis 7.4.11 (Docker) and on Redis 7.0.15 (native) |
| 10 | Mutation checks (table below): 10 deliberate bugs tested with targeted tests | PASS (10/10 caught) | see Mutation checks; source restoration checked byte for byte (`filecmp` identical) |
| 11 | `go test -run '^$' -bench . -benchmem -count 5 ./internal/limiter/ ./internal/gateway/` and `-cpu 1,2,4 ./internal/limiter/memory/` | PASS | see Benchmarks |
| 12 | `RLGW_BENCH_REDIS=… go test -bench BenchmarkAllow -count 5 ./internal/limiter/redisstore/` (native and Docker Redis) | PASS | see Benchmarks |
| 13 | `go build -ldflags "-X main.version=v0.0.0-smoke" -o bin/ ./cmd/...`; `bin/gateway -version` | PASS | `gateway v0.0.0-smoke`; the startup log line carries `"version":"v0.0.0-smoke"` |
| 14 | Binary smoke test: `bin/upstream` + `bin/gateway -config <examples/config.yaml with ports 18080/19090/19000>` | PASS | `POST /login` ×7 → `200 200 200 200 200 429 429`; the 429 carried `Retry-After: 60`, `RateLimit-Limit: 5`, `RateLimit-Remaining: 0`, `RateLimit-Reset: 60`, `RateLimit-Policy: 5;w=60` |
| 15 | Same gateway: `curl --path-as-is -X POST` to `//login`, `/./login`, `/x/../login`, `/x/..%2Flogin` | PASS | all four `429` (they count against `/login`) |
| 16 | Same gateway: `POST /login-help` and `GET /api` (no trailing slash) | PASS | `/login-help` → 200 under the default route (`RateLimit-Policy: 600;w=60`), not the login limit; `/api` → `api` route; with an `X-API-Key` header the policy is `RateLimit-Policy: 300;w=60, 100;w=60` (IP cap plus per-key limit), without one it is `100;w=60` |
| 17 | Same gateway: 400 `GET /api/x`, each with a different `X-API-Key`, from one IP (`api` route: limit 100, `ip_limit: 300`, token bucket per minute) | PASS | `323 ×200, 77 ×429` in a 4.7 s loop (300 burst plus ~5 tokens/s refill); the next requests showed `RateLimit-Limit: 300`, `RateLimit-Remaining: 0` (the IP cap is what denied them) |
| 18 | Same gateway log at `log_level: info` | PASS | 331 `"msg":"request"` lines, 0 lines with `"status":429` |
| 19 | `kill -HUP $GW`, then `kill -TERM $GW` | PASS | `SIGHUP received, reloading config`, `config reloaded`; `draining delay=5s`, then `gateway stopped err=null`; the process exited |
| 20 | `gateway -check` on example, compose, k8s base, k8s local overlay, two Helm-rendered and two Terraform-rendered configs | PASS | `config ok` ×8. Terraform configs use `terraform console` (`local.gateway_config`) with a placeholder ElastiCache address, with the default `routes` and with `terraform.tfvars.example` |
| 21 | `gateway -check` on an invalid file (`methods` on the default route, `redis.timeout: -1s`, `failure_policy: maybe` with the memory backend, route name `a:ipcap`) | PASS | exit 1 with all four errors listed (`default: methods is not allowed…`, `backend.redis.timeout: must be > 0, got -1s`, `backend.failure_policy: want open, closed or local…`, `name must not contain ':'`) |
| 22 | `docker build --no-cache --build-arg VERSION=v0.0.0-verify -t floodgate:dev .` (default `GO_VERSION=1.26`, build toolchain `go1.26.8`) | PASS | 89 s; `docker image inspect .Size` = 9,627,409 bytes (9.63 MB compressed content size); `docker images` disk usage 39.3 MB unpacked; binaries: `gateway` 18,186,402 bytes, `upstream` 5,959,842 bytes; `User=nonroot:nonroot`; `docker run --rm floodgate:dev -version` → `gateway v0.0.0-verify` |
| 23 | `docker compose config -q`; `docker compose up -d --no-build --wait`; `./scripts/demo-shared-limits.sh 16` | PASS | both gateways `healthy`; `allowed=10 limited=6 (limit is 10 per 30s, shared by both replicas)` |
| 24 | Compose: one request with a fresh key | PASS | `RateLimit-Limit: 10`, `RateLimit-Remaining: 9`, `RateLimit-Policy: 30;w=30, 10;w=30` |
| 25 | Compose Redis outage: `docker compose stop redis`, then 14 requests to gateway-1 with a fresh key | PASS | `200`×10 then `429`×4 (local policy); `/readyz` → `ready`, `redis_breaker: open`; `rlgw_backend_errors_total 3`, `rlgw_breaker_state 2`, `rlgw_degraded_decisions_total{policy="local"} 28` (2 checks per request: `ip_limit` and per key) |
| 26 | `docker compose kill -s HUP gateway-1` | PASS | `SIGHUP received, reloading config`, `config reloaded` |
| 27 | Info-level 429 lines in both replicas' compose logs | PASS | 0 |
| 28 | Trivy image scan (`trivy image --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 floodgate:dev`) | PASS | debian 12.15: 0; `usr/local/bin/gateway`: 0; `usr/local/bin/upstream`: 0 |
| 29 | Trivy IaC scan (`trivy config --severity HIGH,CRITICAL --exit-code 1 deploy`) | PASS | 18 config files detected, 0 misconfigurations; 3 intentional exceptions annotated inline (`#trivy:ignore` with a reason) |
| 30 | `govulncheck ./...` (v1.8.0) with the Go 1.24.7 toolchain | FAIL (toolchain) | `Your code is affected by 28 vulnerabilities from the Go standard library`; Go 1.24 is out of security support |
| 31 | `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...` (the pinned CI command; v1.8.0 needs Go ≥ 1.26, toolchain go1.26.8) | PASS | `Your code is affected by 0 vulnerabilities`; 1 unreachable advisory in a required module |
| 32 | `kubectl kustomize deploy/k8s/base \| kubeconform -strict -kubernetes-version 1.31.0` | PASS | `6 resources … Valid: 6, Invalid: 0`; the rendered Deployment has no `replicas` field (the HPA owns it) |
| 33 | `kubectl kustomize deploy/k8s/overlays/local \| kubeconform -strict …` | PASS | `11 resources … Valid: 11, Invalid: 0` |
| 34 | `helm lint deploy/helm/floodgate` | PASS | `1 chart(s) linted, 0 chart(s) failed` |
| 35 | `helm template … \| kubeconform -strict` (defaults; and `autoscaling.enabled=false, redisPasswordSecret=s`) | PASS | `Valid: 6` and `Valid: 5` |
| 36 | `terraform fmt -check -recursive` (deploy/terraform/aws) | PASS | no diff |
| 37 | `terraform init -backend=false` + `terraform validate` | PASS | `Success! The configuration is valid.` (hashicorp/aws 6.67.0, random 3.9.1) |
| 38 | `actionlint .github/workflows/ci.yml`; PyYAML `safe_load_all` of ci.yml, dependabot.yml, docker-compose.yml, .golangci.yml, the example configs, the k8s manifests and the Helm values/Chart | PASS | actionlint: no findings; 19 YAML files parsed |
| 39 | Load test with `hey -z 10s -c 32`, 5 interleaved repetitions per target (localhost) | PASS | see Load test |
| 40 | `kind create cluster` (real local Kubernetes deploy) | NOT_RUN | no successful deployment; kind's `kubeadm init` fails in the local Docker environment. Manifests are validated only |
| 41 | `terraform apply` / AWS deploy | NOT_RUN | never applied; no cloud account or credentials |
| 42 | GitHub Actions run (including the scheduled scan and the tag-triggered release job) | NOT_RUN | no published-repository run; workflow validation uses actionlint only |

## Mutation checks

The table records deliberate mutations detected by the targeted tests (`go test -count=1`, with
`RLGW_TEST_REDIS` pointing at Redis 7.4.11). Original sources are restored and compared byte for byte after each check.

| Mutation | Detecting tests |
|---|---|
| M1 `sliding_window.lua`: `count < limit` → `count <= limit` | 5 conformance subtests on miniredis and 5 on real Redis, `TestReplicasShareOneLimit/sliding_window`, `TestTwoReplicasShareLimitsThroughRedis` |
| M2 Retry-After from the oldest entry (`ZRANGE key 0 0`) instead of rank `count-limit` | `…/sliding_window_after_a_hot_reload_lowers_the_limit` on miniredis and real Redis |
| M3 path cleaning and the `ip_limit` check disabled in `handler.go` | `TestNonCanonicalPathsCannotBypassRoute` (6 subtests), `TestRotatingAPIKeysCannotExceedIPLimit`, `TestHeadersReportTightestOfTwoChecks` |
| M3b only the `ip_limit` check disabled | `TestRotatingAPIKeysCannotExceedIPLimit`, `TestHeadersReportTightestOfTwoChecks` |
| M4 headers from the last check instead of the tightest | `TestHeadersReportTightestOfTwoChecks` |
| M5 `Log` appends instead of inserting in order | `TestConformance/sliding_window_with_a_clock_moving_backwards_keeps_exact_expiry` |
| M6 breaker issues a half-open probe while one is in flight | `TestConcurrentUseKeepsInvariants`, `TestStateMachine`, `TestStaleOutcomesAreIgnored` (3 subtests) |
| M7 `ContextTimeoutEnabled: false` in the Redis client | `TestRedisCallHonoursContextDeadline` (`Allow took 2.0s`, want bounded by the 50 ms deadline) |
| M8 raw string-prefix route matching | `TestMatchRoute` |
| M9 `:` allowed in route names | `TestValidationErrors/colon_in_name` |

## Benchmarks (median of 5)

`-N` is GOMAXPROCS. For `RunParallel` benchmarks, ns/op is wall-clock time per operation across all goroutines.
The host is heavily loaded, so the spread between runs is large; compare medians only when the ranges do not overlap.

| Benchmark | Runs | Median |
|---|---|---|
| `BenchmarkBucketTake-4` (pure math) | 47.42, 52.96, 51.18, 59.27, 51.86 ns | **51.9 ns**, 0 B, 0 allocs |
| `BenchmarkLogTake-4` (pure math, limit 100) | 124.9, 124.0, 157.8, 114.4, 110.8 ns | **124.0 ns**, 0 allocs |
| `BenchmarkHandlerMemory-4` (full HTTP handler, no network) | 3925, 4741, 3454, 3022, 3710 ns | **3,710 ns**, 912 B, 22 allocs |
| `BenchmarkAllowSingleKey` (memory store) -1 / -2 / -4 | -4 runs: 244.7, 212.0, 257.2, 231.6, 201.0 ns | **260 / 248 / 232 ns**, 1 alloc |
| `BenchmarkAllowParallel/token_bucket` (64 shards, random keys from 10k) -1 / -2 / -4 | -4 runs: 308.5, 366.2, 335.4, 378.5, 379.0 ns | **328 / 327 / 366 ns** |
| `BenchmarkAllowParallel/sliding_window` -1 / -2 / -4 | -4 runs: 836.6, 929.1, 689.1, 606.7, 1125 ns | **662 / 774 / 837 ns** |
| `BenchmarkAllowParallelOneShard` (1 shard baseline) -1 / -2 / -4 | -4 runs: 478.2, 461.9, 423.0, 464.1, 441.6 ns | **312 / 435 / 462 ns** |
| `BenchmarkAllow/token_bucket-4`, native Redis 7.0.15 | 40032, 38708, 54266, 37864, 38489 ns | **38.7 µs** |
| `BenchmarkAllow/sliding_window-4`, native Redis 7.0.15 | 44577, 43399, 46339, 64723, 45404 ns | **45.4 µs** |
| `BenchmarkAllow/token_bucket-4`, Redis 7.4.11 in Docker | 124287, 98165, 139490, 140215, 127664 ns | **127.7 µs** |
| `BenchmarkAllow/sliding_window-4`, Redis 7.4.11 in Docker | 154265, 142847, 127296, 89993, 124216 ns | **127.3 µs** |

- Sharding: at `-4`, 64 shards (309–379 ns) and 1 shard (423–478 ns) have non-overlapping ranges; at `-2` they overlap
  (322–494 vs 368–514). Additional samples on the same host at `-4` have overlapping ranges (284–386 vs 291–399).
  Inconsistent measurements on a loaded host do not establish a sharding speedup.
- Across samples on the same host, `BenchmarkLogTake` varies from 182 to 124 ns and the native-Redis token bucket
  from 102 to 39 µs (a 1.5–2.6x difference). The code paths do not explain this variation (the log insert is an append
  with a real clock); host load limits comparisons.
- `BenchmarkHandlerMemory` allocates 912 B / 22 allocs per request while retaining each check's result and building
  the `RateLimit-Policy` list. A sample without that bookkeeping measures 816 B / 20 allocs.
- The Redis figures are dominated by the round trip, not the script: Redis reported 13.62 µs of server time per
  `EVALSHA` call during the native benchmarks and 17.81 µs during the load test (`INFO commandstats`). The Docker
  port-forward adds roughly 80–90 µs per call over the native server.

## Load test

`hey -z 10s -c 32 -H 'X-API-Key: load' http://127.0.0.1:<port>/api/x`, interleaved repetitions
(direct, proxy, memory, Redis; repeated 5 times). The gateway, upstream and native Redis 7.0.15 run in the same
measurement container described above. Gateways use `log_level: info` (one access-log line per 200, stdout sent
to `/dev/null`) and, for the limited ones, a default route with a limit high enough that no requests are rejected. All 1,464,627 recorded responses are `200`.
Redis serves exactly as many `EVALSHA` calls (192,923) as the Redis-backed gateway answers requests, with 0 failed.

| Target | Repetitions (req/s) | Median | Range |
|---|---|---|---|
| upstream directly | 18,823; 15,485; 16,433; 21,126; 17,393 | 17,393 | 15,485–21,126 |
| gateway, no matching route (plain reverse proxy) | 4,323; 3,148; 3,936; 4,248; 3,429 | 3,936 | 3,148–4,323 |
| gateway + in-memory limiter | 3,520; 3,404; 3,655; 4,001; 4,149 | 3,655 | 3,404–4,149 |
| gateway + Redis limiter | 3,510; 3,668; 3,996; 4,209; 3,897 | 3,897 | 3,510–4,209 |

The three gateway ranges overlap and the Redis median is above the in-memory one, so the limiter's overhead is
**within run-to-run noise on this shared host**; no overhead percentage is claimed.

## Bugs found by testing and fixed

| Check | Result | Failure, fix and regression test |
|---|---|---|
| Redis deadline-test assertion | FAIL (fixed) | An assertion requiring a context error fails when the socket deadline fires just before the context timer. `degrade` correctly counts a Redis failure and falls back. The assertion now checks that the call is bounded by the deadline; `TestRedisCallHonoursContextDeadline` and mutation M7 check this bound. All Summary test results include this corrected assertion. |

## Test configuration

- Trivy uses `aquasec/trivy@sha256:af6acf9a…` (0.75.0), pinned by digest; CI uses the same pinned image.
- The binary smoke test uses a copy of `examples/config.yaml` with only `listen`, `admin_listen` and `upstream`
  changed, because another job on the host occupies port 8080. The measured ports are listed in Summary row 14.
- Build outputs and `.terraform/` are temporary; `.terraform.lock.hcl` records the validated provider versions.
