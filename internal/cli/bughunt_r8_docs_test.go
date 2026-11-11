package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// TestR8DocsCommandsExist checks every `crowdvm ...` command line shown in the
// user docs (fenced code blocks and inline code spans) against the real
// command tree: each command path must exist and `--help` must work, and each
// documented flag must be defined on that command (own, inherited or global).
func TestR8DocsCommandsExist(t *testing.T) {
	root := filepath.Join("..", "..")
	var files []string
	for _, g := range []string{"docs/guide/*.md", "README.md", "skills/*/*.md", "skills/*/references/*.md"} {
		m, _ := filepath.Glob(filepath.Join(root, g))
		files = append(files, m...)
	}
	if len(files) < 5 {
		t.Fatalf("docs not found: %v", files)
	}
	tree := r8Root(t)
	helped := map[string]bool{}
	checked := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(root, f)
		for _, cl := range r8CommandLines(string(b)) {
			for _, seg := range r8Segments(cl.text) {
				checked++
				for _, p := range r8Check(tree, seg) {
					t.Errorf("%s:%d: %s\n\t%s", rel, cl.line, p, strings.TrimSpace(cl.text))
				}
				path := r8Path(tree, seg)
				key := strings.Join(path, " ")
				if helped[key] {
					continue
				}
				helped[key] = true
				if out, code := r8Help(path); code != 0 || !strings.Contains(out, "Usage:") {
					t.Errorf("%s:%d: `crowdvm %s --help` exit %d: %s", rel, cl.line, key, code, out)
				}
			}
		}
	}
	if checked < 100 {
		t.Fatalf("only %d command lines found; extractor broken?", checked)
	}
	keys := make([]string, 0, len(helped))
	for k := range helped {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	t.Logf("%d command lines, %d distinct paths", checked, len(keys))
}

// TestR8DocsHelpExamples checks the `crowdvm ...` lines in every command's
// Long and Example help text the same way (they are what --help shows).
func TestR8DocsHelpExamples(t *testing.T) {
	tree := r8Root(t)
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, txt := range []string{c.Long, c.Example} {
			for _, l := range strings.Split(txt, "\n") {
				for _, seg := range r8Segments(l) {
					for _, p := range r8Check(tree, seg) {
						t.Errorf("help of `%s`: %s\n\t%s", c.CommandPath(), p, strings.TrimSpace(l))
					}
				}
			}
		}
		for _, s := range c.Commands() {
			walk(s)
		}
	}
	walk(tree)
}

// TestR8DocsExtractor guards the checker itself against silent passes.
func TestR8DocsExtractor(t *testing.T) {
	tree := r8Root(t)
	md := "x `crowdvm api-key frob` y\n```sh\n$ id=$(crowdvm new --nope | jq .id)\nFOO=1 crowdvm exec \"$id\" --cwd d \\\n  ls -la && crowdvm --json list -Z\ncrowdvm repos sources [add --kind K | rm ID]\n```\n"
	var got []string
	for _, cl := range r8CommandLines(md) {
		for _, seg := range r8Segments(cl.text) {
			got = append(got, r8Check(tree, seg)...)
		}
	}
	want := []string{
		"unknown subcommand \"frob\" of `crowdvm api-key`",
		"unknown flag --nope for `crowdvm new`",
		"unknown shorthand -Z for `crowdvm list`",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

type r8Line struct {
	line int
	text string
}

var r8Span = regexp.MustCompile("`(crowdvm [^`]+)`")

// r8CommandLines returns fenced-code lines (with `\` continuations joined) and
// inline code spans that mention crowdvm.
func r8CommandLines(md string) []r8Line {
	var out []r8Line
	lines := strings.Split(md, "\n")
	inFence := false
	fence := ""
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		tl := strings.TrimSpace(l)
		if !inFence && (strings.HasPrefix(tl, "```") || strings.HasPrefix(tl, "~~~")) {
			inFence, fence = true, tl[:3]
			continue
		}
		if inFence {
			if strings.HasPrefix(tl, fence) {
				inFence = false
				continue
			}
			start := i
			for strings.HasSuffix(strings.TrimRight(l, " "), "\\") && i+1 < len(lines) {
				i++
				l = strings.TrimSuffix(strings.TrimRight(l, " "), "\\") + " " + lines[i]
			}
			if strings.Contains(l, "crowdvm") {
				out = append(out, r8Line{start + 1, l})
			}
			continue
		}
		for _, m := range r8Span.FindAllStringSubmatch(l, -1) {
			out = append(out, r8Line{i + 1, m[1]})
		}
	}
	return out
}

// r8Tokens splits a shell line into words; operators become their own tokens.
func r8Tokens(s string) []string {
	var toks []string
	var cur strings.Builder
	has := false
	flush := func() {
		if has {
			toks = append(toks, cur.String())
		}
		cur.Reset()
		has = false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				j = len(s) - i - 1
			}
			cur.WriteString(s[i+1 : i+1+j])
			has = true
			i += j + 1
		case c == '"':
			j := i + 1
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			if j > len(s) {
				j = len(s)
			}
			cur.WriteString(s[i+1 : j])
			has = true
			i = j
		case c == '\\' && i+1 < len(s):
			cur.WriteByte(s[i+1])
			has = true
			i++
		case c == ' ' || c == '\t':
			flush()
		case c == '#' && !has:
			flush()
			return toks
		case c == '|' || c == ';' || c == '&' || c == '(' || c == ')' || c == '`':
			flush()
			toks = append(toks, ";")
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			flush()
			toks = append(toks, ";")
			i++
		default:
			cur.WriteByte(c)
			has = true
		}
	}
	flush()
	return toks
}

var r8Assign = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// r8Segments returns the argument lists of every `crowdvm` invocation in a line.
func r8Segments(line string) [][]string {
	t := strings.TrimSpace(line)
	for _, p := range []string{"$ ", "PS> ", "> ", "# "} {
		t = strings.TrimPrefix(t, p)
	}
	var segs [][]string
	var cur []string
	toks := append(r8Tokens(t), ";")
	for _, tok := range toks {
		if tok != ";" {
			cur = append(cur, tok)
			continue
		}
		for len(cur) > 0 && (r8Assign.MatchString(cur[0]) || cur[0] == "sudo" || cur[0] == "env" || cur[0] == "time" || cur[0] == "!" || cur[0] == "-" || cur[0] == "watch" || cur[0] == "exec" || cur[0] == "{") {
			cur = cur[1:]
		}
		if len(cur) > 0 && cur[0] == "crowdvm" && !r8Prose(cur) {
			var args []string
			for _, a := range cur[1:] {
				// Synopsis notation: [add --kind K | rm ID], [--flag V]...
				a = strings.TrimSuffix(strings.TrimRight(strings.TrimLeft(a, "["), "]."), "]")
				if a != "" {
					args = append(args, a)
				}
			}
			segs = append(segs, args)
		}
		cur = nil
	}
	return segs
}

// r8Prose spots sentences that start with "crowdvm" in help text, like
// "crowdvm creates and drives persistent Ubuntu VMs ...": many plain words, no flags.
func r8Prose(toks []string) bool {
	if len(toks) < 6 {
		return false
	}
	for _, t := range toks {
		if strings.HasPrefix(t, "-") || strings.ContainsAny(t, "<>[]$") {
			return false
		}
	}
	return true
}

func r8Root(t *testing.T) *cobra.Command {
	a := &App{Getenv: func(string) string { return "" }}
	a.defaults()
	root := a.rootCmd()
	root.InitDefaultHelpCmd()
	root.InitDefaultVersionFlag()
	var init func(c *cobra.Command)
	init = func(c *cobra.Command) {
		c.InitDefaultHelpFlag()
		for _, s := range c.Commands() {
			init(s)
		}
	}
	init(root)
	return root
}

func r8Sub(c *cobra.Command, name string) *cobra.Command {
	for _, s := range c.Commands() {
		if s.Name() == name || s.HasAlias(name) {
			return s
		}
	}
	return nil
}

func r8Lookup(c *cobra.Command, name string) *pflag.Flag {
	for p := c; p != nil; p = p.Parent() {
		if f := p.Flags().Lookup(name); f != nil && (p == c || p.PersistentFlags().Lookup(name) != nil) {
			return f
		}
		if f := p.PersistentFlags().Lookup(name); f != nil {
			return f
		}
	}
	return nil
}

func r8Short(c *cobra.Command, s string) *pflag.Flag {
	for p := c; p != nil; p = p.Parent() {
		if f := p.Flags().ShorthandLookup(s); f != nil && (p == c || p.PersistentFlags().ShorthandLookup(s) != nil) {
			return f
		}
		if f := p.PersistentFlags().ShorthandLookup(s); f != nil {
			return f
		}
	}
	return nil
}

func r8Interspersed(c *cobra.Command) bool {
	return reflect.ValueOf(c.Flags()).Elem().FieldByName("interspersed").Bool()
}

func r8IsPlaceholder(s string) bool {
	switch s {
	case "command", "flags", "options", "args", "subcommand":
		return true
	}
	return strings.ContainsAny(s, "<>[]{}$.:/=@\"'*") || strings.ToUpper(s) == s
}

// r8Path resolves the leading subcommand words of args.
func r8Path(root *cobra.Command, args []string) []string {
	c, path := root, []string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			if f := r8FlagFor(c, a); f != nil && f.NoOptDefVal == "" && !strings.Contains(a, "=") {
				i++
			}
			continue
		}
		s := r8Sub(c, a)
		if s == nil {
			break
		}
		c = s
		path = append(path, s.Name())
	}
	return path
}

func r8FlagFor(c *cobra.Command, a string) *pflag.Flag {
	if strings.HasPrefix(a, "--") {
		name, _, _ := strings.Cut(a[2:], "=")
		return r8Lookup(c, name)
	}
	if len(a) == 2 {
		return r8Short(c, a[1:])
	}
	return nil
}

// r8Check returns the mismatches of one invocation.
func r8Check(root *cobra.Command, args []string) []string {
	var probs []string
	c := root
	descending := true
	positionals := 0
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if strings.HasPrefix(a, ">") || strings.HasPrefix(a, "2>") || strings.HasPrefix(a, "<") && !strings.HasSuffix(a, ">") {
			break
		}
		if len(a) > 1 && a[0] == '-' && !(a[1] >= '0' && a[1] <= '9') {
			if r8IsPlaceholderFlag(a) {
				continue
			}
			if strings.HasPrefix(a, "--") {
				name, _, hasVal := strings.Cut(a[2:], "=")
				f := r8Lookup(c, name)
				if f == nil {
					probs = append(probs, "unknown flag --"+name+" for `"+r8Name(c)+"`")
					continue
				}
				if f.NoOptDefVal == "" && !hasVal {
					i++
				}
				continue
			}
			sh := a[1:]
			for j := 0; j < len(sh); j++ {
				f := r8Short(c, sh[j:j+1])
				if f == nil {
					probs = append(probs, "unknown shorthand -"+sh[j:j+1]+" for `"+r8Name(c)+"`")
					break
				}
				if f.NoOptDefVal == "" {
					if j == len(sh)-1 {
						i++
					}
					break
				}
			}
			continue
		}
		if descending {
			if s := r8Sub(c, a); s != nil {
				c = s
				continue
			}
			descending = false
			if c.HasAvailableSubCommands() && !c.Runnable() && !r8IsPlaceholder(a) {
				probs = append(probs, "unknown subcommand \""+a+"\" of `"+r8Name(c)+"`")
				break
			}
		}
		positionals++
		if !r8Interspersed(c) {
			// exec/run/ssh: flags may follow the sandbox id; the command starts after.
			if positionals == 1 && (looksLikeID(a) || strings.HasPrefix(a, "$") || strings.HasPrefix(a, "<") || a == "ID") {
				continue
			}
			break
		}
	}
	return probs
}

// r8IsPlaceholderFlag skips generic placeholders like --<flag> or -[flags].
func r8IsPlaceholderFlag(a string) bool {
	return strings.ContainsAny(a, "<>[]") || a == "--flag" || a == "--flags"
}

func r8Name(c *cobra.Command) string {
	return c.CommandPath()
}

func r8Help(path []string) (string, int) {
	var out bytes.Buffer
	dir, _ := os.MkdirTemp("", "r8docs")
	defer os.RemoveAll(dir)
	vars := map[string]string{"CROWDVM_CONFIG_DIR": dir, "CROWDVM_NO_UPDATE": "1", "CROWDVM_NO_BROWSER": "1"}
	a := &App{Out: &out, Err: &out, Getenv: func(k string) string { return vars[k] }}
	code := a.Execute(context.Background(), append(append([]string{}, path...), "--help"))
	return out.String(), code
}
