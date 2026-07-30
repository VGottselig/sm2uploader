package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestLedgerRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "uploads.yaml")
	l := LoadLedger(path)
	if l.Len() != 0 {
		t.Fatalf("fresh ledger has %d rows", l.Len())
	}

	now := time.Date(2026, 7, 30, 10, 47, 39, 0, time.UTC)
	rem := 954
	row := &UploadRow{
		ID: "u1-T1", UploadID: "u1", Time: now, File: "Elefant.gcode", Size: 3093450,
		Slot: 1, Preset: "DEEPLEE PLA PRO Rosa", Colour: "#F19CB4", Material: "PLA",
		Vendor: "DEEPLEE", Density: 1.24, Diameter: 1.75, CostPerKg: 13.59,
		GcodeG: 46, GcodeMM: 15398.04, PrintTime: "4h 24m 54s",
		BookedG: 46, RemainingAfterG: &rem, SpoolID: 3, Status: StatusBooked,
	}
	if err := l.Append(row); err != nil {
		t.Fatal(err)
	}

	reloaded := LoadLedger(path)
	if reloaded.Len() != 1 {
		t.Fatalf("reloaded %d rows, want 1", reloaded.Len())
	}
	got, ok := reloaded.Get("u1-T1")
	if !ok {
		t.Fatal("row not found after reload")
	}
	if got.Preset != row.Preset || got.GcodeG != 46 || got.BookedG != 46 || got.SpoolID != 3 {
		t.Errorf("row lost data: %+v", got)
	}
	if got.RemainingAfterG == nil || *got.RemainingAfterG != 954 {
		t.Errorf("RemainingAfterG = %v, want 954", got.RemainingAfterG)
	}
	if !got.Time.Equal(now) {
		t.Errorf("Time = %v, want %v", got.Time, now)
	}
}

// Get/Recent hand out copies -- a caller mutating them must not touch the ledger.
func TestLedgerHandsOutCopies(t *testing.T) {
	l := LoadLedger(filepath.Join(t.TempDir(), "uploads.yaml"))
	if err := l.Append(&UploadRow{ID: "a", GcodeG: 10, BookedG: 0, Status: StatusOpen}); err != nil {
		t.Fatal(err)
	}
	got, _ := l.Get("a")
	got.BookedG = 999
	again, _ := l.Get("a")
	if again.BookedG != 0 {
		t.Errorf("BookedG = %d, ledger was mutated through the copy", again.BookedG)
	}
	rec := l.Recent(1)
	rec[0].BookedG = 888
	again, _ = l.Get("a")
	if again.BookedG != 0 {
		t.Errorf("BookedG = %d, ledger was mutated through Recent()", again.BookedG)
	}
}

func TestLedgerRecentIsNewestFirstAndCapped(t *testing.T) {
	l := LoadLedger(filepath.Join(t.TempDir(), "uploads.yaml"))
	for i := 0; i < 15; i++ {
		if err := l.Append(&UploadRow{ID: fmt.Sprintf("r%02d", i), Status: StatusOpen}); err != nil {
			t.Fatal(err)
		}
	}
	rec := l.Recent(10)
	if len(rec) != 10 {
		t.Fatalf("Recent(10) returned %d rows", len(rec))
	}
	if rec[0].ID != "r14" || rec[9].ID != "r05" {
		t.Errorf("order wrong: first %s, last %s", rec[0].ID, rec[9].ID)
	}
	if l.Len() != 15 {
		t.Errorf("Len = %d, want 15 -- everything is kept, only rendering is capped", l.Len())
	}
}

func TestLedgerUpdateUnknownRow(t *testing.T) {
	l := LoadLedger(filepath.Join(t.TempDir(), "uploads.yaml"))
	if err := l.Update("nope", func(*UploadRow) {}); err == nil {
		t.Error("Update on an unknown ID returned nil error")
	}
}

// An Orca upload (append) and a "book" click (update) really can overlap.
// Run with -race.
func TestLedgerConcurrentAppendAndUpdate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "uploads.yaml")
	l := LoadLedger(path)
	if err := l.Append(&UploadRow{ID: "hot", GcodeG: 46, Status: StatusOpen}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(3)
		go func(i int) {
			defer wg.Done()
			l.Append(&UploadRow{ID: fmt.Sprintf("new%d", i), Status: StatusOpen})
		}(i)
		go func(i int) {
			defer wg.Done()
			l.Update("hot", func(r *UploadRow) { r.BookedG = i })
		}(i)
		go func() {
			defer wg.Done()
			_ = l.Recent(10)
		}()
	}
	wg.Wait()

	if l.Len() != 21 {
		t.Errorf("Len = %d, want 21", l.Len())
	}
	// The file must still be parseable -- atomic writes never leave a torn file.
	if LoadLedger(path).Len() != 21 {
		t.Error("reloaded ledger does not match, file was written non-atomically")
	}
}

// The temp file must not survive a successful write.
func TestLedgerLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "uploads.yaml")
	l := LoadLedger(path)
	if err := l.Append(&UploadRow{ID: "a", Status: StatusOpen}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("uploads.yaml.tmp still exists after the write")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("directory holds %d files, want only uploads.yaml", len(entries))
	}
}

// A damaged file must not take the uploader down.
func TestLoadLedgerWithBrokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "uploads.yaml")
	if err := os.WriteFile(path, []byte("rows: [this is: not: valid: yaml"), 0644); err != nil {
		t.Fatal(err)
	}
	l := LoadLedger(path)
	if l.Len() != 0 {
		t.Errorf("Len = %d, want 0", l.Len())
	}
	if err := l.Append(&UploadRow{ID: "a", Status: StatusOpen}); err != nil {
		t.Errorf("Append after a damaged file: %v", err)
	}
}

func TestNewUploadIDIsUnique(t *testing.T) {
	now := time.Date(2026, 7, 30, 10, 47, 39, 0, time.UTC)
	a, b := newUploadID(now), newUploadID(now)
	if a == b {
		t.Errorf("IDs identical within the same second: %s", a)
	}
}
