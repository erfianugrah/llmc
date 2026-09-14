# Local ninfer patches

Patches applied on top of `NINFER_PIN` before every `make build-ninfer`
(via `make apply-ninfer-patches`, invoked automatically by the build
target). The drift guard (`make check-ninfer-drift`) accepts exactly two
checkout states: a clean tree at `NINFER_PIN`, or `NINFER_PIN` plus
exactly these patches (verified by reverse-apply check + modified-file-set
equality) - any other local edit fails the build.

This directory is the canonical copy of each patch. There is NO GitHub
fork in the flow anymore (erfianugrah/ninfer was deleted 2026-09-14; its
only non-upstream content was an upstream-authored dev snapshot whose
features had already landed in master in different form). Durability comes
from this repo, not from a fork.

## 0001-sse-transport-watchdog.patch

Written 2026-09-10 for upstream issue Neroued/ninfer#184 (OPEN, no
upstream fix as of d492968). During a synchronous engine run (prefill /
context materialization) the worker thread never calls `poll()`, so a
disconnected client goes unnoticed and holds the engine's only slot for
the whole materialization. The patch spawns a watchdog thread that probes
socket liveness (`is_writable`, read-only MSG_PEEK) every 500 ms while the
run is blocked and sets the shared cancelled flag when the peer is gone;
the engine observes it at the next context-transaction boundary and aborts.

Applies cleanly on 487f897 and d492968. If `git apply --check` fails after
a pin bump, upstream moved the serve layer - rebase the patch against the
new pin and refresh this file (`git -C .ninfer/src/ninfer diff >
patches/ninfer/0001-sse-transport-watchdog.patch` after hand-applying).

Retire this patch when upstream merges an equivalent fix for #184.
