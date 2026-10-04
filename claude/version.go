package claude

// Version is the SDK version reported to the CLI in CLAUDE_AGENT_SDK_VERSION.
const Version = "0.1.0"

// entrypoint and entrypointClient are reported to the CLI in
// CLAUDE_CODE_ENTRYPOINT, distinguishing one-shot queries from interactive
// client sessions.
const (
	entrypoint       = "sdk-go"
	entrypointClient = "sdk-go-client"
)
