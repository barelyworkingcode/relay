// The WKWebView loads the document via loadHTMLString:baseURL:nil, which
// cannot resolve relative <script src> URLs, so the bundle must be inlined
// rather than referenced. web/dist/settings.html is committed to the repo so
// a plain `go build` (which //go:embed's it) still works without running
// this generator.
package main

import (
	"log"
	"os"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

const (
	entryPoint = "web/src/entry.js"
	shellPath  = "web/shell.html"
	outPath    = "web/dist/settings.html"
	marker     = "<!--RELAY_BUNDLE-->"
)

func main() {
	result := api.Build(api.BuildOptions{
		EntryPoints: []string{entryPoint},
		Bundle:      true,
		Format:      api.FormatIIFE,
		// ES2017 keeps async/await + spread untouched while staying within what
		// every WebKit the app targets (and goja, in tests) parses cleanly.
		Target:            api.ES2017,
		Write:             false,
		LogLevel:          api.LogLevelWarning,
		MinifyWhitespace:  false, // keep readable in the WKWebView inspector
		MinifyIdentifiers: false,
	})
	if len(result.Errors) > 0 {
		for _, e := range result.Errors {
			loc := ""
			if e.Location != nil {
				loc = e.Location.File + ":" + itoa(e.Location.Line)
			}
			log.Printf("esbuild error: %s %s", loc, e.Text)
		}
		log.Fatalf("bundle failed with %d error(s)", len(result.Errors))
	}
	if len(result.OutputFiles) != 1 {
		log.Fatalf("expected 1 output file, got %d", len(result.OutputFiles))
	}
	js := string(result.OutputFiles[0].Contents)

	shell, err := os.ReadFile(shellPath)
	if err != nil {
		log.Fatalf("read %s: %v", shellPath, err)
	}
	if !strings.Contains(string(shell), marker) {
		log.Fatalf("%s does not contain marker %q", shellPath, marker)
	}

	// Neutralize any literal </script> in the JS so the inline script can't be
	// terminated early by the HTML parser.
	js = strings.ReplaceAll(js, "</script", "<\\/script")
	inline := "<script>\n" + js + "</script>"
	out := strings.Replace(string(shell), marker, inline, 1)

	// Write atomically (temp + rename in the same dir) so an interrupted run can
	// never leave a truncated, committed web/dist/settings.html behind.
	tmp := outPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(out), 0o644); err != nil {
		log.Fatalf("write %s: %v", tmp, err)
	}
	if err := os.Rename(tmp, outPath); err != nil {
		log.Fatalf("rename %s -> %s: %v", tmp, outPath, err)
	}
	log.Printf("wrote %s (%d bytes, bundle %d bytes)", outPath, len(out), len(js))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
