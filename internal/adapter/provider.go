package adapter

import (
	"fmt"
	"sort"
	"time"
)

// LaunchContextEnv points to the coordinator-generated launch brief. Codex
// reads it from lifecycle hooks because it has no per-session system prompt
// flag that preserves the user's existing developer instructions.
const LaunchContextEnv = "AGENT_SHIFTBOARD_LAUNCH_CONTEXT"

// Provider is one coding-agent CLI behind the adapter boundary.
type Provider interface {
	Name() string
	// PreassignsID reports whether Launch fixes the native session ID up
	// front (LaunchSpec.NativeID). Otherwise it is learned from hooks.
	PreassignsID() bool
	// Launch returns the command for a new interactive session. The launch
	// context (LaunchSpec.LaunchContext) and first prompt are passed in the
	// way this CLI supports.
	Launch(s LaunchSpec) ([]string, error)
	// Resume returns the command that resumes LaunchSpec.NativeID.
	Resume(s LaunchSpec) ([]string, error)
	// Observe maps one hook payload to a status observation.
	Observe(h HookInput, now time.Time) Observation
	// LaunchContextEvent reports whether this event should receive the
	// persisted launch brief as provider-injected context.
	LaunchContextEvent(h HookInput) bool
}

var providers = map[string]Provider{}

func register(p Provider) { providers[p.Name()] = p }

// Get returns the adapter for a provider name.
func Get(name string) (Provider, error) {
	if p, ok := providers[name]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("no adapter for %q yet (available: %v)", name, Names())
}

// Names lists implemented adapters.
func Names() []string {
	var out []string
	for n := range providers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

type claude struct{}

func (claude) Name() string                                   { return "claude" }
func (claude) PreassignsID() bool                             { return true }
func (claude) Launch(s LaunchSpec) ([]string, error)          { return ClaudeLaunch(s) }
func (claude) Resume(s LaunchSpec) ([]string, error)          { return ClaudeResume(s) }
func (claude) Observe(h HookInput, now time.Time) Observation { return ClaudeObservation(h, now) }
func (claude) LaunchContextEvent(HookInput) bool              { return false }

func init() { register(claude{}) }
