// Command gen generates claude/tools/zz_generated.go from the sdk-tools.d.ts
// file shipped in the @anthropic-ai/claude-agent-sdk npm package.
//
// The TypeScript declarations are parsed directly (no Node.js or TypeScript
// toolchain is needed). By default the npm tarball for -sdk-version is
// downloaded from the public registry; pass -in to use a local copy instead:
//
//	go run ./internal/gen -sdk-version 0.3.286 -out zz_generated.go
//	go run ./internal/gen -sdk-version 0.3.286 -in /path/to/package/sdk-tools.d.ts -out zz_generated.go
//
// The registry URL can be overridden with the NPM_CONFIG_REGISTRY environment
// variable. With GEN_DEBUG=1 every nested type is listed on stderr as
// "GoName autoName TypeScriptPath" and name conflicts are tolerated, which
// helps when adding typeNames entries after a schema change.
package main

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	in := flag.String("in", "", "path to sdk-tools.d.ts (default: download the npm tarball for -sdk-version)")
	version := flag.String("sdk-version", "", "@anthropic-ai/claude-agent-sdk version the schema comes from (required)")
	out := flag.String("out", "zz_generated.go", "output Go file")
	flag.Parse()
	if *version == "" {
		fail(errors.New("-sdk-version is required"))
	}
	var src []byte
	var err error
	if *in != "" {
		src, err = os.ReadFile(*in)
	} else {
		src, err = download(*version)
	}
	if err != nil {
		fail(err)
	}
	code, err := Generate(src, *version)
	if err != nil {
		fail(err)
	}
	if err := os.WriteFile(*out, code, 0o644); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "gen:", err)
	os.Exit(1)
}

// download fetches package/sdk-tools.d.ts out of the npm tarball for version.
func download(version string) ([]byte, error) {
	registry := strings.TrimRight(os.Getenv("NPM_CONFIG_REGISTRY"), "/")
	if registry == "" {
		registry = "https://registry.npmjs.org"
	}
	url := fmt.Sprintf("%s/@anthropic-ai/claude-agent-sdk/-/claude-agent-sdk-%s.tgz", registry, version)
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return extractDTS(resp.Body)
}

func extractDTS(r io.Reader) ([]byte, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, errors.New("package/sdk-tools.d.ts not found in tarball")
		}
		if err != nil {
			return nil, err
		}
		if h.Name == "package/sdk-tools.d.ts" {
			return io.ReadAll(tr)
		}
	}
}
