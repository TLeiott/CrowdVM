// Package image builds and lists CrowdVM base images: the Ubuntu 24.04 cloud
// image provisioned by the scripts in images/ (embedded), flattened into a
// standalone zstd-compressed qcow2 plus a JSON metadata file.
//
// Build boots the cloud image once under QEMU (KVM, else TCG) with a NoCloud
// seed whose user-data runs images/scripts/build.sh; the VM reports tool
// versions and a success marker on its second serial port and powers off.
package image

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/images"
)

// Options for Build. Zero values get sensible defaults.
type Options struct {
	Profile    string // "minimal" | "full" (default "full")
	CloudImage string // Ubuntu 24.04 cloud image (qcow2 or raw); never modified
	OutDir     string // receives <ImageID>.qcow2 and <ImageID>.json
	ImageID    string // default DefaultID(Profile)
	CPUs       int    // build VM vCPUs (default 4)
	MemoryMiB  int    // build VM memory (default 4096)
	DiskGiB    int    // virtual disk size of the image (default 4 minimal, 24 full)
	Timeout    time.Duration
	Force      bool      // overwrite an existing image with the same id
	KeepWork   bool      // keep the work dir (overlay, seed, console log) on success
	Log        io.Writer // build log + guest serial console (default: discard)
	QEMU       string    // default "qemu-system-x86_64" (from PATH)
	QEMUImg    string    // default "qemu-img"
}

// Tool is one installed tool and its version, as reported by the build VM.
type Tool struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Metadata is written next to the image as <id>.json.
type Metadata struct {
	ID               string    `json:"id"`
	Profile          string    `json:"profile"`
	Version          string    `json:"version"` // images.Version (provisioning assets)
	OS               string    `json:"os"`
	Arch             string    `json:"arch"`
	Format           string    `json:"format"`
	Compression      string    `json:"compression"`
	BaseImage        string    `json:"baseImage"` // file name of the cloud image
	SizeBytes        int64     `json:"sizeBytes"` // qcow2 file size
	VirtualSizeBytes int64     `json:"virtualSizeBytes"`
	SHA256           string    `json:"sha256"`
	CreatedAt        time.Time `json:"createdAt"`
	BuildSeconds     float64   `json:"buildSeconds"`
	Accel            string    `json:"accel"`
	Tools            []Tool    `json:"tools"`
}

// Result of a successful Build.
type Result struct {
	Path     string // <OutDir>/<id>.qcow2
	MetaPath string // <OutDir>/<id>.json
	Meta     Metadata
}

const (
	okMarker   = "CROWDVM-BUILD-OK"
	failMarker = "CROWDVM-BUILD-FAILED"
	guestDir   = "/var/lib/crowdvm-build"
)

// DefaultID is the conventional image id of a profile, e.g.
// "ubuntu-24.04-crowdvm-v1" (full) or "ubuntu-24.04-crowdvm-minimal-v1".
func DefaultID(profile string) string {
	if profile == "full" {
		return "ubuntu-24.04-crowdvm-v" + images.Version
	}
	return "ubuntu-24.04-crowdvm-" + profile + "-v" + images.Version
}

func (o *Options) defaults() error {
	if o.Profile == "" {
		o.Profile = "full"
	}
	if o.Profile != "minimal" && o.Profile != "full" {
		return fmt.Errorf("unknown profile %q (want minimal or full)", o.Profile)
	}
	if o.CloudImage == "" || o.OutDir == "" {
		return errors.New("CloudImage and OutDir are required")
	}
	if o.ImageID == "" {
		o.ImageID = DefaultID(o.Profile)
	}
	if o.ImageID != filepath.Base(o.ImageID) || strings.HasPrefix(o.ImageID, ".") {
		return fmt.Errorf("bad image id %q", o.ImageID)
	}
	full := o.Profile == "full"
	o.CPUs = or(o.CPUs, 4)
	o.MemoryMiB = or(o.MemoryMiB, 4096)
	o.DiskGiB = or(o.DiskGiB, map[bool]int{false: 4, true: 24}[full])
	if o.Timeout == 0 {
		o.Timeout = map[bool]time.Duration{false: 20 * time.Minute, true: 120 * time.Minute}[full]
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
	o.QEMU = orStr(o.QEMU, "qemu-system-x86_64")
	o.QEMUImg = orStr(o.QEMUImg, "qemu-img")
	return nil
}

// Build provisions a base image. See the package doc.
func Build(ctx context.Context, o Options) (res Result, err error) {
	if err := o.defaults(); err != nil {
		return res, err
	}
	start := time.Now()
	logf := func(f string, a ...any) { fmt.Fprintf(o.Log, "image: "+f+"\n", a...) }

	if err := os.MkdirAll(o.OutDir, 0o755); err != nil {
		return res, err
	}
	res.Path = filepath.Join(o.OutDir, o.ImageID+".qcow2")
	res.MetaPath = filepath.Join(o.OutDir, o.ImageID+".json")
	if _, err := os.Stat(res.Path); err == nil && !o.Force {
		return res, fmt.Errorf("image %s already exists (use Force to rebuild)", res.Path)
	}
	cloud, err := filepath.Abs(o.CloudImage)
	if err != nil {
		return res, err
	}
	baseInfo, err := qemuImgInfo(ctx, o.QEMUImg, cloud)
	if err != nil {
		return res, fmt.Errorf("cloud image: %w", err)
	}

	work := filepath.Join(o.OutDir, ".build-"+o.ImageID)
	_ = os.RemoveAll(work)
	if err := os.MkdirAll(work, 0o755); err != nil {
		return res, err
	}
	defer func() {
		if err == nil && !o.KeepWork {
			os.RemoveAll(work)
		} else if err != nil {
			err = fmt.Errorf("%w (work dir kept: %s)", err, work)
		}
	}()

	// 1. Overlay on the cloud image, resized.
	disk := filepath.Join(work, "disk.qcow2")
	if err := run(ctx, o.Log, o.QEMUImg, "create", "-q", "-f", "qcow2", "-F", baseInfo.Format,
		"-b", cloud, disk, fmt.Sprintf("%dG", o.DiskGiB)); err != nil {
		return res, err
	}

	// 2. NoCloud seed.
	seed := filepath.Join(work, "seed.iso")
	userData, err := UserData(o.Profile)
	if err != nil {
		return res, err
	}
	metaData := []byte(fmt.Sprintf("instance-id: crowdvm-build-%d\nlocal-hostname: crowdvm-build\n", start.Unix()))
	if err := writeSeed(ctx, o.Log, seed, userData, metaData); err != nil {
		return res, fmt.Errorf("seed: %w", err)
	}

	// 3. Boot and provision.
	accel := Accel()
	logf("building %s (profile %s, %s, %d vCPU, %d MiB, %d GiB disk)", o.ImageID, o.Profile, accel, o.CPUs, o.MemoryMiB, o.DiskGiB)
	resultFile := filepath.Join(work, "result.txt")
	consoleFile, err := os.Create(filepath.Join(work, "console.log"))
	if err != nil {
		return res, err
	}
	defer consoleFile.Close()
	tail := &tailBuffer{max: 8 << 10}
	vmCtx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	args := qemuArgs(accel, o.CPUs, o.MemoryMiB, "",
		"-drive", "file="+disk+",if=virtio,format=qcow2,discard=unmap,detect-zeroes=unmap,cache=unsafe",
		"-drive", "file="+seed+",if=virtio,format=raw,readonly=on",
		"-serial", "stdio", "-serial", "file:"+resultFile)
	cmd := exec.CommandContext(vmCtx, o.QEMU, args...)
	cmd.Stdout = io.MultiWriter(o.Log, consoleFile, tail)
	cmd.Stderr = cmd.Stdout
	runErr := cmd.Run()
	if vmCtx.Err() != nil {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		return res, fmt.Errorf("build VM timed out after %s; console tail:\n%s", o.Timeout, tail)
	}
	if runErr != nil {
		return res, fmt.Errorf("qemu: %w; console tail:\n%s", runErr, tail)
	}
	tools, err := parseResult(resultFile)
	if err != nil {
		return res, fmt.Errorf("%w; console tail:\n%s", err, tail)
	}
	logf("provisioned in %s", time.Since(start).Round(time.Second))

	// 4. Flatten + compress into a standalone image.
	part := res.Path + ".part"
	if err := run(ctx, o.Log, o.QEMUImg, "convert", "-p", "-c", "-m", "8", "-O", "qcow2",
		"-o", "compression_type=zstd", disk, part); err != nil {
		os.Remove(part)
		return res, err
	}
	info, err := qemuImgInfo(ctx, o.QEMUImg, part)
	if err != nil {
		return res, err
	}
	sum, size, err := sha256File(part)
	if err != nil {
		return res, err
	}
	res.Meta = Metadata{
		ID: o.ImageID, Profile: o.Profile, Version: images.Version,
		OS: "ubuntu-24.04", Arch: "amd64", Format: "qcow2", Compression: "zstd",
		BaseImage: filepath.Base(cloud), SizeBytes: size, VirtualSizeBytes: info.VirtualSize,
		SHA256: sum, CreatedAt: time.Now().UTC().Round(time.Second),
		BuildSeconds: time.Since(start).Round(time.Second).Seconds(), Accel: accel, Tools: tools,
	}
	js, _ := json.MarshalIndent(res.Meta, "", "  ")
	if err := os.WriteFile(res.MetaPath+".part", append(js, '\n'), 0o644); err != nil {
		return res, err
	}
	if err := os.Rename(part, res.Path); err != nil {
		return res, err
	}
	if err := os.Rename(res.MetaPath+".part", res.MetaPath); err != nil {
		return res, err
	}
	logf("done: %s (%d MiB, sha256 %s) in %s", res.Path, size>>20, sum[:12], time.Since(start).Round(time.Second))
	return res, nil
}

// List returns the metadata of every image in dir (sorted by id), skipping
// metadata files whose qcow2 is missing.
func List(dir string) ([]Metadata, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var out []Metadata
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var m Metadata
		if err := json.Unmarshal(b, &m); err != nil || m.ID == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, m.ID+".qcow2")); err != nil {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// UserData returns the NoCloud #cloud-config for a profile: the embedded
// images/ assets as write_files under /var/lib/crowdvm-build, then runcmd
// build.sh <profile>. JSON is valid YAML, so no YAML encoder is needed.
func UserData(profile string) ([]byte, error) {
	type file struct {
		Path        string `json:"path"`
		Encoding    string `json:"encoding"`
		Content     string `json:"content"`
		Permissions string `json:"permissions"`
	}
	var files []file
	err := fs.WalkDir(images.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := images.FS.ReadFile(p)
		if err != nil {
			return err
		}
		perm := "0644"
		if bytes.HasPrefix(b, []byte("#!")) {
			perm = "0755"
		}
		files = append(files, file{guestDir + "/" + p, "b64", base64.StdEncoding.EncodeToString(b), perm})
		return nil
	})
	if err != nil {
		return nil, err
	}
	cfg := map[string]any{
		"users":          []any{}, // no default "ubuntu" user; build.sh creates "user"
		"disable_root":   true,
		"ssh_pwauth":     false,
		"package_update": false,
		"write_files":    files,
		"runcmd":         [][]string{{"bash", guestDir + "/scripts/build.sh", profile}},
	}
	js, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	return append([]byte("#cloud-config\n"), append(js, '\n')...), nil
}

// Accel returns "kvm" when /dev/kvm is usable, else "tcg".
func Accel() string {
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err == nil {
		f.Close()
		return "kvm"
	}
	return "tcg"
}

// qemuArgs: a headless q35 VM with user networking (netOpts appended to
// "-netdev user,id=n0", e.g. ",hostfwd=..."); extra args appended.
func qemuArgs(accel string, cpus, memMiB int, netOpts string, extra ...string) []string {
	cpu := "host"
	if accel != "kvm" {
		cpu = "max"
	}
	args := []string{
		"-nodefaults", "-no-user-config", "-display", "none", "-no-reboot",
		"-machine", "q35,accel=" + accel, "-cpu", cpu,
		"-smp", fmt.Sprint(cpus), "-m", fmt.Sprint(memMiB),
		"-netdev", "user,id=n0" + netOpts, "-device", "virtio-net-pci,netdev=n0",
		"-device", "virtio-rng-pci",
	}
	return append(args, extra...)
}

func writeSeed(ctx context.Context, log io.Writer, path string, userData, metaData []byte) error {
	if xorriso, err := exec.LookPath("xorriso"); err == nil {
		dir := filepath.Join(filepath.Dir(path), "seed")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "user-data"), userData, 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "meta-data"), metaData, 0o644); err != nil {
			return err
		}
		return run(ctx, log, xorriso, "-as", "mkisofs", "-quiet", "-o", path, "-V", "cidata", "-J", "-R", dir)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := WriteISO(f, "cidata", []ISOFile{{"user-data", userData}, {"meta-data", metaData}}); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func parseResult(path string) ([]Tool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("no build result: %w", err)
	}
	var tools []Tool
	ok := false
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == okMarker:
			ok = true
		case strings.HasPrefix(line, failMarker):
			return nil, fmt.Errorf("provisioning failed: %s", line)
		default:
			if k, v, found := strings.Cut(line, "="); found && k != "" {
				tools = append(tools, Tool{k, v})
			}
		}
	}
	if !ok {
		return nil, errors.New("provisioning did not report success (VM powered off early?)")
	}
	return tools, nil
}

type imgInfo struct {
	Format      string `json:"format"`
	VirtualSize int64  `json:"virtual-size"`
}

func qemuImgInfo(ctx context.Context, qemuImg, path string) (imgInfo, error) {
	var info imgInfo
	out, err := exec.CommandContext(ctx, qemuImg, "info", "--output=json", path).Output()
	if err != nil {
		return info, fmt.Errorf("qemu-img info %s: %w", path, err)
	}
	return info, json.Unmarshal(out, &info)
}

func run(ctx context.Context, log io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stdout = log
	cmd.Stderr = io.MultiWriter(log, &stderr)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", filepath.Base(name), strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func sha256File(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = append([]byte(nil), t.b[len(t.b)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.b) }

func or(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func orStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
