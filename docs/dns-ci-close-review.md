# DNS cancellation / CI review (2026-10-09)

This review follows the first failed run of `37907490063` on `9b0b58f7317e7440e8fa8d2b7d4e634af062f63d`. A successful retry was not accepted as a root-cause fix. The changes are confined to fork DNS paths and their tests; ordinary TFO, proxy routing and upstream cache algorithms are unchanged.

## Findings

1. **HTTP/2 close assertion used a completion counter as an invocation counter.** The test adapter intentionally finishes its first dial after cancellation. HTTP can return the request and Close before that detached dial finishes. Reading the adapter's completion counter twice could mislabel the old completion as a new dial. The unmodified H2 test failed 8 of 100 local repetitions. The corrected test counts entries, waits explicitly for the late socket to be closed, and still rejects any new invocation. A new deterministic test keeps the old dial blocked until Close and the request have both returned, then checks rejection, late disposal and absence of a DNS query at the server. It does not use a sleep to synchronize or remove the failing assertion.

2. **Cancellation after bootstrap was not checked before starting DNS transport work.** A valid address can arrive just as its lookup context is canceled. Native TCP/UDP and forwarded port-53 DNS now recheck cancellation after bootstrap. Four deterministic cases fail before this change and pass afterward.

3. **Raw TCP/UDP DNS could try to write after a cancellation-insensitive adapter returned a late socket.** The fork's wire-exchange path now checks before dialing and again on return, closes a late socket and returns the cancellation without writing the DNS query. Both tests fail before the added checks and pass afterward.

4. **The dashboard-close singleflight barrier used the pre-routing context.** The performance changes intentionally keep source-port isolation in flight. The barrier now derives its key from the prepared routing context, just as the real exchange does; it no longer waits on a different key.

## Validation

- Local toolchain matches CI's MetaCubeX Go 1.26.8 on linux/amd64.
- Corrected four-protocol close test plus deterministic H2 ordering test: 200 repetitions each with GOMAXPROCS 1, 4 and 8, with race detection.
- Six post-bootstrap / late-socket cancellation cases: 200 repetitions each with race detection; all have before-fix failures.
- Corrected UDP53 / H2 dashboard-close test: 200 repetitions each with race detection.
- Affected package unit tests and repeated race/shuffle suites are run alongside the new cases.
- CI now requires the lifecycle suite to pass every repetition at GOMAXPROCS 1 and 4 (`-count=50`), rather than retrying a failed job until it turns green.
- Controlled 50-source tests use local test resolvers and retain source rules, pool selection, rejection and proxy-no-probe assertions. They add no production metrics or telemetry.
- Full binary integration and real PROCESS identification must also pass on GitHub. The local container returned `netlink receive: no such file or directory`; the existing integration assertion was not weakened to conceal it.

These tests exercise explicit interleavings and regression cases, not all possible operating systems, adapters or network failures. Final release status must be checked against the exact source commit and the full build matrix.
