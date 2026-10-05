package claude

import (
	"encoding/json/v2"
	"maps"

	"github.com/ironpark/gelati/internal/jsonx"
)

// Settings is a Claude Code settings object, the same shape as a
// settings.json file. It is JSON-encoded for the CLI.
type Settings map[string]any

// SandboxSettings configures command sandboxing. Every field is optional;
// pointer fields distinguish "unset" from an explicit false or zero, which
// matters because settings tiers are merged key by key.
//
// Filesystem and network access themselves are governed by permission rules
// (Read, Edit and WebFetch); these settings control the sandbox's behavior.
type SandboxSettings struct {
	Enabled *bool `json:"enabled,omitzero"`
	// FailIfUnavailable makes the run fail when the sandbox cannot start.
	// Options.Sandbox defaults it to true whenever Enabled is true.
	FailIfUnavailable            *bool                      `json:"failIfUnavailable,omitzero"`
	AutoAllowBashIfSandboxed     *bool                      `json:"autoAllowBashIfSandboxed,omitzero"`
	AllowUnsandboxedCommands     *bool                      `json:"allowUnsandboxedCommands,omitzero"`
	Network                      *SandboxNetworkSettings    `json:"network,omitzero"`
	Filesystem                   *SandboxFilesystemSettings `json:"filesystem,omitzero"`
	Credentials                  map[string]any             `json:"credentials,omitempty"`
	IgnoreViolations             map[string][]string        `json:"ignoreViolations,omitempty"`
	EnableWeakerNestedSandbox    *bool                      `json:"enableWeakerNestedSandbox,omitzero"`
	EnableWeakerNetworkIsolation *bool                      `json:"enableWeakerNetworkIsolation,omitzero"`
	AllowAppleEvents             *bool                      `json:"allowAppleEvents,omitzero"`
	ExcludedCommands             []string                   `json:"excludedCommands,omitempty"`
	Ripgrep                      *SandboxRipgrepConfig      `json:"ripgrep,omitzero"`
	BwrapPath                    string                     `json:"bwrapPath,omitempty"`
	SocatPath                    string                     `json:"socatPath,omitempty"`

	// Extra carries keys this struct does not model; the schema is open.
	// Typed fields win over Extra entries with the same key.
	Extra map[string]any `json:"-"`
}

// SandboxNetworkSettings configures the sandbox's network proxy.
type SandboxNetworkSettings struct {
	AllowedDomains          []string             `json:"allowedDomains,omitempty"`
	DeniedDomains           []string             `json:"deniedDomains,omitempty"`
	StrictAllowlist         *bool                `json:"strictAllowlist,omitzero"`
	AllowManagedDomainsOnly *bool                `json:"allowManagedDomainsOnly,omitzero"`
	AllowUnixSockets        []string             `json:"allowUnixSockets,omitempty"`
	AllowAllUnixSockets     *bool                `json:"allowAllUnixSockets,omitzero"`
	AllowLocalBinding       *bool                `json:"allowLocalBinding,omitzero"`
	AllowMachLookup         []string             `json:"allowMachLookup,omitempty"`
	HTTPProxyPort           *int                 `json:"httpProxyPort,omitzero"`
	SOCKSProxyPort          *int                 `json:"socksProxyPort,omitzero"`
	TLSTerminate            *SandboxTLSTerminate `json:"tlsTerminate,omitzero"`
}

// SandboxTLSTerminate configures TLS termination in the sandbox proxy.
type SandboxTLSTerminate struct {
	CACertPath string `json:"caCertPath,omitempty"`
	CAKeyPath  string `json:"caKeyPath,omitempty"`
}

// SandboxFilesystemSettings configures sandbox filesystem restrictions.
type SandboxFilesystemSettings struct {
	AllowWrite                []string `json:"allowWrite,omitempty"`
	DenyWrite                 []string `json:"denyWrite,omitempty"`
	DenyRead                  []string `json:"denyRead,omitempty"`
	AllowRead                 []string `json:"allowRead,omitempty"`
	AllowManagedReadPathsOnly *bool    `json:"allowManagedReadPathsOnly,omitzero"`
	Disabled                  *bool    `json:"disabled,omitzero"`
}

// SandboxRipgrepConfig points the sandbox at a ripgrep binary.
type SandboxRipgrepConfig struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

// MarshalJSON emits the typed fields merged over Extra.
func (s SandboxSettings) MarshalJSON() ([]byte, error) {
	type alias SandboxSettings
	typed, err := json.Marshal(alias(s), jsonx.LegacyEncode)
	if err != nil || len(s.Extra) == 0 {
		return typed, err
	}
	merged := maps.Clone(s.Extra)
	var fields map[string]any
	if err := jsonx.Unmarshal(typed, &fields); err != nil {
		return nil, err
	}
	maps.Copy(merged, fields)
	return json.Marshal(merged, jsonx.LegacyEncode)
}
