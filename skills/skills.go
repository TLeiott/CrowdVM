// Package skills embeds the agent skills that ship with CrowdVM: one folder
// per skill, each a SKILL.md (frontmatter name + description) plus optional
// references/. The guest agent installs them into every harness's skills
// directory inside each sandbox (internal/guest/skills.go); `crowdvm skill
// install` installs them for agents outside a sandbox.
package skills

import (
	"embed"
	"io/fs"
	"path"
	"sort"
)

//go:embed crowdvm
var FS embed.FS

// Names lists the skills (top-level folders).
func Names() []string {
	ents, _ := fs.ReadDir(FS, ".")
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// Files returns every file of every skill, keyed by its slash path below the
// skills directory ("crowdvm/SKILL.md", "crowdvm/references/cli.md").
func Files() map[string][]byte {
	out := map[string][]byte{}
	_ = fs.WalkDir(FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(p) == ".go" {
			return err
		}
		b, err := FS.ReadFile(p)
		if err == nil {
			out[p] = b
		}
		return err
	})
	return out
}
