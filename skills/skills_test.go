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
