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

// RecordUpload writes the ledger rows for a finished upload and, when the print
// was actually started, books the consumption right away.
//
// Only successful uploads get a row. If the upload landed but the start failed,
// the row stays open (status "offen") and the user books it from the table.
func RecordUpload(use *GcodeUse, name string, size int64, started bool) {
	now := time.Now().In(berlinLoc())
	uploadID := newUploadID(now)

	var rows []*UploadRow
	if use == nil || !use.HasBlock || len(use.Slots) == 0 {
		// Luban, .nc, laser/CNC: timestamp and file name only, no booking.
		// Deliberately no computing from the E moves.
		rows = append(rows, &UploadRow{
			ID: uploadID + "-T-", UploadID: uploadID, Time: now, File: name, Size: size,
			Slot: -1, Status: StatusOpen, NoFilament: true,
		})
	} else {
		for _, slot := range use.Slots {
			rows = append(rows, &UploadRow{
				ID:        fmt.Sprintf("%s-T%d", uploadID, slot.Slot),
				UploadID:  uploadID,
				Time:      now,
				File:      name,
				Size:      size,
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

	spool, err := TheSpoolman.ResolveSpool(SlotUse{
		Slot: row.Slot, Preset: row.Preset, Colour: row.Colour, Material: row.Material,
		Vendor: row.Vendor, Density: row.Density, Diameter: row.Diameter, CostPerKg: row.CostPerKg,
	})
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
