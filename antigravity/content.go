package antigravity

import (
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Content is one part of a prompt: Text, SlashCommand, Image, Document,
// Audio or Video (or pointers to the media types). A prompt is a sequence of
// parts; upstream's Content union of a primitive or a list of primitives
// maps to a variadic ...Content.
type Content interface{ content() }

// Text is a text prompt part.
type Text string

func (Text) content() {}

// SlashCommand is a builtin slash command prompt part.
type SlashCommand string

func (SlashCommand) content() {}

// SlashCommandPlan asks the agent to plan before executing (it writes an
// implementation plan artifact and waits for approval). Needs
// AgentBehaviorInteractive.
const SlashCommandPlan SlashCommand = "plan"

// Supported media MIME types, per media kind.
var (
	supportedImageMIMEs    = []string{"image/bmp", "image/jpeg", "image/png", "image/webp"}
	supportedDocumentMIMEs = []string{
		"application/pdf", "application/json", "text/css", "text/csv", "text/html",
		"text/javascript", "text/plain", "text/rtf", "text/xml",
	}
	supportedAudioMIMEs = []string{
		"audio/wav", "audio/x-wav", "audio/wave", "audio/vnd.wave", "audio/mp3",
		"audio/mp4", "audio/webm", "audio/aac", "audio/ogg", "audio/flac",
		"audio/opus", "audio/mpeg", "audio/m4a", "audio/l16",
	}
	supportedVideoMIMEs = []string{
		"video/3gpp", "video/avi", "video/mp4", "video/mpeg", "video/mpg",
		"video/quicktime", "video/webm", "video/wmv", "video/x-flv",
	}
)

// Media is an attachment prompt part: Image, Document, Audio or Video. A
// Media value is also what a tool returns to hand the model media rather
// than JSON text (see NewTool).
type Media interface {
	Content
	// MediaData returns the raw bytes, the MIME type and the optional
	// description.
	MediaData() (data []byte, mimeType, description string)
	kind() string
	supported() []string
}

// Image is an image attachment (BMP, JPEG, PNG or WebP).
type Image struct {
	Data        []byte
	MIMEType    string
	Description string
}

// Document is a document attachment (PDF, JSON, CSS, CSV, HTML, JavaScript,
// plain text, RTF or XML).
type Document struct {
	Data        []byte
	MIMEType    string
	Description string
}

// Audio is an audio attachment.
type Audio struct {
	Data        []byte
	MIMEType    string
	Description string
}

// Video is a video attachment.
type Video struct {
	Data        []byte
	MIMEType    string
	Description string
}

func (Image) content()    {}
func (Document) content() {}
func (Audio) content()    {}
func (Video) content()    {}

// MediaData implements Media.
func (m Image) MediaData() ([]byte, string, string) { return m.Data, m.MIMEType, m.Description }

// MediaData implements Media.
func (m Document) MediaData() ([]byte, string, string) { return m.Data, m.MIMEType, m.Description }

// MediaData implements Media.
func (m Audio) MediaData() ([]byte, string, string) { return m.Data, m.MIMEType, m.Description }

// MediaData implements Media.
func (m Video) MediaData() ([]byte, string, string) { return m.Data, m.MIMEType, m.Description }

func (Image) kind() string    { return "Image" }
func (Document) kind() string { return "Document" }
func (Audio) kind() string    { return "Audio" }
func (Video) kind() string    { return "Video" }

func (Image) supported() []string    { return supportedImageMIMEs }
func (Document) supported() []string { return supportedDocumentMIMEs }
func (Audio) supported() []string    { return supportedAudioMIMEs }
func (Video) supported() []string    { return supportedVideoMIMEs }

// validateMedia checks that m's MIME type is supported for its kind.
func validateMedia(m Media) error {
	_, mimeType, _ := m.MediaData()
	if !slices.Contains(m.supported(), mimeType) {
		return validationErrorf("Unsupported %s MIME type: '%s'", m.kind(), mimeType)
	}
	return nil
}

// NewImage returns an Image after checking its MIME type.
func NewImage(data []byte, mimeType, description string) (*Image, error) {
	m := &Image{data, mimeType, description}
	if err := validateMedia(m); err != nil {
		return nil, err
	}
	return m, nil
}

// NewDocument returns a Document after checking its MIME type.
func NewDocument(data []byte, mimeType, description string) (*Document, error) {
	m := &Document{data, mimeType, description}
	if err := validateMedia(m); err != nil {
		return nil, err
	}
	return m, nil
}

// NewAudio returns an Audio after checking its MIME type.
func NewAudio(data []byte, mimeType, description string) (*Audio, error) {
	m := &Audio{data, mimeType, description}
	if err := validateMedia(m); err != nil {
		return nil, err
	}
	return m, nil
}

// NewVideo returns a Video after checking its MIME type.
func NewVideo(data []byte, mimeType, description string) (*Video, error) {
	m := &Video{data, mimeType, description}
	if err := validateMedia(m); err != nil {
		return nil, err
	}
	return m, nil
}

// ImageFromFile reads an image file, inferring its MIME type from the
// extension.
func ImageFromFile(path, description string) (*Image, error) {
	data, mimeType, err := readFileAndGuessMIME(path)
	if err != nil {
		return nil, err
	}
	return NewImage(data, mimeType, description)
}

// DocumentFromFile reads a document file, inferring its MIME type from the
// extension.
func DocumentFromFile(path, description string) (*Document, error) {
	data, mimeType, err := readFileAndGuessMIME(path)
	if err != nil {
		return nil, err
	}
	return NewDocument(data, mimeType, description)
}

// AudioFromFile reads an audio file, inferring its MIME type from the
// extension.
func AudioFromFile(path, description string) (*Audio, error) {
	data, mimeType, err := readFileAndGuessMIME(path)
	if err != nil {
		return nil, err
	}
	return NewAudio(data, mimeType, description)
}

// VideoFromFile reads a video file, inferring its MIME type from the
// extension.
func VideoFromFile(path, description string) (*Video, error) {
	data, mimeType, err := readFileAndGuessMIME(path)
	if err != nil {
		return nil, err
	}
	return NewVideo(data, mimeType, description)
}

// FromFile reads a local file into the media kind its MIME type, inferred
// from the extension, belongs to.
func FromFile(path, description string) (Media, error) {
	data, mimeType, err := readFileAndGuessMIME(path)
	if err != nil {
		return nil, err
	}
	return FromBytes(data, mimeType, description)
}

// FromBytes wraps raw bytes in the media kind mimeType belongs to.
func FromBytes(data []byte, mimeType, description string) (Media, error) {
	switch {
	case slices.Contains(supportedImageMIMEs, mimeType):
		return &Image{data, mimeType, description}, nil
	case slices.Contains(supportedDocumentMIMEs, mimeType):
		return &Document{data, mimeType, description}, nil
	case slices.Contains(supportedAudioMIMEs, mimeType):
		return &Audio{data, mimeType, description}, nil
	case slices.Contains(supportedVideoMIMEs, mimeType):
		return &Video{data, mimeType, description}, nil
	}
	all := slices.Concat(supportedImageMIMEs, supportedDocumentMIMEs, supportedAudioMIMEs, supportedVideoMIMEs)
	slices.Sort(all)
	return nil, validationErrorf("Unsupported MIME type: '%s'. Supported file formats in the SDK are: %v", mimeType, all)
}

// extensionMIMEs maps file extensions to MIME types. It mirrors the
// built-in table of Python's mimetypes module for the supported formats, so
// guessing does not depend on the host's MIME database.
var extensionMIMEs = map[string]string{
	".bmp": "image/bmp", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".jpe": "image/jpeg",
	".png": "image/png", ".webp": "image/webp",
	".pdf": "application/pdf", ".json": "application/json", ".css": "text/css",
	".csv": "text/csv", ".html": "text/html", ".htm": "text/html",
	".js": "text/javascript", ".mjs": "text/javascript", ".txt": "text/plain",
	".text": "text/plain", ".rtf": "text/rtf", ".xml": "text/xml",
	".wav": "audio/x-wav", ".mp3": "audio/mpeg", ".m4a": "audio/mp4",
	".aac": "audio/aac", ".ogg": "audio/ogg", ".oga": "audio/ogg", ".flac": "audio/flac",
	".opus": "audio/opus", ".weba": "audio/webm",
	".mp4": "video/mp4", ".webm": "video/webm", ".mpeg": "video/mpeg", ".mpg": "video/mpeg",
	".mov": "video/quicktime", ".qt": "video/quicktime", ".3gp": "video/3gpp",
	".avi": "video/avi", ".flv": "video/x-flv", ".wmv": "video/wmv",
}

func guessMIME(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if m, ok := extensionMIMEs[ext]; ok {
		return m
	}
	if ext == "" {
		return ""
	}
	m := mime.TypeByExtension(ext)
	if i := strings.IndexByte(m, ';'); i >= 0 {
		m = strings.TrimSpace(m[:i])
	}
	return m
}

// readFileSafely reads path, wrapping filesystem errors with the path. The
// errors still match fs.ErrNotExist, fs.ErrPermission and so on.
func readFileSafely(path string) ([]byte, error) {
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		return nil, fmt.Errorf("antigravity: path is a directory, not a file: '%s'", path)
	}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		return data, nil
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("antigravity: file not found at path: '%s': %w", path, err)
	case errors.Is(err, fs.ErrPermission):
		return nil, fmt.Errorf("antigravity: permission denied when reading path: '%s': %w", path, err)
	}
	return nil, fmt.Errorf("antigravity: failed to read file at path '%s': %w", path, err)
}

func readFileAndGuessMIME(path string) ([]byte, string, error) {
	data, err := readFileSafely(path)
	if err != nil {
		return nil, "", err
	}
	m := guessMIME(path)
	if m == "" {
		return nil, "", validationErrorf("Could not infer a valid MIME type for extension: '%s'", filepath.Ext(path))
	}
	return data, m, nil
}

// controlChars matches the control characters stripped from prompt text
// before it is sent (null bytes and C0/C1 controls other than tab, newline
// and carriage return).
var controlChars = regexp.MustCompile(`[\x00-\x08\x0b\x0c\x0e-\x1f\x{7f}-\x{9f}]`)

// sanitizePrompt replaces control characters with spaces. A string left
// with only whitespace becomes a single space, so the harness never sees an
// empty-but-present prompt; the empty string stays empty.
func sanitizePrompt(text string) string {
	if text == "" {
		return ""
	}
	s := controlChars.ReplaceAllString(text, " ")
	if strings.TrimSpace(s) == "" {
		return " "
	}
	return s
}
