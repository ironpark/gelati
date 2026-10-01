package claude

import "strings"

// Miscellaneous constants exported by the TypeScript SDK. ExitReason lives
// with the SessionEnd hook input in hook_inputs.go.

// UsageLimitErrorPrefixes are the prefixes of messages meaning "a usage
// limit was genuinely reached" (the CLI's limit-reached and
// usage-credits-required texts). Alpha.
var UsageLimitErrorPrefixes = []string{
	"You've hit your",
	"You've reached your",
	"You're out of usage credits",
	"Your org is out of usage · add funds to continue",
	"Your org is out of usage · contact your admin",
	"Your seat type doesn't include usage credits",
	"Your seat type doesn't include usage",
	"Your usage allocation has been disabled by your admin",
	"Your group's usage limit is set to $0",
	"Fable 5 requires usage credits",
	"You're out of extra usage",
	"Your seat type doesn't include extra usage",
}

// UsageTransitionPrefixes are the prefixes of overage-transition
// notifications ("now drawing from credits"). They are shown as toasts and
// never arrive as API errors. Alpha.
var UsageTransitionPrefixes = []string{
	"You're now using usage credits",
	"You're now using your usage allocation",
	"Now using your usage allocation",
	"Now using usage credits",
	"You're now using extra usage",
	"Now using extra usage",
}

// UsageWarningPrefixes are the prefixes of approaching-limit warnings
// (severity "warning"). They are shown in the footer or as toasts and never
// arrive as API errors. Alpha.
var UsageWarningPrefixes = []string{
	"You've used",
	"You're close to",
}

// OrgPolicyLimitPrefixes are the prefixes of messages that arrive on the
// same error path as usage limits but mean an organization policy blocks
// the request; present them as "disabled for your org", not as a usage
// limit. Alpha.
var OrgPolicyLimitPrefixes = []string{
	"This service is disabled for your org",
}

// hasAnyPrefix reports whether text starts with one of prefixes.
func hasAnyPrefix(text string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(text, p) {
			return true
		}
	}
	return false
}

// IsUsageLimitError reports whether text starts with one of
// UsageLimitErrorPrefixes. Alpha.
func IsUsageLimitError(text string) bool { return hasAnyPrefix(text, UsageLimitErrorPrefixes) }

// IsUsageTransition reports whether text starts with one of
// UsageTransitionPrefixes. Alpha.
func IsUsageTransition(text string) bool { return hasAnyPrefix(text, UsageTransitionPrefixes) }

// IsUsageWarning reports whether text starts with one of
// UsageWarningPrefixes. Alpha.
func IsUsageWarning(text string) bool { return hasAnyPrefix(text, UsageWarningPrefixes) }

// IsOrgPolicyLimit reports whether text starts with one of
// OrgPolicyLimitPrefixes. Alpha.
func IsOrgPolicyLimit(text string) bool { return hasAnyPrefix(text, OrgPolicyLimitPrefixes) }
