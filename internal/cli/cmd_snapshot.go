package cli

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/crowdvm/crowdvm/internal/client"
	"github.com/spf13/cobra"
)

func (a *App) snapshotsCmd() *cobra.Command {
	var limit int
	var all bool
	cmd := &cobra.Command{
		Use:   "snapshots [ID]",
		Short: "List your saved snapshots, then the automatic snapshots of all your sandboxes (or of one)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			id := ""
			if len(args) == 1 {
				if id, err = a.sandboxID(args[0]); err != nil {
					return err
				}
			}
			snaps := []client.Snapshot{}
			cursor := ""
			for {
				page, err := c.ListSnapshots(cmd.Context(), id, limit, cursor)
				if err != nil {
					return err
				}
				snaps = append(snaps, page.Snapshots...)
				if !all || !page.PageInfo.HasMore || page.PageInfo.NextCursor == "" || page.PageInfo.NextCursor == cursor {
					break
				}
				cursor = page.PageInfo.NextCursor
			}
			// Boat: saved snapshots first, with how many you keep and their cost.
			var saved *client.SavedSnapshotList
			if id == "" {
				if saved, err = c.ListSavedSnapshots(cmd.Context()); err != nil {
					return err
				}
			}
			if a.jsonOut {
				out := map[string]any{"snapshots": snaps}
				if saved != nil {
					out["savedSnapshots"], out["allowance"] = saved.Snapshots, saved.Allowance
				}
				a.emit(out)
				return nil
			}
			if saved != nil {
				var srows [][]string
				for _, s := range saved.Snapshots {
					srows = append(srows, []string{s.Name, a.orgName(cmd.Context(), c, s.Org), s.Status, s.SourceSandboxID, humanBytes(s.SizeBytes), ago(s.CreatedAt)})
				}
				if len(srows) > 0 {
					table(a.Out, "SAVED\tOWNER\tSTATUS\tFROM\tSIZE\tCREATED", srows)
				}
				if al := saved.Allowance; al != nil {
					a.printf("saved snapshots: %v of %v included", al["used"], al["included"])
					if n, _ := al["extra"].(float64); n > 0 {
						a.printf(", %v extra ($%v/month)", al["extra"], al["extraMonthlyDollars"])
					}
					a.printf("\n\n")
				}
			}
			var rows [][]string
			for _, s := range snaps {
				size := "-"
				if s.SizeBytes != nil {
					size = humanBytes(*s.SizeBytes)
				}
				rows = append(rows, []string{s.ID, s.SandboxID, s.Status, s.Kind, size, ago(s.CreatedAt)})
			}
			table(a.Out, "SNAPSHOT\tSANDBOX\tSTATUS\tKIND\tSIZE\tCREATED", rows)
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "snapshots per page")
	cmd.Flags().BoolVar(&all, "all", false, "read every page")
	return cmd
}

func (a *App) snapshotCmd() *cobra.Command {
	var noWait bool
	cmd := &cobra.Command{
		Use:   "snapshot ID NAME | snapshot <subcommand>",
		Short: "Save a sandbox as a named snapshot, and manage saved snapshots",
		Long: `'crowdvm snapshot ID NAME' saves the disk of a sandbox under NAME (replacing
an older one with that name); 'crowdvm new --from NAME' deploys it.`,
		Example: `  crowdvm snapshot bx_f7k2q9hd web-stack
  crowdvm snapshot list
  crowdvm snapshot share web-stack
  crowdvm snapshot pull snap_123 -o ./restore --path /home/user/project`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, id, err := a.clientAndID(args[0])
			if err != nil {
				return err
			}
			s, err := c.SaveSnapshot(cmd.Context(), id, args[1])
			if err != nil {
				return err
			}
			a.info("saving %s from %s...\n", s.Name, id)
			for !noWait && s.Status == "saving" {
				if err := sleepCtx(cmd.Context(), 2*a.Poll); err != nil {
					return err
				}
				if s, err = c.GetSavedSnapshot(cmd.Context(), args[1]); err != nil {
					return err
				}
			}
			if s.Status == "failed" {
				return &client.Error{Code: "snapshot_failed", Message: firstNonEmpty(s.Error, "the snapshot failed")}
			}
			return a.result(s, func() { a.printf("%s: %s\n", s.Name, s.Status) })
		},
	}
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "return while the snapshot is saving")

	cmd.AddCommand(&cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List saved snapshots", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			l, err := c.ListSavedSnapshots(cmd.Context())
			if err != nil {
				return err
			}
			if a.jsonOut {
				a.emit(l)
				return nil
			}
			var rows [][]string
			for _, s := range l.Snapshots {
				rows = append(rows, []string{s.Name, a.orgName(cmd.Context(), c, s.Org), s.Status, s.SourceSandboxID, humanBytes(s.SizeBytes), ago(s.CreatedAt)})
			}
			table(a.Out, "NAME\tOWNER\tSTATUS\tFROM\tSIZE\tCREATED", rows)
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use: "info NAME", Short: "Show one saved snapshot", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			s, err := c.GetSavedSnapshot(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return a.result(s, func() {
				a.printf("name:     %s\nstatus:   %s\nfrom:     %s\nsnapshot: %s\nsize:     %s\ncreated:  %s\n",
					s.Name, s.Status, s.SourceSandboxID, s.SnapshotID, humanBytes(s.SizeBytes), s.CreatedAt)
			})
		},
	})
	rm := &cobra.Command{
		Use: "rm NAME", Aliases: []string{"remove"}, Short: "Remove a saved snapshot and release its storage", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			if err := c.DeleteSavedSnapshot(cmd.Context(), args[0]); err != nil {
				return err
			}
			return a.result(map[string]any{"name": args[0], "status": "deleted"}, func() { a.printf("%s: removed\n", args[0]) })
		},
	}
	rm.Flags().Bool("yes", false, "accepted for Boat compatibility (no confirmation is asked)")
	cmd.AddCommand(rm)
	cmd.AddCommand(&cobra.Command{
		Use: "share NAME", Short: "Make a secret code another account can deploy (shown once)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			r, err := c.CreateSnapshotShare(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return a.result(r, func() {
				a.printf("%s\n", r.ShareCode)
				a.info("save this code now; it is shown once. Deploy with: crowdvm new --from %s\n", r.ShareCode)
			})
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use: "shares NAME", Short: "List share codes of a saved snapshot", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			l, err := c.ListSnapshotShares(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if a.jsonOut {
				a.emit(map[string]any{"name": args[0], "shares": l})
				return nil
			}
			var rows [][]string
			for _, s := range l {
				rows = append(rows, []string{s.ID, s.CodePrefix + "…", s.Status, fmt.Sprint(s.UseCount), ago(s.CreatedAt)})
			}
			table(a.Out, "ID\tCODE\tSTATUS\tUSES\tCREATED", rows)
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use: "unshare NAME SHARE_ID", Short: "Revoke a share code", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			s, err := c.RevokeSnapshotShare(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			return a.result(s, func() { a.printf("%s: %s\n", s.ID, s.Status) })
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use: "latest ID", Short: "Show the newest snapshot of a sandbox", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, id, err := a.clientAndID(args[0])
			if err != nil {
				return err
			}
			s, err := c.LatestSnapshot(cmd.Context(), id)
			if err != nil {
				return err
			}
			if s == nil {
				return &client.Error{Code: "no_snapshot", Message: id + " has no snapshot yet"}
			}
			return a.result(s, func() { a.printf("%s %s %s\n", s.ID, s.Status, s.CreatedAt) })
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use: "tree SNAPSHOT_ID", Short: "List the files and sizes in a snapshot", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			t, err := c.SnapshotTree(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if a.jsonOut {
				a.emit(t)
				return nil
			}
			if !t.TreeAvailable {
				a.printf("no file tree for %s: %s\n", args[0], t.Reason)
				return nil
			}
			for _, e := range t.Entries {
				size := ""
				if e.Kind == "file" {
					size = humanBytes(e.Size)
				}
				a.printf("%-8s %10s  %s\n", e.Kind, size, e.Path)
			}
			a.printf("%d files, %s", t.FileCount, humanBytes(t.TotalSizeBytes))
			if t.Truncated {
				a.printf(" (list truncated)")
			}
			a.printf("\n")
			return nil
		},
	})
	cmd.AddCommand(a.snapshotPullCmd(), a.snapshotDeleteCmd())
	return cmd
}

func (a *App) snapshotPullCmd() *cobra.Command {
	var out string
	var paths []string
	cmd := &cobra.Command{
		Use:   "pull SNAPSHOT_ID|SANDBOX_ID -o DIR",
		Short: "Download a snapshot (or --path parts of it) into a local folder",
		Long: `Download a snapshot. The sandbox can be stopped. A sandbox id pulls its newest
snapshot. /home/user lands in DIR/home_user/ and Docker volumes in DIR/docker/.
--path extracts only that file or folder (repeatable).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			if out == "" {
				return usagef("--out DIR is required")
			}
			snapID := args[0]
			if strings.HasPrefix(snapID, "bx_") || snapID == "current" || snapID == "self" {
				id, err := a.sandboxID(snapID)
				if err != nil {
					return err
				}
				s, err := c.LatestSnapshot(cmd.Context(), id)
				if err != nil {
					return err
				}
				if s == nil {
					return &client.Error{Code: "no_snapshot", Message: id + " has no snapshot yet"}
				}
				snapID = s.ID
			}
			if err := os.MkdirAll(out, 0o755); err != nil {
				return err
			}
			all := len(paths) == 0
			if all {
				// Boat: home_user/ and docker/<volume>/ (the named volumes).
				paths = []string{"/home/user"}
				if tr, err := c.SnapshotTree(cmd.Context(), snapID); err == nil {
					for _, e := range tr.Entries {
						if v, ok := strings.CutPrefix(e.Path, "__dockervol__/"); ok && v != "" && !strings.Contains(v, "/") {
							paths = append(paths, e.Path)
						}
					}
				}
			}
			total := 0
			for _, p := range paths {
				rel := strings.TrimPrefix(path.Clean("/"+p), "/")
				rc, ctype, err := c.SnapshotFiles(cmd.Context(), snapID, rel)
				if err != nil {
					if all && rel != "home/user" && client.IsStatus(err, 404) {
						continue // no Docker volumes in this snapshot
					}
					return err
				}
				dest := filepath.Join(out, localName(rel))
				if strings.Contains(ctype, "tar") {
					n, err := extractTar(rc, dest, "", nil)
					rc.Close()
					if err != nil {
						return err
					}
					total += n
					// A tar whose entries are rooted at the folder name: flatten.
					flattenSingle(dest, path.Base(rel))
					if strings.HasPrefix(rel, "__dockervol__/") {
						flattenSingle(dest, "_data") // docker/<vol>/ holds the volume's files
					}
				} else {
					err = writeFile(dest, rc, 0o644)
					rc.Close()
					if err != nil {
						return err
					}
					total++
				}
				a.info("%s → %s\n", p, dest)
			}
			return a.result(map[string]any{"event": "pulled", "snapshotId": snapID, "out": out, "files": total}, func() {
				a.printf("pulled %d files into %s\n", total, out)
			})
		},
	}
	cmd.Flags().StringVarP(&out, "out", "o", "", "local directory")
	cmd.Flags().StringArrayVar(&paths, "path", nil, "only this path in the sandbox (repeatable)")
	return cmd
}

// localName maps a snapshot path to its folder in the pull output:
// home/user/x → home_user/x, var/lib/docker/volumes/v → docker/v.
func localName(rel string) string {
	switch {
	case rel == "home/user" || strings.HasPrefix(rel, "home/user/"):
		return filepath.FromSlash("home_user" + strings.TrimPrefix(rel, "home/user"))
	case strings.HasPrefix(rel, "__dockervol__/"):
		return filepath.FromSlash("docker/" + strings.TrimPrefix(rel, "__dockervol__/"))
	}
	return filepath.FromSlash(rel)
}

// flattenSingle moves dest/<base>/* up into dest when that is the only entry.
func flattenSingle(dest, base string) {
	ents, err := os.ReadDir(dest)
	if err != nil || len(ents) != 1 || ents[0].Name() != base || !ents[0].IsDir() {
		return
	}
	inner := filepath.Join(dest, base)
	kids, _ := os.ReadDir(inner)
	for _, k := range kids {
		_ = os.Rename(filepath.Join(inner, k.Name()), filepath.Join(dest, k.Name()))
	}
	_ = os.Remove(inner)
}

func (a *App) snapshotDeleteCmd() *cobra.Command {
	var all string
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete SNAPSHOT_ID... | delete --all SANDBOX_ID",
		Short: "Permanently delete ordinary snapshots",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			if all != "" {
				id, err := a.sandboxID(all)
				if err != nil {
					return err
				}
				if err := a.confirm(yes, "Delete every ordinary snapshot of "+id+"?"); err != nil {
					return err
				}
				r, err := c.DeleteSandboxSnapshots(cmd.Context(), id)
				if err != nil {
					return err
				}
				return a.result(r, func() { a.printf("%s: snapshots deleting\n", id) })
			}
			if len(args) == 0 {
				return usagef("give snapshot ids, or --all SANDBOX_ID")
			}
			if err := a.confirm(yes, fmt.Sprintf("Permanently delete %d snapshot(s)?", len(args))); err != nil {
				return err
			}
			for _, s := range args {
				op, err := c.DeleteSnapshot(cmd.Context(), s)
				if err != nil {
					return err
				}
				_ = a.result(map[string]any{"event": "deleting", "snapshotId": s, "operation": op}, func() {
					a.printf("%s: deleting (%s)\n", s, op.ID)
				})
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&all, "all", "", "delete every ordinary snapshot of this sandbox")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	return cmd
}

// orgName shows an organization id by its name ("personal" for none).
func (a *App) orgName(ctx context.Context, c *client.Client, id string) string {
	if id == "" {
		return "personal"
	}
	if a.orgNames == nil {
		a.orgNames = map[string]string{}
		if l, err := c.ListOrgs(ctx); err == nil {
			for _, o := range l.Orgs {
				a.orgNames[o.ID] = o.Name
			}
		}
	}
	return firstNonEmpty(a.orgNames[id], id)
}
