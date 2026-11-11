package server

import (
	"regexp"
	"strings"
)

// .env handling for environment contents. Entries keep their raw text so
// single-variable edits leave comments and other lines alone.

type envEntry struct {
	Key, Value string // Key "" = comment / blank / unparsable line
	Raw        string // original text including the trailing newline
}

var envSafeValueRe = regexp.MustCompile(`^[A-Za-z0-9_./:@,+=%^-]*$`)

// envParseDotenv splits .env text into entries. Supported: KEY=value,
// export KEY=value, "double quoted" (with \n \t \" \\ escapes, may span
// lines), 'single quoted' (literal) and inline " #" comments.
func envParseDotenv(s string) []envEntry {
	var out []envEntry
	for len(s) > 0 {
		line, rest, _ := strings.Cut(s, "\n")
		raw := line
		if len(rest) > 0 || strings.HasSuffix(s, "\n") {
			raw += "\n"
		}
		t := strings.TrimSpace(line)
		t = strings.TrimPrefix(t, "export ")
		k, v, ok := strings.Cut(t, "=")
		k = strings.TrimSpace(k)
		if !ok || strings.HasPrefix(t, "#") || !envKeyRe.MatchString(k) {
			out = append(out, envEntry{Raw: raw})
			s = rest
			continue
		}
		v = strings.TrimLeft(v, " \t")
		switch {
		case strings.HasPrefix(v, `"`):
			// The value may continue on the next lines until the closing quote.
			body := v[1:]
			full := body
			consumed := rest
			for {
				if end, ok := envDQEnd(full); ok {
					v = envDQUnescape(full[:end])
					break
				}
				if consumed == "" {
					v = envDQUnescape(full)
					break
				}
				next, r2, _ := strings.Cut(consumed, "\n")
				full += "\n" + next
				raw += next
				if len(r2) > 0 || strings.HasSuffix(consumed, "\n") {
					raw += "\n"
				}
				consumed = r2
			}
			rest = consumed
		case strings.HasPrefix(v, "'"):
			if end := strings.Index(v[1:], "'"); end >= 0 {
				v = v[1 : 1+end]
			} else {
				v = v[1:]
			}
		default:
			if i := strings.Index(v, " #"); i >= 0 {
				v = v[:i]
			}
			v = strings.TrimSpace(v)
		}
		out = append(out, envEntry{Key: k, Value: v, Raw: raw})
		s = rest
	}
	return out
}

// envDQEnd finds the closing unescaped double quote.
func envDQEnd(s string) (int, bool) {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i, true
		}
	}
	return 0, false
}

func envDQUnescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			default:
				b.WriteByte(s[i])
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// envVars returns the variables of .env text (later definitions win).
func envVars(s string) map[string]string {
	m := map[string]string{}
	for _, e := range envParseDotenv(s) {
		if e.Key != "" {
			m[e.Key] = e.Value
		}
	}
	return m
}

func envFormatLine(k, v string) string {
	if envSafeValueRe.MatchString(v) {
		return k + "=" + v + "\n"
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`)
	return k + `="` + r.Replace(v) + "\"\n"
}

// envSetVar sets (value != nil) or removes key in .env text.
func envSetVar(s, key string, value *string) string {
	var b strings.Builder
	done := false
	for _, e := range envParseDotenv(s) {
		if e.Key != key {
			b.WriteString(e.Raw)
			continue
		}
		if value != nil && !done {
			if !strings.HasSuffix(e.Raw, "\n") && e.Raw != "" {
				b.WriteString(strings.TrimSuffix(envFormatLine(key, *value), "\n"))
			} else {
				b.WriteString(envFormatLine(key, *value))
			}
			done = true
		}
	}
	out := b.String()
	if value != nil && !done {
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		out += envFormatLine(key, *value)
	}
	return out
}
