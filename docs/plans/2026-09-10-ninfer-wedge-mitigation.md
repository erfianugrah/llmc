### Ninfer context-materialization wedge - mitigation

## Root cause (confirmed 2026-09-10, prior session)

Upstream bug: Neroued/ninfer#184 (open). With `--max-concurrency 1`, a client
that disconnects while the engine is materializing a long-context request
leaves the sole slot stuck. The SSE heartbeat cannot run during synchronous
materialization, so the disconnect is never detected, and the engine's own
cancellation path explicitly skips cancellation while a context transaction
is open. Symptom: prefill crawls at ~30 tok/s (baseline ~2.7k tok/s), the
HTTP listener goes unresponsive, and a bare `docker restart ninfer_server`
did NOT clear it in the 2026-09-10 reproduction - only a full stack restart
(model_proxy_go + engine) did. Practical ceiling for `qwen38-ninfer` is
~120k ctx; past that fall back to llama.cpp `qwen38`. Related upstream:
#210 (KV race, full GPU lockup), #181 (prefix cache eviction per interleaved
request).

## Done this session

1. **Wired three previously-unused `ninfer-serve` flags into the preset
   schema** (`proxy-go/internal/proxy/presets.go`, `orchestrator.go`):
   `prefill_chunk`, `max_pending_requests`, `pending_timeout_ms`. Confirmed
   present via `ninfer-serve --help` against the running image
   (`erfianugrah/ninfer:cuda13.1-sm120a-487f897`). None are set on the live
   `qwen38-ninfer.toml` preset yet - wiring only, no behavioural change
   until a preset sets them. Tests: `TestNinferOptionalQueueAndPrefillFlags`,
   `TestNinferQueueAndPrefillFlagsOmittedWhenUnset`.

2. **Built `WedgeWatchdog`** (`proxy-go/internal/proxy/wedge_watchdog.go`):
   on a `client_gone` note against the ninfer engine (server.go's
   `forwardTo`), wait a grace period (20s default), then probe
   `GET /health` (10s timeout default). If the probe fails, call
   `Scheduler.NoteUpstreamDead` - the same path a connection-level upstream
   death already uses, which flips scheduler state to idle so the *next*
   acquire does a full stop+remove+create respawn
   (`DockerOrchestrator.spawnCmd` -> `stopGPU` + `CreateAndStart`), not a
   bare restart. **Disabled by default** - `LLMC_NINFER_WEDGE_WATCHDOG=1` to
   enable. 6 unit tests (healthy/unresponsive/error-status probes, disabled
   no-op, nil-receiver safety, burst coalescing), race-clean.

## What is still unverified

- **Whether the respawn-only recovery path actually clears a real wedge.**
  The 2026-09-10 session's only confirmed recovery was a full stack restart
  (proxy + engine); it is not known whether restarting the proxy itself was
  necessary or just what the operator did at the time. `WedgeWatchdog`
  deliberately reuses the less invasive, already-tested respawn path
  (`NoteUpstreamDead`) rather than a new self-restart mechanism, on the
  reasoning that `stopGPU`'s stop+remove+create is a full container
  teardown (unlike the bare `docker restart` that was observed NOT to work)
  - but this reasoning has not been tested against a real wedge.
- **Whether `--pending-timeout-ms` reaches a request already inside context
  materialization** (the wedge state) or only bounds requests still queued
  before processing starts. `ninfer-serve --help` documents neither the
  queue-vs-processing boundary nor what happens to a request past the
  timeout. No local ninfer source to check; nothing found in upstream docs
  as of this session.
- **The watchdog's grace/probe thresholds (20s / 10s) are unvalidated
  guesses**, not measurements. They need tuning against a real wedge (false
  negative if too short relative to legitimate slow prefill = never fires;
  false positive if too aggressive = kills healthy-but-slow requests).

## Deliberately not done this session

Live reproduction of the wedge (build a long-context prompt against
`qwen38-ninfer`, abort the client mid-materialization via a short
`curl -m N`, and observe recovery with/without
`LLMC_NINFER_WEDGE_WATCHDOG=1` and `pending_timeout_ms` set) was scoped but
not run: it requires taking the shared GPU stack's `qwen38-ninfer` preset
out of service for other consumers (loops, other pi sessions) for the
duration, with a real risk of leaving it wedged if the mitigations don't
work and needing a manual `make clean` + `make deploy` to recover. Needs an
explicit go-ahead and a window when nothing else needs the GPU.

## Reproduction attempts (2026-09-10, later same day) - 7 total, 0 reproduced

Six attempts against the live production `qwen38-ninfer`, one accidentally
overlapping another session's concurrent request (apologized to the user,
no lasting damage - see below). None reproduced the wedge:

| # | Size (tokens) | stream | Abort at | ninfer's own log |
|---|---|---|---|---|
| 1 | ~156k | false | 5s (queued behind a concurrent request from another session) | `cancelled`, clean |
| 2 | ~180k | false | 8s | `cancelled`, clean |
| 3 | ~179k | false | 35s | `cancelled`, clean |
| 4 | ~179k | true | 35s | `cancelled` + `HTTP 499 client disconnected`, clean |

Every attempt: ninfer's own log recorded a clean `cancelled` line and the
next 5s throughput sample showed `running 0` immediately - slot released,
no lingering state. Subsequent trivial requests served normally
(~100ms). Attempt 4 specifically targeted the documented mechanism
(SSE heartbeat during a streaming request past the point a normal request
would still be prefilling) and still did not wedge.

**The concurrency lead from attempt 1 was chased and retracted.** A 5th
and 6th attempt deliberately reproduced two-request contention under
control: request A (~179k tokens, left to run to completion) plus request
B (~179k tokens) fired 3s later and aborted at 6s while B sat queued
behind A (`waiting 1` in ninfer's own log - B never started processing).
The exact same `upstream ninfer-server died mid-stream: context canceled`
line reappeared on the proxy side - but ninfer's own log proves it is
benign: `req#25 done | cancelled | ... total 6.1s | queue 6.0s` (B,
cancelled cleanly while queued) and `req#24 done | output limit | ...
total 1m 3.7s` (A, completed completely normally, unaffected). That log
line is just how the proxy reports a queued request's client disconnect,
not a wedge symptom. The attempt-1 anomaly was the same benign case,
not a lead.

**Net result across all 6 deliberate attempts (5 documented above + this
pair, plus a 7th real-world data point below): not reproduced under any
tested condition** - varying size (150k-230k tokens), stream true/false,
abort depth (5s-35s), and single vs concurrent-with-queued-second-request.
ninfer's own per-request log confirmed a clean `cancelled` + immediate
slot release every single time.
The running image (`erfianugrah/ninfer:cuda13.1-sm120a-487f897` - the
suffix is a baked-in commit short-hash, so the tag is effectively
immutable) is the same one that produced the original wedge earlier the
same day, so this isn't an upstream fix landing under us - the trigger
condition remains unidentified. Candidates not yet tried: a request
genuinely AT the 252,928 ceiling (all attempts stayed comfortably under
it), the client_gone landing while the CANCELLED request is the one
ACTIVELY PROCESSING rather than queued (both concurrency attempts here
aborted the queued one, not the running one), vision/multimodal content,
or a non-curl client library with different connection-teardown behavior
than curl's abrupt socket close.

Also reviewed while at 179,440 real tokens resident: GPU headroom was
31,489 / 32,607 MiB used, 699 MiB free. Checked ninfer's own boot log
rather than assume what this means: `capacity | KV 252,928 tokens, fp8,
explicit | pages 3,952/3,952 | runtime 9.32 GiB | free 751.6 MiB` -
`explicit` + fully-committed pages confirms the KV pool (plus the 20 GiB of
weights) is a ONE-TIME upfront allocation at container start, not
dynamically grown per-request. Once it succeeds, that memory is exclusive
to the ninfer process - desktop GPU consumers (Chrome/Zen/WezTerm, all on
the same card) growing their own usage afterward cannot destabilize an
already-running engine. The exposure the toml's "shrinks further under
desktop GPU load" comment describes is at BOOT TIME only: every preset
swap does a full stop+remove+create (`stopGPU` + `CreateAndStart`),
re-doing the ~31.5 GB allocation from scratch, and if desktop apps hold
more VRAM at that moment than when the 252,928 ceiling was calibrated
("31.7 GB peak, ~900 MiB spare"), the fresh allocation can fail to fit.
This directly matters for `WedgeWatchdog`: its recovery path IS a respawn,
so a watchdog-triggered recovery inherits this same boot-time OOM
exposure - if desktop GPU usage is elevated when the watchdog fires, the
recovery attempt itself could fail to fit, not just fail to fix the wedge.

## Fixed the same day, adjacent to this investigation

- `anthropic.go`'s `/v1/messages` handler hardcoded `Services["llm"]`
  (`LlamaService`) instead of `Server.activeLLMService()` and so could
  never route to ninfer regardless of the active preset - fixed, with the
  same `reasoning_effort` default-injection guard the OpenAI-compatible
  route already had (ninfer's chat template defaults to xhigh thinking
  when no per-request effort is given) and the watchdog's `NoteClientGone`
  hook wired into this route too.
- `qwen38` and `loop` (llama.cpp presets) had no `runtime.max_output_tokens`,
  so pi capped them at its own 16384 default - the same truncation class as
  the 2026-09-07 qwen38-ninfer incident. Set to 65536 on both.

## A 7th data point: a genuine production cancellation, not a synthetic test

While reviewing ninfer's own container log after this session's tests, a
real (not synthetic) client disconnect showed up on an actual growing
agentic conversation, unrelated to any of the 6 deliberate attempts above:

```
req#30 started (85 messages, real conversation)
req#30 cancelled at 7.0s, cache 0.0%, HTTP 499 client disconnected
[3m52s gap]
req#31 started (retry) - ran at normal speed: 3.03k tok/s avg, 36.7s, completed cleanly
```

A genuine mid-materialization disconnect (cache 0% confirms it was a real
fresh prefill, not served from cache) on real production traffic, and the
engine recovered to fully normal throughput on the very next request.

For anyone re-reading this log cold: prefill tok/s DECLINING across the
5s windows *within one request* (e.g. 6.55k -> 4.30k -> 3.48k -> 2.46k ->
1.64k) is normal cost-of-attention-over-a-growing-KV-cache behavior, not
the wedge signature - it happens on every reasonably-sized request in
this log, mine included, and every one still finishes in seconds to
~1-2 minutes. The actual wedge signature from the original incident was
a SUSTAINED ~30 tok/s that computed to an "85-min prefill ETA" - nothing
here gets remotely close to that.

## 8th data point (2026-09-14): field reproduction, and it changes the trigger model

The wedge reproduced in production, without any synthetic attempt. Sequence
(times UTC; ninfer's own request log):

```
04:35:20 req#1 started | 95,067-token prompt, cache 0%, media 1 (an image)
04:44:09 req#1 cancelled at 8m49s (user abort mid-prefill) - logged clean,
         HTTP 499, and the slot released (req#2 queued only 35ms)
04:44:41 req#2 started | 95,073-token retry, cache 0%, media 1
06:14:40 req#2 done | TTFT 1h29m, prefill 17.6 tok/s SUSTAINED, then
         self-recovered: burst through the rest of the prefill in ~4 min
         and completed normally; requests #3-6 right after were fully
         normal (TTFT 188ms-5.7s)
```

Three properties that rewrite the trigger model from the earlier sections:

1. **The wedged request's client NEVER disconnected.** The disconnect was on
   the PREVIOUS request (req#1), which ninfer's log shows cancelling
   cleanly. So the trigger is not "disconnect holds the slot" - it is
   closer to "a mid-materialization cancel leaves engine state that poisons
   the NEXT fresh prefill". Consistent with the untested candidate "abort
   the ACTIVELY PROCESSING request" from the 2026-09-10 repro list, but the
   damage shows up one request later, not on the cancelled one.
2. **Both prompts carried vision content** (`media 1`) - another untested
   candidate from that list.
3. **The wedge self-recovered** (89 min, no restart) - unlike the original
   2026-09-10 incident, which needed a full stack restart. The throughput
   log during the wedge shows `host 0.0% (0 us)` in nearly every 5s window
   with zero tokens processed - the engine was BLOCKED (a lock/wait), not
   computing slowly. Whatever it waited on has an ~85-minute timeout or
   eventual resolution.

Context state at the time: 252,928-token KV pool; req#2 entitlement
(95,073 prompt + 65,536 output reservation = 160,609) plus req#1's
potentially-retained 95k checkpoint puts the pool right at capacity - the
pressure path of upstream #229/#176 (5ms search budget -> maximal fallback
-> full re-prefill) explains the `cache 0%` but NOT the 89-min block.

## Response shipped 2026-09-14

1. **Rebased the pinned engine 487f897 -> d492968.** The two intervening
   upstream commits both touch the wedged path: `b88c0f6` fix(core):
   complete host uploads before returning (a pageable-H2D sync bug) and
   `d492968` perf(runtime): materialization search + planning-budget rework
   (closes #176). Reviewed both diffs; b88c0f6 is 9 lines, d492968 is the
   planner rewrite with its own CTests. NINFER_PIN bumped.
2. **The local #184 watchdog patch is now durable.** It was written
   2026-09-10 as UNCOMMITTED edits in the .ninfer checkout and was never in
   the running image (built 2026-09-07) - the engine that wedged today was
   vanilla 487f897. It now lives at `patches/ninfer/0001-sse-transport-
   watchdog.patch`, applied by `make apply-ninfer-patches` (idempotent) as
   part of `make build-ninfer`. check-ninfer-drift accepts exactly two
   states: clean-at-pin, or pin+exactly-the-tracked-patches (verified by
   reverse-apply check + file-set equality); anything else still fails.
3. **Enabled `--request-log-jsonl` on the qwen38-ninfer preset** (new
   `request_log_jsonl` preset key, wired through presets.go +
   orchestrator.go, mounts the new `llmc-ninfer-logs` volume at /logs).
   Per-request materialization diagnostics (stop_reason, budget_exhausted,
   best_reuse_prompt_tokens) now land at
   ~/docker-volumes/ninfer/logs/engine.jsonl - the fields #229 used, so the
   next wedge is diagnosable instead of inferred from throughput logs.
4. **Enabled the proxy-side WedgeWatchdog** (`LLMC_NINFER_WEDGE_WATCHDOG=1`
   in compose.yaml). Caveat from earlier sections still stands: its
   recovery is a respawn, which inherits the boot-time OOM exposure if
   desktop GPU usage is elevated at that moment. The watchdog only fires on
   a client_gone + failed health probe - today's wedge had NEITHER (the
   wedged request's client never left, and whether /health answered during
   the 89 min is unverified; the watchdog would NOT have fired on this
   incident).

## Next steps

Seven data points now (6 deliberate + this one), all clean. Varied (size, streaming, abort depth, queued
concurrency) all came back clean, so the next attempt needs a genuinely
different condition rather than a repeat with different numbers:

1. **A request genuinely at the 252,928 ceiling** - every attempt so far
   stayed at ~150k-230k tokens; the wedge may require the actual
   context-transaction/materialization path that only engages near the
   documented limit, not just "large".
2. **Abort the ACTIVELY PROCESSING request under contention, not the
   queued one** - both concurrency attempts here aborted the request that
   was still waiting for the slot (never started). The original incident's
   description is about a disconnect while the engine IS materializing -
   that means the running request, not a queued second one.
3. **A non-curl client** - curl's abrupt socket close on `-m` timeout may
   tear the connection down differently (TCP RST vs a more graceful
   half-close) than whatever client produced the original wedge. Worth
   trying pi's actual HTTP client behavior or a language with more
   controllable disconnect semantics.
4. **Vision/multimodal content** - all attempts here were text-only;
   `ninfer.vision = true` on this preset and the original incident's
   context is unknown to have been text-only.
5. If any of the above reproduces it: test whether respawn-only recovery
   (the watchdog's `NoteUpstreamDead` path) actually clears it, whether
   `pending_timeout_ms` prevents it forming, and tune the watchdog's
   grace/probe thresholds (currently unvalidated 20s/10s guesses) from
   what's measured.
6. Set `prefill_chunk` on `qwen38-ninfer.toml` if reproduction shows it
   changes materialization duration (shorter chunks -> more SSE-heartbeat
   opportunities -> smaller wedge window) - currently just a hypothesis,
   untested either way.
7. Boot-time OOM exposure (see the GPU-headroom note above): confirm what
   happens if `WedgeWatchdog`'s respawn is triggered while desktop GPU
   usage (Chrome/Zen/WezTerm) is elevated - does `CreateAndStart` fail
   cleanly (container stays down, scheduler state recoverable) or does it
   leave something worse than the wedge it was trying to fix?
