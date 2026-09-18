#!/usr/bin/env bash
# Phase 0 step-4 behavioral battery: 5 prompts x 2 efforts, run against
# whatever preset is currently loaded. Appends one JSON line per run.
# Usage: phase0-battery.sh <preset-name>   (caller switches preset first)
set -u
PRESET="$1"
OUT=~/infra/ai/llmc/bench/results/phase0-$(date +%Y%m%d-%H%M%S).jsonl
DOC=$(cat /tmp/needle_doc.txt)

fire() { # $1=prompt_id $2=effort $3=json_body
  local id="$1" eff="$2" body="$3" t0 t1 resp
  t0=$(date +%s.%N)
  resp=$(curl -s -m 1800 http://localhost:11434/v1/chat/completions \
    -H 'Content-Type: application/json' -d "$body")
  t1=$(date +%s.%N)
  echo "$resp" | jq -c --arg p "$PRESET" --arg id "$id" --arg e "$eff" --argjson wall "$(echo "$t1 - $t0" | bc)" \
    '{preset:$p, prompt:$id, effort:$e, wall_s:($wall*10|round/10),
      rlen: (.choices[0].message.reasoning_content|length),
      finish: .choices[0].finish_reason,
      completion: .usage.completion_tokens, reasoning: .usage.completion_tokens_details.reasoning_tokens,
      prompt_tok: .usage.prompt_tokens, tok_s: .timings.predicted_per_second,
      answer_tail: (.choices[0].message.content // "" | .[-200:]),
      tool_calls: (.choices[0].message.tool_calls // [] | map({name:.function.name, args:.function.arguments}))}' \
    >> "$OUT" || echo "{\"preset\":\"$PRESET\",\"prompt\":\"$id\",\"effort\":\"$eff\",\"error\":\"jq/parse fail\"}" >> "$OUT"
  echo "done: $PRESET $id $eff"
}

for EFF in medium xhigh; do
  # 1. trivial - does it still overthink small asks
  fire trivial "$EFF" "$(jq -nc --arg e "$EFF" '{model:"auto", reasoning_effort:$e, max_tokens:32768,
    messages:[{role:"user",content:"What is the capital of France? Answer in one word."}]}')"

  # 2. hard math - AIME 2024 I P1 (answer 73), the AIME regression canary
  fire math_aime "$EFF" "$(jq -nc --arg e "$EFF" '{model:"auto", reasoning_effort:$e, max_tokens:65536,
    messages:[{role:"user",content:"Among the 900 residents of Aimeville, there are 195 who own a diamond ring, 367 who own a set of golf clubs, and 562 who own a garden spade. In addition, each of the 900 residents owns a bag of candy hearts. There are 437 residents who own exactly two of these things, and 234 residents who own exactly three of these things. Find the number of residents of Aimeville who own all four of these things."}]}')"

  # 3. tool-call shape - multi-file code edit with tools
  fire code_tools "$EFF" "$(jq -nc --arg e "$EFF" '{model:"auto", reasoning_effort:$e, max_tokens:32768,
    messages:[{role:"user",content:"The tests fail with: AssertionError: expected total_price(3, 19.99) == 59.97 but got 59.96999999999999. The function is in src/shop/cart.py. Read the file, fix the float bug (use Decimal or integer cents), then run the tests."}],
    tools:[
      {type:"function",function:{name:"read",description:"Read a file",parameters:{type:"object",properties:{path:{type:"string"}},required:["path"]}}},
      {type:"function",function:{name:"edit",description:"Replace oldText with newText in a file",parameters:{type:"object",properties:{path:{type:"string"},oldText:{type:"string"},newText:{type:"string"}},required:["path","oldText","newText"]}}},
      {type:"function",function:{name:"bash",description:"Run a shell command",parameters:{type:"object",properties:{command:{type:"string"}},required:["command"]}}}
    ], tool_choice:"auto"}')"

  # 4. long-context recall - 3 needles in ~13k tokens
  fire needle "$EFF" "$(jq -nc --arg e "$EFF" --arg doc "$DOC" '{model:"auto", reasoning_effort:$e, max_tokens:16384,
    messages:[{role:"user",content:("Below is an operations log. Answer precisely: (1) What is the alpha-gate calibration code? (2) Where does Dr. Ilves keep the spare key? (3) When does the satellite uplink window open?\n\n" + $doc)}]}')"

  # 5. debugging agentic task - root cause from traceback
  fire debug "$EFF" "$(jq -nc --arg e "$EFF" '{model:"auto", reasoning_effort:$e, max_tokens:32768,
    messages:[{role:"user",content:"This cron job worked for months and started failing after we moved it to a new server. Code:\n\nimport csv, sys\nwith open(sys.argv[1]) as f:\n    rows = list(csv.DictReader(f))\ntotal = sum(float(r[\"amount\"]) for r in rows)\nprint(f\"total={total:.2f}\")\n\nError on new server:\n\nTraceback (most recent call last):\n  File \"etl.py\", line 4, in <module>\n    rows = list(csv.DictReader(f))\n           ^^^^^^^^^^^^^^^^^^^^^\n  File \"/usr/lib/python3.12/csv.py\", line 112, in __next__\n    row = next(self.reader)\n          ^^^^^^^^^^^^^^^^\n_csv.Error: field larger than field size limit (131072)\n\nDiagnose the root cause and give the fix. Be specific about why it only broke after the server move."}]}')"
done
echo "BATTERY COMPLETE: $PRESET -> $OUT"
