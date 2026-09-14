// Quality telemetry: a per-request canary for tool-call corruption on the
// llm path. Tool-call markup leaking into content, invalid JSON in
// tool_calls arguments, and output-cap truncation are the three failure
// shapes an agent harness actually trips on, and they are the first place
// a quant/engine quality regression shows up in production traffic (the
// bench suite only samples six short-horizon tasks on demand).
//
// One JSONL record per llm request that declared tools, appended to
// /state/quality.jsonl (LLMC_QUALITY_FILE; "off" disables). Records for
// clean requests are the denominator - a failure RATE is computable only
// because every tool-bearing request is logged, not just the bad ones.
//
// Failure isolation mirrors the usage-telemetry spec (R5): the request
// path does a non-blocking send into a buffered channel; a dedicated
// goroutine owns the file. A full channel drops the record (and counts
// the drop) rather than ever slowing a stream.
package proxy

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// QualityRecord is one line in quality.jsonl. Zero-valued counters are
// omitted so clean rows stay short; a row with no flag fields at all is a
// clean pass.
type QualityRecord struct {
	TS           int64  `json:"ts"`
	Model        string `json:"model"`          // requested model id (or "-")
	Preset       string `json:"preset"`         // active preset at serve time (or "-")
	Path         string `json:"path"`           // upstream path, e.g. /v1/chat/completions
	Stream       bool   `json:"stream"`         // SSE response
	Status       int    `json:"status"`         // HTTP status the client saw
	DurMs        int64  `json:"dur_ms"`
	Tools        int    `json:"tools"`          // tools declared in the request
	ToolCalls    int    `json:"tool_calls"`     // tool calls the model emitted
	Leak         int    `json:"leak,omitempty"` // <tool_call> markup in content (parser miss)
	Unclosed     int    `json:"unclosed,omitempty"`
	ArgsBad      int    `json:"args_bad,omitempty"`    // tool_calls whose arguments are not valid JSON
	Truncated    bool   `json:"truncated,omitempty"`   // finish_reason == "length"
	ArgsOverflow bool   `json:"args_overflow,omitempty"` // args exceeded the accumulation cap; not validated
	Finish       string `json:"finish,omitempty"`
	Note         string `json:"note,omitempty"` // forwardTo's note (upstream_died_midstream etc.)
}

// QualityLogger appends records to a JSONL file from a single goroutine.
type QualityLogger struct {
	ch      chan QualityRecord
	done    chan struct{}
	f       *os.File
	dropped atomic.Int64
	logf    func(string, ...any)
}

// NewQualityLogger opens path for append and starts the writer goroutine.
// Returns nil (telemetry disabled) when the file cannot be opened - a
// telemetry failure must never affect serving.
func NewQualityLogger(path string, logf func(string, ...any)) *QualityLogger {
	if path == "" || path == "off" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		if logf != nil {
			logf("quality log disabled: open %s: %v", path, err)
		}
		return nil
	}
	l := &QualityLogger{
		ch:   make(chan QualityRecord, 1024),
		done: make(chan struct{}),
		f:    f,
		logf: logf,
	}
	go l.run()
	return l
}

func (l *QualityLogger) run() {
	defer close(l.done)
	for rec := range l.ch {
		line, err := json.Marshal(rec)
		if err != nil {
			continue
		}
		line = append(line, '\n')
		// Single Write of a full line with O_APPEND: one syscall per
		// record, so a crash loses at most the in-flight line and never
		// leaves a partial line.
		if _, err := l.f.Write(line); err != nil && l.logf != nil {
			l.logf("quality log write failed: %v", err)
		}
	}
}

// Emit enqueues a record without blocking; a full channel drops it.
func (l *QualityLogger) Emit(rec QualityRecord) {
	select {
	case l.ch <- rec:
	default:
		if d := l.dropped.Add(1); d%100 == 1 && l.logf != nil {
			l.logf("quality log: %d records dropped (channel full)", d)
		}
	}
}

// Close drains the channel and closes the file.
func (l *QualityLogger) Close() {
	close(l.ch)
	<-l.done
	l.f.Close()
}

// Dropped reports how many records were dropped for a full channel.
func (l *QualityLogger) Dropped() int64 { return l.dropped.Load() }

const (
	toolCallOpen  = "<tool_call>"
	toolCallClose = "</tool_call>"
	// tagCarry is len("<tool_call>") - 1: the longest prefix of either tag
	// that can be split across two deltas. Both tags are longer than the
	// carry, so a tag is always counted exactly once by the scan that
	// completes it.
	tagCarry = 10
	// argsCap bounds per-request accumulation of tool-call argument text.
	// Past it, validation is skipped (recorded as args_overflow) rather
	// than buffering unboundedly on a pathological stream.
	argsCap = 4 << 20
	// bodyCap bounds non-stream response buffering.
	bodyCap = 4 << 20
)

// qualityObserver accumulates just enough of one response to score the
// corruption signals: tool_calls argument text (to JSON-validate at end),
// a short content tail (to count tag leakage across chunk boundaries), and
// the finish reason. It never retains the full completion.
type qualityObserver struct {
	logger *QualityLogger
	rec    QualityRecord

	sseBuf   []byte // partial SSE line across reads
	body     []byte // non-stream response, capped at bodyCap
	bodyFull bool

	args     map[int]*strings.Builder // tool_calls index -> arguments text
	argNames map[int]string
	argBytes int
	overflow bool

	carry string // last tagCarry bytes of content, for split-tag counting
	opens int
	closes int
}

// peekToolCount reports how many tools the request declared. Access only;
// routing never depends on it. Requests without tools get no observer.
func peekToolCount(body []byte) int {
	if len(body) == 0 {
		return 0
	}
	var v struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if json.Unmarshal(body, &v) != nil {
		return 0
	}
	return len(v.Tools)
}

func newQualityObserver(l *QualityLogger, model, preset, path string, tools int) *qualityObserver {
	return &qualityObserver{
		logger: l,
		rec: QualityRecord{
			TS: time.Now().Unix(), Model: orDash(model), Preset: orDash(preset),
			Path: path, Tools: tools,
		},
		args:     map[int]*strings.Builder{},
		argNames: map[int]string{},
	}
}

// Observe consumes one chunk of the upstream response body. stream selects
// the SSE parser; otherwise bytes are buffered for a final parse.
func (o *qualityObserver) Observe(p []byte, stream bool) {
	if stream {
		o.rec.Stream = true
		o.sseBuf = append(o.sseBuf, p...)
		for {
			i := bytes.IndexByte(o.sseBuf, '\n')
			if i < 0 {
				break
			}
			line := bytes.TrimRight(o.sseBuf[:i], "\r")
			o.sseBuf = o.sseBuf[i+1:]
			o.sseLine(line)
		}
		return
	}
	if o.bodyFull {
		return
	}
	if len(o.body)+len(p) > bodyCap {
		o.body = append(o.body, p[:bodyCap-len(o.body)]...)
		o.bodyFull = true
		return
	}
	o.body = append(o.body, p...)
}

func (o *qualityObserver) sseLine(line []byte) {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(line[len("data:"):])
	if bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Index    *int `json:"index"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(payload, &chunk) != nil {
		return
	}
	for _, c := range chunk.Choices {
		o.scanContent(c.Delta.Content)
		for _, tc := range c.Delta.ToolCalls {
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			}
			if tc.Function.Name != "" {
				o.argNames[idx] = tc.Function.Name
			}
			o.addArgs(idx, tc.Function.Arguments)
		}
		if c.FinishReason != nil && *c.FinishReason != "" {
			o.rec.Finish = *c.FinishReason
		}
	}
}

// scanContent counts tool-call markup in streamed content. Deltas can split
// a tag anywhere, so counting runs over carry+delta; because both tags are
// longer than the carry, a tag is counted exactly once.
func (o *qualityObserver) scanContent(s string) {
	if s == "" {
		return
	}
	window := o.carry + s
	o.opens += strings.Count(window, toolCallOpen)
	o.closes += strings.Count(window, toolCallClose)
	if len(window) > tagCarry {
		o.carry = window[len(window)-tagCarry:]
	} else {
		o.carry = window
	}
}

func (o *qualityObserver) addArgs(idx int, s string) {
	if s == "" || o.overflow {
		return
	}
	if o.argBytes+len(s) > argsCap {
		o.overflow = true
		return
	}
	b := o.args[idx]
	if b == nil {
		b = &strings.Builder{}
		o.args[idx] = b
	}
	b.WriteString(s)
	o.argBytes += len(s)
}

// finish scores the accumulated state and emits the record. Safe to call
// once; forwardTo calls it from a defer so mid-stream failures still emit.
func (o *qualityObserver) finish(status int, dur time.Duration, note string) {
	if !o.rec.Stream {
		o.parseBody()
	}
	rec := o.rec
	rec.Status = status
	rec.DurMs = dur.Milliseconds()
	rec.Note = note
	seen := map[int]bool{}
	for idx := range o.args {
		seen[idx] = true
	}
	for idx := range o.argNames {
		seen[idx] = true
	}
	rec.ToolCalls = len(seen)
	rec.Leak = o.opens
	if o.opens > o.closes {
		rec.Unclosed = o.opens - o.closes
	}
	if rec.Finish == "length" {
		rec.Truncated = true
	}
	if o.overflow {
		rec.ArgsOverflow = true
	} else {
		for idx := range seen {
			var args string
			if b := o.args[idx]; b != nil {
				args = b.String()
			}
			// A named call with no arguments at all is malformed; "{}" is
			// the legitimate zero-argument form.
			if _, named := o.argNames[idx]; args == "" && named {
				rec.ArgsBad++
				continue
			}
			if args != "" && !json.Valid([]byte(args)) {
				rec.ArgsBad++
			}
		}
	}
	o.logger.Emit(rec)
}

// parseBody handles non-stream completions: the whole response is one JSON
// object with message.content / message.tool_calls.
func (o *qualityObserver) parseBody() {
	if len(o.body) == 0 {
		return
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(o.body, &resp) != nil {
		return
	}
	for i, c := range resp.Choices {
		o.scanContent(c.Message.Content)
		for j, tc := range c.Message.ToolCalls {
			idx := i*1000 + j // non-stream tool_calls carry no index; any stable key works
			if tc.Function.Name != "" {
				o.argNames[idx] = tc.Function.Name
			}
			o.addArgs(idx, tc.Function.Arguments)
		}
		if c.FinishReason != "" {
			o.rec.Finish = c.FinishReason
		}
	}
}
