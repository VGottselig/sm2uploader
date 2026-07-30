package main

import (
	"fmt"
	"html"
	"math"
	"strconv"
	"strings"
)

// uploadRowsRendered is how many rows the GUI shows. Everything is kept in
// uploads.yaml, only the rendering is capped.
const uploadRowsRendered = 10

// currencySymbols keeps the common codes short in the table.
var currencySymbols = map[string]string{"EUR": "€", "USD": "$", "GBP": "£", "CHF": "CHF"}

// uploadsHint tells how much of the ledger is on screen.
func uploadsHint() string {
	if TheLedger == nil {
		return ""
	}
	total := TheLedger.Len()
	if total <= uploadRowsRendered {
		return "(newest first)"
	}
	return fmt.Sprintf("(%d newest of %d)", uploadRowsRendered, total)
}

// uploadsHTML renders the upload table: one row per (upload x slot), newest
// first. The amount appears twice on purpose -- on the left the immutable G-Code
// value, on the right what Spoolman actually holds.
func uploadsHTML() string {
	if TheLedger == nil {
		return `<p class="empty">Ledger not initialised.</p>`
	}
	rows := TheLedger.Recent(uploadRowsRendered)
	if len(rows) == 0 {
		return `<p class="empty">No uploads recorded yet.</p>`
	}

	var b strings.Builder
	// No tool and no status column: the tool is implied by the filament name, and
	// booked/open is read off the "gebucht g" column. Only a failed booking needs
	// its own marker, which sits in that same column.
	b.WriteString(`<table class="tbl"><thead><tr>` +
		`<th>Time</th><th>File</th><th>Filament</th>` +
		`<th class="num">G-code g</th><th class="num">booked g</th><th class="num">remaining g</th>` +
		`<th>Print time</th><th class="num">Cost</th><th>Action</th>` +
		`</tr></thead><tbody>`)
	for _, r := range rows {
		writeUploadRow(&b, r)
	}
	b.WriteString(`</tbody></table>`)
	return b.String()
}

func writeUploadRow(b *strings.Builder, r UploadRow) {
	formID := "f-" + r.ID
	b.WriteString(`<tr>`)
	b.WriteString(`<td class="nw">` + html.EscapeString(r.Time.In(berlinLoc()).Format("02 Jan 15:04")) + `</td>`)
	b.WriteString(`<td class="file" title="` + html.EscapeString(r.File) + `">` + html.EscapeString(r.File) + `</td>`)

	if r.NoFilament {
		b.WriteString(`<td class="dim">no consumption block</td>`)
		b.WriteString(`<td class="num dim">—</td><td class="num dim">—</td><td class="num dim">—</td>`)
		b.WriteString(`<td class="dim">—</td><td class="num dim">—</td>`)
		b.WriteString(`<td class="dim">—</td>`)
		b.WriteString(`</tr>`)
		return
	}

	b.WriteString(`<td>` + swatchHTML(r.Colour) + html.EscapeString(r.Preset) + `</td>`)
	b.WriteString(`<td class="num">` + strconv.Itoa(r.GcodeG) + `</td>`)

	// Editable booked amount; the buttons live in the last cell and reach this
	// input through the HTML form owner attribute. data-g carries the G-code
	// amount so the JS can pick the one relevant action while typing. A failed
	// booking is flagged right here -- there is no status column to carry it.
	b.WriteString(`<td class="num"><input class="g" form="` + formID + `" data-g="` + strconv.Itoa(r.GcodeG) +
		`" type="number" name="g" min="0" step="1" value="` +
		strconv.Itoa(r.BookedG) + `">` + errorMarkHTML(r) + `</td>`)

	b.WriteString(`<td class="num">` + remainingHTML(r) + `</td>`)
	b.WriteString(`<td class="nw">` + html.EscapeString(r.PrintTime) + `</td>`)
	b.WriteString(`<td class="num">` + costHTML(r) + `</td>`)

	// Exactly one action shows at a time, driven by the amount in the field:
	// 0 -> Book, == G-code -> Cancel, else -> Set. Rendered correct up front
	// (no flash, works without JS); sm2act() in the page keeps it in sync while
	// the user types.
	active := activeAct(r.BookedG, r.GcodeG)
	b.WriteString(`<td class="act"><form id="` + formID + `" method="POST" action="/book">` +
		`<input type="hidden" name="id" value="` + html.EscapeString(r.ID) + `">` +
		`<button data-act="book" name="action" value="book" title="set to the G-code amount"` + actHidden("book", active) + `>Book</button>` +
		`<button data-act="set" name="action" value="set" title="book the amount entered"` + actHidden("set", active) + `>Set</button>` +
		`<button data-act="cancel" name="action" value="cancel" class="danger" title="refund back to 0 g"` + actHidden("cancel", active) + `>Cancel</button>` +
		`</form></td>`)
	b.WriteString(`</tr>`)
}

// activeAct picks the single action to show for a booked amount, mirroring the
// JS in the page: 0 -> book, == G-code -> cancel, else -> set. The order matters
// so a G-code amount of 0 (sub-gram print rounded down) resolves to book.
func activeAct(booked, gcode int) string {
	switch {
	case booked == 0:
		return "book"
	case booked == gcode:
		return "cancel"
	default:
		return "set"
	}
}

// actHidden hides every action button except the active one.
func actHidden(act, active string) string {
	if act == active {
		return ""
	}
	return ` style="display:none"`
}

// swatchHTML is the colour box in front of the filament name (from
// filament_colour). It always gets a thin border so white does not vanish.
func swatchHTML(colour string) string {
	if colour == "" {
		return ""
	}
	return `<span class="sw" style="background:` + html.EscapeString(colour) + `"></span>`
}

// remainingHTML shows the remaining weight of the spool after this booking. A
// booked row keeps the frozen value; a row that was not booked yet shows the
// spool's current level (its booking is 0, so it is the same number).
func remainingHTML(r UploadRow) string {
	var (
		value float64
		known bool
	)
	if r.RemainingAfterG != nil {
		value, known = float64(*r.RemainingAfterG), true
	} else if TheSpoolman != nil && r.Preset != "" {
		value, known = TheSpoolman.RemainingByPreset(r.Preset)
	}
	if !known {
		return `<span class="dim">—</span>`
	}
	// Spoolman may hold fractions (bookings made through its own UI) -- round for
	// display only, never write it back.
	text := strconv.Itoa(int(math.Round(value)))
	switch {
	case value < 0:
		return `<span class="neg">` + text + `</span>`
	case value < float64(SpoolmanLowG):
		return `<span class="warn">` + text + `</span>`
	}
	return text
}

// costHTML computes from Spoolman data, so the cost follows the *booked* amount:
// correct 120 down to 50 and it drops with it. Anything unknown shows 0.
func costHTML(r UploadRow) string {
	var perG float64
	if TheSpoolman != nil {
		perG = TheSpoolman.PricePerG(r.SpoolID)
	}
	cost := perG * float64(r.BookedG)

	symbol := "€"
	if TheSpoolman != nil {
		cur := TheSpoolman.Currency()
		if s, ok := currencySymbols[cur]; ok {
			symbol = s
		} else if cur != "" {
			symbol = cur
		}
	}
	return html.EscapeString(fmt.Sprintf("%.2f %s", cost, symbol))
}

// errorMarkHTML flags a failed booking. Booked and open are obvious from the
// gram value itself, an error is not -- so it gets a marker with the message as
// its tooltip.
func errorMarkHTML(r UploadRow) string {
	if r.Status != StatusFailure {
		return ""
	}
	msg := r.Error
	if msg == "" {
		msg = "booking failed"
	}
	return `<span class="st-err" title="` + html.EscapeString(msg) + `">&#9888;</span>`
}
