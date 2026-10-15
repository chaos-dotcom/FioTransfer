package xfer

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Per-transfer logs written next to the media: in the download folder, or
// in the folder an upload was taken from.

const folderLogPrefix = "FioTransfer "

func isFolderLog(name string) bool {
	return strings.HasPrefix(name, folderLogPrefix) && strings.HasSuffix(name, ".txt")
}

// logDirs decides where this job's log goes.
func (j *Job) logDirs() []string {
	if j.Kind == "download" {
		if j.logRoot != "" {
			return []string{j.logRoot}
		}
		return []string{j.Dest}
	}
	seen := map[string]bool{}
	var dirs []string
	for _, p := range j.paths {
		d := filepath.Dir(p)
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			d = p
		}
		if !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	if len(dirs) == 0 { // retries without original paths: use the files' folders
		for _, f := range j.files() {
			d := filepath.Dir(f.LocalPath)
			if !seen[d] {
				seen[d] = true
				dirs = append(dirs, d)
			}
		}
		if len(dirs) > 1 {
			dirs = []string{commonDir(dirs)}
		}
	}
	return dirs
}

func commonDir(dirs []string) string {
	c := dirs[0]
	for _, d := range dirs[1:] {
		for c != "/" && !strings.HasPrefix(d+"/", c+"/") {
			c = filepath.Dir(c)
		}
	}
	return c
}

// writeFolderLog (re)writes the log file. Failures are recorded in the app log.
func (j *Job) writeFolderLog(final bool) {
	if !j.mgr.Settings().FolderLogs() {
		return
	}
	if j.watchLog {
		// Watch folders get one log per day; each finished batch is appended.
		if !final {
			return
		}
		name := fmt.Sprintf("%sWatch %s.txt", folderLogPrefix, time.Now().Format("2006-01-02"))
		for _, d := range j.logDirs() {
			fh, err := os.OpenFile(filepath.Join(d, name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				j.mgr.Hist.Log("WATCH    note     [%s] could not write log to %s: %v", j.ID, d, err)
				continue
			}
			fh.WriteString(j.folderLogText() + "\n\n")
			fh.Close()
		}
		return
	}
	if j.logName == "" {
		verb := "Download"
		if j.Kind == "upload" {
			verb = "Upload"
		}
		j.logName = fmt.Sprintf("%s%s %s.txt", folderLogPrefix, verb, j.created.Format("2006-01-02 150405"))
	}
	body := j.folderLogText()
	for _, d := range j.logDirs() {
		p := filepath.Join(d, j.logName)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			j.mgr.Hist.Log("%s note     [%s] could not write log to %s: %v", kindTag(j.Kind), j.ID, d, err)
		}
	}
}

func (j *Job) folderLogText() string {
	rec := j.record()
	files := j.files()
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	ts := func(ms int64) string {
		if ms == 0 {
			return "—"
		}
		return time.UnixMilli(ms).Format("2006-01-02 15:04:05")
	}

	verb := "Download"
	if j.Kind == "upload" {
		verb = "Upload"
	}
	status := map[string]string{"running": "In progress", "resolving": "In progress", "queued": "In progress",
		"done": "Completed", "partial": "Completed with errors", "failed": "Failed", "canceled": "Canceled"}[rec.Status]
	if status == "" {
		status = rec.Status
	}
	w("FioTransfer %s\n", verb)
	w("%s\n\n", strings.Repeat("=", 60))
	w("Name         %s\n", rec.Title)
	w("Status       %s\n", status)
	if j.Kind == "upload" {
		w("From         %s\n", strings.Join(j.logDirs(), ", "))
		w("To           Frame.io / %s\n", rec.Dest)
	} else {
		w("Link         %s\n", rec.Source)
		w("To           %s\n", rec.Dest)
	}
	w("Started      %s\n", ts(rec.StartedAt))
	w("Finished     %s\n", ts(rec.FinishedAt))
	if rec.FinishedAt > 0 && rec.StartedAt > 0 {
		w("Duration     %s\n", humanDuration(time.Duration(rec.FinishedAt-rec.StartedAt)*time.Millisecond))
	}
	w("Files        %d of %d", rec.FilesDone, rec.FilesTotal)
	if rec.FilesFailed > 0 {
		w(" (%d failed)", rec.FilesFailed)
	}
	w("\n")
	w("Size         %s\n", humanBytes(rec.TotalBytes))
	if rec.AvgBps > 0 {
		w("Avg speed    %s/s (%.0f Mbit/s)\n", humanBytes(int64(rec.AvgBps)), rec.AvgBps*8/1e6)
	}
	if rec.Error != "" {
		w("Error        %s\n", rec.Error)
	}
	w("Transfer ID  %s\n\n", rec.ID)

	rows := make([]*File, len(files))
	copy(rows, files)
	sort.SliceStable(rows, func(a, c int) bool { return rows[a].RelPath < rows[c].RelPath })
	w("%-10s %12s  %s\n", "STATUS", "SIZE", "FILE")
	w("%s\n", strings.Repeat("-", 60))
	for _, f := range rows {
		f.mu.Lock()
		st, e, note, url := f.status, f.err, f.note, f.ViewURL
		f.mu.Unlock()
		label := map[Status]string{Done: "OK", Skipped: "SKIPPED", Failed: "FAILED", Canceled: "CANCELED",
			Running: "ACTIVE", Finalizing: "ACTIVE", Queued: "PENDING"}[st]
		w("%-10s %12s  %s", label, humanBytes(f.Size), f.RelPath)
		switch {
		case e != "":
			w("  — %s", e)
		case note != "":
			w("  — %s", note)
		}
		if url != "" {
			w("  %s", url)
		}
		w("\n")
	}
	return b.String()
}
