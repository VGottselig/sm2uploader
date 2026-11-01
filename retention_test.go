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

// Failed uploads of older versions (pending cache entries) become stored rows at
// their original time; the copy then belongs to the row and the retention rule
// treats it like any other.
func TestMigratePendingBecomesStoredRow(t *testing.T) {
	withCacheDir(t)
	f := newFake(t)
	sm, _ := f.start()
	l := withLedgerAndSpoolman(t, sm)

	RecordUpload(&GcodeUse{HasBlock: false}, "new.nc", 10, false, "")
	e, err := cacheFromUpload(strings.NewReader("gcode bytes"), "old.gcode", CacheKindPending)
	if err != nil {
		t.Fatal(err)
	}
	e.Time = time.Now().Add(-48 * time.Hour)
	e.Reason = "printer is not available: snapmaker.fritz.box"
	MigratePending()

	if l.Len() != 2 {
		t.Fatalf("%d rows, want 2", l.Len())
	}
	newest, oldest := l.Recent(2)[0], l.Recent(2)[1]
	if newest.File != "new.nc" || oldest.File != "old.gcode" {
		t.Errorf("order = %s, %s; the migrated row belongs at its old time", newest.File, oldest.File)
	}
	if !oldest.Stored || oldest.FileID != e.ID || oldest.UploadError == "" {
		t.Errorf("migrated row = %+v", oldest)
	}
	if got, _ := CacheGet(e.ID); !got.Held() {
		t.Error("the entry is still pending")
	}
	if len(cacheLegacyPending()) != 0 {
		t.Error("migration must run only once")
	}
	h := uploadsHTML()
	if !strings.Contains(h, "tag-err") || !strings.Contains(h, ">Print</button>") {
		t.Error("the migrated row should show the warning tag and Print")
	}
	if strings.Contains(h, "/cache/") {
		t.Error("the table still carries cache-only actions")
	}
	ApplyRetention()
	if !fileExists(t, cacheDataPath(e.ID)) {
		t.Error("retention deleted the copy of a visible row")
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
