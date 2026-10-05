package main

import (
	"bytes"
	goparser "go/parser"
	gotoken "go/token"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestGeneratedUpToDate regenerates zz_generated.go from a local copy of
// sdk-tools.d.ts and compares it with the committed file. Set
// GELATI_SDK_TOOLS_DTS to the path of sdk-tools.d.ts from the SDK version
// recorded in zz_generated.go to run it.
func TestGeneratedUpToDate(t *testing.T) {
	path := os.Getenv("GELATI_SDK_TOOLS_DTS")
	if path == "" {
		t.Skip("set GELATI_SDK_TOOLS_DTS=/path/to/sdk-tools.d.ts to check zz_generated.go")
	}
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := os.ReadFile("../../zz_generated.go")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`claude-agent-sdk@(\S+) sdk-tools\.d\.ts`).FindSubmatch(committed)
	if m == nil {
		t.Fatal("zz_generated.go header lacks the SDK version")
	}
	got, err := Generate(src, string(m[1]))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, committed) {
		t.Fatal("zz_generated.go is stale; run `go generate ./claude/tools`")
	}
}

const sample = `
/** Inputs */
export type ToolInputSchemas = FooInput | ToolOutputSchemas;
export type ToolOutputSchemas = FooOutput | MixedOutput;
export type FooOutput =
  | { kind: "a"; count: number; note?: string | null }
  | { kind: "b" | "c"; items: [string, ...string[]] };
export type MixedOutput = string | { type: string; [k: string]: unknown }[];
export interface FooInput {
  /**
   * Things.
   *
   * @minItems 1
   * @maxItems 2
   */
  things: [{ id: string }] | [{ id: string }, { id: string }];
  "-x"?: boolean;
  mode?: "fast" | "slow";
  /** Deprecated; use mode. */
  legacy?: string;
  /** Gone.
   * @deprecated */
  gone?: string;
  ok?: true;
  bag?: { [k: string]: unknown };
  [k: string]: unknown;
}
`

func TestGenerateSample(t *testing.T) {
	out, err := generate([]byte(sample), "0.0.0", map[string]string{"FooInput.things[]": "Thing"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := goparser.ParseFile(gotoken.NewFileSet(), "x.go", out, goparser.ParseComments); err != nil {
		t.Fatalf("generated code does not parse: %v\n%s", err, out)
	}
	space := regexp.MustCompile(`[ \t]+`)
	code := space.ReplaceAllString(string(out), " ")
	for _, want := range []string{
		"type FooOutput interface",
		"func UnmarshalFooOutput(data []byte) (FooOutput, error)",
		`case "b", "c":`,
		"Count int `json:\"count\"`", // "count" is a known integer property
		"Note  *string `json:\"note,omitzero\"`",
		"Items []string `json:\"items\"`",
		"type MixedOutput struct",
		"Array  []MixedOutputArrayItem",
		"Things []Thing `json:\"things\"`",
		"// Holds 1 to 2 items.",
		"X *bool `json:\"-x,omitzero\"`",
		"Mode FooInputMode `json:\"mode,omitempty\"`",
		"FooInputModeFast FooInputMode = \"fast\"",
		"// Deprecated: Use mode.",
		"Ok bool `json:\"ok,omitzero\"`",
		"Bag map[string]any `json:\"bag,omitempty\"`",
		"Extra map[string]any `json:\"-\"`",
		"reflect.TypeFor[FooInput]()",
		"reflect.TypeFor[MixedOutput]()",
	} {
		if !strings.Contains(code, space.ReplaceAllString(want, " ")) {
			t.Errorf("generated code lacks %q", want)
		}
	}
	if strings.Contains(code, "Gone") {
		t.Error("@deprecated property was not skipped")
	}
}

func TestUnusedOverrideIsAnError(t *testing.T) {
	_, err := generate([]byte(sample), "0.0.0", map[string]string{"Nope.x": "Y"})
	if err == nil || !strings.Contains(err.Error(), "unused typeNames") {
		t.Fatalf("err = %v, want unused typeNames error", err)
	}
}

func TestPascal(t *testing.T) {
	for in, want := range map[string]string{
		"file_path":                 "FilePath",
		"taskId":                    "TaskID",
		"sessionUrl":                "SessionURL",
		"image/jpeg":                "ImageJpeg",
		"CONFIRMED":                 "Confirmed",
		"auto-merge-enabled":        "AutoMergeEnabled",
		"ephemeral_1h_input_tokens": "Ephemeral1hInputTokens",
		"sha256":                    "SHA256",
		"acceptEdits":               "AcceptEdits",
	} {
		if got := pascal(in); got != want {
			t.Errorf("pascal(%q) = %q, want %q", in, got, want)
		}
	}
}
