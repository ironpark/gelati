package claude

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// validateOptions rejects option combinations the CLI could not serve and
// values that cannot be rendered for it, as the TypeScript SDK does at option
// intake. It runs in prepareOptions, before anything is spawned, so the errors
// surface with a custom Transport too; values whose encoding fails (Settings,
// Sandbox and the like) are reported by resolveLaunch, which prepareOptions
// runs next. opts may be nil.
func validateOptions(opts *Options) error {
	if opts == nil {
		return nil
	}
	// Invalid SessionStore combinations fail before anything is
	// materialized or spawned.
	if err := validateSessionStoreOptions(opts); err != nil {
		return err
	}
	if err := validateCallbackOptions(opts); err != nil {
		return err
	}
	if err := validateProcessOptions(opts); err != nil {
		return err
	}
	if skills, ok := opts.Skills.(SkillList); ok {
		for _, name := range skills {
			if err := validateSkillName(name); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateCallbackOptions rejects callback option combinations the CLI could
// not serve.
func validateCallbackOptions(opts *Options) error {
	if opts.CanUseTool != nil && opts.PermissionPromptToolName != "" {
		return errors.New(
			"claude: Options.CanUseTool cannot be used with Options.PermissionPromptToolName; use one or the other")
	}
	if len(opts.SupportedDialogKinds) > 0 && opts.OnUserDialog == nil {
		return errors.New("claude: Options.SupportedDialogKinds requires Options.OnUserDialog; " +
			"declaring dialog kinds without a handler would park dialogs nothing can answer")
	}
	return nil
}

// validateProcessOptions rejects model, persistence and plugin option
// combinations the TypeScript SDK refuses.
func validateProcessOptions(opts *Options) error {
	if opts.FallbackModel != "" && opts.FallbackModel == opts.Model {
		return errors.New("claude: Options.FallbackModel cannot be the same as Options.Model; " +
			"specify a different fallback model")
	}
	if opts.NoSessionPersistence && opts.SessionStore != nil {
		return errors.New("claude: Options.SessionStore cannot be used with Options.NoSessionPersistence: " +
			"the store mirrors the CLI's local transcript writes; set CLAUDE_CONFIG_DIR to a " +
			"temporary directory for ephemeral local writes with external mirroring")
	}
	switch opts.PluginDelivery {
	case "", PluginDeliveryArgv, PluginDeliveryInitialize:
	default:
		return fmt.Errorf("claude: invalid Options.PluginDelivery %q: expected %q or %q",
			opts.PluginDelivery, PluginDeliveryArgv, PluginDeliveryInitialize)
	}
	for _, p := range opts.Plugins {
		if p.Type != "" && p.Type != "local" {
			return fmt.Errorf("claude: unsupported plugin type: %s", p.Type)
		}
	}
	return nil
}

// validateSkillName rejects names that cannot ride safely in a Skill(name)
// permission rule, or that could never match a discovered skill.
func validateSkillName(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("claude: skill names must be non-empty")
	}
	if !utf8.ValidString(name) {
		return fmt.Errorf("claude: invalid skill name %q: not valid UTF-8, so no discovered skill can match", name)
	}
	if name != strings.TrimSpace(name) {
		return fmt.Errorf("claude: invalid skill name %q: leading or trailing whitespace can never match", name)
	}
	if name == "*" {
		return errors.New(`claude: invalid skill name "*": use SkillsAll{} to enable every skill`)
	}
	if strings.HasSuffix(name, ":*") || strings.HasSuffix(name, " *") {
		return fmt.Errorf("claude: invalid skill name %q: wildcard suffixes are not allowed", name)
	}
	if strings.HasPrefix(name, "/") {
		return fmt.Errorf("claude: invalid skill name %q: use the canonical name, not the slash-command form", name)
	}
	if strings.Contains(name, `\\`) || strings.HasSuffix(name, `\`) {
		return fmt.Errorf("claude: invalid skill name %q: backslash escapes are not allowed", name)
	}
	for _, r := range name {
		if r == '(' || r == ')' || r == ',' || r == '\ufeff' || unicode.IsControl(r) {
			return fmt.Errorf("claude: invalid skill name %q: parentheses, commas and control characters are not allowed", name)
		}
	}
	return nil
}
