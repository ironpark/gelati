package claude

// String enumerations used by the message types. Each is a string alias with
// constants for the values the CLI is known to send; newer CLIs may send
// values not listed here, which pass through unchanged.

// Result subtypes (ResultMessage.Subtype).
const (
	ResultSubtypeSuccess                         = "success"
	ResultSubtypeErrorDuringExecution            = "error_during_execution"
	ResultSubtypeErrorMaxTurns                   = "error_max_turns"
	ResultSubtypeErrorMaxBudgetUSD               = "error_max_budget_usd"
	ResultSubtypeErrorMaxStructuredOutputRetries = "error_max_structured_output_retries"
)

// TerminalReason says why a run ended (ResultMessage.TerminalReason).
type TerminalReason = string

// Known terminal reasons.
const (
	TerminalReasonBlockingLimit                  TerminalReason = "blocking_limit"
	TerminalReasonRapidRefillBreaker             TerminalReason = "rapid_refill_breaker"
	TerminalReasonPromptTooLong                  TerminalReason = "prompt_too_long"
	TerminalReasonImageError                     TerminalReason = "image_error"
	TerminalReasonModelError                     TerminalReason = "model_error"
	TerminalReasonAPIError                       TerminalReason = "api_error"
	TerminalReasonMalformedToolUseExhausted      TerminalReason = "malformed_tool_use_exhausted"
	TerminalReasonAbortedStreaming               TerminalReason = "aborted_streaming"
	TerminalReasonAbortedTools                   TerminalReason = "aborted_tools"
	TerminalReasonStopHookPrevented              TerminalReason = "stop_hook_prevented"
	TerminalReasonHookStopped                    TerminalReason = "hook_stopped"
	TerminalReasonToolDeferred                   TerminalReason = "tool_deferred"
	TerminalReasonMaxTurns                       TerminalReason = "max_turns"
	TerminalReasonBackgroundRequested            TerminalReason = "background_requested"
	TerminalReasonCompleted                      TerminalReason = "completed"
	TerminalReasonBudgetExhausted                TerminalReason = "budget_exhausted"
	TerminalReasonStructuredOutputRetryExhausted TerminalReason = "structured_output_retry_exhausted"
	TerminalReasonToolDeferredUnavailable        TerminalReason = "tool_deferred_unavailable"
	TerminalReasonTurnSetupFailed                TerminalReason = "turn_setup_failed"
)

// StartupFailureReason names a known startup failure
// (ResultMessage.StartupFailureReason).
type StartupFailureReason = string

// Known startup failure reasons.
const (
	StartupFailureOrgPinAPIKeyConflict              StartupFailureReason = "org_pin_api_key_conflict"
	StartupFailureProviderNotAllowed                StartupFailureReason = "provider_not_allowed"
	StartupFailureOrgVerifyFailed                   StartupFailureReason = "org_verify_failed"
	StartupFailureOrgPinMismatch                    StartupFailureReason = "org_pin_mismatch"
	StartupFailureManagedSettingsInvalid            StartupFailureReason = "managed_settings_invalid"
	StartupFailureRemoteSettingsRequiredUnavailable StartupFailureReason = "remote_settings_required_unavailable"
	StartupFailureGatewaySigninRequired             StartupFailureReason = "gateway_signin_required"
	StartupFailureGatewayAccessDenied               StartupFailureReason = "gateway_access_denied"
	StartupFailureProxyInvalid                      StartupFailureReason = "proxy_invalid"
	StartupFailureTempDirUnusable                   StartupFailureReason = "temp_dir_unusable"
	StartupFailureCwdUnavailable                    StartupFailureReason = "cwd_unavailable"
	StartupFailureShellToolMissing                  StartupFailureReason = "shell_tool_missing"
	StartupFailureSessionHeldByBackground           StartupFailureReason = "session_held_by_background"
	StartupFailureWorktreeResumeRefused             StartupFailureReason = "worktree_resume_refused"
	StartupFailureWorktreeUnverified                StartupFailureReason = "worktree_unverified"
	StartupFailureCLIVersionTooOld                  StartupFailureReason = "cli_version_too_old"
	StartupFailureBypassRoot                        StartupFailureReason = "bypass_root"
)

// FastModeState is the fast-mode state reported on init and result messages.
type FastModeState = string

// Known fast-mode states.
const (
	FastModeOff      FastModeState = "off"
	FastModeCooldown FastModeState = "cooldown"
	FastModeOn       FastModeState = "on"
)

// FastModeDisabledReason says why fast mode is unavailable.
type FastModeDisabledReason = string

// Known fast-mode disabled reasons.
const (
	FastModeDisabledFree               FastModeDisabledReason = "free"
	FastModeDisabledPreference         FastModeDisabledReason = "preference"
	FastModeDisabledExtraUsageDisabled FastModeDisabledReason = "extra_usage_disabled"
	FastModeDisabledNetworkError       FastModeDisabledReason = "network_error"
	FastModeDisabledUnknown            FastModeDisabledReason = "unknown"
	FastModeDisabledNotFirstParty      FastModeDisabledReason = "not_first_party"
	FastModeDisabledByEnv              FastModeDisabledReason = "disabled_by_env"
	FastModeDisabledModelNotAllowed    FastModeDisabledReason = "model_not_allowed"
	FastModeDisabledSDKOptInRequired   FastModeDisabledReason = "sdk_opt_in_required"
	FastModeDisabledPending            FastModeDisabledReason = "pending"
)

// MessagePriority is the queue priority of a user message: "now" interrupts
// the running turn, "next" runs right after it, "later" waits behind queued
// work.
type MessagePriority = string

// Known user message priorities.
const (
	MessagePriorityNow   MessagePriority = "now"
	MessagePriorityNext  MessagePriority = "next"
	MessagePriorityLater MessagePriority = "later"
)

// Rate limit windows (RateLimitInfo.RateLimitType).
const (
	RateLimitTypeFiveHour                = "five_hour"
	RateLimitTypeSevenDay                = "seven_day"
	RateLimitTypeSevenDayOpus            = "seven_day_opus"
	RateLimitTypeSevenDaySonnet          = "seven_day_sonnet"
	RateLimitTypeSevenDayOverageIncluded = "seven_day_overage_included"
	RateLimitTypeOverage                 = "overage"
)

// Reasons overage is disabled (RateLimitInfo.OverageDisabledReason).
const (
	OverageDisabledNotProvisioned          = "overage_not_provisioned"
	OverageDisabledOrgLevel                = "org_level_disabled"
	OverageDisabledOrgLevelUntil           = "org_level_disabled_until"
	OverageDisabledOutOfCredits            = "out_of_credits"
	OverageDisabledSeatTierLevel           = "seat_tier_level_disabled"
	OverageDisabledMemberLevel             = "member_level_disabled"
	OverageDisabledSeatTierZeroCreditLimit = "seat_tier_zero_credit_limit"
	OverageDisabledGroupZeroCreditLimit    = "group_zero_credit_limit"
	OverageDisabledMemberZeroCreditLimit   = "member_zero_credit_limit"
	OverageDisabledOrgServiceLevel         = "org_service_level_disabled"
	OverageDisabledNoLimitsConfigured      = "no_limits_configured"
	OverageDisabledFetchError              = "fetch_error"
	OverageDisabledUnknown                 = "unknown"
)

// HookOutcome is how a hook execution ended (HookEventMessage.Outcome).
type HookOutcome = string

// Known hook outcomes.
const (
	HookOutcomeSuccess   HookOutcome = "success"
	HookOutcomeError     HookOutcome = "error"
	HookOutcomeCancelled HookOutcome = "cancelled"
)

// ConversationResetTrigger says what discarded the conversation
// (ConversationResetMessage.Trigger).
type ConversationResetTrigger = string

// Known conversation reset triggers.
const (
	ResetTriggerClear        ConversationResetTrigger = "clear"
	ResetTriggerPlanModeExit ConversationResetTrigger = "plan_mode_exit"
	ResetTriggerFreshSession ConversationResetTrigger = "fresh_session"
	ResetTriggerOnboarding   ConversationResetTrigger = "onboarding"
)

// SessionState is the session's run state (SessionStateChangedMessage.State).
type SessionState = string

// Known session states.
const (
	SessionStateIdle           SessionState = "idle"
	SessionStateRunning        SessionState = "running"
	SessionStateRequiresAction SessionState = "requires_action"
)

// Session statuses (StatusMessage.Status). An empty status means none.
const (
	StatusCompacting = "compacting"
	StatusRequesting = "requesting"
)

// APIKeySource says where the API credential came from (InitMessage).
type APIKeySource = string

// API key sources current CLIs report.
const (
	APIKeySourceEnv      APIKeySource = "ANTHROPIC_API_KEY"
	APIKeySourceHelper   APIKeySource = "apiKeyHelper"
	APIKeySourceLoginKey APIKeySource = "/login managed key"
	APIKeySourceNone     APIKeySource = "none"
)

// Protocol capabilities a CLI may advertise in InitMessage.Capabilities. The
// set is open; check for exactly the behavior you use.
const (
	// CapabilityInterruptReceiptV1: the interrupt response lists the queued
	// messages that survive it (still_queued).
	CapabilityInterruptReceiptV1 = "interrupt_receipt_v1"
	// CapabilityInterruptCancelQueuedV1: interrupt honors cancel_queued.
	CapabilityInterruptCancelQueuedV1 = "interrupt_cancel_queued_v1"
	// CapabilityQueuedNotifications: the CLI accepts queued_notification
	// input messages.
	CapabilityQueuedNotifications = "queued_notifications"
)

// Informational message levels (InformationalMessage.Level).
const (
	InformationalInfo       = "info"
	InformationalNotice     = "notice"
	InformationalSuggestion = "suggestion"
	InformationalWarning    = "warning"
)

// Notification priorities (NotificationMessage.Priority).
const (
	NotificationPriorityLow       = "low"
	NotificationPriorityMedium    = "medium"
	NotificationPriorityHigh      = "high"
	NotificationPriorityImmediate = "immediate"
)
