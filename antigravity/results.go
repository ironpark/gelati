package antigravity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Structured results of builtin tools. PostToolCallHook receives one of
// these in ToolResult.Result when the harness reports a builtin tool's
// output in a form that parses; otherwise Result holds the raw string. Each
// type's String method gives a human-readable summary.

// RunCommandResult is the output of run_command.
type RunCommandResult struct {
	// Output is the combined stdout and stderr.
	Output string
}

func (r *RunCommandResult) String() string { return r.Output }

// ListDirectoryEntry is one entry of a directory listing.
type ListDirectoryEntry struct {
	Name        string `json:"name"`
	IsDirectory bool   `json:"is_directory"`
	FileSize    int64  `json:"file_size"`
}

// ListDirectoryResult is the output of list_directory.
type ListDirectoryResult struct {
	Entries []ListDirectoryEntry
}

func (r *ListDirectoryResult) String() string {
	parts := make([]string, len(r.Entries))
	for i, e := range r.Entries {
		if e.IsDirectory {
			parts[i] = e.Name + "/ (dir)"
		} else {
			parts[i] = fmt.Sprintf("%s (%d bytes)", e.Name, e.FileSize)
		}
	}
	sep := "\n"
	if os.PathSeparator == '\\' {
		sep = "\r\n"
	}
	return strings.Join(parts, sep)
}

// SearchDirectoryResult is the output of search_directory.
type SearchDirectoryResult struct {
	NumResults int64
}

func (r *SearchDirectoryResult) String() string { return fmt.Sprintf("%d results", r.NumResults) }

// FindFileResult is the output of find_file.
type FindFileResult struct {
	Output string
}

func (r *FindFileResult) String() string { return r.Output }

// EditFileResult is the output of edit_file.
type EditFileResult struct {
	Summary string
}

func (r *EditFileResult) String() string { return r.Summary }

// GenerateImageResult is the output of generate_image.
type GenerateImageResult struct {
	// ImageName is the requested filename prefix.
	ImageName string
	// AspectRatio is the requested aspect ratio, such as "16:9".
	AspectRatio string
	// OutputPath is where the image was saved; empty if generation failed.
	OutputPath string
}

func (r *GenerateImageResult) String() string {
	if r.OutputPath != "" {
		return r.OutputPath
	}
	return r.ImageName
}

// SearchWebResult is the output of search_web.
type SearchWebResult struct {
	Summary string
}

func (r *SearchWebResult) String() string { return r.Summary }

// ReadURLContentResult is the output of read_url_content.
type ReadURLContentResult struct {
	Title       string
	Summary     string
	ContentPath string
}

func (r *ReadURLContentResult) String() string {
	switch {
	case r.Summary != "":
		return r.Summary
	case r.Title != "":
		return r.Title
	}
	return r.ContentPath
}

// resultField describes one JSON member of a builtin result: its accepted
// names and where to decode it.
type resultField struct {
	names []string
	dst   any
}

// decodeResultObject decodes a JSON object into fields, mirroring pydantic's
// validation: the input must be an object, a present member must not be null
// and must have the field's type, and unknown members are ignored. The first
// of a field's names that is present wins.
func decodeResultObject(s string, fields ...resultField) bool {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &obj); err != nil || obj == nil {
		return false
	}
	for _, f := range fields {
		for _, name := range f.names {
			raw, ok := obj[name]
			if !ok {
				continue
			}
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				return false
			}
			if err := json.Unmarshal(raw, f.dst); err != nil {
				return false
			}
			break
		}
	}
	return true
}

// extractToolResult parses a builtin tool's output into its structured
// result type, or returns nil when the tool has none or the output does not
// parse. edit_file and find_file fall back to wrapping the raw text.
func extractToolResult(toolName, result string) any {
	if result == "" {
		return nil
	}
	switch BuiltinTool(toolName) {
	case BuiltinRunCommand:
		var r RunCommandResult
		if decodeResultObject(result, resultField{[]string{"output", "combined_output"}, &r.Output}) {
			return &r
		}
	case BuiltinListDir:
		var r ListDirectoryResult
		if decodeResultObject(result, resultField{[]string{"entries", "results"}, &r.Entries}) {
			return &r
		}
	case BuiltinFindFile:
		var r FindFileResult
		if decodeResultObject(result, resultField{[]string{"output"}, &r.Output}) {
			return &r
		}
		return &FindFileResult{Output: result}
	case BuiltinSearchDir:
		var r SearchDirectoryResult
		if decodeResultObject(result, resultField{[]string{"num_results"}, &r.NumResults}) {
			return &r
		}
	case BuiltinEditFile:
		var r EditFileResult
		if decodeResultObject(result, resultField{[]string{"summary"}, &r.Summary}) {
			return &r
		}
		return &EditFileResult{Summary: result}
	case BuiltinGenerateImage:
		var r GenerateImageResult
		if decodeResultObject(result,
			resultField{[]string{"image_name"}, &r.ImageName},
			resultField{[]string{"aspect_ratio"}, &r.AspectRatio},
			resultField{[]string{"output_path"}, &r.OutputPath}) {
			return &r
		}
	case BuiltinSearchWeb:
		var r SearchWebResult
		if decodeResultObject(result, resultField{[]string{"summary"}, &r.Summary}) {
			return &r
		}
	case BuiltinReadURLContent:
		var r ReadURLContentResult
		if decodeResultObject(result,
			resultField{[]string{"title"}, &r.Title},
			resultField{[]string{"summary"}, &r.Summary},
			resultField{[]string{"content_path"}, &r.ContentPath}) {
			return &r
		}
	}
	return nil
}
