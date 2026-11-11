package guest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"

	"github.com/crowdvm/crowdvm/skills"
)

// harnessSkillDirs are the skills directories of the harnesses in the image
// (Boat integrated-agents/customize#skills: "Every harness on a sandbox
// already has a skills directory with the `boat` skill in it"), relative to
// the home folder. ~/.agents/skills is the shared one (Codex, Kimi Code,
// Mistral Vibe and others read it too).
var harnessSkillDirs = []string{
	".claude/skills",          // Claude Code
	".codex/skills",           // Codex
	".pi/agent/skills",        // pi
	".config/opencode/skills", // OpenCode
	".prime/agent/skills",     // Prime Agent
	".kimi-code/skills",       // Kimi Code
	".vibe/skills",            // Mistral Vibe
	".agents/skills",          // shared
	".hermes/skills",          // Hermes Agent
	".openclaw/skills",        // OpenClaw
	".cursor/skills",          // Cursor CLI
}

func (a *Agent) managedSkillsPath() string { return filepath.Join(a.data, "managed-skills.json") }

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// installSkills writes the skills that ship with this guest (skills/, the
// `crowdvm` skill) into every harness's skills directory at each boot, so
// a new guest brings the new text. A file the user edited or deleted is
// left as it is; files of an older version that this one no longer ships
// are deleted when unchanged. Managed files are tracked by sha256 in
// <data dir>/managed-skills.json.
func (a *Agent) installSkills() {
	files := skills.Files()
	old := map[string]string{}
	if b, err := os.ReadFile(a.managedSkillsPath()); err == nil {
		_ = json.Unmarshal(b, &old)
	}
	next := map[string]string{}
	for _, dir := range harnessSkillDirs {
		for rel, content := range files {
			p := filepath.Join(a.home, dir, filepath.FromSlash(rel))
			want := sha(content)
			cur, err := os.ReadFile(p)
			switch {
			case err == nil && sha(cur) == want:
				next[p] = want
				continue
			case err == nil && old[p] != sha(cur):
				continue // the user's own version
			case err != nil && old[p] != "":
				next[p] = old[p] // the user deleted it: keep it deleted
				continue
			}
			if err := a.mkdirAll(filepath.Dir(p), 0o755); err != nil {
				a.log.Printf("skill %s: %v", p, err)
				continue
			}
			if err := a.writeFileAtomic(p, content, 0o644, true); err != nil {
				a.log.Printf("skill %s: %v", p, err)
				continue
			}
			next[p] = want
		}
	}
	for p, h := range old {
		if _, ok := next[p]; ok {
			continue
		}
		if cur, err := os.ReadFile(p); err == nil && sha(cur) == h {
			_ = os.Remove(p) // dropped by this version, unchanged by the user
			_ = os.Remove(filepath.Dir(p))
		}
	}
	if maps.Equal(old, next) {
		return
	}
	b, _ := json.Marshal(next)
	if err := a.writeFileAtomic(a.managedSkillsPath(), b, 0o644, false); err != nil {
		a.log.Printf("managed skills: %v", err)
	}
}
