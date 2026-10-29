package history

import "time"

const unfinishedSchema = `
CREATE TABLE IF NOT EXISTS unfinished_uploads(
  file_id TEXT PRIMARY KEY, account_id TEXT, transfer_id TEXT, rel_path TEXT, pid INTEGER, run TEXT, created_at INTEGER);
CREATE TABLE IF NOT EXISTS replaced_files(
  file_id TEXT PRIMARY KEY, account_id TEXT, new_id TEXT, transfer_id TEXT, rel_path TEXT, pid INTEGER, run TEXT, created_at INTEGER);
`

// Unfinished is a file created on Frame.io whose bytes haven't all been sent
// yet. Its owner (PID and Run) is the process sending it. A row with no owner,
// or whose owner is gone (the app quit or crashed mid-upload), is a copy to
// delete from Frame.io.
type Unfinished struct {
	FileID     string
	AccountID  string
	TransferID string
	RelPath    string
	PID        int
	Run        string
	CreatedAt  int64
}

// AddUnfinished records an upload in progress, or changes an existing row's
// owner (none: delete it).
func (s *Store) AddUnfinished(u Unfinished) {
	if u.CreatedAt == 0 {
		u.CreatedAt = time.Now().UnixMilli()
	}
	s.db.Exec(`INSERT INTO unfinished_uploads VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(file_id) DO UPDATE SET pid=excluded.pid, run=excluded.run`,
		u.FileID, u.AccountID, u.TransferID, u.RelPath, u.PID, u.Run, u.CreatedAt)
}

func (s *Store) UnfinishedUploads() ([]Unfinished, error) {
	rows, err := s.db.Query(`SELECT file_id, account_id, transfer_id, rel_path, pid, run, created_at FROM unfinished_uploads ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Unfinished
	for rows.Next() {
		var u Unfinished
		if err := rows.Scan(&u.FileID, &u.AccountID, &u.TransferID, &u.RelPath, &u.PID, &u.Run, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UploadSent drops the row of an upload whose bytes all reached Frame.io.
func (s *Store) UploadSent(fileID string) {
	s.db.Exec(`DELETE FROM unfinished_uploads WHERE file_id=?`, fileID)
}

// UploadDeleted drops the rows of a file deleted from Frame.io (an unfinished
// upload, or a version that was replaced), and the links to it in history.
func (s *Store) UploadDeleted(fileID string) {
	s.db.Exec(`DELETE FROM unfinished_uploads WHERE file_id=?`, fileID)
	s.db.Exec(`DELETE FROM replaced_files WHERE file_id=?`, fileID)
	s.db.Exec(`UPDATE transfers SET link='' WHERE link IN (SELECT view_url FROM transfer_files WHERE remote_id=? AND view_url!='')`, fileID)
	s.db.Exec(`UPDATE transfer_files SET remote_id='', view_url='' WHERE remote_id=?`, fileID)
	s.db.Exec(`UPDATE watch_files SET remote_id='' WHERE remote_id=?`, fileID)
}

// Replaced is a Frame.io file that a new upload of the same file (NewID)
// replaces. It's deleted once Frame.io confirms the new upload. A row with no
// owner, or whose owner is gone (the app quit while Frame.io was confirming),
// is left to CleanUp, which checks the new upload first.
type Replaced struct {
	FileID     string
	AccountID  string
	NewID      string
	TransferID string
	RelPath    string
	PID        int
	Run        string
	CreatedAt  int64
}

// AddReplaced records a file to delete once its replacement is confirmed, or
// changes an existing row's owner (none: CleanUp's to finish).
func (s *Store) AddReplaced(r Replaced) {
	if r.CreatedAt == 0 {
		r.CreatedAt = time.Now().UnixMilli()
	}
	s.db.Exec(`INSERT INTO replaced_files VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(file_id) DO UPDATE SET new_id=excluded.new_id, pid=excluded.pid, run=excluded.run`,
		r.FileID, r.AccountID, r.NewID, r.TransferID, r.RelPath, r.PID, r.Run, r.CreatedAt)
}

func (s *Store) ReplacedFiles() ([]Replaced, error) {
	rows, err := s.db.Query(`SELECT file_id, account_id, new_id, transfer_id, rel_path, pid, run, created_at FROM replaced_files ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Replaced
	for rows.Next() {
		var r Replaced
		if err := rows.Scan(&r.FileID, &r.AccountID, &r.NewID, &r.TransferID, &r.RelPath, &r.PID, &r.Run, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// KeepReplaced drops the row of a file whose replacement didn't make it to
// Frame.io: the earlier version stays.
func (s *Store) KeepReplaced(fileID string) {
	s.db.Exec(`DELETE FROM replaced_files WHERE file_id=?`, fileID)
}

// Uploaded returns which of these Frame.io files FioTransfer uploaded and
// Frame.io confirmed (none if history can't be read).
func (s *Store) Uploaded(fileIDs []string) map[string]bool {
	out := map[string]bool{}
	for _, id := range fileIDs {
		var n int
		if id != "" && s.db.QueryRow(`SELECT COUNT(*) FROM transfer_files WHERE remote_id=? AND status='done'`, id).Scan(&n) == nil && n > 0 {
			out[id] = true
		}
	}
	return out
}
