package main

import (
	"html"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// swatchFallback keeps a colourless filament visible: the chip still lines up
// with the others instead of losing its box.
const swatchFallback = "#4b5563"

// reHex accepts what may be inlined into a style attribute -- a hex triplet with
// or without the leading '#'. Anything else is dropped instead of escaped, so no
// Spoolman field can end up inside the CSS.
var reHex = regexp.MustCompile(`^#?([0-9a-fA-F]{3}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$`)

// stockHTML renders the filament stock underneath the upload table: one chip per
// spool, colour and remaining weight and nothing else, emptiest first -- the
// number that decides whether the next print fits comes first.
//
// Read-only, and it shares the table's 30 s spool cache, so the section costs no
// extra Spoolman call. The filament name is only a tooltip.
func stockHTML() string {
	if TheSpoolman == nil {
		return `<p class="empty">SPOOLMAN_URL is not set.</p>`
	}

	type chip struct {
		style string
		name  string
		rem   float64
	}
	var chips []chip
	for _, sp := range TheSpoolman.cachedSpools() {
		if sp.Archived {
			continue
		}
		// Without a net weight there is no remaining amount, and a chip without a
		// number would say nothing -- so the spool is left out.
		rem, ok := RemainingG(&sp)
		if !ok {
			continue
		}
		chips = append(chips, chip{style: swatchStyle(sp.Filament), name: spoolName(sp), rem: rem})
	}
	if len(chips) == 0 {
		return `<p class="empty">No spool data from Spoolman.</p>`
	}
	sort.SliceStable(chips, func(i, j int) bool { return chips[i].rem < chips[j].rem })

	var b strings.Builder
	for _, c := range chips {
		// Spoolman may hold fractions (bookings made in its own UI) -- rounded for
		// display only, exactly like the table's remaining column.
		b.WriteString(`<span class="chip` + chipClass(c.rem) + `" title="` + html.EscapeString(c.name) + `">` +
			`<span class="sw" style="` + c.style + `"></span>` +
			strconv.Itoa(int(math.Round(c.rem))) + ` g</span>`)
	}
	return b.String()
}

// chipClass colours the number like the table does: below SpoolmanLowG amber,
// over-booked (negative) red.
func chipClass(rem float64) string {
	switch {
	case rem < 0:
		return " neg"
	case rem < float64(SpoolmanLowG):
		return " warn"
	}
	return ""
}

// spoolName is the tooltip text. Vendor is skipped when the filament name
// already starts with it, which is how Spoolman names them here.
func spoolName(sp SmSpool) string {
	name := strings.TrimSpace(sp.Filament.Name)
	vendor := ""
	if sp.Filament.Vendor != nil {
		vendor = strings.TrimSpace(sp.Filament.Vendor.Name)
	}
	if vendor != "" && !strings.HasPrefix(strings.ToLower(name), strings.ToLower(vendor)) {
		name = strings.TrimSpace(vendor + " " + name)
	}
	if name == "" {
		return "Spool #" + strconv.Itoa(sp.ID)
	}
	return name
}

// swatchStyle paints the colour box of a spool. Multi-colour filaments become a
// gradient oriented the way Spoolman records it: coaxial (layered strand) top to
// bottom, longitudinal (along the strand) left to right -- same orientation as
// the Home-Assistant card, so both views look alike.
func swatchStyle(f SmFilament) string {
	if hexes := hexList(f.MultiColorHexes); len(hexes) >= 2 {
		deg := "180deg"
		if strings.EqualFold(strings.TrimSpace(f.MultiColorDirection), "longitudinal") {
			deg = "90deg"
		}
		return "background:linear-gradient(" + deg + "," + strings.Join(hexes, ",") + ")"
	}
	if c := cssHex(f.ColorHex); c != "" {
		return "background:" + c
	}
	return "background:" + swatchFallback
}

// hexList parses Spoolman's comma separated multi_color_hexes and keeps the
// valid entries.
func hexList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if c := cssHex(part); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// cssHex normalises one colour to '#rrggbb'; it returns "" for anything that is
// not a hex triplet. Spoolman stores color_hex without the '#'.
func cssHex(s string) string {
	s = strings.TrimSpace(s)
	if !reHex.MatchString(s) {
		return ""
	}
	return "#" + strings.TrimPrefix(s, "#")
}
