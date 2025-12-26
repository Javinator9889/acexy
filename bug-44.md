# Bug #44: Proxy hangs when switching channels quickly

## Analysis
The issue was identified as a deadlock in the `PMultiWriter` implementation used by the proxy to broadcast stream data to multiple clients.

When a client stops reading data (but stays connected), the `PMultiWriter.Write` method blocks waiting for that client's write to complete (or fail).
The original implementation of `PMultiWriter.Write` held a `RLock` on the `PMultiWriter` instance while waiting for all writes to complete.

When another client connects or disconnects (switching channels), the proxy attempts to Add or Remove a writer from the `PMultiWriter`.
Both `Add` and `Remove` require a `Lock` (write lock).
Because `Write` was holding the `RLock` indefinitely (due to the blocking client), `Add`/`Remove` would block indefinitely.

Since `StartStream` and `StopStream` (which call `Add`/`Remove`) are called with the global `Acexy` mutex held (or `ongoingStream` mutex), this blocked the entire proxy, preventing any new connections or status checks.

## Reproduction
A synthetic test case was created in `acexy/repro_test.go` (`TestDeadlockReproduction`).
The test is self-contained and uses a mock AceStream backend (`startMockBackend`) created with `httptest.NewServer`. It simulates:
1. Client A connecting and blocking reads (filling the buffer).
2. Client B connecting to the same stream.
3. Client B disconnecting (triggering `StopStream` -> `Remove`).
4. Client C connecting to a different stream (triggering `StartStream` -> `Lock`).

### Verification of Test and Fix
- **Without the fix**: The test consistently fails with a timeout because Step 4 (Client C connecting) is blocked by the deadlock.
- **With the fix**: The test passes consistently, even under parallel stress testing.

## Fix
The `PMultiWriter.Write` method in `acexy/lib/pmw/pmw.go` was modified to:
1. Acquire `RLock`.
2. Create a local copy of the list of writers.
3. Release `RLock` immediately.
4. Perform the writes using the local copy.

### Slow Consumer Eviction
Additionally, `PMultiWriter` now supports automatic eviction of slow consumers:
- Each `Write` operation is protected by a `writeTimeout` (defaulting to 5 seconds).
- If a writer stalls and exceeds this timeout, it is automatically removed from the `PMultiWriter` broadcast list.
- This prevents a single bad client from blocking the entire data stream for other healthy clients.
- Even if a write is stalled, the main proxy remains responsive because the state is decoupled from the actual IO operations.

## Verification
The regression test `acexy/repro_test.go` passes with the fix applied.
```
=== RUN   TestDeadlockReproduction
...
Client C connected successfully!
...
--- PASS: TestDeadlockReproduction (3.01s)
PASS
ok      javinator9889/acexy     3.010s
```
