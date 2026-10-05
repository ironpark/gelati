package claude

import (
	"slices"
	"strings"
)

// The prefix lists mirror the TypeScript SDK's exported usage-limit lists;
// each backs the Is* function of the same name, which documents it.

var usageLimitErrorPrefixes = []string{
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

var usageTransitionPrefixes = []string{
	"You're now using usage credits",
	"You're now using your usage allocation",
	"Now using your usage allocation",
	"Now using usage credits",
	"You're now using extra usage",
	"Now using extra usage",
}

var usageWarningPrefixes = []string{
	"You've used",
	"You're close to",
}

var orgPolicyLimitPrefixes = []string{
	"This service is disabled for your org",
}

// hasAnyPrefix reports whether text starts with one of prefixes.
func hasAnyPrefix(text string, prefixes []string) bool {
	return slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(text, p) })
}

// IsUsageLimitError reports whether text is a message meaning a usage limit
// was genuinely reached (the CLI's limit-reached and usage-credits-required
// texts). Alpha.
func IsUsageLimitError(text string) bool { return hasAnyPrefix(text, usageLimitErrorPrefixes) }

// IsUsageTransition reports whether text is an overage-transition
// notification ("now drawing from credits"). Such texts are shown as toasts
// and never arrive as API errors. Alpha.
func IsUsageTransition(text string) bool { return hasAnyPrefix(text, usageTransitionPrefixes) }

// IsUsageWarning reports whether text is an approaching-limit warning
// (severity "warning"). Such texts are shown in the footer or as toasts and
// never arrive as API errors. Alpha.
func IsUsageWarning(text string) bool { return hasAnyPrefix(text, usageWarningPrefixes) }

// IsOrgPolicyLimit reports whether text is a message that arrives on the same
// error path as usage limits but means an organization policy blocks the
// request; present it as "disabled for your org", not as a usage limit.
// Alpha.
func IsOrgPolicyLimit(text string) bool { return hasAnyPrefix(text, orgPolicyLimitPrefixes) }
