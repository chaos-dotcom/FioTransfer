package history

import "time"

const unfinishedSchema = `
CREATE TABLE IF NOT EXISTS unfinished_uploads(
  file_id TEXT PRIMARY KEY, account_id TEXT, transfer_id TEXT, rel_path TEXT, pid INTEGER, run TEXT, created_at INTEGER);
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

// UploadDeleted drops the row of an unfinished upload deleted from Frame.io,
// and the links to it in history.
func (s *Store) UploadDeleted(fileID string) {
	s.db.Exec(`DELETE FROM unfinished_uploads WHERE file_id=?`, fileID)
	s.db.Exec(`UPDATE transfer_files SET remote_id='', view_url='' WHERE remote_id=?`, fileID)
	s.db.Exec(`UPDATE watch_files SET remote_id='' WHERE remote_id=?`, fileID)
}
