// Package history records every transfer in SQLite and in a human-readable
// monthly log under ~/Library/Logs/FioTransfer.
package history

import (
	"database/sql"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Transfer struct {
	ID          string  `json:"id"`
	Kind        string  `json:"kind"` // upload | download
	Title       string  `json:"title"`
	Source      string  `json:"source"` // share link, or local path summary
	Dest        string  `json:"dest"`   // local folder, or Frame.io destination label
	AccountID   string  `json:"account_id"`
	FolderID    string  `json:"folder_id"`
	ShareID     string  `json:"share_id"`
	Status      string  `json:"status"`
	Error       string  `json:"error"`
	TotalBytes  int64   `json:"total_bytes"`
	DoneBytes   int64   `json:"done_bytes"`
	FilesTotal  int     `json:"files_total"`
	FilesDone   int     `json:"files_done"`
	FilesFailed int     `json:"files_failed"`
	CreatedAt   int64   `json:"created_at"` // unix ms
	StartedAt   int64   `json:"started_at"`
	FinishedAt  int64   `json:"finished_at"`
	AvgBps      float64 `json:"avg_bps"`
	PeakBps     float64 `json:"peak_bps"`
	Options     string  `json:"options"`
}

type File struct {
	TransferID   string `json:"transfer_id"`
	Idx          int    `json:"idx"`
	RelPath      string `json:"rel_path"`
	LocalPath    string `json:"local_path"`
	Size         int64  `json:"size"`
	Status       string `json:"status"`
	Error        string `json:"error"`
	Note         string `json:"note"`
	RemoteParent string `json:"remote_parent"`
	RemoteID     string `json:"remote_id"`
	ViewURL      string `json:"view_url"`
	AssetID      string `json:"asset_id"`
	FinishedAt   int64  `json:"finished_at"`
}

type Store struct {
	db     *sql.DB
	logDir string
	logMu  sync.Mutex
}

const schema = `
CREATE TABLE IF NOT EXISTS transfers(
  id TEXT PRIMARY KEY, kind TEXT, title TEXT, source TEXT, dest TEXT,
  account_id TEXT, folder_id TEXT, share_id TEXT, status TEXT, error TEXT,
  total_bytes INTEGER, done_bytes INTEGER, files_total INTEGER, files_done INTEGER, files_failed INTEGER,
  created_at INTEGER, started_at INTEGER, finished_at INTEGER, avg_bps REAL, peak_bps REAL, options TEXT);
CREATE INDEX IF NOT EXISTS transfers_created ON transfers(created_at DESC);
CREATE TABLE IF NOT EXISTS transfer_files(
  transfer_id TEXT, idx INTEGER, rel_path TEXT, local_path TEXT, size INTEGER,
  status TEXT, error TEXT, note TEXT, remote_parent TEXT, remote_id TEXT, view_url TEXT,
  asset_id TEXT, finished_at INTEGER, PRIMARY KEY(transfer_id, idx));
`

func Open(dir, logDir string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "history.db")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema + watchSchema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, logDir: logDir}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) LogDir() string { return s.logDir }

// MarkInterrupted flags transfers that were running when the app last quit.
func (s *Store) MarkInterrupted() {
	now := time.Now().UnixMilli()
	s.db.Exec(`UPDATE transfer_files SET status='interrupted' WHERE status IN ('queued','running','finalizing','resolving')`)
	s.db.Exec(`UPDATE transfers SET status='interrupted', finished_at=? WHERE status IN ('queued','running','resolving','finalizing')`, now)
}

func (s *Store) Start(t Transfer, files []File) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT OR REPLACE INTO transfers VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.Kind, t.Title, t.Source, t.Dest, t.AccountID, t.FolderID, t.ShareID, t.Status, t.Error,
		t.TotalBytes, t.DoneBytes, t.FilesTotal, t.FilesDone, t.FilesFailed,
		t.CreatedAt, t.StartedAt, t.FinishedAt, t.AvgBps, t.PeakBps, t.Options)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM transfer_files WHERE transfer_id=?`, t.ID); err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO transfer_files VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, f := range files {
		if _, err := stmt.Exec(t.ID, f.Idx, f.RelPath, f.LocalPath, f.Size, f.Status, f.Error, f.Note,
			f.RemoteParent, f.RemoteID, f.ViewURL, f.AssetID, f.FinishedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) UpdateFile(f File) {
	s.db.Exec(`UPDATE transfer_files SET local_path=?, size=?, status=?, error=?, note=?, remote_parent=?, remote_id=?, view_url=?, finished_at=? WHERE transfer_id=? AND idx=?`,
		f.LocalPath, f.Size, f.Status, f.Error, f.Note, f.RemoteParent, f.RemoteID, f.ViewURL, f.FinishedAt, f.TransferID, f.Idx)
}

func (s *Store) Update(t Transfer) {
	s.db.Exec(`UPDATE transfers SET title=?, source=?, dest=?, status=?, error=?, total_bytes=?, done_bytes=?, files_total=?, files_done=?, files_failed=?, started_at=?, finished_at=?, avg_bps=?, peak_bps=?, share_id=?, options=? WHERE id=?`,
		t.Title, t.Source, t.Dest, t.Status, t.Error, t.TotalBytes, t.DoneBytes, t.FilesTotal, t.FilesDone, t.FilesFailed,
		t.StartedAt, t.FinishedAt, t.AvgBps, t.PeakBps, t.ShareID, t.Options, t.ID)
}

const cols = `id, kind, title, source, dest, account_id, folder_id, share_id, status, error, total_bytes, done_bytes, files_total, files_done, files_failed, created_at, started_at, finished_at, avg_bps, peak_bps, options`

func scanTransfer(sc interface{ Scan(...any) error }) (Transfer, error) {
	var t Transfer
	var title, source, dest, acct, folder, share, status, errS, opts sql.NullString
	var started, finished sql.NullInt64
	var avg, peak sql.NullFloat64
	err := sc.Scan(&t.ID, &t.Kind, &title, &source, &dest, &acct, &folder, &share, &status, &errS,
		&t.TotalBytes, &t.DoneBytes, &t.FilesTotal, &t.FilesDone, &t.FilesFailed,
		&t.CreatedAt, &started, &finished, &avg, &peak, &opts)
	t.Title, t.Source, t.Dest, t.AccountID, t.FolderID, t.ShareID = title.String, source.String, dest.String, acct.String, folder.String, share.String
	t.Status, t.Error, t.Options = status.String, errS.String, opts.String
	t.StartedAt, t.FinishedAt, t.AvgBps, t.PeakBps = started.Int64, finished.Int64, avg.Float64, peak.Float64
	return t, err
}

func (s *Store) Get(id string) (*Transfer, error) {
	t, err := scanTransfer(s.db.QueryRow(`SELECT `+cols+` FROM transfers WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// List returns the most recent transfers, optionally filtered by text and kind.
func (s *Store) List(query, kind string, limit int) ([]Transfer, error) {
	if limit <= 0 {
		limit = 200
	}
	where := []string{"1=1"}
	args := []any{}
	if q := strings.TrimSpace(query); q != "" {
		like := "%" + q + "%"
		where = append(where, `(title LIKE ? OR source LIKE ? OR dest LIKE ? OR error LIKE ? OR id IN (SELECT transfer_id FROM transfer_files WHERE rel_path LIKE ?))`)
		args = append(args, like, like, like, like, like)
	}
	if kind == "upload" || kind == "download" {
		where = append(where, "kind=?")
		args = append(args, kind)
	}
	args = append(args, limit)
	rows, err := s.db.Query(`SELECT `+cols+` FROM transfers WHERE `+strings.Join(where, " AND ")+` ORDER BY created_at DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Transfer
	for rows.Next() {
		t, err := scanTransfer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) Files(id string) ([]File, error) {
	rows, err := s.db.Query(`SELECT transfer_id, idx, rel_path, local_path, size, status, error, note, remote_parent, remote_id, view_url, asset_id, finished_at FROM transfer_files WHERE transfer_id=? ORDER BY idx`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []File
	for rows.Next() {
		var f File
		var lp, st, e, n, rp, rid, vu, aid sql.NullString
		var fin sql.NullInt64
		if err := rows.Scan(&f.TransferID, &f.Idx, &f.RelPath, &lp, &f.Size, &st, &e, &n, &rp, &rid, &vu, &aid, &fin); err != nil {
			return nil, err
		}
		f.LocalPath, f.Status, f.Error, f.Note, f.RemoteParent, f.RemoteID, f.ViewURL, f.AssetID, f.FinishedAt =
			lp.String, st.String, e.String, n.String, rp.String, rid.String, vu.String, aid.String, fin.Int64
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) Delete(id string) {
	s.db.Exec(`DELETE FROM transfer_files WHERE transfer_id=?`, id)
	s.db.Exec(`DELETE FROM transfers WHERE id=?`, id)
}

// ExportCSV writes one row per file across all transfers.
func (s *Store) ExportCSV(path string) error {
	fh, err := os.Create(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	w := csv.NewWriter(fh)
	w.Write([]string{"transfer_id", "direction", "transfer", "started", "finished", "transfer_status", "avg_MBps", "source", "destination", "file", "local_path", "size_bytes", "file_status", "error", "frameio_url"})
	rows, err := s.db.Query(`SELECT t.id, t.kind, t.title, t.started_at, t.finished_at, t.status, t.avg_bps, t.source, t.dest,
		f.rel_path, f.local_path, f.size, f.status, COALESCE(NULLIF(f.error,''), t.error), f.view_url
		FROM transfers t LEFT JOIN transfer_files f ON f.transfer_id = t.id ORDER BY t.created_at DESC, f.idx`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, kind, title, status, source, dest string
		var rel, lp, fst, errS, vu sql.NullString
		var started, finished, size sql.NullInt64
		var avg sql.NullFloat64
		if err := rows.Scan(&id, &kind, &title, &started, &finished, &status, &avg, &source, &dest, &rel, &lp, &size, &fst, &errS, &vu); err != nil {
			return err
		}
		w.Write([]string{id, kind, title, fmtTime(started.Int64), fmtTime(finished.Int64), status,
			strconv.FormatFloat(avg.Float64/1e6, 'f', 2, 64), source, dest, rel.String, lp.String,
			strconv.FormatInt(size.Int64, 10), fst.String, errS.String, vu.String})
	}
	w.Flush()
	return w.Error()
}

func fmtTime(ms int64) string {
	if ms == 0 {
		return ""
	}
	return time.UnixMilli(ms).Format("2006-01-02 15:04:05")
}

// Log appends a line to this month's transfer log.
func (s *Store) Log(format string, args ...any) {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	now := time.Now()
	p := filepath.Join(s.logDir, "transfers-"+now.Format("2006-01")+".log")
	fh, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer fh.Close()
	fmt.Fprintf(fh, "%s  %s\n", now.Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}

// LogPath returns the current month's log file path.
func (s *Store) LogPath() string {
	return filepath.Join(s.logDir, "transfers-"+time.Now().Format("2006-01")+".log")
}
