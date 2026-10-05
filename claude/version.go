package claude

import "github.com/ironpark/gelati/internal/buildinfo"

// Version returns the SDK version reported to the CLI in
// CLAUDE_AGENT_SDK_VERSION: the gelati module version built into the binary
// without its "v" prefix, or "0.0.0-dev" when it is unknown.
func Version() string { return buildinfo.Version() }

// entrypoint and entrypointClient are reported to the CLI in
// CLAUDE_CODE_ENTRYPOINT, distinguishing one-shot queries from interactive
// client sessions.
const (
	entrypoint       = "sdk-go"
	entrypointClient = "sdk-go-client"
)
