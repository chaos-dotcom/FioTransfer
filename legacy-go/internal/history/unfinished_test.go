package history

import "testing"

// Only a confirmed upload that's still on Frame.io counts as FioTransfer's.
func TestUploaded(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	err = s.Start(Transfer{ID: "t1", Kind: "upload"}, []File{
		{TransferID: "t1", Idx: 0, Status: "done", RemoteID: "ok"},
		{TransferID: "t1", Idx: 1, Status: "failed", RemoteID: "failed"},
		{TransferID: "t1", Idx: 2, Status: "canceled", RemoteID: "canceled"},
		{TransferID: "t1", Idx: 3, Status: "done", RemoteID: "deleted"},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.UploadDeleted("deleted")
	got := s.Uploaded([]string{"ok", "failed", "canceled", "deleted", "other", ""})
	if len(got) != 1 || !got["ok"] {
		t.Errorf("Uploaded = %v", got)
	}
}
