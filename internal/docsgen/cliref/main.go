// Command cliref generates the committed mdBook CLI reference from the
// urfave/cli/v3 command tree assembled by internal/cli.NewApp. It walks the
// live command definitions — commands, subcommands, and flags — and emits a
// single deterministic Markdown page (commands sorted by name, flags sorted by
// name) to docs/src/reference/cli.md.
//
// Because it reads the actual command/flag definitions, the reference reflects
// reality automatically: positional arguments come from each command's
// ArgsUsage, and the shared --seed/--store/--account/--principal flags appear
// in the flag table of whichever command declares them (they are per-command,
// not app-level globals).
//
// The output is committed; regenerate it on demand with `make docs-gen` (or
// `go generate ./...`). There is no CI drift gate — contributors run the
// command when the CLI tree changes.
package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	acli "github.com/frankbardon/aperture/internal/cli"

	ucli "github.com/urfave/cli/v3"
)

//go:generate go run . -o ../../../docs/src/reference/cli.md

const header = "<!-- DO NOT EDIT — regenerate with `make docs-gen` -->\n"

func main() {
	out := "docs/src/reference/cli.md"
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

// render builds the full Markdown page as bytes. It is deterministic: commands
// and flags are stable-sorted lexically so regeneration produces no spurious
// diff. version is fixed to a placeholder so the assembled tree (and thus the
// page) does not vary with the build's -ldflags stamp.
func render() []byte {
	app := acli.NewApp("<version>")

	var b bytes.Buffer
	b.WriteString(header)
	b.WriteString("\n")
	b.WriteString("# Command-Line Reference\n\n")
	b.WriteString("**Audience:** operators and integrators driving Aperture from a shell.\n\n")
	b.WriteString(fmt.Sprintf("`%s` — %s. This page is generated from the ", app.Name, app.Usage))
	b.WriteString("urfave/cli command tree in `internal/cli` (`cli.NewApp`); every ")
	b.WriteString("command, subcommand, and flag below is read from the live definitions.\n\n")

	// Global flags: aperture attaches flags per command rather than persistently
	// on the root, so the shared --seed/--store/--account/--principal options are
	// documented in each command's own flag table. Reflect whatever the root
	// actually declares rather than hardcoding a global set.
	b.WriteString("## Global flags\n\n")
	globals := visibleFlags(app.Flags)
	if len(globals) == 0 {
		b.WriteString("`aperture` declares no persistent global flags. The commonly shared ")
		b.WriteString("options — `--seed`, `--store`, `--account`, and `--principal` ")
		b.WriteString("(the acting principal on mutations, sourced from `APERTURE_PRINCIPAL`) ")
		b.WriteString("— are defined per command and appear in each command's flag table below.\n\n")
	} else {
		writeFlagTable(&b, globals)
	}

	// Command index.
	cmds := visibleCommands(app.Commands)
	b.WriteString("## Commands\n\n")
	b.WriteString("| Command | Summary |\n")
	b.WriteString("| --- | --- |\n")
	for _, c := range cmds {
		b.WriteString("| [`")
		b.WriteString(mdCell(c.Name))
		b.WriteString("`](#aperture-")
		b.WriteString(anchor(c.Name))
		b.WriteString(") | ")
		b.WriteString(mdCell(c.Usage))
		b.WriteString(" |\n")
	}
	b.WriteString("\n")

	// Per-command detail sections.
	for _, c := range cmds {
		writeCommand(&b, c, []string{app.Name}, 2)
	}

	return b.Bytes()
}

// writeCommand renders one command (and recursively its subcommands). path is
// the ancestor names ending at this command's parent; level is the Markdown
// heading depth (## for top-level commands, ### for their subcommands, capped at
// h6 for defensively deep trees).
func writeCommand(b *bytes.Buffer, c *ucli.Command, parents []string, level int) {
	full := append(append([]string{}, parents...), c.Name)
	title := strings.Join(full, " ")

	if level > 6 {
		level = 6
	}
	b.WriteString(strings.Repeat("#", level))
	b.WriteString(" `")
	b.WriteString(title)
	b.WriteString("`\n\n")

	if c.Usage != "" {
		b.WriteString(c.Usage)
		b.WriteString("\n\n")
	}
	if c.Description != "" {
		b.WriteString(c.Description)
		b.WriteString("\n\n")
	}

	// Synopsis.
	b.WriteString("```\n")
	b.WriteString(title)
	if len(visibleFlags(c.Flags)) > 0 {
		b.WriteString(" [options]")
	}
	if len(visibleCommands(c.Commands)) > 0 {
		b.WriteString(" <command>")
	}
	if c.ArgsUsage != "" {
		b.WriteString(" ")
		b.WriteString(c.ArgsUsage)
	}
	b.WriteString("\n```\n\n")

	if flags := visibleFlags(c.Flags); len(flags) > 0 {
		writeFlagTable(b, flags)
	}

	for _, sub := range visibleCommands(c.Commands) {
		writeCommand(b, sub, full, level+1)
	}
}

// writeFlagTable renders the Name | Aliases | Type | Default | Usage table for
// a pre-sorted, visible flag slice.
func writeFlagTable(b *bytes.Buffer, flags []ucli.Flag) {
	b.WriteString("| Name | Aliases | Type | Default | Usage |\n")
	b.WriteString("| --- | --- | --- | --- | --- |\n")
	for _, f := range flags {
		names := f.Names()
		name := ""
		aliases := "—"
		if len(names) > 0 {
			name = "--" + names[0]
			if len(names) > 1 {
				alts := make([]string, len(names)-1)
				for i, a := range names[1:] {
					alts[i] = "`--" + a + "`"
				}
				aliases = strings.Join(alts, ", ")
			}
		}

		typ, def, usage := "—", "—", ""
		if df, ok := f.(ucli.DocGenerationFlag); ok {
			if t := df.TypeName(); t != "" {
				typ = t
			}
			def = defaultText(df)
			usage = df.GetUsage()
			if env := df.GetEnvVars(); len(env) > 0 {
				for i := range env {
					env[i] = "`" + env[i] + "`"
				}
				usage = strings.TrimSpace(usage)
				usage += " (env: " + strings.Join(env, ", ") + ")"
			}
		}
		if rf, ok := f.(ucli.RequiredFlag); ok && rf.IsRequired() {
			usage = strings.TrimSpace(usage) + " (**required**)"
		}

		b.WriteString("| `")
		b.WriteString(mdCell(name))
		b.WriteString("` | ")
		b.WriteString(aliases)
		b.WriteString(" | ")
		b.WriteString(mdCell(typ))
		b.WriteString(" | ")
		b.WriteString(def)
		b.WriteString(" | ")
		b.WriteString(mdCell(strings.TrimSpace(usage)))
		b.WriteString(" |\n")
	}
	b.WriteString("\n")
}

// defaultText mirrors urfave/cli's own default rendering: prefer explicit
// DefaultText, fall back to the value's string form, else an em dash.
func defaultText(df ucli.DocGenerationFlag) string {
	if s := df.GetDefaultText(); s != "" {
		return "`" + mdCell(s) + "`"
	}
	if df.TakesValue() {
		if v := df.GetValue(); v != "" {
			return "`" + mdCell(v) + "`"
		}
	}
	return "—"
}

// visibleCommands returns the non-hidden commands sorted by name for a stable,
// byte-identical page across regenerations.
func visibleCommands(in []*ucli.Command) []*ucli.Command {
	out := make([]*ucli.Command, 0, len(in))
	for _, c := range in {
		if c.Hidden {
			continue
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// visibleFlags returns the non-hidden flags sorted by primary name.
func visibleFlags(in []ucli.Flag) []ucli.Flag {
	out := make([]ucli.Flag, 0, len(in))
	for _, f := range in {
		if vf, ok := f.(ucli.VisibleFlag); ok && !vf.IsVisible() {
			continue
		}
		out = append(out, f)
	}
	sort.SliceStable(out, func(i, j int) bool { return primaryName(out[i]) < primaryName(out[j]) })
	return out
}

func primaryName(f ucli.Flag) string {
	if n := f.Names(); len(n) > 0 {
		return n[0]
	}
	return ""
}

// anchor converts a command name to its GitHub/mdBook heading anchor fragment.
func anchor(name string) string {
	return strings.ToLower(strings.ReplaceAll(name, " ", "-"))
}

// mdCell escapes the characters that would break a Markdown table cell: angle
// brackets are HTML-escaped so literal tokens like <principal> render verbatim
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
	fmt.Fprintf(os.Stderr, "cliref: "+format+"\n", args...)
	os.Exit(1)
}
