package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
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
	Enabled *bool `json:"enabled,omitempty"`
	// FailIfUnavailable makes the run fail when the sandbox cannot start.
	// Options.Sandbox defaults it to true whenever Enabled is true.
	FailIfUnavailable            *bool                      `json:"failIfUnavailable,omitempty"`
	AutoAllowBashIfSandboxed     *bool                      `json:"autoAllowBashIfSandboxed,omitempty"`
	AllowUnsandboxedCommands     *bool                      `json:"allowUnsandboxedCommands,omitempty"`
	Network                      *SandboxNetworkSettings    `json:"network,omitempty"`
	Filesystem                   *SandboxFilesystemSettings `json:"filesystem,omitempty"`
	Credentials                  map[string]any             `json:"credentials,omitempty"`
	IgnoreViolations             map[string][]string        `json:"ignoreViolations,omitempty"`
	EnableWeakerNestedSandbox    *bool                      `json:"enableWeakerNestedSandbox,omitempty"`
	EnableWeakerNetworkIsolation *bool                      `json:"enableWeakerNetworkIsolation,omitempty"`
	AllowAppleEvents             *bool                      `json:"allowAppleEvents,omitempty"`
	ExcludedCommands             []string                   `json:"excludedCommands,omitempty"`
	Ripgrep                      *SandboxRipgrepConfig      `json:"ripgrep,omitempty"`
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
	StrictAllowlist         *bool                `json:"strictAllowlist,omitempty"`
	AllowManagedDomainsOnly *bool                `json:"allowManagedDomainsOnly,omitempty"`
	AllowUnixSockets        []string             `json:"allowUnixSockets,omitempty"`
	AllowAllUnixSockets     *bool                `json:"allowAllUnixSockets,omitempty"`
	AllowLocalBinding       *bool                `json:"allowLocalBinding,omitempty"`
	AllowMachLookup         []string             `json:"allowMachLookup,omitempty"`
	HTTPProxyPort           *int                 `json:"httpProxyPort,omitempty"`
	SOCKSProxyPort          *int                 `json:"socksProxyPort,omitempty"`
	TLSTerminate            *SandboxTLSTerminate `json:"tlsTerminate,omitempty"`
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
	AllowManagedReadPathsOnly *bool    `json:"allowManagedReadPathsOnly,omitempty"`
	Disabled                  *bool    `json:"disabled,omitempty"`
}

// SandboxRipgrepConfig points the sandbox at a ripgrep binary.
type SandboxRipgrepConfig struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

// MarshalJSON emits the typed fields merged over Extra.
func (s SandboxSettings) MarshalJSON() ([]byte, error) {
	type alias SandboxSettings
	typed, err := json.Marshal(alias(s))
	if err != nil || len(s.Extra) == 0 {
		return typed, err
	}
	merged := maps.Clone(s.Extra)
	var fields map[string]any
	if err := json.Unmarshal(typed, &fields); err != nil {
		return nil, err
	}
	maps.Copy(merged, fields)
	return json.Marshal(merged)
}

// encodeJSONObject encodes v and decodes it back as a JSON object. A nil
// value, or one that encodes to null, reports ok == false.
func encodeJSONObject(v any, what string) (map[string]any, bool, error) {
	var raw []byte
	switch value := v.(type) {
	case nil:
		return nil, false, nil
	case json.RawMessage:
		raw = value
	case []byte:
		raw = value
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, false, fmt.Errorf("claude: encoding %s: %w", what, err)
		}
		raw = encoded
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, false, fmt.Errorf("claude: %s must be a JSON object: %w", what, err)
	}
	return obj, true, nil
}

// isInlineJSONObject reports whether a --settings string is inline JSON
// rather than a file path, using the TypeScript SDK's test.
func isInlineJSONObject(s string) bool {
	t := strings.TrimSpace(s)
	return strings.HasPrefix(t, "{") && strings.HasSuffix(t, "}")
}

// buildSettingsValue renders --settings: Options.Settings, with
// Options.Sandbox merged in as its "sandbox" key. A sandbox that is enabled
// without failIfUnavailable gets failIfUnavailable: true.
func buildSettingsValue(opts *Options) (string, error) {
	var (
		text   string
		isPath bool
	)
	switch value := opts.Settings.(type) {
	case nil:
	case string:
		text = value
		isPath = strings.TrimSpace(value) != "" && !isInlineJSONObject(value)
	default:
		obj, ok, err := encodeJSONObject(value, "Options.Settings")
		if err != nil {
			return "", err
		}
		if ok {
			encoded, err := json.Marshal(obj)
			if err != nil {
				return "", fmt.Errorf("claude: encoding Options.Settings: %w", err)
			}
			text = string(encoded)
		}
	}

	sandbox, ok, err := encodeJSONObject(opts.Sandbox, "Options.Sandbox")
	if err != nil {
		return "", err
	}
	if !ok {
		return text, nil
	}
	if isPath {
		return "", errors.New("claude: cannot use both a settings file path and Options.Sandbox; " +
			"include the sandbox configuration in the settings file instead")
	}
	if sandbox["enabled"] == true {
		if _, set := sandbox["failIfUnavailable"]; !set {
			sandbox["failIfUnavailable"] = true
		}
	}
	settings := map[string]any{}
	if strings.TrimSpace(text) != "" {
		if err := json.Unmarshal([]byte(text), &settings); err != nil {
			return "", fmt.Errorf("claude: parsing inline settings: %w", err)
		}
	}
	settings["sandbox"] = sandbox
	payload, err := json.Marshal(settings)
	if err != nil {
		return "", fmt.Errorf("claude: encoding settings: %w", err)
	}
	return string(payload), nil
}

// ---------------------------------------------------------------------------
// Resolved settings
// ---------------------------------------------------------------------------

// ResolvedSettings is an effective settings cascade with per-source detail,
// the result of the TypeScript SDK's resolveSettings. Alpha.
//
// This package does not port resolveSettings itself, which re-implements the
// CLI's settings merge engine; the type exists so FilterEscalatingDefaultMode
// can be applied to a cascade obtained elsewhere.
type ResolvedSettings struct {
	// Effective is the merged result.
	Effective Settings `json:"effective"`
	// Provenance names, per top-level key of Effective, the source that
	// supplied it.
	Provenance map[string]SettingsProvenance `json:"provenance,omitempty"`
	// Sources lists each source's raw settings from low to high precedence.
	Sources []ResolvedSettingsSource `json:"sources"`
}

// SettingsProvenance names the source of one effective setting. Alpha.
type SettingsProvenance struct {
	// Source is a SettingSource, "managed" or "flag".
	Source string `json:"source"`
	// Path is the settings file, for filesystem-backed sources.
	Path string `json:"path,omitempty"`
	// PolicyOrigin names the policy sub-source when Source is "managed".
	PolicyOrigin string `json:"policyOrigin,omitempty"`
}

// ResolvedSettingsSource is one tier of a ResolvedSettings cascade. Alpha.
type ResolvedSettingsSource struct {
	// Source is a SettingSource, "managed" or "flag".
	Source       string   `json:"source"`
	Settings     Settings `json:"settings"`
	Path         string   `json:"path,omitempty"`
	PolicyOrigin string   `json:"policyOrigin,omitempty"`
}

// FilterEscalatingDefaultMode applies the trust filter the CLI applies before
// honoring an escalating permissions.defaultMode from settings. Alpha.
//
// When the effective defaultMode is bypassPermissions or auto and the
// highest-precedence source that set it is project or local settings, or it
// is acceptEdits set by project settings, the returned settings omit
// permissions.defaultMode. Otherwise Effective is returned unchanged. The
// input is never modified.
func FilterEscalatingDefaultMode(resolved *ResolvedSettings) Settings {
	if resolved == nil {
		return nil
	}
	mode, _ := defaultModeOf(resolved.Effective)
	var untrusted []string
	switch mode {
	case PermissionModeBypassPermissions, PermissionModeAuto:
		untrusted = []string{SettingSourceProject, SettingSourceLocal}
	case PermissionModeAcceptEdits:
		untrusted = []string{SettingSourceProject}
	default:
		return resolved.Effective
	}
	for i := len(resolved.Sources) - 1; i >= 0; i-- {
		source := resolved.Sources[i]
		if _, set := defaultModeOf(source.Settings); !set {
			continue
		}
		for _, u := range untrusted {
			if source.Source == u {
				return withoutDefaultMode(resolved.Effective)
			}
		}
		return resolved.Effective
	}
	return resolved.Effective
}

// defaultModeOf reads permissions.defaultMode, reporting whether it is set.
func defaultModeOf(s Settings) (string, bool) {
	perms, ok := settingsObject(s["permissions"])
	if !ok {
		return "", false
	}
	value, set := perms["defaultMode"]
	mode, _ := value.(string)
	return mode, set
}

// withoutDefaultMode copies s with permissions.defaultMode removed.
func withoutDefaultMode(s Settings) Settings {
	out := maps.Clone(s)
	perms, _ := settingsObject(s["permissions"])
	perms = maps.Clone(perms)
	delete(perms, "defaultMode")
	out["permissions"] = perms
	return out
}

// settingsObject views a nested settings value as an object.
func settingsObject(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case Settings:
		return m, true
	}
	return nil, false
}
