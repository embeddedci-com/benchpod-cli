package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/ci"
)

// ── what a run records about its inputs ─────────────────────────────────────

// gitInfo is the git state of dir: the repository (owner/name from origin), branch, commit and
// whether the tree has changes. Fields stay "" outside a git checkout.
func gitInfo(dir string) ci.Tests {
	git := func(args ...string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	t := ci.Tests{SHA: git("rev-parse", "HEAD")}
	if t.SHA == "" {
		return ci.Tests{}
	}
	t.Ref = git("rev-parse", "--abbrev-ref", "HEAD")
	if t.Ref == "HEAD" { // detached (Actions checks out a commit)
		t.Ref = firstNonEmptyStr(os.Getenv("GITHUB_HEAD_REF"), os.Getenv("GITHUB_REF_NAME"), "")
	}
	t.Dirty = git("status", "--porcelain", "--untracked-files=no") != ""
	t.Repo = repoFromRemote(git("remote", "get-url", "origin"))
	if t.Repo == "" {
		t.Repo = os.Getenv("GITHUB_REPOSITORY")
	}
	return t
}

var remoteRE = regexp.MustCompile(`[:/]([^/:]+/[^/]+?)(?:\.git)?/?$`)

// repoFromRemote is "owner/name" from a git remote URL (https or ssh).
func repoFromRemote(url string) string {
	if m := remoteRE.FindStringSubmatch(strings.TrimSpace(url)); m != nil {
		return m[1]
	}
	return ""
}

// githubInputs is the Actions run, from its environment ("" outside Actions).
func githubInputs() (ci.GitHub, string) {
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		return ci.GitHub{}, "manual"
	}
	g := ci.GitHub{
		RunID:      os.Getenv("GITHUB_RUN_ID"),
		RunAttempt: os.Getenv("GITHUB_RUN_ATTEMPT"),
		Workflow:   os.Getenv("GITHUB_WORKFLOW"),
		Actor:      os.Getenv("GITHUB_ACTOR"),
	}
	if server, repo := os.Getenv("GITHUB_SERVER_URL"), os.Getenv("GITHUB_REPOSITORY"); server != "" && repo != "" && g.RunID != "" {
		g.RunURL = strings.TrimRight(server, "/") + "/" + repo + "/actions/runs/" + g.RunID
	}
	return g, "github"
}

// firmwareInputs hashes a local image.
func firmwareInputs(path string) (ci.DUTFirmware, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ci.DUTFirmware{}, err
	}
	return ci.DUTFirmware{File: filepath.Base(path), SHA256: ci.SHA256Hex(data), Size: int64(len(data))}, nil
}

// osName is "darwin-arm64".
func osName() string { return runtime.GOOS + "-" + runtime.GOARCH }
