// Package vocab holds the fixed status vocabularies defined in
// docs/STATE-CONTRACT.md ("Status vocabularies"). No other values are valid.
package vocab

import (
	"fmt"
	"slices"
)

// Set is a closed vocabulary.
type Set struct {
	Name   string
	Values []string
}

// Check returns an error when v is not in the vocabulary.
func (s Set) Check(v string) error {
	if slices.Contains(s.Values, v) {
		return nil
	}
	return fmt.Errorf("invalid %s %q (allowed: %v)", s.Name, v, s.Values)
}

var (
	SessionStatus = Set{"session status", []string{
		"starting", "running", "idle", "waiting_for_approval", "retrying",
		"cooling_down", "quota_unavailable", "authentication_required",
		"stopped", "failed", "unknown",
	}}
	StopReason = Set{"stop reason", []string{
		"quota", "authentication", "context_limit", "network_service",
		"process_crash", "host_lost", "user_stopped", "completed", "unknown",
	}}
	TaskStatus = Set{"task status", []string{
		"queued", "active", "blocked", "done", "cancelled",
	}}
	LeaseState = Set{"lease state", []string{
		"vacant", "held", "suspended", "handoff_pending",
	}}
	RuleStatus       = Set{"rule status", []string{"proposed", "active", "superseded", "revoked"}}
	RuleScope        = Set{"rule scope", []string{"user", "project", "task"}}
	RuleConfirmation = Set{"rule confirmation", []string{"stated", "confirmed"}}
	PendingKind      = Set{"pending kind", []string{"question", "approval"}}
	PendingStatus    = Set{"pending status", []string{
		"pending", "approved", "denied", "answered", "withdrawn", "expired",
	}}
	AssignmentStatus = Set{"assignment status", []string{
		"assigned", "running", "submitted", "accepted", "rejected", "cancelled",
	}}
	ItemStatus       = Set{"item status", []string{"pending", "in_progress", "review", "done", "blocked", "cancelled"}}
	DelegationStatus = Set{"delegation status", []string{"proposed", "active", "expired", "revoked"}}
	EffectStatus     = Set{"side-effect status", []string{
		"planned", "in_flight", "confirmed", "failed", "not_performed", "unknown",
	}}
	MilestoneStatus = Set{"milestone status", []string{
		"not_started", "in_progress", "succeeded", "failed", "unknown",
	}}
	Preflight      = Set{"preflight result", []string{"available", "missing", "unknown"}}
	CheckResult    = Set{"check result", []string{"passed", "mismatch", "unknown"}}
	ToolStatus     = Set{"tool status", []string{"active", "deprecated", "superseded"}}
	ToolEffect     = Set{"tool effect class", []string{"read_only", "local_write", "external_write"}}
	LockStatus     = Set{"lock status", []string{"held", "released", "stale"}}
	ResourceScope  = Set{"resource scope", []string{"user", "project"}}
	ObservationSrc = Set{"observation source", []string{"hook", "notify", "stream", "pane", "process", "user"}}
)

// StatusForReason maps a stop/failure reason to the session status it is
// shown with. resetKnown applies to quota only; retrying applies to
// network_service while the CLI still retries by itself.
func StatusForReason(reason string, resetKnown, retrying bool) (string, error) {
	if err := StopReason.Check(reason); err != nil {
		return "", err
	}
	switch reason {
	case "quota":
		if resetKnown {
			return "cooling_down", nil
		}
		return "quota_unavailable", nil
	case "authentication":
		return "authentication_required", nil
	case "network_service":
		if retrying {
			return "retrying", nil
		}
		return "failed", nil
	case "context_limit", "process_crash":
		return "failed", nil
	case "host_lost", "user_stopped", "completed":
		return "stopped", nil
	default:
		return "unknown", nil
	}
}

// Suspends reports whether a holder in this status suspends its lease
// immediately. A persisting "unknown" suspends only after the grace period,
// which callers check separately.
func Suspends(status string) bool {
	switch status {
	case "stopped", "failed", "cooling_down", "quota_unavailable", "authentication_required":
		return true
	}
	return false
}
