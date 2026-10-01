package claude

import (
	"errors"
	"fmt"
)

// validateProcessOptions rejects option combinations the TypeScript SDK
// refuses at option intake. It runs before the CLI is spawned.
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
