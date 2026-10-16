package history

import (
	"database/sql"
	"time"
)

const watchSchema = `
CREATE TABLE IF NOT EXISTS watches(
  id TEXT PRIMARY KEY, local_path TEXT, account_id TEXT, folder_id TEXT, dest_label TEXT,
  enabled INTEGER, created_at INTEGER);
CREATE TABLE IF NOT EXISTS watch_files(
  watch_id TEXT, rel_path TEXT, size INTEGER, mtime INTEGER, state TEXT, attempts INTEGER,
  error TEXT, remote_id TEXT, updated_at INTEGER, PRIMARY KEY(watch_id, rel_path));
CREATE TABLE IF NOT EXISTS watch_dirs(
  watch_id TEXT, rel_dir TEXT, remote_id TEXT, PRIMARY KEY(watch_id, rel_dir));
CREATE TABLE IF NOT EXISTS watch_remote(
  watch_id TEXT, remote_id TEXT, rel_path TEXT, size INTEGER, state TEXT, attempts INTEGER,
  error TEXT, updated_at INTEGER, PRIMARY KEY(watch_id, remote_id));
`

// Watch directions.
const (
	DirUpload   = "upload"   // local folder → Frame.io
	DirDownload = "download" // Frame.io → local folder
	DirMirror   = "mirror"   // both ways; nothing is deleted on either side
)

type Watch struct {
	ID        string `json:"id"`
	LocalPath string `json:"local_path"`
	AccountID string `json:"account_id"`
	FolderID  string `json:"folder_id"`
	DestLabel string `json:"dest_label"`
	Enabled   bool   `json:"enabled"`
	CreatedAt int64  `json:"created_at"`
	Direction string `json:"direction"`
	// IgnoreRemote: the next Frame.io listing records what it finds as existing
	// files instead of downloading them (set when downloading starts, unless
	// existing files are included).
	IgnoreRemote bool `json:"ignore_remote"`
}

func (w Watch) Uploads() bool   { return w.Direction != DirDownload }
func (w Watch) Downloads() bool { return w.Direction == DirDownload || w.Direction == DirMirror }

// ValidDirection reports whether d names a watch direction.
func ValidDirection(d string) bool { return d == DirUpload || d == DirDownload || d == DirMirror }

// Watch file states. Local files (watch_files) are uploaded, downloaded,
// failed, ignored or canceled; Frame.io files (watch_remote) are downloaded,
// waiting, failed, ignored or canceled.
const (
	WatchUploaded   = "uploaded"
	WatchDownloaded = "downloaded" // written by the watch from Frame.io
	WatchFailed     = "failed"
	WatchIgnored    = "ignored"  // existed before the watch was created, or already on the other side
	WatchCanceled   = "canceled" // the user canceled its transfer; not retried automatically
	WatchWaiting    = "waiting"  // Frame.io isn't serving the original yet; tried again at the next listing
)

type WatchFile struct {
	RelPath   string
	Size      int64
	Mtime     int64
	State     string
	Attempts  int
	Error     string
	RemoteID  string
	UpdatedAt int64
}

// WatchRemote is a Frame.io file a downloading watch has dealt with, by file ID.
type WatchRemote struct {
	RemoteID  string
	RelPath   string
	Size      int64
	State     string
	Attempts  int
	Error     string
	UpdatedAt int64
}

func (s *Store) AddWatch(w Watch) error {
	_, err := s.db.Exec(`INSERT INTO watches(id, local_path, account_id, folder_id, dest_label, enabled, created_at, direction, ignore_remote)
		VALUES(?,?,?,?,?,?,?,?,?)`, w.ID, w.LocalPath, w.AccountID, w.FolderID, w.DestLabel, w.Enabled, w.CreatedAt, w.Direction, w.IgnoreRemote)
	return err
}

func (s *Store) Watches() ([]Watch, error) {
	rows, err := s.db.Query(`SELECT id, local_path, account_id, folder_id, dest_label, enabled, created_at, direction, ignore_remote
		FROM watches ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Watch
	for rows.Next() {
		var w Watch
		if err := rows.Scan(&w.ID, &w.LocalPath, &w.AccountID, &w.FolderID, &w.DestLabel, &w.Enabled, &w.CreatedAt, &w.Direction, &w.IgnoreRemote); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) UpdateWatch(w Watch) error {
	_, err := s.db.Exec(`UPDATE watches SET local_path=?, account_id=?, folder_id=?, dest_label=?, direction=?, ignore_remote=? WHERE id=?`,
		w.LocalPath, w.AccountID, w.FolderID, w.DestLabel, w.Direction, w.IgnoreRemote, w.ID)
	return err
}

// SetIgnoreRemote clears or sets the "record what Frame.io holds as existing" flag.
func (s *Store) SetIgnoreRemote(id string, on bool) {
	s.db.Exec(`UPDATE watches SET ignore_remote=? WHERE id=?`, on, id)
}

// ResetWatchFiles forgets every local file record (used when the watched folder changes).
func (s *Store) ResetWatchFiles(id string) {
	s.db.Exec(`DELETE FROM watch_files WHERE watch_id=?`, id)
}

// ResetWatchRemotes forgets every Frame.io file record (used when the Frame.io folder changes).
func (s *Store) ResetWatchRemotes(id string) {
	s.db.Exec(`DELETE FROM watch_remote WHERE watch_id=?`, id)
}

// RequeueCanceled lets canceled files transfer again.
func (s *Store) RequeueCanceled(id string) {
	s.db.Exec(`DELETE FROM watch_files WHERE watch_id=? AND state='canceled'`, id)
	s.db.Exec(`DELETE FROM watch_remote WHERE watch_id=? AND state='canceled'`, id)
}

// ForgetWatchFiles drops the failed and canceled records of these local files
// so the watch uploads them again.
func (s *Store) ForgetWatchFiles(id string, rels []string) error {
	return s.forget(`DELETE FROM watch_files WHERE watch_id=? AND rel_path=? AND state IN ('failed','canceled')`, id, rels)
}

// ForgetWatchRemotes drops the failed, waiting and canceled records of these
// Frame.io files so the watch downloads them again.
func (s *Store) ForgetWatchRemotes(id string, remoteIDs []string) error {
	return s.forget(`DELETE FROM watch_remote WHERE watch_id=? AND remote_id=? AND state IN ('failed','waiting','canceled')`, id, remoteIDs)
}

func (s *Store) forget(query, id string, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(query)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, k := range keys {
		if _, err := stmt.Exec(id, k); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) SetWatchEnabled(id string, on bool) {
	s.db.Exec(`UPDATE watches SET enabled=? WHERE id=?`, on, id)
}

func (s *Store) DeleteWatch(id string) {
	s.db.Exec(`DELETE FROM watch_files WHERE watch_id=?`, id)
	s.db.Exec(`DELETE FROM watch_remote WHERE watch_id=?`, id)
	s.db.Exec(`DELETE FROM watch_dirs WHERE watch_id=?`, id)
	s.db.Exec(`DELETE FROM watches WHERE id=?`, id)
}

func (s *Store) WatchFiles(id string) (map[string]WatchFile, error) {
	rows, err := s.db.Query(`SELECT rel_path, size, mtime, state, attempts, error, remote_id, updated_at FROM watch_files WHERE watch_id=?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]WatchFile{}
	for rows.Next() {
		var f WatchFile
		var e, r sql.NullString
		if err := rows.Scan(&f.RelPath, &f.Size, &f.Mtime, &f.State, &f.Attempts, &e, &r, &f.UpdatedAt); err != nil {
			return nil, err
		}
		f.Error, f.RemoteID = e.String, r.String
		out[f.RelPath] = f
	}
	return out, rows.Err()
}

func (s *Store) PutWatchFiles(id string, files []WatchFile) error {
	return s.putWatchFiles(`INSERT OR REPLACE`, id, files)
}

// BaselineWatchFiles records files as existing, keeping any record they already have.
func (s *Store) BaselineWatchFiles(id string, files []WatchFile) error {
	return s.putWatchFiles(`INSERT OR IGNORE`, id, files)
}

func (s *Store) putWatchFiles(verb, id string, files []WatchFile) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(verb + ` INTO watch_files VALUES(?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	now := time.Now().UnixMilli()
	for _, f := range files {
		if f.UpdatedAt == 0 {
			f.UpdatedAt = now
		}
		if _, err := stmt.Exec(id, f.RelPath, f.Size, f.Mtime, f.State, f.Attempts, f.Error, f.RemoteID, f.UpdatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) WatchRemotes(id string) (map[string]WatchRemote, error) {
	rows, err := s.db.Query(`SELECT remote_id, rel_path, size, state, attempts, error, updated_at FROM watch_remote WHERE watch_id=?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]WatchRemote{}
	for rows.Next() {
		var f WatchRemote
		var e sql.NullString
		if err := rows.Scan(&f.RemoteID, &f.RelPath, &f.Size, &f.State, &f.Attempts, &e, &f.UpdatedAt); err != nil {
			return nil, err
		}
		f.Error = e.String
		out[f.RemoteID] = f
	}
	return out, rows.Err()
}

func (s *Store) PutWatchRemotes(id string, files []WatchRemote) error {
	return s.putWatchRemotes(`INSERT OR REPLACE`, id, files)
}

// BaselineWatchRemotes records Frame.io files as existing, keeping any record they already have.
func (s *Store) BaselineWatchRemotes(id string, files []WatchRemote) error {
	return s.putWatchRemotes(`INSERT OR IGNORE`, id, files)
}

func (s *Store) putWatchRemotes(verb, id string, files []WatchRemote) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(verb + ` INTO watch_remote VALUES(?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	now := time.Now().UnixMilli()
	for _, f := range files {
		if f.UpdatedAt == 0 {
			f.UpdatedAt = now
		}
		if _, err := stmt.Exec(id, f.RemoteID, f.RelPath, f.Size, f.State, f.Attempts, f.Error, f.UpdatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type WatchStats struct {
	Uploaded     int   `json:"uploaded"`
	Downloaded   int   `json:"downloaded"`
	Failed       int   `json:"failed"`
	Canceled     int   `json:"canceled"`
	LastTransfer int64 `json:"last_transfer"`
}

func (s *Store) WatchStats(id string) WatchStats {
	var st WatchStats
	var lastUp, lastDown sql.NullInt64
	s.db.QueryRow(`SELECT COUNT(*) FILTER (WHERE state='uploaded'), COUNT(*) FILTER (WHERE state='failed'),
		COUNT(*) FILTER (WHERE state='canceled'), MAX(CASE WHEN state='uploaded' THEN updated_at END)
		FROM watch_files WHERE watch_id=?`, id).Scan(&st.Uploaded, &st.Failed, &st.Canceled, &lastUp)
	var failed, canceled int
	s.db.QueryRow(`SELECT COUNT(*) FILTER (WHERE state='downloaded'), COUNT(*) FILTER (WHERE state='failed'),
		COUNT(*) FILTER (WHERE state='canceled'), MAX(CASE WHEN state='downloaded' THEN updated_at END)
		FROM watch_remote WHERE watch_id=?`, id).Scan(&st.Downloaded, &failed, &canceled, &lastDown)
	st.Failed += failed
	st.Canceled += canceled
	st.LastTransfer = max(lastUp.Int64, lastDown.Int64)
	return st
}

func (s *Store) WatchDirs(id string) map[string]string {
	out := map[string]string{}
	rows, err := s.db.Query(`SELECT rel_dir, remote_id FROM watch_dirs WHERE watch_id=?`, id)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var d, r string
		if rows.Scan(&d, &r) == nil {
			out[d] = r
		}
	}
	return out
}

func (s *Store) PutWatchDir(id, rel, remote string) {
	s.db.Exec(`INSERT OR REPLACE INTO watch_dirs VALUES(?,?,?)`, id, rel, remote)
}

func (s *Store) ClearWatchDirs(id string) {
	s.db.Exec(`DELETE FROM watch_dirs WHERE watch_id=?`, id)
}
