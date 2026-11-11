package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/crowdvm/crowdvm/internal/node"
	"github.com/crowdvm/crowdvm/internal/node/image"
)

func imageCmd(g *globals) *cobra.Command {
	c := &cobra.Command{Use: "image", Short: "Build, list, push and pull base images"}
	c.AddCommand(imageBuildCmd(g), imageImportCmd(g), imageListCmd(g), imageTransferCmd(g, "push"), imageTransferCmd(g, "pull"))
	return c
}

func imageBuildCmd(g *globals) *cobra.Command {
	var o image.Options
	var timeout time.Duration
	var noImport, quiet bool
	c := &cobra.Command{
		Use:   "build --cloud-image FILE",
		Short: "Build a CrowdVM base image from an Ubuntu 24.04 cloud image and add it to the local store",
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := g.config()
			if err != nil {
				return err
			}
			if o.CloudImage == "" {
				return fmt.Errorf("--cloud-image is required (download noble-server-cloudimg-amd64.img from cloud-images.ubuntu.com)")
			}
			o.OutDir = filepath.Join(cfg.DataDir, "build")
			o.Timeout = timeout
			if !quiet {
				o.Log = os.Stderr
			}
			ctx, cancel := signalCtx()
			defer cancel()
			res, err := image.Build(ctx, o)
			if err != nil {
				return err
			}
			fmt.Printf("built %s (%s, %d MiB, sha256 %s)\n", res.Meta.ID, res.Path, res.Meta.SizeBytes>>20, res.Meta.SHA256)
			if noImport {
				return nil
			}
			body, _ := json.Marshal(map[string]string{"path": res.Path})
			if ran, err := daemonBody(ctx, cfg, "POST", "/api/images/"+res.Meta.ID+"/import", string(body), nil); ran || err != nil {
				if err == nil {
					fmt.Printf("imported into the running node's store; share it with: crowdvm-node image push %s\n", res.Meta.ID)
				}
				return err
			}
			drv, err := newDriver(cfg, g.logger())
			if err != nil {
				return err
			}
			defer drv.Close()
			if err := drv.Store().ImportImage(res.Meta.ID, res.Path); err != nil {
				return fmt.Errorf("import into the %s store: %w", drv.Name(), err)
			}
			fmt.Printf("imported into the local store; share it with: crowdvm-node image push %s\n", res.Meta.ID)
			return nil
		},
	}
	fl := c.Flags()
	fl.StringVar(&o.CloudImage, "cloud-image", "", "Ubuntu 24.04 cloud image (qcow2/raw), never modified")
	fl.StringVar(&o.Profile, "profile", "full", "minimal | full")
	fl.StringVar(&o.ImageID, "id", "", "image id (default ubuntu-24.04-crowdvm[-minimal]-v<ver>)")
	fl.IntVar(&o.CPUs, "cpus", 0, "build VM vCPUs (default 4)")
	fl.IntVar(&o.MemoryMiB, "memory-mib", 0, "build VM memory in MiB (default 4096)")
	fl.IntVar(&o.DiskGiB, "disk-gib", 0, "image virtual disk size in GiB")
	fl.DurationVar(&timeout, "timeout", 0, "build timeout (default 20m minimal / 120m full)")
	fl.BoolVar(&o.Force, "force", false, "overwrite an existing build with the same id")
	fl.BoolVar(&o.KeepWork, "keep-work", false, "keep the work dir")
	fl.BoolVar(&noImport, "no-import", false, "leave the image in <data-dir>/build only")
	fl.BoolVar(&quiet, "quiet", false, "don't stream the build log")
	return c
}

func imageListCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List images in the local store and local builds",
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := g.config()
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "IMAGE\tWHERE\tSIZE\tCREATED")
			builds, _ := image.List(filepath.Join(cfg.DataDir, "build"))
			for _, m := range builds {
				fmt.Fprintf(tw, "%s\tbuild\t%d MiB\t%s\n", m.ID, m.SizeBytes>>20, m.CreatedAt.Local().Format(time.DateTime))
			}
			var st node.Status
			if ran, _ := daemon(c.Context(), cfg, "GET", "/api/status", &st); ran {
				for _, id := range st.Images {
					fmt.Fprintf(tw, "%s\tstore (%s)\t\t\n", id, st.Driver)
				}
			} else if drv, err := newDriver(cfg, g.logger()); err == nil {
				for _, id := range drv.Store().Images() {
					size := ""
					if p, err := drv.Store().ImagePath(id); err == nil {
						if fi, err := os.Stat(p); err == nil {
							size = fmt.Sprintf("%d MiB", fi.Size()>>20)
						}
					}
					fmt.Fprintf(tw, "%s\tstore (%s)\t%s\t\n", id, drv.Name(), size)
				}
				drv.Close()
			}
			return tw.Flush()
		},
	}
}

// imageTransferCmd: push uploads a store image to the server, pull downloads one.
// Uses the running daemon's session if there is one, else a short-lived session.
func imageTransferCmd(g *globals, verb string) *cobra.Command {
	return &cobra.Command{
		Use:   verb + " IMAGE",
		Short: map[string]string{"push": "Upload a local image to the server", "pull": "Download an image from the server"}[verb],
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cfg, err := g.config()
			if err != nil {
				return err
			}
			id := args[0]
			ctx, cancel := signalCtx()
			defer cancel()
			ran, err := daemon(ctx, cfg, "POST", "/api/images/"+id+"/"+verb, nil)
			if err != nil || ran {
				if err == nil {
					fmt.Printf("%sed %s (via the running daemon)\n", verb, id)
				}
				return err
			}
			drv, err := newDriver(cfg, g.logger())
			if err != nil {
				return err
			}
			defer drv.Close()
			n := node.New(cfg, drv, g.logger())
			err = n.WithSession(ctx, func(ctx context.Context) error {
				if verb == "push" {
					return n.PushImage(ctx, id)
				}
				return n.PullImage(ctx, "", id)
			})
			if err == nil {
				fmt.Printf("%sed %s\n", verb, id)
			}
			return err
		},
	}
}

// imageImportCmd adds an existing base image file (e.g. built on another
// machine, or downloaded) to the node's store. The file is copied, not moved.
func imageImportCmd(g *globals) *cobra.Command {
	var id string
	c := &cobra.Command{
		Use:   "import FILE [--id ID]",
		Short: "Copy an existing base image (qcow2) into the local store",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cfg, err := g.config()
			if err != nil {
				return err
			}
			src := args[0]
			if id == "" {
				id = strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
			}
			tmpDir := filepath.Join(cfg.DataDir, "build")
			if err := os.MkdirAll(tmpDir, 0o755); err != nil {
				return err
			}
			tmp := filepath.Join(tmpDir, ".import-"+id+".qcow2")
			if err := copyFile(src, tmp); err != nil {
				return err
			}
			defer os.Remove(tmp)
			ctx, cancel := signalCtx()
			defer cancel()
			body, _ := json.Marshal(map[string]string{"path": tmp})
			if ran, err := daemonBody(ctx, cfg, "POST", "/api/images/"+id+"/import", string(body), nil); ran || err != nil {
				if err == nil {
					fmt.Printf("imported %s into the running node's store; share it with: crowdvm-node image push %s\n", id, id)
				}
				return err
			}
			drv, err := newDriver(cfg, g.logger())
			if err != nil {
				return err
			}
			defer drv.Close()
			if err := drv.Store().ImportImage(id, tmp); err != nil {
				return fmt.Errorf("import into the %s store: %w", drv.Name(), err)
			}
			fmt.Printf("imported %s; share it with: crowdvm-node image push %s\n", id, id)
			return nil
		},
	}
	c.Flags().StringVar(&id, "id", "", "image id (default: file name without extension)")
	return c
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
