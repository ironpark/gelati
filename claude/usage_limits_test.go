package claude

import (
	"slices"
	"testing"
)

func TestConstantsExitReasons(t *testing.T) {
	t.Parallel()
	want := []ExitReason{"clear", "resume", "logout", "prompt_input_exit", "other"}
	if !slices.Equal(ExitReasons, want) {
		t.Errorf("ExitReasons = %v", ExitReasons)
	}
}

func TestConstantsUsagePrefixes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		text                                  string
		limit, transition, warning, orgPolicy bool
	}{
		{"You've hit your limit · resets 5pm", true, false, false, false},
		{"Your org is out of usage · add funds to continue", true, false, false, false},
		{"Your group's usage limit is set to $0", true, false, false, false},
		{"You're now using extra usage", false, true, false, false},
		{"Now using usage credits", false, true, false, false},
		{"You've used 90% of your weekly limit", false, false, true, false},
		{"You're close to your limit", false, false, true, false},
		{"This service is disabled for your org", false, false, false, true},
		{" You've hit your limit", false, false, false, false}, // prefix match only
		{"", false, false, false, false},
	}
	for _, tt := range tests {
		if got := IsUsageLimitError(tt.text); got != tt.limit {
			t.Errorf("IsUsageLimitError(%q) = %v", tt.text, got)
		}
		if got := IsUsageTransition(tt.text); got != tt.transition {
			t.Errorf("IsUsageTransition(%q) = %v", tt.text, got)
		}
		if got := IsUsageWarning(tt.text); got != tt.warning {
			t.Errorf("IsUsageWarning(%q) = %v", tt.text, got)
		}
		if got := IsOrgPolicyLimit(tt.text); got != tt.orgPolicy {
			t.Errorf("IsOrgPolicyLimit(%q) = %v", tt.text, got)
		}
	}
	if len(usageLimitErrorPrefixes) != 12 || len(usageTransitionPrefixes) != 6 || len(usageWarningPrefixes) != 2 || len(orgPolicyLimitPrefixes) != 1 {
		t.Error("prefix list sizes differ from the TS SDK")
	}
}

func TestIsTerminalTaskStatus(t *testing.T) {
	t.Parallel()
	for s, want := range map[string]bool{"completed": true, "failed": true, "stopped": true, "killed": true, "running": false, "": false} {
		if got := IsTerminalTaskStatus(s); got != want {
			t.Errorf("IsTerminalTaskStatus(%q) = %v, want %v", s, got, want)
		}
	}
}
