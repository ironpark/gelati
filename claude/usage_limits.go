package claude

import (
	"slices"
	"strings"
)

// Usage-limit message prefixes exported by the TypeScript SDK. The Is*
// functions match against private copies taken at package initialization, so
// modifying an exported list affects only the caller.

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

// Private copies of the exported lists, which callers may modify.
var (
	usageLimitErrorPrefixes = slices.Clone(UsageLimitErrorPrefixes)
	usageTransitionPrefixes = slices.Clone(UsageTransitionPrefixes)
	usageWarningPrefixes    = slices.Clone(UsageWarningPrefixes)
	orgPolicyLimitPrefixes  = slices.Clone(OrgPolicyLimitPrefixes)
)

// hasAnyPrefix reports whether text starts with one of prefixes.
func hasAnyPrefix(text string, prefixes []string) bool {
	return slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(text, p) })
}

// IsUsageLimitError reports whether text starts with one of
// UsageLimitErrorPrefixes. Alpha.
func IsUsageLimitError(text string) bool { return hasAnyPrefix(text, usageLimitErrorPrefixes) }

// IsUsageTransition reports whether text starts with one of
// UsageTransitionPrefixes. Alpha.
func IsUsageTransition(text string) bool { return hasAnyPrefix(text, usageTransitionPrefixes) }

// IsUsageWarning reports whether text starts with one of
// UsageWarningPrefixes. Alpha.
func IsUsageWarning(text string) bool { return hasAnyPrefix(text, usageWarningPrefixes) }

// IsOrgPolicyLimit reports whether text starts with one of
// OrgPolicyLimitPrefixes. Alpha.
func IsOrgPolicyLimit(text string) bool { return hasAnyPrefix(text, orgPolicyLimitPrefixes) }
