package authstore

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func sample() *Tokens {
	return &Tokens{
		AccessToken:      "acc",
		RefreshToken:     "ref",
		AccessExpiresAt:  time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC),
		RefreshExpiresAt: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC),
		SessionID:        "sess-1",
		UserID:           "user-1",
	}
}

func TestDefaultPathPrefersXDGConfigHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	got, err := DefaultPath()
	if err != nil || got != filepath.Join("/xdg", "benchpod-cli", "token.json") {
		t.Fatalf("DefaultPath() = %q, %v", got, err)
	}
}

func TestDefaultPathFallsBackToHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.UserHomeDir reads USERPROFILE on Windows")
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/pod")
	got, err := DefaultPath()
	if err != nil || got != filepath.Join("/home/pod", ".config", "benchpod-cli", "token.json") {
		t.Fatalf("DefaultPath() = %q, %v", got, err)
	}
}

func TestDefaultPathWithoutAHomeIsAnError(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "plan9" {
		t.Skip("home comes from other variables here")
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	if got, err := DefaultPath(); err == nil {
		t.Fatalf("DefaultPath() = %q, want an error", got)
	}
}

func TestSaveThenLoadRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "benchpod-cli", "token.json")
	if err := Save(path, sample()); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if *got != *sample() {
		t.Fatalf("Load = %+v, want %+v", *got, *sample())
	}
}

func TestSaveIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix permission bits on Windows")
	}
	dir := filepath.Join(t.TempDir(), "benchpod-cli")
	path := filepath.Join(dir, "token.json")
	if err := Save(path, sample()); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := st.Mode().Perm(); mode != 0o600 {
		t.Errorf("token file mode = %o, want 600", mode)
	}
	dst, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if mode := dst.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("token directory mode = %o, want no group/other access", mode)
	}
}

func TestSaveReplacesAnExistingFileAndLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")
	if err := os.WriteFile(path, []byte(`{"access_token":"old"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, sample()); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil || got.AccessToken != "acc" {
		t.Fatalf("Load = %+v, %v", got, err)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
			t.Errorf("replaced file mode = %o, want 600", st.Mode().Perm())
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("directory holds %v, want only token.json", names)
	}
}

func TestAFailedSaveKeepsTheOldFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")
	if err := Save(path, sample()); err != nil {
		t.Fatal(err)
	}
	// The target is now a non-empty directory: the rename fails after the temp file was written.
	blocked := filepath.Join(dir, "blocked")
	if err := os.MkdirAll(filepath.Join(blocked, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Save(blocked, sample()); err == nil {
		t.Fatal("Save over a directory succeeded")
	}
	if got, err := Load(path); err != nil || got.AccessToken != "acc" {
		t.Fatalf("old file damaged: %+v, %v", got, err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "token-") {
			t.Errorf("temp file %s left behind", e.Name())
		}
	}
}

func TestSaveAndLoadRejectBadArguments(t *testing.T) {
	if err := Save("", sample()); err == nil {
		t.Error("Save with an empty path succeeded")
	}
	if err := Save(filepath.Join(t.TempDir(), "t.json"), nil); err == nil {
		t.Error("Save of nil tokens succeeded")
	}
	if _, err := Load(""); err == nil {
		t.Error("Load with an empty path succeeded")
	}
}

func TestLoadOfAMissingFileIsErrNotExist(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want os.ErrNotExist (callers branch on it to create a guest)", err)
	}
}

func TestLoadOfACorruptFileNamesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadReadsTheDocumentedFileShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	body := `{"access_token":"a","refresh_token":"r","access_expires_at":"2026-10-08T12:00:00Z",` +
		`"refresh_expires_at":"2026-11-08T12:00:00Z","session_id":"s","user_id":"u","extra":1}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "a" || got.RefreshToken != "r" || got.SessionID != "s" || got.UserID != "u" ||
		!got.AccessExpiresAt.Equal(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("Load = %+v", got)
	}
}

func TestExpiryHonorsTheSkew(t *testing.T) {
	tok := sample()
	exp := tok.AccessExpiresAt
	cases := []struct {
		now  time.Time
		want bool
	}{
		{exp.Add(-time.Hour), false},
		{exp.Add(-expirySkew - time.Second), false},
		{exp.Add(-expirySkew), true}, // inside the margin counts as expired
		{exp, true},
		{exp.Add(time.Hour), true},
	}
	for _, c := range cases {
		if got := tok.AccessExpired(c.now); got != c.want {
			t.Errorf("AccessExpired(%v before expiry) = %v, want %v", exp.Sub(c.now), got, c.want)
		}
	}
	rexp := tok.RefreshExpiresAt
	if tok.RefreshExpired(rexp.Add(-expirySkew-time.Second)) || !tok.RefreshExpired(rexp.Add(-expirySkew)) {
		t.Error("RefreshExpired does not honor the skew")
	}
}

func TestMissingTokensAreExpiredAndMissingExpiryIsNot(t *testing.T) {
	now := time.Now()
	var nilTok *Tokens
	if !nilTok.AccessExpired(now) || !nilTok.RefreshExpired(now) {
		t.Error("nil tokens should be expired")
	}
	if !(&Tokens{}).AccessExpired(now) || !(&Tokens{}).RefreshExpired(now) {
		t.Error("empty tokens should be expired")
	}
	// A token saved without an expiry (the server sent none) is used until the server rejects it.
	noExpiry := &Tokens{AccessToken: "a", RefreshToken: "r"}
	if noExpiry.AccessExpired(now) || noExpiry.RefreshExpired(now) {
		t.Error("a token without an expiry should not count as expired")
	}
}
