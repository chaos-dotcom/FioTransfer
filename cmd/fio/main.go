// Command fio is the command-line front end to the FioTransfer engine.
//
//	fio inspect <link> [-p passphrase]
//	fio get <link> [-o dir] [-p passphrase] [-n streams] [--subfolder]
//	fio login | logout | whoami
//	fio ls [account-id [folder-id]]
//	fio up <path>... --account <id> --folder <id>
//	fio history [-n 20]
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"fiotransfer/internal/api"
	"fiotransfer/internal/auth"
	"fiotransfer/internal/config"
	"fiotransfer/internal/history"
	"fiotransfer/internal/xfer"
)

func usage() {
	fmt.Fprintln(os.Stderr, `fio — fast Frame.io transfers

  fio inspect <link> [-p passphrase]          show what a share link contains
  fio get <link> [-o dir] [-p pass] [-n N]    download a share link (resumable)
        [--subfolder] [--seg MB]
  fio login | logout | whoami                 Adobe sign-in for uploads
  fio ls [account [folder]]                   browse your Frame.io accounts/projects/folders
  fio up <path>... --account ID --folder ID   upload files/folders
  fio history [-n 20]                         recent transfers`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	settings := config.Load()
	am := auth.NewManager(func() string { return config.Load().ClientID })
	client := api.New(am)

	switch cmd {
	case "inspect":
		fs := flag.NewFlagSet("inspect", flag.ExitOnError)
		pass := fs.String("p", "", "passphrase")
		link := parseWithPositional(fs, args)
		m := newManager(client, settings)
		li := m.InspectLink(ctx, link, *pass)
		if li.Error != "" {
			fatal(li.Error)
		}
		fmt.Printf("%s link %s\nName:   %s\nFiles:  %d (%d unavailable)\nSize:   %s\n", li.Kind, li.ShareID, li.Name, li.Files, li.Unavailable, human(li.TotalBytes))

	case "get":
		fs := flag.NewFlagSet("get", flag.ExitOnError)
		out := fs.String("o", settings.DownloadDir, "destination folder")
		pass := fs.String("p", "", "passphrase")
		streams := fs.Int("n", settings.Streams, "parallel streams")
		seg := fs.Int("seg", settings.SegmentMB, "segment size in MiB")
		sub := fs.Bool("subfolder", false, "create a folder named after the share")
		link := parseWithPositional(fs, args)
		checkRange("-n", *streams, 1, 128)
		checkRange("--seg", *seg, 8, 512)
		settings.Streams, settings.SegmentMB = *streams, *seg
		m := newManager(client, settings)
		if err := os.MkdirAll(*out, 0o755); err != nil {
			fatal(err.Error())
		}
		j, err := m.StartDownload(xfer.DownloadOptions{Link: link, Passphrase: *pass, DestDir: *out, Subfolder: *sub})
		if err != nil {
			fatal(err.Error())
		}
		os.Exit(follow(ctx, m, j.ID))

	case "login":
		if settings.ClientID == "" {
			fatal(config.ErrNoClientID.Error())
		}
		fmt.Println("Opening your browser for Adobe sign-in…")
		err := am.Login(ctx, settings.RedirectPort, func(u string) { exec.Command("open", u).Run() })
		if err != nil {
			fatal(err.Error())
		}
		me, err := client.Me(ctx)
		if err != nil {
			fatal(err.Error())
		}
		fmt.Printf("Signed in as %s <%s>\n", me.Name, me.Email)

	case "logout":
		am.Logout()
		fmt.Println("Signed out.")

	case "whoami":
		me, err := client.Me(ctx)
		if err != nil {
			fatal(err.Error())
		}
		fmt.Printf("%s <%s>\n", me.Name, me.Email)

	case "ls":
		lsCmd(ctx, client, args)

	case "up":
		fs := flag.NewFlagSet("up", flag.ExitOnError)
		acct := fs.String("account", settings.LastAccountID, "account ID")
		folder := fs.String("folder", settings.LastFolderID, "destination folder ID")
		streams := fs.Int("n", settings.Streams, "parallel streams")
		var paths []string
		rest := args
		for len(rest) > 0 {
			fs.Parse(rest)
			rest = fs.Args()
			if len(rest) > 0 {
				paths = append(paths, rest[0])
				rest = rest[1:]
			}
		}
		if len(paths) == 0 {
			usage()
		}
		checkRange("-n", *streams, 1, 128)
		settings.Streams = *streams
		m := newManager(client, settings)
		j, err := m.StartUpload(xfer.UploadOptions{Paths: paths, AccountID: *acct, FolderID: *folder, DestLabel: "folder " + *folder})
		if err != nil {
			fatal(err.Error())
		}
		os.Exit(follow(ctx, m, j.ID))

	case "history":
		fs := flag.NewFlagSet("history", flag.ExitOnError)
		n := fs.Int("n", 20, "rows")
		fs.Parse(args)
		h := openHistory()
		rows, err := h.List("", "", *n)
		if err != nil {
			fatal(err.Error())
		}
		for _, r := range rows {
			fmt.Printf("%s  %-8s %-9s %4d/%-4d %10s  %8s/s  %s\n", time.UnixMilli(r.CreatedAt).Format("2006-01-02 15:04"), r.Kind, r.Status,
				r.FilesDone, r.FilesTotal, human(r.DoneBytes), human(int64(r.AvgBps)), r.Title)
		}
		fmt.Println("Log:", h.LogPath())

	default:
		usage()
	}
}

// parseWithPositional allows flags before or after the single positional argument.
func parseWithPositional(fs *flag.FlagSet, args []string) string {
	fs.Parse(args)
	pos := fs.Args()
	if len(pos) == 0 {
		usage()
	}
	fs.Parse(pos[1:])
	return pos[0]
}

func openHistory() *history.Store {
	h, err := history.Open(config.Dir(), config.LogDir())
	if err != nil {
		fatal(err.Error())
	}
	return h
}

func newManager(c *api.Client, s config.Settings) *xfer.Manager {
	m := xfer.NewManager(c, openHistory(), func() config.Settings { return s })
	m.SetKeepAwake(true)
	return m
}

// checkRange exits with an error if a numeric flag is out of bounds.
func checkRange(flag string, v, lo, hi int) {
	if v < lo || v > hi {
		fatal(fmt.Sprintf("%s must be between %d and %d", flag, lo, hi))
	}
}

// follow prints progress until the job finishes; returns an exit code.
func follow(ctx context.Context, m *xfer.Manager, id string) int {
	tty := isTTY()
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	lastLine := time.Time{}
	for {
		select {
		case <-ctx.Done():
			m.Cancel(id)
			fmt.Fprintln(os.Stderr, "\nCanceling… (run the same command again to resume)")
			ctx = context.Background()
		case <-t.C:
		}
		snap := m.Snapshot()
		var v xfer.JobView
		for _, j := range snap.Jobs {
			if j.ID == id {
				v = j
			}
		}
		line := fmt.Sprintf("%-9s %5.1f%%  %s / %s  %s/s  files %d/%d  streams %d",
			v.Status, pct(v.Done, v.Total), human(v.Done), human(v.Total), human(int64(v.Speed)), v.FilesDone, v.FilesTotal, snap.ActiveStreams)
		if v.Phase != "" {
			line = string(v.Status) + "  " + v.Phase
		}
		if v.ETA > 0 {
			line += "  ETA " + (time.Duration(v.ETA) * time.Second).String()
		}
		if tty {
			fmt.Printf("\r\033[K%s", line)
		} else if time.Since(lastLine) > 5*time.Second {
			fmt.Println(line)
			lastLine = time.Now()
		}
		switch v.Status {
		case xfer.Done, xfer.Partial, xfer.Failed, xfer.Canceled:
			fmt.Printf("\n%s: %d/%d files, %s in %.1fs (avg %s/s)\n", strings.ToUpper(string(v.Status)), v.FilesDone, v.FilesTotal,
				human(v.Done), v.Elapsed, human(int64(v.AvgSpeed)))
			if v.Error != "" {
				fmt.Println("Error:", v.Error)
			}
			for _, f := range m.Files(id) {
				if f.Status == xfer.Failed {
					fmt.Printf("  failed: %s — %s\n", f.RelPath, f.Error)
				}
			}
			if v.Status == xfer.Done {
				return 0
			}
			return 1
		}
	}
}

func lsCmd(ctx context.Context, c *api.Client, args []string) {
	switch len(args) {
	case 0:
		accts, err := c.Accounts(ctx)
		if err != nil {
			fatal(err.Error())
		}
		for _, a := range accts {
			fmt.Printf("account %s  %s\n", a.ID, a.DisplayName)
		}
	case 1:
		wss, err := c.Workspaces(ctx, args[0])
		if err != nil {
			fatal(err.Error())
		}
		for _, w := range wss {
			ps, err := c.Projects(ctx, args[0], w.ID)
			if err != nil {
				fatal(err.Error())
			}
			for _, p := range ps {
				fmt.Printf("%-20s / %-30s root folder %s\n", w.Name, p.Name, p.RootFolderID)
			}
		}
	default:
		kids, err := c.Children(ctx, args[0], args[1])
		if err != nil {
			fatal(err.Error())
		}
		for _, k := range kids {
			fmt.Printf("%-13s %s  %s\n", k.Type, k.ID, k.Name)
		}
	}
}

func pct(a, b int64) float64 {
	if b <= 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}

func human(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}

func isTTY() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "error:", msg)
	os.Exit(1)
}
