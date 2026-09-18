#!/usr/bin/env bash
# Phase 0 hard probes: 3 spiral-territory prompts, xhigh, CaptainArni's
# sampling regime (temp 1.0 / top_p 0.95 / top_k 20). Caller switches preset.
# Usage: phase0-hard.sh <preset-name>
set -u
PRESET="$1"
OUT=~/infra/ai/llmc/bench/results/phase0-hard-$(date +%Y%m%d-%H%M%S).jsonl

fire() {
  local id="$1" body="$2" t0 t1 resp
  t0=$(date +%s.%N)
  resp=$(curl -s -m 2400 http://localhost:11434/v1/chat/completions \
    -H 'Content-Type: application/json' -d "$body")
  t1=$(date +%s.%N)
  echo "$resp" | jq -c --arg p "$PRESET" --arg id "$id" --argjson wall "$(echo "$t1 - $t0" | bc)" \
    '{preset:$p, prompt:$id, wall_s:($wall*10|round/10),
      rlen: (.choices[0].message.reasoning_content|length),
      finish: .choices[0].finish_reason,
      completion: .usage.completion_tokens, tok_s: .timings.predicted_per_second,
      answer_tail: (.choices[0].message.content // "" | .[-300:])}' >> "$OUT"
  echo "done: $PRESET $id"
}

for r in 1 2; do   # 2 seeds: sampled decoding varies run to run
  fire combinatorics_r$r "$(jq -nc '{model:"auto", reasoning_effort:"xhigh", temperature:1.0, top_p:0.95, top_k:20, max_tokens:32768,
    messages:[{role:"user",content:"Let N = 20!. Find the number of ordered pairs (a, b) of positive integers with a < b, gcd(a, b) = 1, and a * b = N. Justify your answer carefully."}]}')"

  fire horses_r$r "$(jq -nc '{model:"auto", reasoning_effort:"xhigh", temperature:1.0, top_p:0.95, top_k:20, max_tokens:32768,
    messages:[{role:"user",content:"You have 25 horses and a track that fits 5 horses per race. You have no stopwatch; you can only observe the finishing order of each race. What is the minimum number of races needed to determine the 3 fastest horses, and what is the exact race schedule? Prove minimality."}]}')"

  fire locale_debug_r$r "$(jq -nc '{model:"auto", reasoning_effort:"xhigh", temperature:1.0, top_p:0.95, top_k:20, max_tokens:32768,
    messages:[{role:"user",content:"A C tool has run in CI for a year. It reads a config file containing lines like `threshold=3.14` and parses with strtod(). After migrating CI to a new container image, thresholds above 1 are silently read as their integer part (3.14 becomes 3.0). The code did not change. The old image was based on Debian bullseye, the new one on Alpine edge. Source:\n\nchar line[256];\nwhile (fgets(line, sizeof line, fp)) {\n    char *eq = strchr(line, 61);\n    if (eq) values[n++] = strtod(eq + 1, NULL);\n}\n\nDiagnose the root cause and give the fix. Explain why the base image change triggered it."}]}')"
done
echo "HARD BATTERY COMPLETE: $PRESET -> $OUT"
