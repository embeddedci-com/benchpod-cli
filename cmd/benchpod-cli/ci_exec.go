package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// ciMaxLogBytes caps one step's log file (logs/<step>.log).
const ciMaxLogBytes = 8 << 20

// cappedLog is a step's log file: it stops writing past its cap (with one note), and keeps the
// last 64 KB for the case's log field.
type cappedLog struct {
	mu      sync.Mutex
	f       *os.File
	written int64
	capped  bool
	tail    []byte
}

func openCappedLog(path string) (*cappedLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &cappedLog{f: f}, nil
}

func (l *cappedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tail = append(l.tail, p...)
	if over := len(l.tail) - 64<<10; over > 0 {
		l.tail = append([]byte(nil), l.tail[over:]...)
	}
	if l.f == nil || l.capped {
		return len(p), nil
	}
	if l.written+int64(len(p)) > ciMaxLogBytes {
		l.capped = true
		_, _ = l.f.WriteString("\n[log cut at 8 MB]\n")
		return len(p), nil
	}
	n, err := l.f.Write(p)
	l.written += int64(n)
	if err != nil {
		return n, err
	}
	return len(p), nil
}

// Tail is the last 64 KB written.
func (l *cappedLog) Tail() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return string(l.tail)
}

func (l *cappedLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// childResult is how a child process ended.
type childResult struct {
	ExitCode int  // -1 when it did not exit on its own
	TimedOut bool // the step's deadline stopped it
	Canceled bool // Ctrl-C (or the run's end) stopped it
	Err      error
	Duration time.Duration
}

// runChild runs argv in dir with env, its output going to stdout and stderr, and stops it (its
// whole process group) when ctx ends.
func runChild(ctx context.Context, argv []string, dir string, env []string, stdout, stderr io.Writer) childResult {
	start := time.Now()
	if len(argv) == 0 {
		return childResult{ExitCode: -1, Err: errors.New("empty command")}
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Stdin = nil
	childInGroup(cmd)
	cmd.WaitDelay = 10 * time.Second
	err := cmd.Run()
	if cmd.Process != nil {
		killChildGroup(cmd)
	}
	res := childResult{ExitCode: -1, Duration: time.Since(start)}
	var ee *exec.ExitError
	switch {
	case ctx.Err() != nil:
		res.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
		res.Canceled = !res.TimedOut
		res.Err = ctx.Err()
	case errors.As(err, &ee):
		res.ExitCode = ee.ExitCode()
	case err != nil:
		res.Err = err
	default:
		res.ExitCode = 0
	}
	return res
}

// mergeEnv is base with the overrides applied (later wins); a value of "\x00" removes the name.
func mergeEnv(base []string, overrides ...map[string]string) []string {
	idx := map[string]int{}
	out := make([]string, 0, len(base)+16)
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		if i, ok := idx[name]; ok {
			out[i] = kv
			continue
		}
		idx[name] = len(out)
		out = append(out, kv)
	}
	for _, o := range overrides {
		for _, k := range sortedStrKeys(o) {
			v := o[k]
			if i, ok := idx[k]; ok {
				out[i] = k + "=" + v
				continue
			}
			idx[k] = len(out)
			out = append(out, k+"="+v)
		}
	}
	kept := out[:0]
	for _, kv := range out {
		if strings.HasSuffix(kv, "=\x00") {
			continue
		}
		kept = append(kept, kv)
	}
	return kept
}

func sortedStrKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
