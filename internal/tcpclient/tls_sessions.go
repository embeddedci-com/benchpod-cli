package tcpclient

import (
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The pod's TLS session tickets, kept between runs of the CLI in tls_sessions.json next to the
// pins (mode 0600). Each benchpod run is a new process; without this every run pays the pod's full
// handshake (~0.46 s of ECDHE + ECDSA on the pod) instead of resuming (~30 ms). A ticket holds the
// session's secret, so the file is as private as the sign-in token next to it. Entries older than
// the pod's ticket lifetime are dropped.

const ticketLifetime = time.Hour // the pod's ALTCP_MBEDTLS_SESSION_TICKET_TIMEOUT_SECONDS

type savedSession struct {
	Ticket string `json:"ticket"` // base64
	State  string `json:"state"`  // base64 of tls.SessionState.Bytes
	Saved  string `json:"saved"`  // RFC 3339
}

type fileSessionCache struct {
	mu  sync.Mutex
	mem tls.ClientSessionCache
}

var sessionCache tls.ClientSessionCache = &fileSessionCache{mem: tls.NewLRUClientSessionCache(32)}

func sessionsPath() (string, error) {
	p, err := pinPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(p), "tls_sessions.json"), nil
}

func (c *fileSessionCache) load() (map[string]savedSession, string) {
	path, err := sessionsPath()
	if err != nil {
		return nil, ""
	}
	m := map[string]savedSession{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &m)
	}
	return m, path
}

func (c *fileSessionCache) save(m map[string]savedSession, path string) {
	if path == "" {
		return
	}
	for k, s := range m {
		if t, err := time.Parse(time.RFC3339, s.Saved); err != nil || time.Since(t) > ticketLifetime {
			delete(m, k)
		}
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil || os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tls_sessions-*")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())
	_ = tmp.Chmod(0o600)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return
	}
	if tmp.Close() == nil {
		_ = os.Rename(tmp.Name(), path)
	}
}

func (c *fileSessionCache) Get(key string) (*tls.ClientSessionState, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cs, ok := c.mem.Get(key); ok {
		return cs, true
	}
	m, _ := c.load()
	s, ok := m[key]
	if !ok {
		return nil, false
	}
	if t, err := time.Parse(time.RFC3339, s.Saved); err != nil || time.Since(t) > ticketLifetime {
		return nil, false
	}
	ticket, err1 := base64.StdEncoding.DecodeString(s.Ticket)
	raw, err2 := base64.StdEncoding.DecodeString(s.State)
	if err := errors.Join(err1, err2); err != nil {
		return nil, false
	}
	st, err := tls.ParseSessionState(raw)
	if err != nil {
		return nil, false
	}
	cs, err := tls.NewResumptionState(ticket, st)
	if err != nil {
		return nil, false
	}
	c.mem.Put(key, cs)
	return cs, true
}

func (c *fileSessionCache) Put(key string, cs *tls.ClientSessionState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mem.Put(key, cs)
	m, path := c.load()
	if cs == nil {
		if _, ok := m[key]; !ok {
			return
		}
		delete(m, key)
		c.save(m, path)
		return
	}
	ticket, st, err := cs.ResumptionState()
	if err != nil || st == nil {
		return
	}
	raw, err := st.Bytes()
	if err != nil {
		return
	}
	m[key] = savedSession{Ticket: base64.StdEncoding.EncodeToString(ticket),
		State: base64.StdEncoding.EncodeToString(raw), Saved: time.Now().UTC().Format(time.RFC3339)}
	c.save(m, path)
}

// forgetSessions drops every saved session (a pin changed: start with a full handshake).
func forgetSessions() {
	if c, ok := sessionCache.(*fileSessionCache); ok {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.mem = tls.NewLRUClientSessionCache(32)
		if path, err := sessionsPath(); err == nil {
			_ = os.Remove(path)
		}
	}
}
