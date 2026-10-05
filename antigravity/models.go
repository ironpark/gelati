package antigravity

import (
	"maps"
	"os"
	"slices"
)

// Default model names (upstream models.DEFAULT_MODEL and
// DEFAULT_IMAGE_GENERATION_MODEL).
const (
	DefaultModel                = "gemini-3.8-flash"
	DefaultImageGenerationModel = "gemini-3.1-flash-lite-image"
)

// ThinkingLevel controls how much reasoning a Gemini model performs before
// responding. See https://ai.google.dev/gemini-api/docs/thinking.
type ThinkingLevel string

// ThinkingLevel values.
const (
	ThinkingMinimal   ThinkingLevel = "minimal"
	ThinkingLow       ThinkingLevel = "low"
	ThinkingMedium    ThinkingLevel = "medium"
	ThinkingHigh      ThinkingLevel = "high"
	ThinkingExtraHigh ThinkingLevel = "extra_high"
)

// ServiceTier selects the Gemini inference queue. See
// https://ai.google.dev/gemini-api/docs/priority-inference.
type ServiceTier string

// ServiceTier values.
const (
	ServiceTierStandard ServiceTier = "standard"
	ServiceTierPriority ServiceTier = "priority"
	ServiceTierFlex     ServiceTier = "flex"
)

func knownServiceTier(s string) (ServiceTier, bool) {
	switch t := ServiceTier(s); t {
	case ServiceTierStandard, ServiceTierPriority, ServiceTierFlex:
		return t, true
	}
	return "", false
}

// ModelType says what a model target is used for.
type ModelType string

// ModelType values.
const (
	ModelTypeText  ModelType = "text"
	ModelTypeImage ModelType = "image"
)

// GeminiModelOptions holds Gemini-specific model options. Empty fields are
// left to the backend.
type GeminiModelOptions struct {
	ThinkingLevel ThinkingLevel
	ServiceTier   ServiceTier
}

func (o *GeminiModelOptions) clone() *GeminiModelOptions {
	if o == nil {
		return nil
	}
	c := *o
	return &c
}

func (o *GeminiModelOptions) isEmpty() bool {
	return o == nil || (o.ThinkingLevel == "" && o.ServiceTier == "")
}

// ModelEndpoint is the authentication and routing of a model target:
// *GeminiAPIEndpoint, *VertexEndpoint or *OpenAIEndpoint.
type ModelEndpoint interface {
	// Validate reports whether the endpoint has the credentials it needs.
	Validate() error
	modelEndpoint()
}

// GeminiAPIEndpoint routes model calls to the Gemini Developer API.
type GeminiAPIEndpoint struct {
	// BaseURL overrides the API base URL (for proxies and gateways). When
	// set, credential validation is left to the remote side.
	BaseURL     string
	HTTPHeaders map[string]string
	// APIKey authenticates the calls. Empty falls back to the GEMINI_API_KEY
	// environment variable of the harness.
	APIKey  string
	Options *GeminiModelOptions
}

func (*GeminiAPIEndpoint) modelEndpoint() {}

// Validate requires an API key, explicit or from GEMINI_API_KEY, unless
// BaseURL is set.
func (e *GeminiAPIEndpoint) Validate() error {
	if e.BaseURL != "" {
		return nil
	}
	if e.APIKey == "" && os.Getenv("GEMINI_API_KEY") == "" {
		return validationErrorf("A Gemini API key is required. Set it via GEMINI_API_KEY environment variable or via Config.APIKey.")
	}
	return nil
}

// VertexEndpoint routes model calls to Vertex AI, either in standard mode
// (Project and Location) or express mode (APIKey).
//
// Like upstream, when BaseURL and APIKey are both empty, an empty Project or
// Location is filled from GOOGLE_CLOUD_PROJECT and GOOGLE_CLOUD_LOCATION.
// Upstream does that when the endpoint is constructed; here it happens when
// the endpoint is used (Validate and session start).
type VertexEndpoint struct {
	BaseURL     string
	HTTPHeaders map[string]string
	Project     string
	Location    string
	APIKey      string
	Options     *GeminiModelOptions
}

func (*VertexEndpoint) modelEndpoint() {}

// withEnvDefaults returns a copy of e with project and location filled from
// the environment, per the rules above.
func (e *VertexEndpoint) withEnvDefaults() *VertexEndpoint {
	c := *e
	if c.BaseURL == "" && c.APIKey == "" {
		if c.Project == "" {
			c.Project = os.Getenv("GOOGLE_CLOUD_PROJECT")
		}
		if c.Location == "" {
			c.Location = os.Getenv("GOOGLE_CLOUD_LOCATION")
		}
	}
	return &c
}

// Validate requires either Project and Location, or APIKey, but not both,
// unless BaseURL is set.
func (e *VertexEndpoint) Validate() error {
	r := e.withEnvDefaults()
	if r.BaseURL != "" {
		return nil
	}
	regional := r.Project != "" && r.Location != ""
	anyRegional := r.Project != "" || r.Location != ""
	express := r.APIKey != ""
	if anyRegional && express {
		return validationErrorf("Cannot specify both api_key (Express Mode) and project/location (Standard Mode) on VertexEndpoint.")
	}
	if !regional && !express {
		return validationErrorf("For Vertex AI, either (project and location) or api_key must be set.")
	}
	return nil
}

// OpenAIEndpoint routes model calls to a server speaking the OpenAI chat
// completions API, such as Ollama, LM Studio, llama.cpp or vLLM (upstream
// LocalOpenAIAgentConfig.base_url). Set it as Config.OpenAI to run the whole
// session on such a server, or use it in a ModelTarget.
type OpenAIEndpoint struct {
	// BaseURL is the server's API base URL, including the version path,
	// e.g. "http://localhost:11434/v1".
	BaseURL string
}

func (*OpenAIEndpoint) modelEndpoint() {}

// Validate requires BaseURL.
func (e *OpenAIEndpoint) Validate() error {
	if e.BaseURL == "" {
		return validationErrorf("An OpenAI-compatible endpoint requires a non-empty BaseURL.")
	}
	return nil
}

// ModelTarget configures one model.
type ModelTarget struct {
	// Name is the model name; empty lets the backend choose.
	Name string
	// Types lists what the model is used for. Nil means text only.
	Types []ModelType
	// Endpoint authenticates and routes the model's calls. A nil endpoint
	// fails validation at session start.
	Endpoint ModelEndpoint
}

func (m ModelTarget) types() []ModelType {
	if m.Types == nil {
		return []ModelType{ModelTypeText}
	}
	return m.Types
}

func (m ModelTarget) hasType(t ModelType) bool { return slices.Contains(m.types(), t) }

// clone deep-copies m, including its endpoint.
func (m ModelTarget) clone() ModelTarget {
	m.Types = slices.Clone(m.Types)
	switch e := m.Endpoint.(type) {
	case *GeminiAPIEndpoint:
		c := *e
		c.HTTPHeaders = maps.Clone(e.HTTPHeaders)
		c.Options = e.Options.clone()
		m.Endpoint = &c
	case *VertexEndpoint:
		c := *e
		c.HTTPHeaders = maps.Clone(e.HTTPHeaders)
		c.Options = e.Options.clone()
		m.Endpoint = &c
	case *OpenAIEndpoint:
		c := *e
		m.Endpoint = &c
	}
	return m
}

// endpointOptions returns the options pointer slot of a Gemini or Vertex
// endpoint, or nil for other endpoints.
func endpointOptions(e ModelEndpoint) **GeminiModelOptions {
	switch e := e.(type) {
	case *GeminiAPIEndpoint:
		return &e.Options
	case *VertexEndpoint:
		return &e.Options
	}
	return nil
}
