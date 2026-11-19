package ci

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Trace caps (spec section 3).
const (
	MaxInlineJSON  = 4 << 10  // req/resp larger than this go to att/
	MaxTraceBytes  = 50 << 20 // trace + att per run
	traceDirName   = "trace"
	attDirName     = "att"
	redactedMarker = "<redacted>"
)

// TraceLine is one line of a trace file.
type TraceLine struct {
	T        float64 `json:"t"`
	Op       string  `json:"op"`
	Cmd      string  `json:"cmd,omitempty"`
	Mode     string  `json:"mode,omitempty"`
	Req      any     `json:"req,omitempty"`
	MS       float64 `json:"ms"`
	OK       bool    `json:"ok"`
	Resp     any     `json:"resp,omitempty"`
	Error    string  `json:"error,omitempty"`
	N        *int    `json:"n,omitempty"`
	BytesIn  *int64  `json:"bytes_in,omitempty"`
	BytesOut *int64  `json:"bytes_out,omitempty"`
	Att      string  `json:"att,omitempty"`
}

// Recorder writes the trace of the pod calls the CLI makes itself (native steps), one case at a
// time, in the same NDJSON format the SDK uses, and keeps every case's call statistics.
type Recorder struct {
	mu        sync.Mutex
	dir       string // the run folder
	t0        time.Time
	keep      string // all | failed | none
	written   int64
	truncated bool
	attN      int
	cur       *caseTrace
}

type caseTrace struct {
	lines []TraceLine
	atts  map[string][]byte
	ms    map[string][]float64
	errs  map[string]int
}

// NewRecorder records into runDir; t0 is the run start (trace t is seconds since it).
func NewRecorder(runDir string, t0 time.Time, keep string) *Recorder {
	return &Recorder{dir: runDir, t0: t0, keep: keep}
}

// Begin starts recording a new case; calls before End belong to it.
func (r *Recorder) Begin() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cur = &caseTrace{atts: map[string][]byte{}, ms: map[string][]float64{}, errs: map[string]int{}}
}

// Truncated says the run's trace hit its size cap.
func (r *Recorder) Truncated() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.truncated
}

// Record adds one call to the current case. op is command, samples, stream, raw, status or
// power; req and resp are anything JSON (resp may be json.RawMessage); err is the call's error.
func (r *Recorder) Record(op, cmd string, req any, resp any, took time.Duration, err error) {
	r.RecordLine(TraceLine{Op: op, Cmd: cmd, Req: req, Resp: resp}, took, err)
}

// RecordLine is Record with the extra fields (mode, n, bytes) filled in by the caller.
func (r *Recorder) RecordLine(l TraceLine, took time.Duration, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == nil {
		r.cur = &caseTrace{atts: map[string][]byte{}, ms: map[string][]float64{}, errs: map[string]int{}}
	}
	ms := round3(float64(took) / float64(time.Millisecond))
	l.T = round3(time.Since(r.t0).Seconds())
	l.MS = ms
	l.OK = err == nil
	if err != nil {
		l.Error = Cap(err.Error(), 2048)
	}
	name := l.Cmd
	if name == "" {
		name = l.Op
		if l.Mode != "" {
			name = l.Op + ":" + l.Mode
		}
	}
	r.cur.ms[name] = append(r.cur.ms[name], ms)
	if err != nil {
		r.cur.errs[name]++
	}

	l.Req = Redact(toGeneric(l.Req))
	l.Resp = Redact(toGeneric(l.Resp))
	if r.truncated {
		l = TraceLine{T: l.T, Op: l.Op, Cmd: l.Cmd, MS: l.MS, OK: l.OK}
	} else {
		reqJSON, _ := json.Marshal(l.Req)
		respJSON, _ := json.Marshal(l.Resp)
		if len(reqJSON) > MaxInlineJSON || len(respJSON) > MaxInlineJSON {
			r.attN++
			att := fmt.Sprintf("%s/%04d-%s.json", attDirName, r.attN, safeName(name))
			body, _ := json.Marshal(map[string]any{"req": l.Req, "resp": l.Resp})
			r.cur.atts[att] = body
			r.written += int64(len(body))
			l.Req, l.Resp, l.Att = nil, nil, att
		}
		line, _ := json.Marshal(l)
		r.written += int64(len(line)) + 1
		if r.written > MaxTraceBytes {
			r.truncated = true
			l = TraceLine{T: l.T, Op: l.Op, Cmd: l.Cmd, MS: l.MS, OK: l.OK}
		}
	}
	r.cur.lines = append(r.cur.lines, l)
}

// End finishes the current case: it sets c.Calls, and writes the trace (and its attachments)
// when the keep policy keeps it for this outcome, setting c.Trace to its path in the run folder.
func (r *Recorder) End(c *Case) error {
	r.mu.Lock()
	cur := r.cur
	r.cur = nil
	r.mu.Unlock()
	c.Calls = map[string]CallStats{}
	if cur == nil {
		return nil
	}
	for name, ms := range cur.ms {
		c.Calls[name] = callStats(ms, cur.errs[name])
	}
	keep := r.keep == "all" || (r.keep == "failed" && c.Failing())
	if !keep || len(cur.lines) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(r.dir, traceDirName), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	for _, l := range cur.lines {
		line, err := json.Marshal(l)
		if err != nil {
			continue
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	rel := traceDirName + "/" + CaseSlug(c.ID)
	if c.Attempt > 1 {
		rel += fmt.Sprintf("-attempt%d", c.Attempt)
	}
	rel += ".ndjson"
	if err := os.WriteFile(filepath.Join(r.dir, filepath.FromSlash(rel)), []byte(b.String()), 0o644); err != nil {
		return err
	}
	for name, body := range cur.atts {
		if err := os.MkdirAll(filepath.Join(r.dir, attDirName), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(r.dir, filepath.FromSlash(name)), body, 0o644); err != nil {
			return err
		}
	}
	c.Trace = rel
	return nil
}

func callStats(ms []float64, errs int) CallStats {
	s := append([]float64(nil), ms...)
	sort.Float64s(s)
	return CallStats{N: len(s), P50MS: percentile(s, 0.50), P95MS: percentile(s, 0.95), MaxMS: s[len(s)-1], Errors: errs}
}

// percentile is the nearest-rank percentile of sorted values.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// toGeneric turns v into plain JSON values (maps, slices, numbers, strings), nil stays nil.
func toGeneric(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case json.RawMessage:
		if len(t) == 0 {
			return nil
		}
		var out any
		if json.Unmarshal(t, &out) != nil {
			return string(t)
		}
		return out
	case map[string]any, []any, string, float64, bool:
		return t
	}
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	var out any
	if json.Unmarshal(data, &out) != nil {
		return string(data)
	}
	return out
}

var redactKeys = map[string]bool{
	"password": true, "passwd": true, "psk": true, "token": true, "api_key": true, "apikey": true,
	"secret": true, "authorization": true, "wifi_pass": true,
}

// Redact replaces secrets at any depth (case-insensitive key match): password, passwd, psk, token,
// api_key, apikey, secret, authorization, wifi_pass, and key when its value is a string longer
// than 16 characters.
func Redact(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			lk := strings.ToLower(k)
			switch {
			case redactKeys[lk]:
				out[k] = redactedMarker
			case lk == "key":
				if s, ok := val.(string); ok && len(s) > 16 {
					out[k] = redactedMarker
				} else {
					out[k] = Redact(val)
				}
			default:
				out[k] = Redact(val)
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = Redact(val)
		}
		return out
	}
	return v
}
