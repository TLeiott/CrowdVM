//go:build linux

package guest

// Pre-boot of image v4 (docs/design/secure-layer.md §5.4, one-disk variant).
//
// The image's grub starts /usr/local/lib/crowdvm/preinit as PID 1. It mounts
// the config drive and runs `crowdvm-guest preinit` from it. For a channel
// sandbox this redeems the boot ticket over the guest channel, opens (on the
// first boot: creates) a LUKS2 partition at the end of the disk with the key
// the server sends, and mounts an overlay root: the image's partition
// read-only below, the encrypted partition above. /home and /var/lib/docker
// sit on the encrypted ext4 directly (Docker's overlay2 refuses overlayfs).
// Then it hands over to systemd. Everything the sandbox writes lands in
// ciphertext; the node never sees the key. The agent later reconnects with
// the session this step leaves in /run/crowdvm.
//
// Fail closed: an encrypted disk never boots without its key, and a wrong
// key never formats anything.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
	"golang.org/x/sys/unix"
)

const (
	dataPartName = "crowdvm-data"
	dataPartType = "CA7D7CCB-63ED-4C53-861C-1742536059CC" // LUKS
	dataMount    = "/run/crowdvm/data"
	newRootDir   = "/mnt"
	minDataBytes = 2 << 30
	runState     = "/run/crowdvm"
)

// Preinit runs as PID 1 before systemd and ends by exec'ing /sbin/init. It
// returns only on a fatal error, after which the caller powers off.
func Preinit() error {
	logger := log.New(os.Stderr, "crowdvm-preinit: ", 0)
	var stub proto.GuestConfig
	b, err := os.ReadFile("/run/crowdvm/cfg/config.json")
	if err == nil {
		err = json.Unmarshal(b, &stub)
	}
	disk, err2 := rootDisk()
	if err2 != nil {
		logger.Printf("root disk: %v; booting as is", err2)
		return execInit()
	}
	part, perr := dataPartition(disk)
	if err != nil || stub.Channel == nil {
		// No guest channel (an older node): the key cannot come. An
		// encrypted sandbox must not boot as an empty machine (and then
		// write plaintext over its base).
		if part != "" && blkType(part) == "crypto_LUKS" {
			return errors.New("this sandbox's disk is encrypted, but its node did not give it a guest channel to fetch the key over; refusing to boot")
		}
		return execInit()
	}
	if perr != nil {
		return fmt.Errorf("partition table of %s: %w", disk, perr)
	}
	mode := "luks"
	if part == "" {
		if part, err = addDataPartition(disk); errors.Is(err, errNoRoom) {
			// An older disk (its root partition was grown): nothing was
			// written, it stays unencrypted.
			logger.Printf("no encrypted data partition: %v (the disk stays unencrypted)", err)
			mode, part = "plain", ""
		} else if err != nil {
			return fmt.Errorf("creating the encrypted data partition: %w", err)
		}
	}
	if err := netUp(); err != nil {
		return fmt.Errorf("network: %w", err)
	}
	key, err := redeem(stub, mode, logger)
	netDown()
	if err != nil {
		return err
	}
	if mode == "plain" {
		return execInit()
	}
	if len(key) < 32 {
		return errors.New("the server sent no disk key")
	}
	logger.Printf("boot ticket redeemed; opening the encrypted data partition %s", part)
	if err := openData(disk, part, key, logger); err != nil {
		return err
	}
	if err := overlayRoot(); err != nil {
		return err
	}
	logger.Printf("starting systemd on the encrypted overlay root")
	return switchRoot()
}

// redeem fetches the config over the channel (retrying until it works) and
// returns the disk key. The session stays in /run/crowdvm for the agent.
func redeem(stub proto.GuestConfig, mode string, logger *log.Logger) ([]byte, error) {
	c := &Channel{stub: stub, statePath: filepath.Join(runState, "channel.json"), log: logger, disk: mode}
	backoff := 500 * time.Millisecond
	for {
		_, err := c.connect(context.Background())
		if err == nil {
			break
		}
		logger.Printf("guest channel: %v (retrying)", err)
		if strings.Contains(err.Error(), "server refused") {
			// ticket_used (someone else redeemed it), expired, unknown
			// sandbox ...: no retry makes this boot valid.
			return nil, err
		}
		time.Sleep(backoff)
		backoff = min(backoff*2, 5*time.Second)
	}
	key := c.TakeDataKey()
	c.mu.Lock()
	sess := c.sess
	c.sess = nil
	c.mu.Unlock()
	if sess != nil {
		_ = sess.Close()
	}
	return key, nil
}

func run(stdin []byte, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	// No udevd runs this early. After an initramfs boot its database is
	// still in /run, and libdevmapper would wait forever for udev to
	// create /dev/mapper nodes: let it create them itself.
	cmd.Env = append(os.Environ(), "DM_DISABLE_UDEV=1")
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

// rootDisk: the disk under / (the kernel mounted the image's partition).
func rootDisk() (string, error) {
	var st unix.Stat_t
	if err := unix.Stat("/", &st); err != nil {
		return "", err
	}
	link, err := os.Readlink(fmt.Sprintf("/sys/dev/block/%d:%d", unix.Major(st.Dev), unix.Minor(st.Dev)))
	if err != nil {
		return "", err
	}
	// .../block/vda/vda1 -> vda
	parent := filepath.Base(filepath.Dir(link))
	if parent == "block" {
		return "", errors.New("/ is not on a partition")
	}
	return "/dev/" + parent, nil
}

type sfdiskTable struct {
	PartitionTable struct {
		Label      string `json:"label"`
		Partitions []struct {
			Node, Name, Type string
			Start, Size      int64
		} `json:"partitions"`
	} `json:"partitiontable"`
}

func readTable(disk string) (sfdiskTable, error) {
	var t sfdiskTable
	out, err := exec.Command("sfdisk", "-J", disk).Output()
	if err != nil {
		return t, err
	}
	return t, json.Unmarshal(out, &t)
}

// dataPartition finds the partition named crowdvm-data.
func dataPartition(disk string) (string, error) {
	t, err := readTable(disk)
	if err != nil {
		return "", err
	}
	for _, p := range t.PartitionTable.Partitions {
		if p.Name == dataPartName {
			return p.Node, nil
		}
	}
	return "", nil
}

func sectors(disk string) (int64, error) {
	b, err := os.ReadFile("/sys/block/" + filepath.Base(disk) + "/size")
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}

// addDataPartition puts the data partition into the free space behind the
// image's partitions (the node grew the disk to the sandbox's size).
func addDataPartition(disk string) (string, error) {
	t, err := readTable(disk)
	if err != nil {
		return "", err
	}
	if t.PartitionTable.Label != "gpt" {
		return "", fmt.Errorf("%w: not a GPT disk", errNoRoom)
	}
	size, err := sectors(disk)
	if err != nil {
		return "", err
	}
	var end int64
	for _, p := range t.PartitionTable.Partitions {
		end = max(end, p.Start+p.Size)
	}
	if (size-end-34)*512 < minDataBytes {
		return "", fmt.Errorf("%w: only %d MiB free behind the image (an older disk whose root partition was grown)", errNoRoom, (size-end)*512>>20)
	}
	// The image's backup GPT sits where its disk ended; move it to the end.
	if err := run(nil, "sfdisk", "--no-reread", "--no-tell-kernel", "--relocate", "gpt-bak-std", disk); err != nil {
		return "", err
	}
	in := []byte(fmt.Sprintf("name=%s, type=%s\n", dataPartName, dataPartType))
	if err := run(in, "sfdisk", "--no-reread", "--no-tell-kernel", "--append", disk); err != nil {
		return "", err
	}
	_ = run(nil, "partx", "-a", disk) // the partitions in use stay; the new one is added
	part, err := dataPartition(disk)
	if err == nil && part == "" {
		err = errors.New("the new partition did not show up")
	}
	for i := 0; err == nil && i < 50; i++ { // devtmpfs creates the node
		if _, serr := os.Stat(part); serr == nil {
			return part, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err == nil {
		err = fmt.Errorf("%s did not appear", part)
	}
	return "", err
}

// errNoRoom: the disk cannot take a data partition (checked before writing).
var errNoRoom = errors.New("no room for a data partition")

// freshLabel marks a LUKS volume whose file system is not made yet (an
// interrupted first boot formats it again instead of failing on it).
const freshLabel = dataPartName + "-new"

func blkLabel(dev string) string {
	out, _ := exec.Command("blkid", "-p", "-s", "LABEL", "-o", "value", dev).Output()
	return strings.TrimSpace(string(out))
}

func blkType(dev string) string {
	out, _ := exec.Command("blkid", "-p", "-s", "TYPE", "-o", "value", dev).Output()
	return strings.TrimSpace(string(out))
}

// openData opens the LUKS2 partition (formatting it when it is blank),
// grows it with the disk and mounts its ext4 at /run/crowdvm/data.
func openData(disk, part string, key []byte, logger *log.Logger) error {
	_ = run(nil, "modprobe", "dm_crypt")
	fresh := false
	switch t := blkType(part); t {
	case "crypto_LUKS":
		fresh = blkLabel(part) == freshLabel // the last first boot ended before mkfs
	case "":
		if err := blank(part); err != nil {
			return err
		}
		logger.Printf("formatting the encrypted data partition %s", part)
		if err := run(key, "cryptsetup", "luksFormat", "--batch-mode", "--type", "luks2", "--pbkdf", "pbkdf2",
			"--pbkdf-force-iterations", "1000", "--sector-size", "4096", "--label", freshLabel, "--key-file=-", part); err != nil {
			return err
		}
		fresh = true
	default:
		return fmt.Errorf("%s holds %q, not an encrypted data partition; refusing to touch it", part, t)
	}
	if err := run(key, "cryptsetup", "open", "--type", "luks2", "--key-file=-", "--allow-discards",
		"--perf-no_read_workqueue", "--perf-no_write_workqueue", part, dataPartName); err != nil {
		return fmt.Errorf("the disk key was refused: %w", err)
	}
	dev := "/dev/mapper/" + dataPartName
	if fresh {
		// Nothing is zeroed up front: over dm-crypt zeros become
		// ciphertext, which would fill a fresh sandbox's first snapshot
		// layer with a gigabyte of encrypted inode tables and journal.
		if err := run(nil, "mkfs.ext4", "-q", "-F", "-m", "1", "-E", "nodiscard,lazy_itable_init=1,lazy_journal_init=1", "-L", dataPartName, dev); err != nil {
			return err
		}
		if err := run(nil, "cryptsetup", "config", "--label", dataPartName, part); err != nil {
			return err
		}
	} else {
		// A larger sandbox disk: grow partition, LUKS and ext4 (offline).
		_ = run(nil, "sfdisk", "--no-reread", "--no-tell-kernel", "--relocate", "gpt-bak-std", disk)
		if n := strings.TrimPrefix(strings.TrimPrefix(part, disk), "p"); n != "" {
			if run(nil, "growpart", disk, n) == nil {
				_ = run(nil, "partx", "-u", "-n", n, disk)
				_ = run(key, "cryptsetup", "resize", "--key-file=-", dataPartName)
			}
		}
		if err := exec.Command("e2fsck", "-p", dev).Run(); err != nil {
			if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() >= 4 {
				return fmt.Errorf("e2fsck %s: %v", dev, err)
			}
		}
	}
	if err := os.MkdirAll(dataMount, 0o700); err != nil {
		return err
	}
	// noinit_itable: no background zeroing of inode tables either (the
	// group descriptors say which inodes are unused).
	if err := unix.Mount(dev, dataMount, "ext4", 0, "noinit_itable"); err != nil {
		return err
	}
	// Online: offline resize2fs wants a fresh e2fsck -f first.
	_ = run(nil, "resize2fs", dev)
	return nil
}

// blank: the first MiB is all zeros (a partition nobody wrote yet).
func blank(part string) error {
	f, err := os.Open(part)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	if _, err := io.ReadFull(f, buf); err != nil {
		return err
	}
	if !bytes.Equal(buf, make([]byte, len(buf))) {
		return fmt.Errorf("%s is not blank but holds no LUKS header; refusing to format it", part)
	}
	return nil
}

// dataBinds: directories on the encrypted ext4 itself, not on the overlay
// (Docker's and containerd's overlay snapshotters refuse an overlayfs below).
var dataBinds = []string{"home", "var/lib/docker", "var/lib/containerd", "var/lib/containers"}

// overlayRoot mounts the new root at /mnt: the image below (read-only), the
// encrypted partition above, dataBinds on its ext4.
func overlayRoot() error {
	for _, d := range []string{"upper", "work", "binds"} {
		if err := os.MkdirAll(filepath.Join(dataMount, d), 0o755); err != nil {
			return err
		}
	}
	// Layout of the first v4 builds: home and docker next to upper/.
	for old, rel := range map[string]string{"home": "home", "docker": "var/lib/docker"} {
		o, n := filepath.Join(dataMount, old), filepath.Join(dataMount, "binds", strings.ReplaceAll(rel, "/", "_"))
		if _, err := os.Stat(o); err == nil {
			if _, err := os.Stat(n); err != nil {
				_ = os.Rename(o, n)
			}
		}
	}
	_ = run(nil, "modprobe", "overlay")
	opts := "lowerdir=/,upperdir=" + dataMount + "/upper,workdir=" + dataMount + "/work"
	if err := unix.Mount("overlay", newRootDir, "overlay", 0, opts); err != nil {
		return fmt.Errorf("overlay root: %w", err)
	}
	for _, rel := range dataBinds {
		src := filepath.Join(dataMount, "binds", strings.ReplaceAll(rel, "/", "_"))
		dst := filepath.Join(newRootDir, rel)
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		if _, err := os.Stat(src); err != nil {
			// First time: move what the root has there (the image's
			// /home, or what an earlier version kept on the overlay).
			tmp := src + ".new"
			_ = os.RemoveAll(tmp)
			if err := os.MkdirAll(tmp, 0o755); err != nil {
				return err
			}
			if err := run(nil, "cp", "-a", dst+"/.", tmp); err != nil {
				return err
			}
			if err := os.Rename(tmp, src); err != nil {
				return err
			}
		}
		if err := unix.Mount(src, dst, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("bind %s: %w", rel, err)
		}
	}
	return dropRootFromFstab(newRootDir + "/etc/fstab")
}

// dropRootFromFstab: / is the overlay now; systemd must neither fsck nor
// remount it with the image's ext4 options.
func dropRootFromFstab(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var keep []string
	changed := false
	for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if f := strings.Fields(l); len(f) >= 2 && !strings.HasPrefix(f[0], "#") && f[1] == "/" {
			changed = true
			keep = append(keep, "# "+l+"  # crowdvm: / is an overlay on the encrypted data partition")
			continue
		}
		keep = append(keep, l)
	}
	if !changed {
		return nil
	}
	return os.WriteFile(path, []byte(strings.Join(keep, "\n")+"\n"), 0o644)
}

// switchRoot moves /dev, /run, /proc and /sys into the new root, makes it /
// and execs systemd.
func switchRoot() error {
	for _, m := range []string{"/dev", "/run", "/proc", "/sys"} {
		if err := unix.Mount(m, newRootDir+m, "", unix.MS_MOVE, ""); err != nil {
			return fmt.Errorf("move %s: %w", m, err)
		}
	}
	if err := unix.Chdir(newRootDir); err != nil {
		return err
	}
	if err := unix.PivotRoot(".", "mnt"); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := unix.Chdir("/"); err != nil {
		return err
	}
	_ = unix.Unmount("/mnt", unix.MNT_DETACH) // the image root stays as the overlay's lower
	// The overlay and the binds hold the data file system; its own mount
	// would only show the upper layer twice (fstrim reaches it via /home).
	_ = unix.Unmount(dataMount, unix.MNT_DETACH)
	return execInit()
}

func execInit() error {
	return syscall.Exec("/sbin/init", []string{"/sbin/init"}, os.Environ())
}

// netUp: a static address on the node's network (the same one its DHCP
// hands out later), enough to reach the server through the gateway alias.
func netUp() error {
	_ = run(nil, "modprobe", "virtio_net")
	var nic string
	for i := 0; i < 100 && nic == ""; i++ {
		ents, _ := os.ReadDir("/sys/class/net")
		for _, e := range ents {
			if e.Name() != "lo" {
				nic = e.Name()
				break
			}
		}
		if nic == "" {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if nic == "" {
		return errors.New("no network interface")
	}
	preinitNIC = nic
	_ = run(nil, "ip", "link", "set", "lo", "up")
	if err := run(nil, "ip", "link", "set", nic, "up"); err != nil {
		return err
	}
	if err := run(nil, "ip", "addr", "add", "10.0.2.15/24", "dev", nic); err != nil {
		return err
	}
	return run(nil, "ip", "route", "add", "default", "via", "10.0.2.2")
}

var preinitNIC string

// netDown leaves the interface as the kernel made it: udev renames it, and
// networkd configures it.
func netDown() {
	if preinitNIC != "" {
		_ = run(nil, "ip", "addr", "flush", "dev", preinitNIC)
		_ = run(nil, "ip", "link", "set", preinitNIC, "down")
	}
}
