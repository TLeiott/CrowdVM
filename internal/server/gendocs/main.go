// Command gendocs writes docs/guide/cli-reference.md from the crowdvm CLI's
// command tree (go generate ./docs). It drives the CLI only through its
// public entry point: each command's --help text, and cobra's hidden
// __complete command to list subcommands, so the reference is exactly what
// `crowdvm <cmd> --help` prints.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/crowdvm/crowdvm/internal/cli"
)

var cfgDir string

// run executes the CLI in-process with args and returns its stdout.
func run(args ...string) string {
	var out, errb bytes.Buffer
	a := &cli.App{In: strings.NewReader(""), Out: &out, Err: &errb, ConfigDir: cfgDir,
		Getenv: func(k string) string {
			if k == "CROWDVM_NO_UPDATE" {
				return "1"
			}
			return ""
		}}
	a.Execute(context.Background(), args)
	return out.String()
}

var moreRe = regexp.MustCompile(`(?m)^Use "crowdvm .*--help" for more information about a command\.\s*$`)

// children lists the visible subcommands of path (none for leaf commands,
// whose __complete would complete arguments instead).
func children(path []string, help string) []string {
	if !moreRe.MatchString(help) {
		return nil
	}
	args := append([]string{"__complete"}, path...)
	var names []string
	for _, line := range strings.Split(run(append(args, "")...), "\n") {
		if line == "" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "-") || strings.HasPrefix(line, "Completion ended") {
			continue
		}
		name, _, _ := strings.Cut(line, "\t")
		if name == "help" || name == "completion" || name == "__complete" {
			continue
		}
		names = append(names, name)
	}
	return names
}

// trimGlobal drops the repeated "Global Flags" block below the root.
func trimGlobal(help string) string {
	if i := strings.Index(help, "\nGlobal Flags:\n"); i >= 0 {
		rest := help[i+1:]
		end := strings.Index(rest, "\n\n")
		if end < 0 {
			return strings.TrimRight(help[:i], "\n") + "\n"
		}
		return help[:i+1] + rest[end+2:]
	}
	return help
}

func main() {
	out := flag.String("out", "guide/cli-reference.md", "output file")
	flag.Parse()
	dir, err := os.MkdirTemp("", "crowdvm-gendocs-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)
	cfgDir = dir

	var b strings.Builder
	b.WriteString("# CLI reference\n\n")
	b.WriteString("Every `crowdvm` command with the text `crowdvm <command> --help` prints. ")
	b.WriteString("Generated from the CLI's command tree by `go generate ./docs`; do not edit by hand.\n\n")
	b.WriteString("Global flags work on every command: `--api-url`, `--org`, `--json` (on by default when the output is piped), `--profile`, `--no-update`.\n\n")
	var walk func(path []string, depth int)
	walk = func(path []string, depth int) {
		help := run(append(append([]string{}, path...), "--help")...)
		if strings.TrimSpace(help) == "" {
			return
		}
		title := strings.Join(append([]string{"crowdvm"}, path...), " ")
		level := "##"
		if depth > 1 {
			level = "###"
		}
		if depth > 0 {
			help = trimGlobal(help)
		}
		help = moreRe.ReplaceAllString(help, "")
		fmt.Fprintf(&b, "%s %s\n\n```text\n%s\n```\n\n", level, title, strings.TrimRight(help, "\n"))
		for _, c := range children(path, run(append(append([]string{}, path...), "--help")...)) {
			walk(append(append([]string{}, path...), c), depth+1)
		}
	}
	walk(nil, 0)
	if err := os.WriteFile(*out, []byte(b.String()), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
