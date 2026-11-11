// Package detect finds installed provider CLIs without making model calls.
package detect

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// Provider describes a supported CLI.
type Provider struct {
	Name   string `json:"name"`
	Binary string `json:"binary"`
	Phase  int    `json:"phase"` // delivery phase in which its adapter lands
	// VersionArg prints the version without starting a session; default --version.
	VersionArg string `json:"-"`
}

// Providers are the initial targets (D-02).
var Providers = []Provider{
	{Name: "claude", Binary: "claude", Phase: 2},
	{Name: "codex", Binary: "codex", Phase: 3},
	{Name: "agy", Binary: "agy", Phase: 5},
	{Name: "hermes", Binary: "hermes", Phase: 5},
}

// Result is one detection.
type Result struct {
	Provider
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
	Status  string `json:"status"` // available | missing | unknown
	Note    string `json:"note,omitempty"`
}

// Detect runs `<binary> --version` with a short timeout.
func Detect(ctx context.Context, p Provider) Result {
	r := Result{Provider: p}
	path, err := exec.LookPath(p.Binary)
	if err != nil {
		r.Status = "missing"
		return r
	}
	r.Path = path
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	arg := p.VersionArg
	if arg == "" {
		arg = "--version"
	}
	out, err := exec.CommandContext(ctx, path, arg).CombinedOutput()
	if err != nil {
		r.Status = "unknown"
		r.Note = "version check failed"
		return r
	}
	r.Status = "available"
	r.Version = firstLine(string(out))
	return r
}

// All detects every provider.
func All(ctx context.Context) []Result {
	var out []Result
	for _, p := range Providers {
		out = append(out, Detect(ctx, p))
	}
	return out
}

// Tool checks a non-provider executable such as tmux (-V) or git.
func Tool(ctx context.Context, name, versionArg string) Result {
	return Detect(ctx, Provider{Name: name, Binary: name, VersionArg: versionArg})
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
