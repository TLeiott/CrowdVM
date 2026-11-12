package guest

// Docker across stop, resume, fork and deploy (Boat snapshots/whats-saved
// #docker): containers that ran start again by themselves, named volumes and
// the images a container or a Compose project uses are kept, the build cache
// and the other images are not.
//
// A stop runs `docker stop` (Boat: each container gets 30 s), which makes
// Docker treat the containers as stopped by hand: without help only
// `--restart always` ones would come back. So the guest records the running
// containers on the disk before every stop and before every live snapshot
// (forks, saved snapshots, periodic snapshots) and starts them again at the
// next boot from that disk.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Test knobs: the docker CLI and the daemon socket whose presence means
// Docker is installed and enabled.
var (
	dockerBin  = "docker"
	dockerSock = "/var/run/docker.sock"
	// dockerWait bounds how long a boot waits for dockerd before it gives up
	// starting the recorded containers.
	dockerWait = 3 * time.Minute
)

const (
	dockerStateFile = "docker-running.json"
	EvDockerStarted = "docker.restarted" // {started:[names], failed:[{container,error}]}
)

// dockerState is <data dir>/docker-running.json.
type dockerState struct {
	SavedAt    time.Time `json:"savedAt"`
	Containers []string  `json:"containers"` // full ids, oldest first
}

// dockerOK: Docker is there. Unprivileged agents (tests, the process
// driver) never touch the host's real Docker.
func (a *Agent) dockerOK() bool {
	if a.unpriv && dockerBin == "docker" {
		return false
	}
	if _, err := exec.LookPath(dockerBin); err != nil {
		return false
	}
	_, err := os.Stat(dockerSock)
	return err == nil
}

func (a *Agent) docker(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, dockerBin, args...)
	cmd.WaitDelay = time.Second
	return cmd.Output()
}

func (a *Agent) dockerStatePath() string { return filepath.Join(a.data, dockerStateFile) }

func (a *Agent) loadDockerState() dockerState {
	var s dockerState
	if b, err := os.ReadFile(a.dockerStatePath()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

// recordDocker saves which containers run now, oldest first, so the next
// boot from this disk starts them again. A failed query keeps the previous
// record; an unchanged list is not rewritten.
func (a *Agent) recordDocker(ctx context.Context, timeout time.Duration) []string {
	if !a.dockerOK() {
		return nil
	}
	if a.dockerRestoring.Load() {
		// The recorded containers are not started again yet: what runs
		// now is not the list (bugs-r20-opus 4).
		return a.loadDockerState().Containers
	}
	out, err := a.docker(ctx, timeout, "ps", "-q", "--no-trunc")
	if err != nil {
		a.log.Printf("docker ps: %v", err)
		return nil
	}
	ids := strings.Fields(string(out))
	slices.Reverse(ids) // docker ps lists the newest first; start dependencies first
	old := a.loadDockerState()
	if slices.Equal(old.Containers, ids) && (len(ids) > 0 || !old.SavedAt.IsZero()) {
		return ids
	}
	if len(ids) == 0 && old.SavedAt.IsZero() {
		return ids // nothing ran, nothing recorded: no write
	}
	b, _ := json.Marshal(dockerState{SavedAt: time.Now().UTC(), Containers: ids})
	if err := a.writeFileAtomic(a.dockerStatePath(), b, 0o644, false); err != nil {
		a.log.Printf("record docker containers: %v", err)
	}
	return ids
}

// stopDocker records and stops every running container (Boat stop step 2:
// 30 s grace each, in parallel as `docker stop` does). Returns how many.
func (a *Agent) stopDocker(ctx context.Context) int {
	ids := a.recordDocker(ctx, 10*time.Second)
	if len(ids) == 0 {
		return 0
	}
	if out, err := a.docker(ctx, 45*time.Second, append([]string{"stop", "-t", "30"}, ids...)...); err != nil {
		a.log.Printf("docker stop: %v: %s", err, strings.TrimSpace(lastLines(string(out), 3)))
	}
	return len(ids)
}

// waitDocker waits until dockerd answers.
func (a *Agent) waitDocker(ctx context.Context, max time.Duration) bool {
	deadline := time.Now().Add(max)
	for {
		if _, err := a.docker(ctx, 5*time.Second, "info", "--format", "{{.ID}}"); err == nil {
			return true
		}
		if time.Now().After(deadline) || !sleepFor(ctx, time.Second) {
			return false
		}
	}
}

func sleepFor(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// restartDocker starts the containers recorded on this disk that do not
// run (after a resume, fork, deploy or recover; Boat: "Docker containers
// that ran start again by themselves"). Containers Docker already started
// (restart policy) and removed ones are skipped. Emits docker.restarted.
func (a *Agent) restartDocker(ctx context.Context) (started []string, failed []map[string]string) {
	st := a.loadDockerState()
	if len(st.Containers) == 0 || !a.dockerOK() {
		return nil, nil
	}
	if !a.waitDocker(ctx, dockerWait) {
		a.log.Printf("docker: daemon not answering after %s; %d recorded containers not started", dockerWait, len(st.Containers))
		return nil, nil
	}
	// One inspect for all: missing containers only drop out of the output.
	out, _ := a.docker(ctx, 30*time.Second, append([]string{"container", "inspect", "--format", "{{.Id}} {{.State.Running}} {{.Name}}"}, st.Containers...)...)
	running, names := map[string]bool{}, map[string]string{}
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) == 3 {
			running[f[0]], names[f[0]] = f[1] == "true", strings.TrimPrefix(f[2], "/")
		}
	}
	for _, id := range st.Containers {
		r, exists := running[id]
		if !exists || r {
			continue
		}
		if b, err := a.docker(ctx, 2*time.Minute, "start", id); err != nil {
			msg := strings.TrimSpace(lastLines(string(b)+errText(err), 2))
			if o, _ := a.docker(ctx, 10*time.Second, "container", "inspect", "--format", "{{.State.Running}}", id); strings.TrimSpace(string(o)) == "true" {
				started = append(started, names[id]) // Docker started it meanwhile
				continue
			}
			failed = append(failed, map[string]string{"container": names[id], "error": msg})
			continue
		}
		started = append(started, names[id])
	}
	if len(started)+len(failed) > 0 {
		a.log.Printf("docker: started %d recorded containers again (%d failed)", len(started), len(failed))
		a.Emit(EvDockerStarted, map[string]any{"started": started, "failed": failed})
	}
	return started, failed
}

func errText(err error) string {
	if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
		return string(ee.Stderr)
	}
	return err.Error()
}

// pruneDocker deletes what a snapshot does not keep: the build cache and,
// with images, the images that no container and no Compose project (one
// that has a container) uses. When a Compose project's images cannot be
// listed, no image is deleted.
func (a *Agent) pruneDocker(ctx context.Context, images bool, errs *[]string) {
	if !a.dockerOK() {
		return
	}
	if out, err := a.docker(ctx, 2*time.Minute, "builder", "prune", "-af"); err != nil {
		*errs = append(*errs, "docker builder prune: "+strings.TrimSpace(lastLines(string(out)+errText(err), 2)))
	}
	if images {
		a.pruneImages(ctx, false)
	}
}

// pruneImages deletes the images no container and no Compose project uses.
// pulledOnly keeps images built in the sandbox and dangling layers: they
// cannot be downloaded again. When that is unknown, nothing is deleted.
func (a *Agent) pruneImages(ctx context.Context, pulledOnly bool) {
	var pulled func(id, digest string) bool
	if pulledOnly {
		if pulled = a.pulledFilter(ctx); pulled == nil {
			return
		}
	}
	keep, ok := a.dockerImagesInUse(ctx)
	if !ok {
		return
	}
	out, err := a.docker(ctx, 30*time.Second, "image", "ls", "--no-trunc", "--format", "{{.ID}} {{.Repository}}:{{.Tag}} {{.Digest}}")
	if err != nil {
		return
	}
	for _, ref := range unusedImages(imageLines(string(out), pulled), keep) {
		if b, err := a.docker(ctx, time.Minute, "rmi", ref); err != nil {
			a.log.Printf("docker rmi %s: %s", ref, strings.TrimSpace(lastLines(string(b)+errText(err), 1)))
		}
	}
	if !pulledOnly {
		_, _ = a.docker(ctx, time.Minute, "image", "prune", "-f") // layers the untagging left dangling
	}
}

// pulledFilter tells images pulled from a registry from images built in the
// sandbox, or returns nil when it cannot. The classic store shows no digest
// for a built image. The containerd image store (Docker 29's default) shows
// one for every image, so there an image counts as pulled only when
// containerd recorded a registry source for it.
func (a *Agent) pulledFilter(ctx context.Context) func(id, digest string) bool {
	info, err := a.docker(ctx, 30*time.Second, "info", "-f", "{{json .DriverStatus}}")
	if err != nil {
		return nil
	}
	if !strings.Contains(string(info), "io.containerd.snapshotter") {
		return func(_, digest string) bool { return strings.HasPrefix(digest, "sha256:") }
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "ctr", "-n", "moby", "content", "ls").Output()
	if err != nil {
		a.log.Printf("disk cleanup: ctr content ls: %v; keeping every image", err)
		return nil
	}
	src := registrySourced(string(out))
	return func(id, _ string) bool { return src[id] }
}

// registrySourced: digests in `ctr content ls` output that carry a
// containerd.io/distribution.source label (set by pulls, never by builds).
func registrySourced(ls string) map[string]bool {
	m := map[string]bool{}
	for _, l := range strings.Split(ls, "\n") {
		if f := strings.Fields(l); len(f) > 0 && strings.Contains(l, "containerd.io/distribution.source.") {
			m[f[0]] = true
		}
	}
	return m
}

// imageLines turns "ID REF DIGEST" lines into the "ID REF" lines unusedImages
// reads, keeping only the images pulled accepts (all when pulled is nil).
func imageLines(ls string, pulled func(id, digest string) bool) string {
	var sb strings.Builder
	for _, l := range strings.Split(ls, "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		if pulled != nil && !pulled(f[0], strings.Join(f[2:], "")) {
			continue
		}
		sb.WriteString(f[0] + " " + f[1] + "\n")
	}
	return sb.String()
}

// dockerImagesInUse: ids and normalized names of the images containers use,
// plus the images of every Compose project that has a container.
func (a *Agent) dockerImagesInUse(ctx context.Context) (map[string]bool, bool) {
	keep := map[string]bool{}
	ids, err := a.docker(ctx, 30*time.Second, "ps", "-aq", "--no-trunc")
	if err != nil {
		return nil, false
	}
	if len(strings.Fields(string(ids))) == 0 {
		return keep, true
	}
	const f = `{{.Image}}|{{.Config.Image}}|{{index .Config.Labels "com.docker.compose.project"}}|` +
		`{{index .Config.Labels "com.docker.compose.project.working_dir"}}|{{index .Config.Labels "com.docker.compose.project.config_files"}}`
	out, err := a.docker(ctx, 30*time.Second, append([]string{"container", "inspect", "--format", f}, strings.Fields(string(ids))...)...)
	if err != nil {
		return nil, false
	}
	projects := map[string][2]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		p := strings.SplitN(l, "|", 5)
		if len(p) != 5 {
			continue
		}
		keep[p[0]], keep[normImage(p[1])] = true, true
		if p[2] != "" {
			projects[p[2]] = [2]string{p[3], p[4]}
		}
	}
	for name, pc := range projects {
		args := []string{"compose", "-p", name}
		if pc[0] != "" {
			args = append(args, "--project-directory", pc[0])
		}
		for _, cf := range strings.Split(pc[1], ",") {
			if cf = strings.TrimSpace(cf); cf != "" {
				args = append(args, "-f", cf)
			}
		}
		b, err := a.docker(ctx, 30*time.Second, append(args, "--profile", "*", "config", "--images")...) // every profile: a profile-gated service's image is kept too
		if err != nil {
			a.log.Printf("docker compose project %s: images unknown (%s); keeping every image", name, strings.TrimSpace(lastLines(errText(err), 1)))
			return nil, false
		}
		for _, img := range strings.Fields(string(b)) {
			keep[normImage(img)] = true
		}
	}
	return keep, true
}

// normImage gives one spelling per image name: "nginx" and
// "docker.io/library/nginx:latest" are both "nginx:latest".
func normImage(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "sha256:") {
		return s
	}
	s = strings.TrimPrefix(strings.TrimPrefix(s, "docker.io/"), "library/")
	if strings.Contains(s, "@") {
		return s
	}
	if i := strings.LastIndex(s, ":"); i < 0 || strings.Contains(s[i:], "/") {
		s += ":latest"
	}
	return s
}

// unusedImages picks, from `docker image ls --format '{{.ID}} {{.Repository}}:{{.Tag}}'`
// lines, the references to delete: tags (or ids of untagged images) whose
// image id and name are both unused.
func unusedImages(ls string, keep map[string]bool) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range strings.Split(ls, "\n") {
		id, ref, ok := strings.Cut(strings.TrimSpace(l), " ")
		if !ok || keep[id] {
			continue
		}
		if strings.HasSuffix(ref, ":<none>") {
			ref = id
		} else if keep[normImage(ref)] {
			continue
		}
		if !seen[ref] {
			seen[ref] = true
			out = append(out, ref)
		}
	}
	return out
}
