package proxy

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testLogger returns a logger writing to a temp file plus a reader for the
// records written once the logger is closed.
func testLogger(t *testing.T) (*QualityLogger, func() []QualityRecord) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "quality.jsonl")
	l := NewQualityLogger(path, nil)
	if l == nil {
		t.Fatal("logger disabled unexpectedly")
	}
	return l, func() []QualityRecord {
		l.Close()
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("open quality log: %v", err)
		}
		defer f.Close()
		var recs []QualityRecord
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var r QualityRecord
			if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
				t.Fatalf("bad record line %q: %v", sc.Text(), err)
			}
			recs = append(recs, r)
		}
		return recs
	}
}

func observeAll(o *qualityObserver, chunks []string, stream bool) {
	for _, c := range chunks {
		o.Observe([]byte(c), stream)
	}
}

func TestQualityCleanStreamToolCall(t *testing.T) {
	l, read := testLogger(t)
	o := newQualityObserver(l, "qwen", "qwen38-ninfer", "/v1/chat/completions", 3)
	observeAll(o, []string{
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"path\\\"\"}}]}}]}\n",
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\":\\\"/tmp/x\\\"}\"}}]}}]}\n",
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n",
		"data: [DONE]\n",
	}, true)
	o.finish(200, 120*time.Millisecond, "")
	recs := read()
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	r := recs[0]
	if r.ToolCalls != 1 || r.ArgsBad != 0 || r.Leak != 0 || r.Truncated || r.Unclosed != 0 {
		t.Fatalf("clean stream flagged: %+v", r)
	}
	if r.Finish != "tool_calls" || r.Tools != 3 || r.Model != "qwen" || r.Preset != "qwen38-ninfer" || r.Status != 200 || !r.Stream {
		t.Fatalf("record metadata wrong: %+v", r)
	}
}

func TestQualityToolCallLeakInContent(t *testing.T) {
	l, read := testLogger(t)
	o := newQualityObserver(l, "m", "p", "/v1/chat/completions", 1)
	// Tag split across two deltas must still count exactly once.
	observeAll(o, []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"sure, calling <tool_\"}}]}\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"call>{\\\"x\\\":1}\"}}]}\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"</tool_call> done\"}}]}\n",
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n",
	}, true)
	o.finish(200, time.Millisecond, "")
	r := read()[0]
	if r.Leak != 1 {
		t.Fatalf("leak = %d, want 1", r.Leak)
	}
	if r.Unclosed != 0 {
		t.Fatalf("unclosed = %d, want 0 (open and close both present)", r.Unclosed)
	}
}

func TestQualityUnclosedToolTag(t *testing.T) {
	l, read := testLogger(t)
	o := newQualityObserver(l, "m", "p", "/v1/chat/completions", 1)
	observeAll(o, []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"<tool_call>{\\\"x\\\":1\"}}]}\n",
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n",
	}, true)
	o.finish(200, time.Millisecond, "")
	r := read()[0]
	if r.Leak != 1 || r.Unclosed != 1 {
		t.Fatalf("leak=%d unclosed=%d, want 1/1", r.Leak, r.Unclosed)
	}
}

func TestQualityInvalidArgsJSON(t *testing.T) {
	l, read := testLogger(t)
	o := newQualityObserver(l, "m", "p", "/v1/chat/completions", 2)
	observeAll(o, []string{
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"edit\",\"arguments\":\"{\\\"path\\\":\"}}]}}]}\n",
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"function\":{\"name\":\"noop\",\"arguments\":\"{}\"}}]}}]}\n",
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n",
	}, true)
	o.finish(200, time.Millisecond, "")
	r := read()[0]
	if r.ArgsBad != 1 {
		t.Fatalf("args_bad = %d, want 1 (only the truncated call)", r.ArgsBad)
	}
	if !r.Truncated {
		t.Fatal("finish_reason=length not flagged as truncated")
	}
	if r.ToolCalls != 2 {
		t.Fatalf("tool_calls = %d, want 2", r.ToolCalls)
	}
}

func TestQualityNamedCallNoArgsIsBad(t *testing.T) {
	l, read := testLogger(t)
	o := newQualityObserver(l, "m", "p", "/v1/chat/completions", 1)
	observeAll(o, []string{
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"bash\"}}]}}]}\n",
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n",
	}, true)
	o.finish(200, time.Millisecond, "")
	r := read()[0]
	if r.ArgsBad != 1 {
		t.Fatalf("args_bad = %d, want 1 for named call with empty arguments", r.ArgsBad)
	}
}

func TestQualityNonStreamBody(t *testing.T) {
	l, read := testLogger(t)
	o := newQualityObserver(l, "m", "p", "/v1/chat/completions", 1)
	body := `{"choices":[{"message":{"content":"ok","tool_calls":[{"function":{"name":"read","arguments":"{\"path\":\"/a\"}"}},{"function":{"name":"write","arguments":"{broken"}}]},"finish_reason":"tool_calls"}]}`
	o.Observe([]byte(body), false)
	o.finish(200, time.Millisecond, "")
	r := read()[0]
	if r.Stream {
		t.Fatal("non-stream flagged as stream")
	}
	if r.ToolCalls != 2 || r.ArgsBad != 1 {
		t.Fatalf("tool_calls=%d args_bad=%d, want 2/1", r.ToolCalls, r.ArgsBad)
	}
}

func TestQualityMidstreamDeathStillEmits(t *testing.T) {
	l, read := testLogger(t)
	o := newQualityObserver(l, "m", "p", "/v1/chat/completions", 1)
	observeAll(o, []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n",
	}, true)
	// No finish chunk; stream dies.
	o.finish(502, 3*time.Second, " upstream_died_midstream")
	r := read()[0]
	if r.Status != 502 || r.Note != " upstream_died_midstream" || r.Finish != "" {
		t.Fatalf("midstream death record wrong: %+v", r)
	}
}

func TestPeekToolCount(t *testing.T) {
	if n := peekToolCount([]byte(`{"model":"m","messages":[],"tools":[{"type":"function"},{"type":"function"}]}`)); n != 2 {
		t.Fatalf("tools = %d, want 2", n)
	}
	if n := peekToolCount([]byte(`{"model":"m","messages":[]}`)); n != 0 {
		t.Fatalf("tools = %d, want 0", n)
	}
	if n := peekToolCount([]byte("not json")); n != 0 {
		t.Fatalf("tools = %d, want 0 for garbage", n)
	}
	if n := peekToolCount(nil); n != 0 {
		t.Fatalf("tools = %d, want 0 for nil body", n)
	}
}

func TestQualityLoggerDisabled(t *testing.T) {
	if l := NewQualityLogger("off", nil); l != nil {
		t.Fatal("off should disable")
	}
	if l := NewQualityLogger("", nil); l != nil {
		t.Fatal("empty should disable")
	}
	if l := NewQualityLogger(filepath.Join(t.TempDir(), "no", "such", "dir", "q.jsonl"), nil); l != nil {
		t.Fatal("unopenable path should disable, not panic")
	}
}

func TestQualityLoggerDropsOnFullChannel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// Construct a logger with a tiny channel and no running writer so the
	// buffer fills deterministically.
	l := &QualityLogger{ch: make(chan QualityRecord, 2), done: make(chan struct{}), f: f}
	for i := 0; i < 10; i++ {
		l.Emit(QualityRecord{TS: int64(i)})
	}
	if d := l.Dropped(); d != 8 {
		t.Fatalf("dropped = %d, want 8", d)
	}
	f.Close()
}

func TestQualityArgsOverflowSkipsValidation(t *testing.T) {
	l, read := testLogger(t)
	o := newQualityObserver(l, "m", "p", "/v1/chat/completions", 1)
	o.addArgs(0, strings.Repeat("a", argsCap)) // exactly at cap
	o.addArgs(0, "b")                          // pushes past the cap
	if !o.overflow {
		t.Fatal("overflow not set past argsCap")
	}
	o.finish(200, time.Millisecond, "")
	r := read()[0]
	if !r.ArgsOverflow || r.ArgsBad != 0 {
		t.Fatalf("overflow=%v args_bad=%d, want true/0", r.ArgsOverflow, r.ArgsBad)
	}
}
