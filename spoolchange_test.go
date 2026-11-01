package main

import (
	"strings"
	"testing"
)

// twoSpools is the fake's stock for the filament-change tests: the spool the
// preset maps to (22) and a second one the user picks by hand (30).
func twoSpools(f *fakeSpoolman) {
	f.spools = []SmSpool{
		{ID: 22, InitialWeight: f64(1000), Filament: SmFilament{Name: "DEEPLEE PLA PRO Rosa", ColorHex: "F19CB4"},
			Extra: map[string]string{presetField: jsonString("DEEPLEE PLA PRO Rosa")}},
		{ID: 30, InitialWeight: f64(1000), Filament: SmFilament{Name: "DEEPLEE PLA+ Black", ColorHex: "000000"}},
	}
}

func spool(f *fakeSpoolman, id int) SmSpool {
	for _, sp := range f.spools {
		if sp.ID == id {
			return sp
		}
	}
	return SmSpool{}
}

// A booked row: refund on the old spool, the same grams on the new one.
func TestChangeSpoolRebooksBookedRow(t *testing.T) {
	f := newFake(t)
	twoSpools(f)
	sm, _ := f.start()
	l := withLedgerAndSpoolman(t, sm)

	RecordUpload(sampleUse(), "Elefant.gcode", 100, true, "")
	id := l.Recent(1)[0].ID

	if err := ChangeSpool(id, 30); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.useIDs, ","); got != "22,22,30" {
		t.Fatalf("use calls went to %s, want 22,22,30", got)
	}
	if f.uses[1] != -46 || f.uses[2] != 46 {
		t.Fatalf("use deltas = %v, want [46 -46 46]", f.uses)
	}
	if spool(f, 22).UsedWeight != 0 || spool(f, 30).UsedWeight != 46 {
		t.Errorf("used = %v / %v, want 0 / 46", spool(f, 22).UsedWeight, spool(f, 30).UsedWeight)
	}
	r, _ := l.Get(id)
	if r.SpoolID != 30 || !r.SpoolManual || r.BookedG != 46 || r.Status != StatusBooked {
		t.Errorf("row = spool %d manual %v %d g %s, want 30/true/46/booked", r.SpoolID, r.SpoolManual, r.BookedG, r.Status)
	}
	if r.RemainingAfterG == nil || *r.RemainingAfterG != 954 {
		t.Errorf("RemainingAfterG = %v, want 954", r.RemainingAfterG)
	}
	// The preset mapping is not moved: one row is corrected, not future uploads.
	if extraString(spool(f, 30).Extra, presetField) != "" {
		t.Error("the preset mapping must stay on the old spool")
	}
}

// An open row only switches the spool -- nothing is booked.
func TestChangeSpoolOpenRowBooksNothing(t *testing.T) {
	f := newFake(t)
	twoSpools(f)
	sm, _ := f.start()
	l := withLedgerAndSpoolman(t, sm)

	RecordUpload(sampleUse(), "Elefant.gcode", 100, false, "")
	id := l.Recent(1)[0].ID

	if err := ChangeSpool(id, 30); err != nil {
		t.Fatal(err)
	}
	if len(f.uses) != 0 {
		t.Errorf("booked %v on an open row", f.uses)
	}
	r, _ := l.Get(id)
	if r.SpoolID != 30 || !r.SpoolManual || r.Status != StatusOpen {
		t.Errorf("row = spool %d manual %v %s", r.SpoolID, r.SpoolManual, r.Status)
	}
}

// After a change, Book/Set/Cancel follow the chosen spool, not the preset.
func TestBookRowFollowsChosenSpool(t *testing.T) {
	f := newFake(t)
	twoSpools(f)
	sm, _ := f.start()
	l := withLedgerAndSpoolman(t, sm)

	RecordUpload(sampleUse(), "Elefant.gcode", 100, false, "")
	id := l.Recent(1)[0].ID
	if err := ChangeSpool(id, 30); err != nil {
		t.Fatal(err)
	}
	if err := BookRow(id, 46); err != nil {
		t.Fatal(err)
	}
	if err := BookRow(id, 0); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.useIDs, ","); got != "30,30" {
		t.Errorf("use calls went to %s, want 30,30", got)
	}
}

// A booked row refunds on the spool it was booked on, even when the preset
// meanwhile resolves to another spool (new spool of the same filament).
func TestCancelRefundsTheBookedSpool(t *testing.T) {
	f := newFake(t)
	twoSpools(f)
	sm, _ := f.start()
	l := withLedgerAndSpoolman(t, sm)

	RecordUpload(sampleUse(), "Elefant.gcode", 100, true, "")
	id := l.Recent(1)[0].ID
	// The preset now points at spool 30 instead.
	f.spools[0].Extra = nil
	f.spools[1].Extra = map[string]string{presetField: jsonString("DEEPLEE PLA PRO Rosa")}

	if err := BookRow(id, 0); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.useIDs, ","); got != "22,22" {
		t.Errorf("use calls went to %s, want the refund on 22", got)
	}
}

// Unknown target spool: an error, and nothing moves.
func TestChangeSpoolUnknownSpool(t *testing.T) {
	f := newFake(t)
	twoSpools(f)
	sm, _ := f.start()
	l := withLedgerAndSpoolman(t, sm)

	RecordUpload(sampleUse(), "Elefant.gcode", 100, true, "")
	id := l.Recent(1)[0].ID
	if err := ChangeSpool(id, 99); err == nil {
		t.Fatal("no error for an unknown spool")
	}
	r, _ := l.Get(id)
	if r.SpoolID != 22 || r.SpoolManual || r.BookedG != 46 || len(f.uses) != 1 {
		t.Errorf("row changed: %+v, uses %v", r, f.uses)
	}
}

// Refund done, booking on the new spool rejected: the row is open on the new
// spool and flagged, so "Book" finishes the job.
func TestChangeSpoolSecondBookingFails(t *testing.T) {
	f := newFake(t)
	twoSpools(f)
	sm, _ := f.start()
	l := withLedgerAndSpoolman(t, sm)

	RecordUpload(sampleUse(), "Elefant.gcode", 100, true, "")
	id := l.Recent(1)[0].ID
	f.failUse = map[string]bool{"30": true}

	if err := ChangeSpool(id, 30); err == nil {
		t.Fatal("no error although the booking was rejected")
	}
	r, _ := l.Get(id)
	if r.SpoolID != 30 || r.BookedG != 0 || r.Status != StatusFailure || r.Error == "" {
		t.Errorf("row = spool %d %d g %s %q, want 30/0/failed/msg", r.SpoolID, r.BookedG, r.Status, r.Error)
	}
	if spool(f, 22).UsedWeight != 0 {
		t.Errorf("old spool still holds %v g", spool(f, 22).UsedWeight)
	}

	f.failUse = nil
	if err := BookRow(id, 46); err != nil {
		t.Fatal(err)
	}
	if spool(f, 30).UsedWeight != 46 {
		t.Errorf("new spool = %v g after Book, want 46", spool(f, 30).UsedWeight)
	}
}

// Table: the menu carries the rare actions, the picker lists the spools and a
// hand-picked spool shows its own name.
func TestUploadsHTMLMenuAndPicker(t *testing.T) {
	f := newFake(t)
	twoSpools(f)
	sm, _ := f.start()
	l := withLedgerAndSpoolman(t, sm)

	RecordUpload(sampleUse(), "Elefant.gcode", 100, true, "")
	id := l.Recent(1)[0].ID

	h := uploadsHTML()
	for _, want := range []string{
		`<details class="more">`, `data-chg`, `action="/row/spool"`,
		`<option value="22" selected>`, `<option value="30">DEEPLEE PLA+ Black — 1000 g</option>`,
	} {
		if !strings.Contains(h, want) {
			t.Errorf("table is missing %q", want)
		}
	}

	if err := ChangeSpool(id, 30); err != nil {
		t.Fatal(err)
	}
	h = uploadsHTML()
	if !strings.Contains(h, `<option value="30" selected>`) {
		t.Error("the picker does not preselect the chosen spool")
	}
	if !strings.Contains(h, `G-code preset: DEEPLEE PLA PRO Rosa`) || !strings.Contains(h, `background:#000000`) {
		t.Error("the filament cell does not show the chosen spool")
	}
}
