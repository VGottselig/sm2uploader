package main

import (
	"strings"
	"testing"
)

// Orca "Upload" without start: open rows at 0 g, the spool resolved, the file
// kept -- and nothing booked.
func TestRecordStoredLeavesRowsOpen(t *testing.T) {
	withCacheDir(t)
	f := newFake(t)
	sm, _ := f.start()
	l := withLedgerAndSpoolman(t, sm)

	id := keepPrintedFile(strings.NewReader("G1\n"), "Elefant.gcode")
	if id == "" {
		t.Fatal("file not kept")
	}
	RecordStored(sampleUse(), "Elefant.gcode", 3, id)

	r := l.Recent(1)[0]
	if !r.Stored || r.FileID != id || r.BookedG != 0 || r.Status != StatusOpen || r.SpoolID != 22 {
		t.Errorf("row = %+v", r)
	}
	if len(f.uses) != 0 {
		t.Errorf("booked %v on a stored upload", f.uses)
	}
	h := uploadsHTML()
	for _, want := range []string{`only sm2`, `>Print</button>`, `value="book"`} {
		if !strings.Contains(h, want) {
			t.Errorf("table is missing %q", want)
		}
	}
	if strings.Contains(h, "Print again") {
		t.Error("a never printed row must offer Print, not Print again")
	}
	if len(CachePending()) != 0 {
		t.Error("a stored upload is not a failed upload")
	}
}

// "Print" on a stored row books that same row -- no second row.
func TestPrintedStoredBooksTheSameRows(t *testing.T) {
	withCacheDir(t)
	f := newFake(t)
	sm, _ := f.start()
	l := withLedgerAndSpoolman(t, sm)

	use := sampleUse()
	use.Slots = append(use.Slots, SlotUse{Slot: 2, Preset: "DEEPLEE PLA PRO Rosa", GcodeG: 4})
	RecordStored(use, "Elefant.gcode", 3, keepPrintedFile(strings.NewReader("G1\n"), "Elefant.gcode"))
	rows := l.Recent(2)

	PrintedStored(rows[0].UploadID, true)

	if l.Len() != 2 {
		t.Fatalf("%d rows, want the same 2", l.Len())
	}
	for _, r := range l.Recent(2) {
		if r.Stored || r.Status != StatusBooked || r.BookedG != r.GcodeG {
			t.Errorf("T%d = stored %v %s %d g, want booked with the G-code amount", r.Slot, r.Stored, r.Status, r.BookedG)
		}
	}
	if !strings.Contains(uploadsHTML(), "Print again") {
		t.Error("after the first print the row should offer Print again")
	}
}

// Uploaded but the start failed: no longer stored, nothing booked.
func TestPrintedStoredWithoutStart(t *testing.T) {
	f := newFake(t)
	sm, _ := f.start()
	l := withLedgerAndSpoolman(t, sm)
	RecordStored(sampleUse(), "Elefant.gcode", 3, "")
	r := l.Recent(1)[0]

	PrintedStored(r.UploadID, false)
	r, _ = l.Get(r.ID)
	if r.Stored || r.BookedG != 0 || len(f.uses) != 0 {
		t.Errorf("row = stored %v %d g, uses %v", r.Stored, r.BookedG, f.uses)
	}
}

// Delete takes all slot rows of the upload and the file copy, and leaves the
// Spoolman booking alone.
func TestDeleteUpload(t *testing.T) {
	dir := withCacheDir(t)
	f := newFake(t)
	sm, _ := f.start()
	l := withLedgerAndSpoolman(t, sm)

	keep := keepPrintedFile(strings.NewReader("keep\n"), "Other.gcode")
	RecordUpload(sampleUse(), "Other.gcode", 5, false, keep)
	use := sampleUse()
	use.Slots = append(use.Slots, SlotUse{Slot: 2, Preset: "DEEPLEE PLA PRO Rosa", GcodeG: 4})
	gone := keepPrintedFile(strings.NewReader("G1\n"), "Elefant.gcode")
	RecordUpload(use, "Elefant.gcode", 3, true, gone)
	booked := len(f.uses)

	row, n, err := DeleteUpload(l.Recent(1)[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || row.File != "Elefant.gcode" || l.Len() != 1 {
		t.Errorf("removed %d rows of %s, %d left; want 2 of Elefant, 1 left", n, row.File, l.Len())
	}
	if _, ok := CacheGet(gone); ok || fileExists(t, dir+"/"+gone+".data") {
		t.Error("the file copy of the deleted upload is still there")
	}
	if _, ok := CacheGet(keep); !ok {
		t.Error("the copy of the other upload was deleted too")
	}
	if len(f.uses) != booked {
		t.Errorf("delete touched Spoolman: uses %v", f.uses)
	}
	if !strings.Contains(uploadsHTML(), `action="/row/delete"`) {
		t.Error("no Delete entry in the menu")
	}
}

// A failed upload can be deleted by hand; a held copy cannot be deleted that way.
func TestCacheDelete(t *testing.T) {
	withCacheDir(t)
	withLedgerAndSpoolman(t, nil)
	e, err := cacheFromUpload(strings.NewReader("G1\n"), "Fail.gcode", CacheKindPending)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(uploadsHTML(), `action="/cache/delete"`) {
		t.Error("the cached row has no Delete entry")
	}
	if err := CacheDelete(e.ID); err != nil {
		t.Fatal(err)
	}
	if len(CachePending()) != 0 {
		t.Error("entry still pending")
	}
	held := keepPrintedFile(strings.NewReader("G1\n"), "Ok.gcode")
	if err := CacheDelete(held); err == nil {
		t.Error("a held copy must not be deletable on its own")
	}
}
