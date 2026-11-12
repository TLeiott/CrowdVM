package skills

import (
	"bytes"
	"strings"
	"testing"
)

func TestSkillsHaveFrontmatter(t *testing.T) {
	files := Files()
	for _, name := range Names() {
		md, ok := files[name+"/SKILL.md"]
		if !ok {
			t.Fatalf("%s: no SKILL.md", name)
		}
		if !bytes.HasPrefix(md, []byte("---\n")) || !strings.Contains(string(md), "\nname: "+name+"\n") ||
			!strings.Contains(string(md), "\ndescription:") {
			t.Errorf("%s/SKILL.md: frontmatter needs name: %s and description", name, name)
		}
		// The description is what makes an agent load the skill. A harness that
		// parses the frontmatter as strict YAML drops a skill it cannot parse, so
		// the description must be a plain scalar on one line (": " inside one is
		// a YAML error). The Agent Skills format caps it at 1024 characters.
		desc := ""
		for _, l := range strings.Split(strings.SplitN(string(md), "\n---", 2)[0], "\n") {
			if v, ok := strings.CutPrefix(l, "description: "); ok {
				desc = v
			}
		}
		switch {
		case desc == "" || len(desc) > 1024:
			t.Errorf("%s/SKILL.md: description must be one line of 1 to 1024 characters, got %d", name, len(desc))
		case strings.Contains(desc, ": ") || strings.Contains(desc, " #") || strings.HasSuffix(desc, ":") ||
			strings.ContainsAny(desc[:1], "\"'`[]{}>|*&!%@?-#,"):
			t.Errorf("%s/SKILL.md: description is not a plain YAML scalar (no \": \", no \" #\", no leading quote or indicator)", name)
		}
		// every references/ link resolves
		for _, l := range strings.Split(string(md), "](") {
			if i := strings.Index(l, ")"); i > 0 && strings.HasPrefix(l, "references/") {
				if _, ok := files[name+"/"+l[:i]]; !ok {
					t.Errorf("%s/SKILL.md links missing %s", name, l[:i])
				}
			}
		}
	}
	if len(Names()) == 0 {
		t.Fatal("no skills embedded")
	}
}
