package main

import (
	"fmt"
	"log"
	"math"
	"sync"
	"time"
)

// SpoolmanLowG is the threshold below which the remaining weight is highlighted
// and a warning is logged before a booking (env SPOOLMAN_LOW_G).
var SpoolmanLowG = 100

// bookMu serialises bookings. It is deliberately separate from the ledger mutex:
// a booking makes HTTP calls that may take seconds, and the ledger lock must not
// be held across them or a slow Spoolman would stall the next Orca upload. One
// printer, one user -- serialising bookings costs nothing and makes the
// read-modify-write of booked_g safe.
var bookMu sync.Mutex

// RecordStored writes the rows for an Orca "Upload" without start: the file was
// kept on sm2uploader only and never sent to the printer. The rows are open at
// 0 g -- the same state as after a cancel -- with the spool already resolved.
func RecordStored(use *GcodeUse, name string, size int64, fileID string) {
	recordRows(use, name, size, false, fileID, true)
}

// RecordUpload writes the ledger rows for a finished upload and, when the print
// was actually started, books the consumption right away.
func RecordUpload(use *GcodeUse, name string, size int64, started bool, fileID string) {
	recordRows(use, name, size, started, fileID, false)
}

// recordRows is the body of RecordUpload and RecordStored.
//
// Only successful uploads get a row. If the upload landed but the start failed,
// the row stays open (status "offen") and the user books it from the table.
// fileID is the cached copy of the very file that was sent ("" when no copy could
// be kept); all rows of the upload share it, so any of them can print again.
func recordRows(use *GcodeUse, name string, size int64, started bool, fileID string, stored bool) {
	now := time.Now().In(berlinLoc())
	uploadID := newUploadID(now)

	var rows []*UploadRow
	if use == nil || !use.HasBlock || len(use.Slots) == 0 {
		// Luban, .nc, laser/CNC: timestamp and file name only, no booking.
		// Deliberately no computing from the E moves.
		rows = append(rows, &UploadRow{
			ID: uploadID + "-T-", UploadID: uploadID, Time: now, File: name, Size: size,
			FileID: fileID,
			Slot:   -1, Status: StatusOpen, NoFilament: true,
			Stored: stored,
		})
	} else {
		for _, slot := range use.Slots {
			rows = append(rows, &UploadRow{
				ID:        fmt.Sprintf("%s-T%d", uploadID, slot.Slot),
				UploadID:  uploadID,
				Time:      now,
				File:      name,
				Size:      size,
				FileID:    fileID,
				Stored:    stored,
				Slot:      slot.Slot,
				Preset:    slot.Preset,
				Colour:    slot.Colour,
				Material:  slot.Material,
				Vendor:    slot.Vendor,
				Density:   slot.Density,
				Diameter:  slot.Diameter,
				CostPerKg: slot.CostPerKg,
				GcodeG:    slot.GcodeG,
				GcodeMM:   slot.GcodeMM,
				PrintTime: use.PrintTime,
				Status:    StatusOpen,
			})
		}
	}

	if TheLedger == nil {
		return
	}
	// The table is the retention rule: once the new rows are in, whatever no longer
	// fits gets deleted -- also on a booking failure, which is why this is deferred
	// rather than called on the happy path only.
	defer ApplyRetention()
	if err := TheLedger.Append(rows...); err != nil {
		log.Printf("Ledger: could not write the rows for '%s': %s", name, err)
	}
	for _, r := range rows {
		if r.NoFilament {
			log.Printf("Upload recorded: %s (no consumption block, no booking)", r.File)
			continue
		}
		log.Printf("Upload recorded: %s T%d %s -- %d g from the G-Code", r.File, r.Slot, r.Preset, r.GcodeG)
	}
	if stored && fileID == "" {
		log.Printf("Ledger: stored upload '%s' has no file copy -- it cannot be printed from the table", name)
	}

	if TheSpoolman == nil {
		log.Printf("Spoolman not configured (SPOOLMAN_URL) -- rows stay open")
		return
	}

	if !started {
		// A plain upload books nothing -- but the row should still show the
		// spool's current level, and that needs the spool to exist. So the
		// filament/spool is created here already; consumption is untouched.
		ensureSpools(rows)
		return
	}

	// A failed booking must never affect the print or the answer to Orca.
	for _, r := range rows {
		if r.NoFilament {
			continue
		}
		if err := BookRow(r.ID, r.GcodeG); err != nil {
			log.Printf("Spoolman: booking %d g for '%s' T%d failed: %s", r.GcodeG, r.File, r.Slot, err)
		}
	}
}

// PrintedStored finishes a stored upload once "Print" got it onto the printer:
// every row of the upload stops being "stored", and when the print really
// started each row that still holds 0 g is booked with its G-code amount -- the
// same booking Upload+Start would have made. A row the user already booked by
// hand is left alone. A failed booking only marks its row, like RecordUpload.
func PrintedStored(uploadID string, started bool) {
	if TheLedger == nil {
		return
	}
	rows := TheLedger.UploadRows(uploadID)
	for _, r := range rows {
		if err := TheLedger.Update(r.ID, func(row *UploadRow) { row.Stored = false }); err != nil {
			log.Printf("Ledger: %s", err)
		}
	}
	if !started || TheSpoolman == nil {
		return
	}
	for _, r := range rows {
		if r.NoFilament || r.BookedG != 0 {
			continue
		}
		if err := BookRow(r.ID, r.GcodeG); err != nil {
			log.Printf("Spoolman: booking %d g for '%s' T%d failed: %s", r.GcodeG, r.File, r.Slot, err)
		}
	}
}

// DeleteUpload removes an upload from the table: all its rows (one per slot) and,
// through the retention rule, the file copy nothing points at any more.
// Bookings in Spoolman are deliberately left alone -- a printed job did use the
// filament; whoever wants it back cancels the row first.
func DeleteUpload(rowID string) (UploadRow, int, error) {
	bookMu.Lock() // no booking may write a row while it disappears
	defer bookMu.Unlock()
	if TheLedger == nil {
		return UploadRow{}, 0, fmt.Errorf("no ledger")
	}
	row, ok := TheLedger.Get(rowID)
	if !ok {
		return UploadRow{}, 0, fmt.Errorf("row %q unknown", rowID)
	}
	n, err := TheLedger.RemoveUpload(row.UploadID)
	if err != nil {
		return row, 0, err
	}
	log.Printf("Deleted from the table: %s (%d row(s); Spoolman bookings unchanged)", row.File, n)
	ApplyRetention()
	return row, n, nil
}

// ensureSpools resolves the spool for every row -- creating filament and spool
// when the preset is new -- without booking anything. Only spool_id is recorded:
// remaining_after_g stays empty so an unbooked row keeps showing the spool's live
// level instead of freezing a number at upload time (decision 18).
//
// A failure here is not a booking failure: the row stays open and only the
// remaining column has nothing to show.
func ensureSpools(rows []*UploadRow) {
	bookMu.Lock()
	defer bookMu.Unlock()

	for _, r := range rows {
		if r.NoFilament || r.SpoolID != 0 {
			continue
		}
		spool, err := TheSpoolman.ResolveSpool(SlotUse{
			Slot: r.Slot, Preset: r.Preset, Colour: r.Colour, Material: r.Material,
			Vendor: r.Vendor, Density: r.Density, Diameter: r.Diameter, CostPerKg: r.CostPerKg,
		})
		if err != nil {
			log.Printf("Spoolman: no spool for '%s' T%d (%s): %s", r.File, r.Slot, r.Preset, err)
			continue
		}
		spoolID := spool.ID
		if err := TheLedger.Update(r.ID, func(row *UploadRow) { row.SpoolID = spoolID }); err != nil {
			log.Printf("Ledger: could not store the spool ID on %s: %s", r.ID, err)
		}
	}
}

// BookRow sets the booked amount of a row to targetG and sends the difference to
// the already booked amount to Spoolman. That is the whole correction model:
// "book" = target is the G-Code value, "cancel" = target 0 (booked 120,
// corrected to 50 -> use_weight: -70).
func BookRow(id string, targetG int) error {
	bookMu.Lock()
	defer bookMu.Unlock()

	if TheLedger == nil {
		return fmt.Errorf("no ledger")
	}
	row, ok := TheLedger.Get(id)
	if !ok {
		return fmt.Errorf("row %q unknown", id)
	}
	if row.NoFilament {
		return fmt.Errorf("row without filament data cannot be booked")
	}
	if targetG < 0 {
		return fmt.Errorf("negative amounts are not bookable")
	}
	if TheSpoolman == nil {
		return fmt.Errorf("SPOOLMAN_URL is not set")
	}

	spool, err := rowSpool(row)
	if err != nil {
		markFailure(id, err)
		return err
	}

	delta := targetG - row.BookedG
	warnLowStock(spool, row.Preset, delta)

	if delta != 0 {
		// The response carries the updated spool, so the new remaining weight
		// needs no extra GET.
		if spool, err = TheSpoolman.Use(spool.ID, delta); err != nil {
			markFailure(id, err)
			return err
		}
		log.Printf("Spoolman: %s T%d %s -- %+d g booked (total %d g)", row.File, row.Slot, row.Preset, delta, targetG)
	}

	var remaining *int
	if rem, ok := RemainingG(spool); ok {
		// The value from Spoolman may be fractional (earlier bookings through the
		// Spoolman UI) -- round for display only, never write it back.
		v := int(math.Round(rem))
		remaining = &v
	}

	now := time.Now().In(berlinLoc())
	return TheLedger.Update(id, func(r *UploadRow) {
		r.BookedG = targetG
		r.SpoolID = spool.ID
		r.RemainingAfterG = remaining
		r.Error = ""
		r.BookedAt = &now
		// No separate cancelled state: a row corrected back to 0 looks exactly
		// like one that was never booked.
		if targetG > 0 {
			r.Status = StatusBooked
		} else {
			r.Status = StatusOpen
		}
	})
}

// rowSpool picks the spool a booking of this row goes to. A spool chosen by hand
// (ChangeSpool) always wins. A row that already holds grams stays on the spool
// they were booked on -- otherwise a refund after a spool change in Spoolman
// (new spool of the same filament, newer last_used) would credit the wrong spool.
// Everything else resolves through the preset as before. Caller holds bookMu.
func rowSpool(row UploadRow) (*SmSpool, error) {
	if row.SpoolID != 0 && (row.SpoolManual || row.BookedG != 0) {
		sp, err := TheSpoolman.SpoolByID(row.SpoolID)
		if err != nil {
			return nil, err
		}
		if sp != nil {
			return sp, nil
		}
		if row.SpoolManual {
			return nil, fmt.Errorf("spool %d is no longer in Spoolman (archived?) -- choose another filament", row.SpoolID)
		}
		log.Printf("Spoolman: spool %d of %s T%d is gone, resolving through the preset", row.SpoolID, row.File, row.Slot)
	}
	return TheSpoolman.ResolveSpool(SlotUse{
		Slot: row.Slot, Preset: row.Preset, Colour: row.Colour, Material: row.Material,
		Vendor: row.Vendor, Density: row.Density, Diameter: row.Diameter, CostPerKg: row.CostPerKg,
	})
}

// ChangeSpool moves a row to another spool. A booked row is refunded on its old
// spool first and the same grams are then booked on the new one; an open row only
// switches the spool. The preset mapping in Spoolman stays untouched -- this
// corrects one row, not future uploads.
//
// Refund first: if it fails nothing has changed. If the second booking fails the
// row is left open on the new spool and flagged, so "Book" finishes the job.
func ChangeSpool(id string, spoolID int) error {
	bookMu.Lock()
	defer bookMu.Unlock()

	if TheLedger == nil {
		return fmt.Errorf("no ledger")
	}
	row, ok := TheLedger.Get(id)
	if !ok {
		return fmt.Errorf("row %q unknown", id)
	}
	if row.NoFilament {
		return fmt.Errorf("row without filament data has no spool")
	}
	if TheSpoolman == nil {
		return fmt.Errorf("SPOOLMAN_URL is not set")
	}
	target, err := TheSpoolman.SpoolByID(spoolID)
	if err != nil {
		return err
	}
	if target == nil {
		return fmt.Errorf("spool %d not found in Spoolman", spoolID)
	}
	if row.SpoolID == spoolID && row.SpoolManual {
		return nil // already there
	}

	amount := row.BookedG
	if amount == 0 {
		return TheLedger.Update(id, func(r *UploadRow) {
			r.SpoolID, r.SpoolManual = spoolID, true
			r.RemainingAfterG, r.Error, r.Status = nil, "", StatusOpen
		})
	}

	old, err := rowSpool(row)
	if err != nil {
		return err
	}
	if old.ID == spoolID {
		return TheLedger.Update(id, func(r *UploadRow) { r.SpoolID, r.SpoolManual = spoolID, true })
	}
	if _, err := TheSpoolman.Use(old.ID, -amount); err != nil {
		return err
	}
	log.Printf("Spoolman: %s T%d -- %d g refunded on spool %d (filament change)", row.File, row.Slot, amount, old.ID)
	if err := TheLedger.Update(id, func(r *UploadRow) {
		r.BookedG, r.SpoolID, r.SpoolManual = 0, spoolID, true
		r.RemainingAfterG, r.Status, r.Error = nil, StatusOpen, ""
	}); err != nil {
		return err
	}

	warnLowStock(target, spoolName(*target), amount)
	booked, err := TheSpoolman.Use(spoolID, amount)
	if err != nil {
		markFailure(id, fmt.Errorf("refunded on the old spool, booking on the new one failed: %w", err))
		return err
	}
	log.Printf("Spoolman: %s T%d -- %d g booked on spool %d (%s)", row.File, row.Slot, amount, spoolID, spoolName(*booked))

	var remaining *int
	if rem, ok := RemainingG(booked); ok {
		v := int(math.Round(rem))
		remaining = &v
	}
	now := time.Now().In(berlinLoc())
	return TheLedger.Update(id, func(r *UploadRow) {
		r.BookedG, r.RemainingAfterG, r.BookedAt, r.Status = amount, remaining, &now, StatusBooked
	})
}

// markFailure records the error on the row. booked_g stays untouched so a retry
// sends the same delta again.
func markFailure(id string, cause error) {
	if TheLedger == nil {
		return
	}
	_ = TheLedger.Update(id, func(r *UploadRow) {
		r.Status = StatusFailure
		r.Error = cause.Error()
	})
}

// warnLowStock warns when a booking pushes the spool below the threshold. It
// never blocks -- a print must not fail because of stock bookkeeping.
func warnLowStock(spool *SmSpool, preset string, delta int) {
	if delta <= 0 {
		return
	}
	rem, ok := RemainingG(spool)
	if !ok {
		return
	}
	if after := rem - float64(delta); after < float64(SpoolmanLowG) {
		log.Printf("Warning: spool '%s' only has %.0f g left, %d g needed -- %.0f g remaining after this job",
			preset, rem, delta, after)
	}
}
