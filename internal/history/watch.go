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
`

type Watch struct {
	ID        string `json:"id"`
	LocalPath string `json:"local_path"`
	AccountID string `json:"account_id"`
	FolderID  string `json:"folder_id"`
	DestLabel string `json:"dest_label"`
	Enabled   bool   `json:"enabled"`
	CreatedAt int64  `json:"created_at"`
}

// Watch file states.
const (
	WatchUploaded = "uploaded"
	WatchFailed   = "failed"
	WatchIgnored  = "ignored" // existed before the watch was created
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

func (s *Store) AddWatch(w Watch) error {
	_, err := s.db.Exec(`INSERT INTO watches VALUES(?,?,?,?,?,?,?)`, w.ID, w.LocalPath, w.AccountID, w.FolderID, w.DestLabel, w.Enabled, w.CreatedAt)
	return err
}

func (s *Store) Watches() ([]Watch, error) {
	rows, err := s.db.Query(`SELECT id, local_path, account_id, folder_id, dest_label, enabled, created_at FROM watches ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Watch
	for rows.Next() {
		var w Watch
		if err := rows.Scan(&w.ID, &w.LocalPath, &w.AccountID, &w.FolderID, &w.DestLabel, &w.Enabled, &w.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) SetWatchEnabled(id string, on bool) {
	s.db.Exec(`UPDATE watches SET enabled=? WHERE id=?`, on, id)
}

func (s *Store) DeleteWatch(id string) {
	s.db.Exec(`DELETE FROM watch_files WHERE watch_id=?`, id)
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
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO watch_files VALUES(?,?,?,?,?,?,?,?,?)`)
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

type WatchStats struct {
	Uploaded   int   `json:"uploaded"`
	Failed     int   `json:"failed"`
	LastUpload int64 `json:"last_upload"`
}

func (s *Store) WatchStats(id string) WatchStats {
	var st WatchStats
	var last sql.NullInt64
	s.db.QueryRow(`SELECT COUNT(*) FILTER (WHERE state='uploaded'), COUNT(*) FILTER (WHERE state='failed'),
		MAX(CASE WHEN state='uploaded' THEN updated_at END) FROM watch_files WHERE watch_id=?`, id).Scan(&st.Uploaded, &st.Failed, &last)
	st.LastUpload = last.Int64
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
