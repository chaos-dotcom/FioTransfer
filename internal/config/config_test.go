package config

import (
	"strings"
	"sync"
	"testing"
)

// Concurrent updates must not overwrite each other's changes.
func TestUpdateIsAtomic(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // keep the real settings file out of it
	const n = 50
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := Update(func(s *Settings) { s.LastFolderLabel += "x" }); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := Load().LastFolderLabel; got != strings.Repeat("x", n) {
		t.Errorf("label has %d of %d updates", len(got), n)
	}
}
