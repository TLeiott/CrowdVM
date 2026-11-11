package guest

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Ignore files, highest priority first: one per folder is used.
var ignoreNames = []string{".crowdvmignore", ".boatignore", ".boxignore", ".oneignore"}

// Folders not searched for ignore files (they are still matched by rules).
var ignoreSkipDirs = map[string]bool{"node_modules": true, ".next": true, "target": true, "vendor": true}

const ignoreMaxDepth = 6

type ignoreRule struct {
	base    string // folder of the ignore file
	neg     bool
	dirOnly bool
	re      *regexp.Regexp
}

// parseIgnore parses gitignore syntax.
func parseIgnore(base, text string) []ignoreRule {
	var rules []ignoreRule
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if !strings.HasSuffix(line, "\\ ") {
			line = strings.TrimRight(line, " \t")
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		r := ignoreRule{base: base}
		if strings.HasPrefix(line, "!") {
			r.neg, line = true, line[1:]
		} else if strings.HasPrefix(line, `\!`) || strings.HasPrefix(line, `\#`) {
			line = line[1:]
		}
		if strings.HasSuffix(line, "/") {
			r.dirOnly, line = true, strings.TrimSuffix(line, "/")
		}
		if line == "" {
			continue
		}
		anchored := strings.Contains(line, "/")
		line = strings.TrimPrefix(line, "/")
		expr := globToRegexp(line)
		if !anchored {
			expr = "(?:.*/)?" + expr
		}
		re, err := regexp.Compile("^" + expr + "$")
		if err != nil {
			continue
		}
		r.re = re
		rules = append(rules, r)
	}
	return rules
}

func globToRegexp(g string) string {
	var b strings.Builder
	for i := 0; i < len(g); i++ {
		c := g[i]
		switch {
		case strings.HasPrefix(g[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(g[i:], "/**") && i+3 == len(g):
			b.WriteString("/.*")
			i += 2
		case strings.HasPrefix(g[i:], "**"):
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		case c == '\\' && i+1 < len(g):
			i++
			b.WriteString(regexp.QuoteMeta(string(g[i])))
		case c == '[':
			j := strings.IndexByte(g[i+1:], ']')
			if j < 0 {
				b.WriteString(`\[`)
				continue
			}
			class := g[i+1 : i+1+j]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i += j + 1
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}

// ignored applies rules (outer files first); the last match wins.
func ignored(rules []ignoreRule, p string, isDir bool) bool {
	res := false
	for _, r := range rules {
		rel, ok := under(p, r.base)
		if !ok || rel == "" || (r.dirOnly && !isDir) {
			continue
		}
		if r.re.MatchString(filepath.ToSlash(rel)) {
			res = !r.neg
		}
	}
	return res
}

func readIgnoreFile(dir string) (string, bool) {
	for _, n := range ignoreNames {
		if b, err := os.ReadFile(filepath.Join(dir, n)); err == nil {
			return string(b), true
		}
	}
	return "", false
}

// cleanIgnored deletes everything under root matched by ignore files and
// returns the deleted paths. .git folders are never deleted; skip holds real
// paths that are never touched (e.g. the agent's own data dir).
func cleanIgnored(root string, skip map[string]bool) []string {
	var removed []string
	var walk func(dir string, depth int, rules []ignoreRule)
	walk = func(dir string, depth int, rules []ignoreRule) {
		if depth <= ignoreMaxDepth {
			if text, ok := readIgnoreFile(dir); ok {
				rules = append(append([]ignoreRule(nil), rules...), parseIgnore(dir, text)...)
			}
		}
		ents, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range ents {
			p := filepath.Join(dir, e.Name())
			if e.Name() == ".git" || skip[p] {
				continue
			}
			isDir := e.IsDir()
			if len(rules) > 0 && ignored(rules, p, isDir) {
				if os.RemoveAll(p) == nil {
					removed = append(removed, p)
				}
				continue
			}
			if isDir && (len(rules) > 0 || (depth < ignoreMaxDepth && !ignoreSkipDirs[e.Name()])) {
				walk(p, depth+1, rules)
			}
		}
	}
	walk(root, 0, nil)
	return removed
}
