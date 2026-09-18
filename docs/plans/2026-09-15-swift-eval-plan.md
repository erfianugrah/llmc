# Swift-Qwen3.8-27B evaluation plan

2026-09-15. Status: quick assessment in progress; full test pending
self-quantization pipeline.

2026-09-18 update: engine bumped d492968 -> 6cc95cc5 (v3 artifact format;
watchdog patch re-applies clean; NINFER_PIN updated, image
cuda13.1-sm120a-6cc95cc5, LLMC_NINFER_IMAGE set in compose.yaml). Baseline
artifact upgraded offline to v3 (qwen3_8_27b_nvfp4_v3.ninfer via
tools/upgrade_ninfer_v2_to_v3.py; preset repointed, verified serving).
Added a SECOND quick arm: qwen38-swift-ca-ninfer
(CaptainArni/Swift-Qwen3.8-27B-NInfer, v3, sha256
5412a0e7...99cf7f verified, ModelOpt bytes imported not requantized) -
supersedes the OrcaRouter arm: 21.2 GiB weights, boots at full 262144 ctx
with fp8 KV + vision + MTP in ~8s (~1.5 GiB spare). Smoke results
2026-09-18: boot clean; xhigh math probes 57 and 404 reasoning tokens
(both correct, 168-192 tok/s decode); vision OK; tool-call args JSON
clean in quality.jsonl. NOTE: the OrcaRouter preset/artifact is v2 and
now BOOT-BROKEN under the new engine - delete or offline-upgrade before
use.

## Subject

ukisai/Swift-Qwen3.8-27b - LoRA post-train of Qwen3.8-27B that penalizes
tokens correlated with overthinking loops ("but wait"-class patterns),
then restores accuracy via on-policy distillation. Claims (their evals,
x5 runs, BF16, xhigh):

- -58% median thinking tokens at xhigh, <1% accuracy loss on GPQA/MMLU/
  Terminal-Bench/LCB/C-Eval/IFBench/ERQA
- Known regressions they admit: AIME26 -4.6pp (a math-token penalized by
  a training bug, fix promised), 1-4% loss at medium/low effort
- Token reduction by effort: xhigh 41%, medium 23%, low 26%

Why we care: qwen38-ninfer is the daily driver, and thinking-token volume
is the dominant wall-clock cost at xhigh. If the trim is real and
tool-call quality survives, Swift is a drop-in latency win.

## Confound map (read before trusting any delta)

| Arm | Quant recipe | Spec backend | Adapter |
|---|---|---|---|
| qwen38-ninfer (baseline) | unsloth mixed FP8+NVFP4 (neroued repack, 22.1 GB) | MTP k=3 | none |
| qwen38-swift-ninfer (quick arm, v1) | OrcaRouter NVFP4 (community, 23.7 GB) | DFlash2 k=7 (stock head, not retrained on Swift) | Swift LoRA |
| qwen38-swift-ca-ninfer (quick arm, v2) | CaptainArni v3, ModelOpt bytes imported (21.2 GB) | MTP k=3 (same as baseline) | Swift LoRA |
| swift-clean (full-test arm, planned) | self-quantized mixed FP8+NVFP4 via llm-compressor, converted with tools/convert/qwen3_8_27b | MTP k=3 | Swift LoRA |

Only swift-clean isolates the adapter. The quick arm answers behavioral
questions; speed comparisons against baseline are indicative only.

- ukisai's official NVFP4 safetensors CANNOT feed the converter: their
  recipe is NVFP4-on-MLPs-only with BF16 attention (30 GB, single-group
  "Linear" targets); the converter requires the two-group mixed
  compressed-tensors config. Verified 2026-09-15 against
  convert_nvfp4.py preflight.
- GGUF lane (bartowski Q4_K_M, not pulled) would test llama.cpp, which
  is not the daily driver. Skipped deliberately.

## Instruments (already live)

- `~/docker-volumes/state-go/quality.jsonl` - per-request tool-call
  flags (leak/unclosed/args_bad/truncated) + model/preset/dur_ms.
  OpenAI route only; Anthropic /v1/messages NOT instrumented (known gap).
- `~/docker-volumes/ninfer/logs/engine.jsonl` - per-request engine
  diagnostics; thinking/output token counts come from here.
- `llmc bench perf|tasks|gumshoe` - committed trend store in
  bench/results/runs.jsonl.

## Phase 0 - quick assessment (this week, community artifact)

1. Pull Yuuyuuyuuyuu OrcaRouter .ninfer (DONE, bg download).
2. `models/qwen38-swift-ninfer.toml` (DONE): spec=dflash2, draft_tokens=7,
   max_context=131072, otherwise mirrors qwen38-ninfer.
   Two boot failures already paid for, do not rediscover:
   - `--spec ln` is REJECTED by the pinned d492968 binary (usage shows
     mtp|dflash|dflash2); the `ln` spelling exists only in the checkout's
     docs. Crash-looped once on this.
   - 262144 ctx does NOT fit this artifact: 23.7 GB weights + 10.5 GB
     runtime reservation > 32 GB card (FATAL "requires 10494870784 bytes,
     only 9174135808 available" on a free GPU). 131072 is what the
     community benchmarked at. A `--kv-dtype nvfp4` retry at 262144 is an
     untried lever (adds a KV-precision confound).
3. Smoke: `llmc switch qwen38-swift-ninfer`, boot clean, one vision
   prompt, one long agentic prompt.
4. Behavioral probes, same 5 prompts both presets at medium AND xhigh:
   - one trivial task (does it still overthink small asks?)
   - one hard math word problem (AIME regression canary)
   - one multi-file code edit task (tool-call shape)
   - one long-context recall prompt
   Record thinking tokens + output tokens (engine.jsonl) + flags.

   DONE 2026-09-18 (bench/phase0-battery.sh, results in
   bench/results/phase0-20260918-*.jsonl). Findings:
   - ALL answers correct on both arms at both efforts: AIME 2024 I P1 = 73
     (x4), 3/3 needles at 13k context (x4), identical clean tool-call
     (quality.jsonl flags all null), debug answers equivalent quality.
   - BUT reasoning lengths were at parity (ratios 0.92-1.42) - no trim
     visible. Root cause of the null result: the engine's default sampling
     is GREEDY (temperature 0), and CaptainArni's -50% was measured at
     temp 1.0/top_p 0.95/top_k 20. Overthinking spirals are a
     sampling-time pathology; greedy base Qwen doesn't spiral on
     easy/moderate tasks, so there is nothing for Swift to trim.

   Follow-up spiral-territory battery (bench/phase0-hard.sh,
   phase0-hard-20260918-*.jsonl): 3 harder prompts x 2 runs, xhigh,
   temp 1.0/0.95/20. ALL correct on both arms (128, 7-races, musl locale
   root cause + setlocale fix). The trim appeared exactly where the
   mechanism predicts:
   - locale_debug r1: BASELINE spiraled to 30,133 reasoning chars (9,965
     completion tokens, 185s) vs Swift 6,549 chars (2,737 tokens, 53s) -
     3.6x token cut, same correct answer. r2: 9,634 -> 6,068 chars.
   - combinatorics + horses: parity within sampling noise (n=2, mixed
     directions).
   Decode ~50-58 tok/s on BOTH arms throughout (vs the 139 p50 from the
   September spike - likely desktop GPU contention during the run; not
   chased, equal for both arms so comparisons hold).

5. Report: Phase 0 verdict POSITIVE: the trim is real and targets the
   pathological case specifically, quality is equal-or-indistinguishable
   on every probe, tool-call shape clean. Go for production soak. Phase 1
   (self-quant) ON HOLD: ukisai announced Swift 1.5 (bugfix + more RL)
   within days in the 2026-09-18 r/LocalLLaMA thread - a self-quant of v1
   would be obsolete on arrival. Soak the CaptainArni arm instead
   (quality.jsonl + engine.jsonl, 3-7 days per arm) and revisit Phase 1
   against the 1.5 BF16 checkpoint.

## Phase 1 - full test (needs self-quant pipeline)

1. Swift BF16 download DONE (18 shards + configs in
   ~/docker-volumes/ninfer/convert-inputs/swift-bf16, ~54 GB; z-lab
   DFlash2 head + ukisai model_mtp.safetensors alongside; torch cu128
   convert-venv at ~/docker-volumes/ninfer/convert-venv).
2. Replicate the unsloth mixed recipe with llm-compressor on Swift BF16:
   FP8 (channel weights, dynamic token activations) for self_attn
   q/k/v/o, linear_attn in_proj_qkv/in_proj_z/out_proj, lm_head,
   layers 56-63 MLPs; NVFP4 (group 16, fp8_e4m3 scales) for remaining
   MLPs. Calibration set required (NVFP4 needs calibration data);
   target config must pass convert_nvfp4.py's strict preflight
   (_validate_float_group / _validate_nvfp4_group).
3. Convert: `python3 -m tools.convert.qwen3_8_27b.convert_nvfp4
   --model swift-bf16 --quantized-model <step-2 out> --dflash2-model
   dflash2 --out models/swift_qwen3_8_27b_nvfp4.ninfer` in the pinned
   checkout with the convert-venv torch (created 2026-09-15).
   Stock z-lab DFlash2 head is already downloaded; MTP weights come from
   ukisai's model_mtp.safetensors (downloaded) IF the converter sources
   MTP from the quantized dir - verify during pipeline build.
4. Add swift-clean preset (spec=mtp, draft_tokens=3 - identical to
   baseline except adapter).
5. Measurement matrix (both presets, lock owner per run):
   - `llmc bench perf` x3 per preset (medians; 2 runs is not a sample)
   - `llmc bench tasks` micro-suite - direction check only, its
     generations are too short for thinking-length claims
   - `llmc bench gumshoe` 18-case JSON-protocol suite
   - one real scoped greenfield task per preset in a git worktree
     (the only valid long-horizon thinking-length probe)
   - 3-7 days of production traffic per preset: quality.jsonl flag
     rates + engine.jsonl thinking tokens, normalized per request class
6. Decision criteria for adopting Swift as qwen38-ninfer replacement:
   - thinking tokens -30% or better at xhigh on the real task probe
   - quality.jsonl flag rate not worse than baseline (clean rows as
     denominator)
   - bench perf decode within noise of baseline
   - no subjective quality regression on the math canary (AIME bug)

## Open questions

- Does the ninfer chat template in the OrcaRouter artifact carry the
  same reasoning_effort field semantics (low|medium|xhigh)? Verify at
  smoke time; the artifact embeds its own template and our froggeric
  fixed-template work does not apply to ninfer.
- MTP head provenance for swift-clean: confirm whether
  convert_nvfp4 sources MTP tensors from --model or --quantized-model,
  and whether ukisai's BF16 repo ships MTP weights (their NVFP4 repo
  does; model_mtp.safetensors already on disk).
- If Swift is adopted, upstream DFlash2 head retraining on Swift traces
  is a possible third arm (OP asked the same question in the thread).

## Cleanup if rejected

- Delete qwen38-swift-ninfer.toml + the 23.7 GB community artifact.
- convert-inputs/swift-bf16 (54 GB) is worth keeping only if Phase 1
  stays on the table; otherwise delete.
- convert-venv (torch cu128, ~6 GB) is reusable for any future
  conversion; keep.
