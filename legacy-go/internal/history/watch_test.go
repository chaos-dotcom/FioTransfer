package history

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// A database from before watch directions keeps its watches, as uploads.
func TestWatchMigration(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE watches(id TEXT PRIMARY KEY, local_path TEXT, account_id TEXT, folder_id TEXT, dest_label TEXT,
		enabled INTEGER, created_at INTEGER);
		INSERT INTO watches VALUES('w1', '/Volumes/Media/In', 'acct', 'folder', 'P / In', 1, 1700000000000)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(dir, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	list, err := s.Watches()
	if err != nil || len(list) != 1 {
		t.Fatalf("Watches = %v, %v", list, err)
	}
	w := list[0]
	if w.Direction != DirUpload || w.IgnoreRemote || !w.Uploads() || w.Downloads() || w.LocalPath != "/Volumes/Media/In" || w.OnChange != OnChangeOverwrite {
		t.Errorf("migrated watch = %+v", w)
	}

	n := Watch{ID: "w2", LocalPath: "/tmp/x", AccountID: "a", FolderID: "f", Enabled: true, CreatedAt: 1700000001000, Direction: DirMirror, IgnoreRemote: true,
		OnChange: OnChangeKeep}
	if err := s.AddWatch(n); err != nil {
		t.Fatal(err)
	}
	n.IgnoreRemote = false
	if err := s.UpdateWatch(n); err != nil {
		t.Fatal(err)
	}
	list, _ = s.Watches()
	if len(list) != 2 || list[1].Direction != DirMirror || list[1].IgnoreRemote || list[1].OnChange != OnChangeKeep {
		t.Errorf("watches = %+v", list)
	}

	s.PutWatchRemotes("w2", []WatchRemote{{RemoteID: "r1", RelPath: "a.mov", Size: 1, State: WatchDownloaded}})
	s.BaselineWatchRemotes("w2", []WatchRemote{{RemoteID: "r1", State: WatchIgnored}, {RemoteID: "r2", State: WatchIgnored}})
	got, _ := s.WatchRemotes("w2")
	if got["r1"].State != WatchDownloaded || got["r2"].State != WatchIgnored {
		t.Errorf("a baseline must not overwrite records: %+v", got)
	}
	if st := s.WatchStats("w2"); st.Downloaded != 1 || st.LastTransfer == 0 {
		t.Errorf("stats = %+v", st)
	}
}
