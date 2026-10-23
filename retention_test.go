package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withCacheDir points CacheDir at a fresh directory and restores the globals
// afterwards -- the cache is package state, like the ledger.
func withCacheDir(t *testing.T) string {
	t.Helper()
	oldDir := CacheDir
	cacheMu.Lock()
	oldMap, oldSeq := cacheMap, cacheSeq
	cacheMap, cacheSeq = map[string]*CacheEntry{}, 0
	cacheMu.Unlock()
	CacheDir = filepath.Join(t.TempDir(), "cache")
	t.Cleanup(func() {
		CacheDir = oldDir
		cacheMu.Lock()
		cacheMap, cacheSeq = oldMap, oldSeq
		cacheMu.Unlock()
	})
	return CacheDir
}

func fileExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

// The table is the retention rule: more uploads than rows means the oldest rows
// are gone from the ledger, not just hidden.
func TestRetentionTrimsLedgerToTable(t *testing.T) {
	withCacheDir(t)
	l := withLedgerAndSpoolman(t, nil)

	total := uploadRowsRendered + 2
	for i := 0; i < total; i++ {
		RecordUpload(&GcodeUse{HasBlock: false}, fmt.Sprintf("f%02d.nc", i), 10, false, "")
	}
	if got := l.Len(); got != uploadRowsRendered {
		t.Fatalf("ledger has %d rows, want %d", got, uploadRowsRendered)
	}
	for _, gone := range []string{"f00.nc", "f01.nc"} {
		for _, r := range l.Recent(uploadRowsRendered) {
			if r.File == gone {
				t.Errorf("%s is still in the ledger", gone)
			}
		}
	}
	newest := l.Recent(1)[0].File
	if newest != fmt.Sprintf("f%02d.nc", total-1) {
		t.Errorf("newest row = %s, want %s", newest, fmt.Sprintf("f%02d.nc", total-1))
	}
}

// A held copy lives as long as a row points at it: while the row is visible
// "Print again" works, once it fell out of the table the file is gone.
func TestRetentionKeepsFileWhileRowVisible(t *testing.T) {
	withCacheDir(t)
	withLedgerAndSpoolman(t, nil)

	e, err := cacheFromUpload(strings.NewReader("gcode bytes"), "keep.gcode", CacheKindHeld)
	if err != nil {
		t.Fatal(err)
	}
	RecordUpload(&GcodeUse{HasBlock: false}, "keep.gcode", 11, false, e.ID)
	if !fileExists(t, cacheDataPath(e.ID)) {
		t.Fatal("file of a visible row was deleted")
	}

	// Push the row out of the table.
	for i := 0; i < uploadRowsRendered; i++ {
		RecordUpload(&GcodeUse{HasBlock: false}, fmt.Sprintf("n%02d.nc", i), 10, false, "")
	}
	if fileExists(t, cacheDataPath(e.ID)) {
		t.Error("file survived the row that was its only reference")
	}
	if fileExists(t, cacheMetaPath(e.ID)) {
		t.Error("cache record survived as well")
	}
}

// A failed upload has no ledger row of its own -- it is always rendered, so its
// file is never pruned, no matter how many uploads follow.
func TestRetentionKeepsPendingUploads(t *testing.T) {
	withCacheDir(t)
	withLedgerAndSpoolman(t, nil)

	e, err := cacheFromUpload(strings.NewReader("gcode bytes"), "pending.gcode", CacheKindPending)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < uploadRowsRendered+5; i++ {
		RecordUpload(&GcodeUse{HasBlock: false}, fmt.Sprintf("n%02d.nc", i), 10, false, "")
	}
	if !fileExists(t, cacheDataPath(e.ID)) {
		t.Error("cached upload was pruned although the table still shows it")
	}
	if n := len(CachePending()); n != 1 {
		t.Errorf("pending entries = %d, want 1", n)
	}
}

// "Print again" only appears where a copy exists -- a row without one must not
// offer a dead button.
func TestReprintButtonNeedsACopy(t *testing.T) {
	withCacheDir(t)

	TheLedger = LoadLedger(filepath.Join(t.TempDir(), "uploads.yaml"))
	e, err := cacheFromUpload(strings.NewReader("gcode bytes"), "again.gcode", CacheKindHeld)
	if err != nil {
		t.Fatal(err)
	}
	withCopy := &UploadRow{ID: "u1-T1", Time: time.Now(), File: "again.gcode", Status: StatusOpen, GcodeG: 10, FileID: e.ID}
	without := &UploadRow{ID: "u2-T1", Time: time.Now(), File: "old.gcode", Status: StatusOpen, GcodeG: 10}
	if err := TheLedger.Append(withCopy, without); err != nil {
		t.Fatal(err)
	}

	html := uploadsHTML()
	if !strings.Contains(html, "/row/print") {
		t.Error("no reprint form for a row with a cached copy")
	}
	if strings.Count(html, "/row/print") != 1 {
		t.Errorf("reprint offered %d times, want 1", strings.Count(html, "/row/print"))
	}
}
