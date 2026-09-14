# Swift-Qwen3.8-27B evaluation plan

2026-09-15. Status: quick assessment in progress; full test pending
self-quantization pipeline.

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
| qwen38-swift-ninfer (quick arm) | OrcaRouter NVFP4 (community, 23.7 GB) | DFlash2 k=7 (stock head, not retrained on Swift) | Swift LoRA |
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
5. Report: thinking-token ratio swift/baseline per effort, flag diff,
   subjective quality notes. Decide go/no-go for Phase 1.

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
