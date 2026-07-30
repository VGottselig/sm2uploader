package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// withLedgerAndSpoolman wires the package globals to a temp ledger and a fake
// Spoolman for the duration of one test.
func withLedgerAndSpoolman(t *testing.T, sm *Spoolman) *Ledger {
	t.Helper()
	oldLedger, oldSpoolman, oldLow := TheLedger, TheSpoolman, SpoolmanLowG
	t.Cleanup(func() { TheLedger, TheSpoolman, SpoolmanLowG = oldLedger, oldSpoolman, oldLow })
	TheLedger = LoadLedger(filepath.Join(t.TempDir(), "uploads.yaml"))
	TheSpoolman = sm
	SpoolmanLowG = 100
	return TheLedger
}

func sampleUse() *GcodeUse {
	return &GcodeUse{
		HasBlock:  true,
		PrintTime: "4h 24m 54s",
		TotalG:    45.93,
		Slots: []SlotUse{{
			Slot: 1, Preset: "DEEPLEE PLA PRO Rosa", Colour: "#F19CB4", Material: "PLA",
			Vendor: "DEEPLEE", Density: 1.24, Diameter: 1.75, CostPerKg: 13.59,
			GcodeG: 46, GcodeMM: 15398.04,
		}},
	}
}

// Plain upload (no start): the row is recorded and the spool is prepared, but
// nothing is consumed.
func TestRecordUploadWithoutStartBooksNothing(t *testing.T) {
	f := newFake(t)
	sm, _ := f.start()
	l := withLedgerAndSpoolman(t, sm)

	RecordUpload(sampleUse(), "Elefant.gcode", 3093450, false)

	rows := l.Recent(10)
	if len(rows) != 1 {
		t.Fatalf("%d rows, want 1", len(rows))
	}
	r := rows[0]
	if r.Status != StatusOpen || r.BookedG != 0 {
		t.Errorf("status/booked = %s/%d, want open/0", r.Status, r.BookedG)
	}
	if r.GcodeG != 46 || r.Preset != "DEEPLEE PLA PRO Rosa" || r.Slot != 1 {
		t.Errorf("row lost G-Code data: %+v", r)
	}
	if r.PrintTime != "4h 24m 54s" {
		t.Errorf("PrintTime = %q", r.PrintTime)
	}
	if len(f.uses) != 0 {
		t.Errorf("booked %v grams although no print was started", f.uses)
	}
	if r.BookedAt != nil {
		t.Error("BookedAt must stay empty without a booking")
	}
}

// The spool is created on a plain upload too, so the remaining column has
// something to show right away -- but the remaining value is NOT frozen on the
// row: it has to keep following the spool.
func TestRecordUploadWithoutStartCreatesSpool(t *testing.T) {
	f := newFake(t)
	sm, _ := f.start()
	l := withLedgerAndSpoolman(t, sm)

	RecordUpload(sampleUse(), "Elefant.gcode", 3093450, false)

	r := l.Recent(1)[0]
	if r.SpoolID != 22 {
		t.Errorf("SpoolID = %d, want the created spool 22", r.SpoolID)
	}
	if r.RemainingAfterG != nil {
		t.Errorf("RemainingAfterG = %v, must stay empty until booked", *r.RemainingAfterG)
	}
	if f.calls["POST /filament"] != 1 || f.calls["POST /spool"] != 1 {
		t.Errorf("filament/spool creations = %d/%d, want 1/1",
			f.calls["POST /filament"], f.calls["POST /spool"])
	}
	if len(f.uses) != 0 {
		t.Errorf("booked %v -- a plain upload must not consume anything", f.uses)
	}
	// The remaining column reads live from the spool.
	if rem, ok := sm.RemainingByPreset("DEEPLEE PLA PRO Rosa"); !ok || rem != 1000 {
		t.Errorf("RemainingByPreset = %v (%v), want 1000", rem, ok)
	}
}

// An unreachable Spoolman during a plain upload must not mark the row as failed
// -- nothing was supposed to be booked.
func TestRecordUploadWithoutStartSurvivesSpoolmanDown(t *testing.T) {
	l := withLedgerAndSpoolman(t, NewSpoolman("http://127.0.0.1:1"))

	RecordUpload(sampleUse(), "Elefant.gcode", 100, false)

	r := l.Recent(1)[0]
	if r.Status != StatusOpen {
		t.Errorf("status = %s, want open (a failed creation is not a booking failure)", r.Status)
	}
	if r.Error != "" {
		t.Errorf("Error = %q, want empty", r.Error)
	}
	if r.SpoolID != 0 {
		t.Errorf("SpoolID = %d, want 0", r.SpoolID)
	}
}

// Upload + start: booked immediately, remaining weight frozen on the row.
func TestRecordUploadWithStartBooksImmediately(t *testing.T) {
	f := newFake(t)
	sm, _ := f.start()
	l := withLedgerAndSpoolman(t, sm)

	RecordUpload(sampleUse(), "Elefant.gcode", 3093450, true)

	rows := l.Recent(10)
	if len(rows) != 1 {
		t.Fatalf("%d rows, want 1", len(rows))
	}
	r := rows[0]
	if r.Status != StatusBooked {
		t.Errorf("status = %s, want booked (error: %s)", r.Status, r.Error)
	}
	if r.BookedG != 46 {
		t.Errorf("BookedG = %d, want 46", r.BookedG)
	}
	if r.SpoolID != 22 {
		t.Errorf("SpoolID = %d, want the created spool 22", r.SpoolID)
	}
	if r.RemainingAfterG == nil || *r.RemainingAfterG != 954 {
		t.Errorf("RemainingAfterG = %v, want 954", r.RemainingAfterG)
	}
	if r.BookedAt == nil {
		t.Error("BookedAt not set")
	}
	if len(f.uses) != 1 || f.uses[0] != 46 {
		t.Errorf("use deltas = %v, want [46]", f.uses)
	}
}

// One row per (upload x slot), all sharing one upload ID.
func TestRecordUploadMultipleSlots(t *testing.T) {
	f := newFake(t)
	sm, _ := f.start()
	l := withLedgerAndSpoolman(t, sm)

	use := &GcodeUse{HasBlock: true, Slots: []SlotUse{
		{Slot: 0, Preset: "A", Density: 1.24, Diameter: 1.75, GcodeG: 10},
		{Slot: 2, Preset: "B", Density: 1.24, Diameter: 1.75, GcodeG: 4},
	}}
	RecordUpload(use, "Zwei.gcode", 100, false)

	rows := l.Recent(10)
	if len(rows) != 2 {
		t.Fatalf("%d rows, want 2", len(rows))
	}
	if rows[0].UploadID != rows[1].UploadID {
		t.Error("slots of one job should share the upload ID")
	}
	if rows[0].ID == rows[1].ID {
		t.Error("row IDs must differ")
	}
}

// Luban / .nc / laser: one row without filament data, no booking even with start.
func TestRecordUploadWithoutConsumptionBlock(t *testing.T) {
	f := newFake(t)
	sm, _ := f.start()
	l := withLedgerAndSpoolman(t, sm)

	RecordUpload(&GcodeUse{HasBlock: false}, "laser.nc", 4096, true)

	rows := l.Recent(10)
	if len(rows) != 1 {
		t.Fatalf("%d rows, want 1", len(rows))
	}
	if !rows[0].NoFilament || rows[0].Slot != -1 {
		t.Errorf("row not marked as filament-free: %+v", rows[0])
	}
	if len(f.uses) != 0 {
		t.Errorf("booked %v although there is no consumption block", f.uses)
	}
	if err := BookRow(rows[0].ID, 10); err == nil {
		t.Error("a filament-free row must not be bookable")
	}
}

// The correction model: every change sends the difference to what is already
// booked -- 46 booked, corrected to 20, means -26.
func TestBookRowSendsDeltaOnCorrection(t *testing.T) {
	f := newFake(t)
	sm, _ := f.start()
	l := withLedgerAndSpoolman(t, sm)

	RecordUpload(sampleUse(), "Elefant.gcode", 100, true)
	id := l.Recent(1)[0].ID

	if err := BookRow(id, 20); err != nil {
		t.Fatal(err)
	}
	if len(f.uses) != 2 || f.uses[1] != -26 {
		t.Fatalf("use deltas = %v, want [46 -26]", f.uses)
	}
	r, _ := l.Get(id)
	if r.BookedG != 20 || r.Status != StatusBooked {
		t.Errorf("row = %d g / %s, want 20/booked", r.BookedG, r.Status)
	}
	if r.RemainingAfterG == nil || *r.RemainingAfterG != 980 {
		t.Errorf("RemainingAfterG = %v, want 980", r.RemainingAfterG)
	}
	// The G-Code value is immutable.
	if r.GcodeG != 46 {
		t.Errorf("GcodeG = %d, want an unchanged 46", r.GcodeG)
	}
}

// Cancelling means target 0 -- and looks exactly like a row that was never
// booked (no separate cancelled state, decision 9).
func TestBookRowCancelReturnsToOpen(t *testing.T) {
	f := newFake(t)
	sm, _ := f.start()
	l := withLedgerAndSpoolman(t, sm)

	RecordUpload(sampleUse(), "Elefant.gcode", 100, true)
	id := l.Recent(1)[0].ID

	if err := BookRow(id, 0); err != nil {
		t.Fatal(err)
	}
	if len(f.uses) != 2 || f.uses[1] != -46 {
		t.Fatalf("use deltas = %v, want [46 -46]", f.uses)
	}
	r, _ := l.Get(id)
	if r.BookedG != 0 || r.Status != StatusOpen {
		t.Errorf("row = %d g / %s, want 0/open", r.BookedG, r.Status)
	}
	if r.RemainingAfterG == nil || *r.RemainingAfterG != 1000 {
		t.Errorf("RemainingAfterG = %v, want 1000", r.RemainingAfterG)
	}
}

// Booking the same value twice must not send a second delta.
func TestBookRowIdempotent(t *testing.T) {
	f := newFake(t)
	sm, _ := f.start()
	l := withLedgerAndSpoolman(t, sm)

	RecordUpload(sampleUse(), "Elefant.gcode", 100, true)
	id := l.Recent(1)[0].ID

	if err := BookRow(id, 46); err != nil {
		t.Fatal(err)
	}
	if len(f.uses) != 1 {
		t.Errorf("use deltas = %v, want just the first booking", f.uses)
	}
}

// Spoolman failure: the row is marked, booked_g stays put so a retry sends the
// same delta -- and the upload itself is unaffected.
func TestBookingFailureKeepsBookedG(t *testing.T) {
	sm := NewSpoolman("http://127.0.0.1:1") // nothing listens here
	l := withLedgerAndSpoolman(t, sm)

	RecordUpload(sampleUse(), "Elefant.gcode", 100, true)

	rows := l.Recent(10)
	if len(rows) != 1 {
		t.Fatalf("%d rows, want 1 -- a failed booking must not lose the row", len(rows))
	}
	r := rows[0]
	if r.Status != StatusFailure {
		t.Errorf("status = %s, want failed", r.Status)
	}
	if r.BookedG != 0 {
		t.Errorf("BookedG = %d, want an unchanged 0", r.BookedG)
	}
	if r.Error == "" {
		t.Error("no error text stored")
	}

	// Retry once Spoolman is back: the full amount is still owed.
	f := newFake(t)
	back, _ := f.start()
	TheSpoolman = back
	if err := BookRow(r.ID, r.GcodeG); err != nil {
		t.Fatal(err)
	}
	if len(f.uses) != 1 || f.uses[0] != 46 {
		t.Errorf("use deltas after the retry = %v, want [46]", f.uses)
	}
	again, _ := l.Get(r.ID)
	if again.Status != StatusBooked || again.Error != "" {
		t.Errorf("row after the retry = %s / %q", again.Status, again.Error)
	}
}

// Without SPOOLMAN_URL the table still records, it just never books.
func TestRecordUploadWithoutSpoolman(t *testing.T) {
	l := withLedgerAndSpoolman(t, nil)

	RecordUpload(sampleUse(), "Elefant.gcode", 100, true)

	rows := l.Recent(10)
	if len(rows) != 1 || rows[0].Status != StatusOpen {
		t.Fatalf("rows = %+v", rows)
	}
	if err := BookRow(rows[0].ID, 46); err == nil {
		t.Error("BookRow without a Spoolman client should fail")
	}
}

func TestBookRowRejectsUnknownRowAndNegative(t *testing.T) {
	f := newFake(t)
	sm, _ := f.start()
	l := withLedgerAndSpoolman(t, sm)

	if err := BookRow("does-not-exist", 10); err == nil {
		t.Error("unknown row should fail")
	}
	RecordUpload(sampleUse(), "Elefant.gcode", 100, false)
	id := l.Recent(1)[0].ID
	if err := BookRow(id, -5); err == nil {
		t.Error("a negative amount should fail")
	}
}

// A slot that rounds to 0 g still gets a row; booking it touches nothing.
func TestBookRowZeroGrams(t *testing.T) {
	f := newFake(t)
	sm, _ := f.start()
	l := withLedgerAndSpoolman(t, sm)

	use := &GcodeUse{HasBlock: true, Slots: []SlotUse{
		{Slot: 0, Preset: "Tiny", Density: 1.24, Diameter: 1.75, GcodeG: 0},
	}}
	RecordUpload(use, "tiny.gcode", 100, true)

	r := l.Recent(1)[0]
	if len(f.uses) != 0 {
		t.Errorf("use deltas = %v, want none for 0 g", f.uses)
	}
	if r.Status != StatusOpen {
		t.Errorf("status = %s, want open for a 0 g booking", r.Status)
	}
	if r.SpoolID == 0 {
		t.Error("the spool should still be resolved so the remaining column works")
	}
}

// The low-stock warning must never block -- it only logs.
func TestLowStockWarningDoesNotBlock(t *testing.T) {
	f := newFake(t)
	f.spools = []SmSpool{{ID: 9, InitialWeight: f64(1000), UsedWeight: 950,
		Filament: SmFilament{Name: "DEEPLEE PLA PRO Rosa"}}}
	sm, _ := f.start()
	l := withLedgerAndSpoolman(t, sm)

	RecordUpload(sampleUse(), "Elefant.gcode", 100, true)

	r := l.Recent(1)[0]
	if r.Status != StatusBooked {
		t.Errorf("status = %s, want booked -- a low spool must not block", r.Status)
	}
	// 1000 - 950 - 46 = 4 g left: booked past the threshold, still booked.
	if r.RemainingAfterG == nil || *r.RemainingAfterG != 4 {
		t.Errorf("RemainingAfterG = %v, want 4", r.RemainingAfterG)
	}
}

// Rendering must survive every row shape (and escape the file name).
func TestUploadsHTMLRendersRows(t *testing.T) {
	f := newFake(t)
	sm, _ := f.start()
	l := withLedgerAndSpoolman(t, sm)

	RecordUpload(sampleUse(), "Ele<fant>.gcode", 100, true)
	RecordUpload(&GcodeUse{HasBlock: false}, "laser.nc", 100, false)

	html := uploadsHTML()
	for _, want := range []string{
		`class="tbl"`,
		`background:#F19CB4`,             // colour swatch
		`DEEPLEE PLA PRO Rosa`,           // filament name
		`name="g"`,                       // editable booked amount
		`value="book"`, `value="cancel"`, // actions
		`Ele&lt;fant&gt;.gcode`, // escaped
		`no consumption block`,  // the .nc row
	} {
		if !strings.Contains(html, want) {
			t.Errorf("table is missing %q", want)
		}
	}
	// The input reaches its buttons through the HTML form owner attribute.
	// Recent() is newest first, so the .gcode row is the second one.
	id := l.Recent(2)[1].ID
	if !strings.Contains(html, `form="f-`+id+`"`) {
		t.Error("input is not wired to its row form")
	}
	if strings.Contains(html, "<form") && !strings.Contains(html, `method="POST"`) {
		t.Error("the row form must be POST")
	}
	// Tool and status columns were dropped on purpose.
	for _, gone := range []string{`<th>Tool</th>`, `<th>Status</th>`, `>booked<`, `>open<`} {
		if strings.Contains(html, gone) {
			t.Errorf("table still carries %q", gone)
		}
	}
	// Nine columns -- and every row has to match the header.
	head := strings.Count(html[:strings.Index(html, "</thead>")], "</th>")
	if head != 9 {
		t.Errorf("%d header cells, want 9", head)
	}
	for i, row := range strings.Split(html[strings.Index(html, "<tbody>"):], "<tr>")[1:] {
		if n := strings.Count(row, "</td>"); n != head {
			t.Errorf("row %d has %d cells, header has %d", i, n, head)
		}
	}
}

// Without a status column a failed booking still has to be visible.
func TestUploadsHTMLShowsBookingError(t *testing.T) {
	l := withLedgerAndSpoolman(t, NewSpoolman("http://127.0.0.1:1"))
	RecordUpload(sampleUse(), "Elefant.gcode", 100, true)

	r := l.Recent(1)[0]
	if r.Status != StatusFailure {
		t.Fatalf("status = %s, want failed", r.Status)
	}
	html := uploadsHTML()
	if !strings.Contains(html, `class="st-err"`) {
		t.Error("no error marker in the table")
	}
	if !strings.Contains(html, `title="`) {
		t.Error("the error message should be the marker's tooltip")
	}
}

func TestUploadsHTMLEmptyLedger(t *testing.T) {
	withLedgerAndSpoolman(t, nil)
	if got := uploadsHTML(); !strings.Contains(got, "No uploads recorded yet") {
		t.Errorf("empty ledger renders %q", got)
	}
	if got := uploadsHint(); got != "(newest first)" {
		t.Errorf("hint = %q", got)
	}
}
