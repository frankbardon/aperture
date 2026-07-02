// Command errcodes generates the committed mdBook error-code reference from the
// error Registry. It reads github.com/frankbardon/aperture/errors and emits a
// deterministic Markdown table (sorted by code) to
// docs/src/reference/error-codes.md.
//
// The output is committed; regenerate it on demand with `make docs-gen` (or
// `go generate ./...`). There is no CI drift gate — contributors run the
// command when the Registry changes.
package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/frankbardon/aperture/errors"
)

//go:generate go run . -o ../../../docs/src/reference/error-codes.md

const header = "<!-- DO NOT EDIT — regenerate with `make docs-gen` -->\n"

func main() {
	out := "docs/src/reference/error-codes.md"
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-o", "--out":
			if i+1 >= len(args) {
				fatal("missing value for %s", args[i])
			}
			out = args[i+1]
			i++
		default:
			fatal("unknown argument %q", args[i])
		}
	}

	page := render()

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		fatal("create output directory: %v", err)
	}
	if err := os.WriteFile(out, page, 0o644); err != nil {
		fatal("write %s: %v", out, err)
	}
}

// render builds the full Markdown page as bytes. It is deterministic: codes are
// stable-sorted lexically so regeneration produces no spurious diff.
func render() []byte {
	codes := make([]errors.Code, len(errors.AllCodes))
	copy(codes, errors.AllCodes)
	sort.SliceStable(codes, func(i, j int) bool { return codes[i] < codes[j] })

	var b bytes.Buffer
	b.WriteString(header)
	b.WriteString("\n")
	b.WriteString("# Error Codes\n\n")
	b.WriteString("Every failure surfaced by Aperture is an `APERTURE_*` coded error. ")
	b.WriteString("Each code carries a canonical message and, where operator action is ")
	b.WriteString("meaningful, one or more fixup hints. This page is generated from the ")
	b.WriteString("error `Registry` in `errors/codes.go`.\n\n")
	b.WriteString("| Code | Message | Fixups |\n")
	b.WriteString("| --- | --- | --- |\n")

	for _, code := range codes {
		meta := errors.Registry[code]
		b.WriteString("| `")
		b.WriteString(mdCell(string(code)))
		b.WriteString("` | ")
		b.WriteString(mdCell(meta.Message))
		b.WriteString(" | ")
		b.WriteString(fixupCell(meta))
		b.WriteString(" |\n")
	}

	return b.Bytes()
}

// fixupCell renders the fixups for a code. FixupNotApplicable is rendered
// explicitly as an em dash so a code with no operator remediation is visible
// rather than blank.
func fixupCell(meta errors.Metadata) string {
	if meta.FixupNotApplicable {
		return "— _not applicable_"
	}
	if len(meta.Fixups) == 0 {
		return "—"
	}
	parts := make([]string, len(meta.Fixups))
	for i, f := range meta.Fixups {
		parts[i] = mdCell(f)
	}
	return strings.Join(parts, "<br>")
}

// mdCell escapes the characters that would break a Markdown table cell: angle
// brackets are HTML-escaped so literal tokens like <acct> render verbatim
// instead of being parsed as HTML tags, pipes are escaped, and newlines become
// <br> so multi-line content stays on one row.
func mdCell(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", "<br>")
	return s
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "errcodes: "+format+"\n", args...)
	os.Exit(1)
}
