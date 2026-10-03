# floodgate design notes

## Goals

1. Run as a correct, low-overhead limiter in front of any HTTP service, as a standalone proxy or a sidecar.
2. Share one limit across many replicas (horizontal scaling must not multiply the limit).
3. Behave predictably when the shared state store fails.
4. Be operable: health and readiness, metrics, structured logs, config changes without restarts, clean shutdown.

Non-goals: authentication, request routing to multiple upstreams, a global multi-region limit.

## Key decisions

### One `Limiter` interface, pure algorithm math

`limiter.Limiter.Allow(ctx, key, rule) (Decision, error)` is the only contract the HTTP layer knows.
The algorithms (`Bucket.Take`, `Log.Take`) are pure functions of `(state, now, rule)`.
They do no I/O and never read the clock themselves. This gives three benefits:

- They are trivially unit-testable with a fake clock.
- The memory backend is just "lock a shard, call `Take`".
- The Redis Lua scripts are line-by-line ports of the same math, and a shared **conformance
  suite** (`limitertest.Run`) checks that both backends return the same decisions, including
  `Remaining`, `RetryAfter` and `ResetAfter`.

*Alternative considered:* a separate `Algorithm` interface with pluggable storage
(`Get/Set state`). It was rejected because Redis needs the read-modify-write to happen
*inside* Redis for atomicity, so storage and algorithm cannot be split cleanly there.

### Token bucket and sliding-window log

| | Token bucket | Sliding-window log |
|---|---|---|
| Semantics | burst up to `limit`, then `limit/window` per second | never more than `limit` in any trailing window |
| State per key | 2 numbers | up to `limit` timestamps |
| Good for | general API quotas (bursty clients are fine) | login/OTP endpoints where an exact cap matters |

A fixed-window counter was rejected because it allows up to `2 x limit` requests across a window boundary.
The test "sliding window has no burst at the boundary unlike a fixed window" pins this down. A
sliding-window *counter* (weighted average of two fixed windows) is the usual approximation when
O(limit) memory per key is too much. It is listed under "Potential improvements".

### Redis: one Lua script per decision

Each decision is a single `EVALSHA` of a script that reads state, computes, writes and sets
a TTL. Redis runs scripts atomically, so two replicas can never both read "1 token left" and
both admit a request. `redis.NewScript` sends `EVALSHA` and falls back to `EVAL` if the
script is not cached yet (for example after a Redis restart).

- **Keys**: `<prefix>:<algorithm>:<route>:<identity>`, where identity is `ip:<addr>`, `key:<hash>` or
  `ipcap:ip:<addr>`. Route names may not contain `:` (config validation rejects it); otherwise a route named `a:ipcap`
  could produce exactly route `a`'s IP-cap key and share its budget. The algorithm is part of the key, so
  changing a route's algorithm during a hot reload never misreads old state. Every script
  touches exactly one key, which keeps it compatible with Redis Cluster slot rules.
- **TTL**: keys expire once the state is back to "full", so Redis memory tracks only active clients.
- **Time source**: the gateway passes `now` (in microseconds) to the script. This makes
  the fake-clock tests and the real-Redis conformance tests possible. The trade-off is clock
  skew between replicas, and the two scripts handle it differently:
  - `token_bucket.lua` clamps: if a replica's `now` is older than the stored timestamp, it
    refills nothing and keeps the stored timestamp, so a lagging replica never adds tokens or
    moves the key's time backwards.
  - `sliding_window.lua` does **not** clamp. Each replica prunes with `now - window` and scores
    its entry with its own `now`. With skew `s`, a replica whose clock leads prunes entries
    written by a lagging replica up to `s` early, so the "exact cap" holds only up to the skew
    (for example, with 5 s of skew a 2-per-10 s window can admit 3 requests within about 5 s of real time).
    Clamping the score to the newest entry would help only while the set is non-empty, so it was not added.

  *Alternative:* `redis.call('TIME')` inside the scripts removes skew completely, but makes behaviour
  untestable with a fake clock. With NTP-synced hosts (milliseconds of skew against windows of
  seconds) client time is the better trade; for login protection across badly synced hosts, `TIME` would be the fix.
- **Number precision**: Lua numbers are doubles, and Redis converts script return values to
  integers by truncation. The scripts therefore return whole microseconds (rounded up with
  `math.ceil`) and store values with `%.17g` / `%.0f` so nothing is lost to Lua's default
  14-digit `tostring`. For the same reason a token-bucket limit is capped at 2^53, the largest integer a double holds
  exactly; above it, converting the token count to `int64` for `RateLimit-Remaining` overflowed (a limit of
  `9223372036854775807` produced `RateLimit-Remaining: -9223372036854775808`).

### Failure handling: timeout, then circuit breaker, then explicit policy

A rate limiter is on the hot path of every request, so a slow Redis must not become a slow API.

1. Each Redis call has its own budget (`backend.redis.timeout`, 50 ms by default), set as a context deadline in
   `degrade.Limiter`. go-redis ignores context deadlines for socket I/O unless `ContextTimeoutEnabled` is set, so the
   client sets it; the deadline then bounds dial, write and read together (a test against a TCP server that never
   answers checks this). go-redis does not react to plain cancellation, though: a client that disconnects mid-call
   does not abort the Redis call, which still ends within the budget. go-redis retries are disabled
   (`MaxRetries: -1`) because retries only add latency here.
2. After N consecutive failures the **breaker opens**, and the gateway stops calling Redis for
   `cooldown`. A dead Redis then costs nothing per request instead of a full timeout. After the
   cooldown a single **half-open probe** is allowed. Success closes the breaker, failure re-opens it.
3. While Redis is unavailable, the **failure policy** decides:
   - `open`: allow everything (availability first; fine for "nice to have" quotas).
   - `closed`: reject with **503** + `Retry-After`. It is a 503 and not a 429 because the client did nothing wrong.
   - `local` (default): keep enforcing the same rules per replica with the in-memory store.
     Protection degrades gracefully: with N replicas the effective limit is up to N times the limit.

A client that disconnects mid-call is not counted as a Redis failure (`breaker.Ignore`), so
impatient clients cannot trip the breaker.

`Breaker.Allow` returns a ticket that records the breaker's generation (incremented on every state change) and
whether the call is the half-open probe. `Success`, `Failure` and `Ignore` take that ticket and do nothing if it is
stale. Without it, a slow call that started while the breaker was closed could finish after the breaker opened and
close it without a probe, or free the probe slot so that a second concurrent probe got through.

### Readiness does not depend on Redis

`/readyz` only fails while the pod is draining. If readiness checked Redis, a Redis outage
would remove *every* replica from the load balancer, which is worse than any failure
policy. `/healthz` (liveness) is equally shallow so that Kubernetes never restart-loops
pods because of a dependency. The breaker state is shown on `/readyz` and exported as
`rlgw_breaker_state`, which is where alerting should look.

### Hot reload

`config.Reloader` holds the live config in an `atomic.Pointer`. Each request does one atomic
load, with no locks on the hot path. A reload parses and validates the whole file first, then swaps the pointer.
An invalid file is logged, counted (`rlgw_config_reloads_total{result="failure"}`) and ignored.
Only routes, the default route and key settings are hot-reloadable. Listener addresses, upstream
and backend changes need a restart, and the reloader says so in a warning instead of applying them halfway.

Validation reports every problem at once and treats a config that would misbehave as invalid, not just one that fails
to parse: timeouts and the breaker cooldown must be positive and the drain delay non-negative (a negative Redis timeout
would make every call fail and hold the breaker open), `failure_policy` is checked even with the memory backend, and route
names may not contain `:`. `gateway -check` runs the same validation, so CI and humans see the same errors.

Triggers: `SIGHUP` and content-hash polling. Polling was chosen over inotify (`fsnotify`) for
two reasons. It adds no dependency. And Kubernetes updates ConfigMap volumes by atomically
swapping a `..data` symlink, which inotify watchers commonly miss.

### Client identity and spoofing

A rate limit is only as strong as the identity it counts. There are two ways for a client to pick a fresh
identity per request, and both are closed by default:

- **Spoofed `X-Forwarded-For`.** For IP-based keys the gateway trusts `X-Forwarded-For` only as far as
  the configured `trusted_proxy_hops`. It takes the Nth entry from the right, because anything further left
  is client-controlled. Trusting the leftmost entry would let any client pick a fresh identity per request.
  The local Kubernetes overlay has no load balancer in front, so it sets `trusted_proxy_hops: 0`.
- **Made-up API keys.** The gateway does not authenticate API keys; it only counts them. With `key_by: api_key`,
  a client that sends a new random key on every request would get a fresh budget each time, and every key would
  create a new memory or Redis entry. So `key_by` defaults to `ip`, and an `api_key` route can set `ip_limit`: a
  second rule with the same algorithm and window keyed by client IP across all keys. The IP check runs first, so
  a rejected request never creates per-key state. The shipped configs all use `ip_limit` on their `api_key`
  routes. `api_key` without `ip_limit` is only safe when an edge or the upstream rejects unknown keys.

  The IP cap counts *attempts*, not admissions: a request it admits and the per-key rule then refuses has already
  spent an IP-cap unit. So one client hammering an exhausted key also drains the shared budget for other keys behind
  the same NAT. Checking the key first would let a key-rotating client create per-key
  state before the IP cap stops it, which is the attack the cap exists for. Avoiding the double spend would need a
  "peek" (check without consuming) or a refund operation in the `Limiter` interface, and in Redis either one becomes a
  second script call or a two-key script, which conflicts with the one-key-per-script rule that keeps Redis Cluster
  working. The cap is meant to be generous (the shipped configs use 3x the per-key limit) so that NAT sharing rarely
  reaches it.

API keys are hashed (SHA-256, first 16 bytes) before use, so secrets never reach Redis,
logs or metric labels.

### Canonical paths and route matching

Routes match whole path segments: a prefix matches the path itself or anything below it, so `/login` covers
`/login` and `/login/reset` but not `/login-help`, and `/api/` (the trailing slash is ignored) also covers the bare
`/api`. A plain string prefix would incorrectly limit `/login-help` under `/login` and let a request to `/api`
skip the `/api/` limit. A route that lists `GET` also matches `HEAD`, because Go and most frameworks answer HEAD with
the GET handler. Matching is case-sensitive, like `ServeMux`; an upstream that ignores case would need extra routes.

The default route covers every path and method. It therefore rejects `methods` and `path_prefix`, preventing
silently ignored methods and a user-set prefix being overwritten with `/`.

The matched path must also be the path the upstream will act on. Go's `http.Server`
does not clean paths (only `ServeMux` does), and many upstreams (nginx `merge_slashes`, Go `ServeMux`, most
frameworks) treat `//login`, `/./login` and `/x/../login` as `/login`. Without cleaning, those requests would miss a
`/login` limit. The handler therefore cleans the path with the `ServeMux` rules (`path.Clean`, trailing slash kept)
before matching and forwards the cleaned path, dropping `RawPath` so encoded forms like `/x/..%2Flogin` are resolved
too. *Alternative:* reject non-canonical paths with 400. That is stricter, but cleaning is what proxies such as
Envoy (`normalize_path`) do and does not break clients that send harmless double slashes.

### HTTP details

- Headers follow the IETF httpapi RateLimit draft (`RateLimit-Limit/-Remaining/-Reset`,
  `RateLimit-Policy: 100;w=60`). `Retry-After` is rounded up and is never 0.
- When a route runs two checks (`ip_limit` and the per-key rule), an allowed response reports the more restrictive
  one: the lowest `Remaining` with its `Limit`, and the latest `Reset`, because the client has its full quota back only
  when every limit has reset. `RateLimit-Policy` lists both policies (`300;w=60, 100;w=60`), as the draft allows. A
  denied response reports the check that denied it. Reporting only the last check would mislead clients: with a
  per-key limit of 10 and an IP cap of 2, `RateLimit-Remaining: 9` could precede a refusal on the next request.
- `httputil.ReverseProxy` uses the `Rewrite` hook (not the older `Director`). Hop-by-hop
  headers are stripped *before* `Rewrite` runs, so a client cannot list `X-Forwarded-For` in
  `Connection:` to make the proxy drop it. The inbound `X-Forwarded-For` chain is preserved and the peer appended.
- The admin endpoints listen on a separate port, and Kubernetes/Helm expose that port only through a separate
  `ClusterIP` Service (`floodgate-admin`), so changing the public Service's type to `LoadBalancer` does not
  expose `/metrics`.
- The access log writes one Info line per request, but 429s and fail-closed 503s are logged at Debug. Under a flood,
  one log line per rejection would make the logging pipeline the next victim; rejections are still counted in
  `rlgw_requests_total`.
- Prometheus collectors live on a private registry. Tests can build many gateways in one process without
  "duplicate registration" panics.

### In-memory store

There are 64 shards by default. Each shard has a `sync.Mutex` and a `map`, and a key picks its shard with `maphash`.
A janitor evicts entries whose state has fully reset, one shard at a time.
The parallel benchmark gives each goroutine its own random key stream to distinguish sharding costs from
same-key contention. Measurements in a shared 4-vCPU Linux container at 4 goroutines include medians of
294 ns/op with 64 shards and 357 ns/op with one shard, with overlapping ranges over 5 runs (284–386 vs 291–399),
and medians of 366 vs 462 ns/op with non-overlapping ranges. The loaded host prevents a reliable speedup claim.
Sharding limits lock contention as core count grows, but these measurements do not establish a gain.

A sliding-window rule stores one timestamp per admitted request, so `Rule.Validate` rejects sliding-window limits
above 10,000 (a typo like `limit: 1000000` would otherwise allow unbounded memory per key).
After a hot reload lowers a sliding-window limit, a key can hold more entries than the new limit. `Retry-After` is then
computed from the entry at index `n - limit` (the last one that must expire), not the oldest, and `Remaining` is clamped at 0.
The log keeps its timestamps sorted. With the real clock (`time.Now` has a monotonic reading) a new entry is always
the newest and is appended; an injected clock that steps backwards inserts it in order, as the Redis sorted set does.
Appending blindly would make the two backends disagree: a Go log containing `[5s, 0s]` would stop pruning at the
5 s entry and wrongly refuse a request at 11 s.

*Alternatives:* `sync.Map` suits read-mostly workloads, but every limiter call is a write. Per-key
mutexes would mean more allocations and still need a map lock.

### Packaging and deployment

- **Image**: multi-stage build into `distroless/static-debian12:nonroot`. The image has no shell or package
  manager and runs as uid 65532. The recorded Linux build was 9.6 MB compressed (39 MB unpacked; the stripped gateway binary was 18 MB with Go 1.26.8), with
  no HIGH/CRITICAL Trivy findings ([verification](VERIFICATION.md#summary)); sizes and scan results depend on the build. Because the image has no curl,
  the binary has a `-probe` mode for Docker and ECS health checks.
- **Toolchain**: `go.mod` and the Dockerfile require Go 1.26.8, a patched release that
  replaces the unsupported Go 1.24 minimum. CI tests and runs the pinned govulncheck
  on both Go 1.26.8 and current stable Go. Scan outcomes are dated in [VERIFICATION.md](VERIFICATION.md).
- **Kubernetes**: 2+ replicas set by the HPA's `minReplicas` (the Deployment has no `spec.replicas`, which would make
  every `kubectl apply -k` reset the count and fight the autoscaler), a `maxUnavailable: 0` rolling update, a PDB with `minAvailable: 1`, a CPU HPA,
  topology spread, a read-only root filesystem with all capabilities dropped, and `terminationGracePeriodSeconds` (30s)
  greater than drain delay plus shutdown timeout (20s). The ConfigMap keeps a stable name (no kustomize hash
  suffix) so edits reach pods through hot reload rather than a rollout. The trade-off is that a bad edit is not rolled out gradually.
  The gateway rejects invalid files, but a valid-but-wrong limit applies everywhere within one poll interval.
- **AWS (Terraform)**: ECS Fargate in private subnets behind an ALB. ElastiCache Redis runs with 2 nodes,
  Multi-AZ, TLS in transit, encryption at rest, and an AUTH token that ECS injects from Secrets Manager.
  Logs go to a CloudWatch log group, and target tracking scales on CPU. ECS has no ConfigMap, so an init container writes the
  rendered config into a task volume. Intentional scanner exceptions (public ALB, HTTP listener when no certificate is set,
  HTTPS egress) are annotated inline with a reason. **It has never been applied.**
- **CI and release**: lint (`go vet`, golangci-lint), tests with `-race` on Go 1.26.8 and the current stable Go (with a
  real Redis service container), govulncheck pinned to a version, image build, Trivy, a compose smoke test, Terraform
  and Kubernetes validation. The vulnerability scans also run weekly on a schedule, so a newly published advisory turns
  up between code changes. On a version tag the release job builds the image once,
  scans it with Trivy, checks that `gateway -version` prints the tag (passed in with `-ldflags -X main.version`), and
  pushes that same image to GHCR.

## Potential improvements

1. A sliding-window counter algorithm (O(1) memory per key) for high limits.
2. Per-route failure policies (for example `closed` for `/login`, `open` for `/search`).
3. A "local budget = limit / replicas" option for the `local` policy, with the replica count taken from the HPA or ECS.
4. Redis Cluster / Sentinel client options and a test against a real cluster.
5. OpenTelemetry tracing around the limiter call. A Grafana dashboard JSON for the exported metrics.
6. A k6 or vegeta load test in CI on a dedicated runner, to track the overhead over time (on the shared container
   used here, the limiter's overhead was within run-to-run noise).
7. A dry-run mode ("would have limited") to roll out new limits safely.
8. Optional `redis.call('TIME')` mode for the sliding window, for deployments where host clocks cannot be trusted.
