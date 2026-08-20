# SOLUTION.md

## What was broken, and why

Four defects, all concurrency bugs the existing tests didn't exercise because they only ever ran things sequentially.

1. **`stats.Cache.Record` had no mutex.** `Get` correctly used `RLock`/`RUnlock`, but `Record` wrote to the shared map with no locking at all. Under concurrent webhook deliveries, two goroutines writing to the same map entry corrupt it, or trigger Go's fatal `concurrent map writes` crash. Fixed by locking `Record` with the same `sync.RWMutex`, proven with a test that fires 100 concurrent writers under `go test -race`.

2. **Duplicate events (TOCTOU race).** `Ingest` checked `EventExists` and then, several lines later, called `InsertEvent` — two separate round trips with a window between them. Two concurrent deliveries of the same `event_id` (expected, since the provider retries on any non-2xx and even occasionally redelivers after a `200`) could both pass the check before either insert landed, producing duplicate `events` rows and double-incrementing `account_stats`. Fixed by replacing the check-then-insert with one atomic `INSERT ... ON CONFLICT (event_id) DO NOTHING`, backed by a new `UNIQUE` constraint on `events.event_id`. A duplicate is now detected and skipped in the same statement that would have inserted it — there's no gap left to race in.

3. **Recordings silently never got marked processed.** The background goroutine that processes a recording reused the *request's* `context.Context`. `net/http` cancels that context the instant the handler returns, and since `Ingest` returns almost immediately, the goroutine was routinely still running against an already-canceled context when it tried to write `recording_processed = true` — a write that `pgx` aborts once it sees cancellation. The resulting error was then discarded by a bare `// TODO: handle`, so the failure left no trace. Fixed by giving the goroutine its own context (rooted at `context.Background()`, bounded by a 30s timeout so it can't hang forever) and actually logging the error instead of swallowing it.

4. **In-flight work vanished on deploy.** That same goroutine was never tracked anywhere. `srv.Shutdown()` only waits for active HTTP handlers, which had already returned by the time the goroutine finished — so the process could exit mid-write. Fixed with a `sync.WaitGroup` on `Service`, exposed as `Wait()`, called from `main` after `srv.Shutdown()` returns and before the store/Redis connections close.

## Why this deduplication strategy

I used Postgres — a `UNIQUE` constraint plus `INSERT ... ON CONFLICT DO NOTHING` — rather than a Redis-based approach (e.g. `SETNX event_id`).

The events table is already the system of record for this data; the insert and the dedup check are the same write. A Redis-based lock would make them two separate writes against two separate systems, which opens a new consistency problem: what happens if the Redis `SETNX` succeeds but the Postgres insert fails, or vice versa? That needs its own reconciliation logic to solve, for a guarantee Postgres already gives for free via its unique index. Redis also isn't durable by default — a restart or eviction can silently drop the dedup key, letting a duplicate back in.

The other alternative — an in-process lock (`sync.Mutex`) around inserts — was never viable beyond a single instance. A Go-level mutex only coordinates goroutines within one process; it has no effect once this service is horizontally scaled behind a load balancer, since two replicas don't share memory. Postgres is the one thing every replica already shares, so it's the only place correctness can actually be enforced.

## At 10,000 webhooks/second

The single Postgres primary becomes the bottleneck — every delivery pays a synchronous round trip and contends on the same unique index. I'd add Redis as a fast-path filter in front of it: `SET event_id NX EX <ttl>` to reject the large majority of duplicates in-memory before they ever reach Postgres, while keeping the `UNIQUE` constraint as the durable correctness backstop for the cases Redis misses (restart, eviction, race at the boundary). I'd also move recording processing off ad-hoc goroutines onto a bounded worker pool or a real queue (e.g. SQS/Kafka) so ingestion throughput isn't coupled to, or capable of being overwhelmed by, however long recording downloads take, and so a burst of traffic produces backpressure instead of an unbounded number of concurrent goroutines and DB connections.
