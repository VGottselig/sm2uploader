package main

import (
	"strings"
	"testing"
)

// The stock section shows colour and remaining weight only, emptiest first.
func TestStockHTMLRendersChips(t *testing.T) {
	f := newFake(t)
	f.spools = []SmSpool{
		{ID: 3, InitialWeight: f64(1000), UsedWeight: 357,
			Filament: SmFilament{Name: "DEEPLEE PLA PRO Rosa", ColorHex: "F19CB4"}},
		{ID: 4, InitialWeight: f64(1000), UsedWeight: 995.0029,
			Filament: SmFilament{Name: "DEEPLEE Basic PLA Beige", ColorHex: "D8C6A1"}},
		{ID: 9, InitialWeight: f64(1000), UsedWeight: 100, Archived: true,
			Filament: SmFilament{Name: "Alte Rolle", ColorHex: "00FF00"}},
		{ID: 5, UsedWeight: 10, // no net weight -> no remaining amount
			Filament: SmFilament{Name: "Unbekannte Rolle", ColorHex: "112233"}},
	}
	sm, _ := f.start()
	withLedgerAndSpoolman(t, sm)

	got := stockHTML()
	for _, want := range []string{
		`background:#F19CB4`,
		`background:#D8C6A1`,
		`5 g`,   // 995.0029 used, rounded
		`643 g`, // 1000 - 357
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stock is missing %q\ngot: %s", want, got)
		}
	}
	// Archived spools and spools without a net weight are left out.
	for _, gone := range []string{`Alte Rolle`, `Unbekannte Rolle`, `background:#00FF00`} {
		if strings.Contains(got, gone) {
			t.Errorf("stock still carries %q", gone)
		}
	}
	// Emptiest first, and the low one is marked.
	if i, j := strings.Index(got, "5 g"), strings.Index(got, "643 g"); i > j {
		t.Errorf("not sorted by remaining amount: %s", got)
	}
	if !strings.Contains(got, `class="chip warn"`) {
		t.Errorf("the spool below %d g is not marked: %s", SpoolmanLowG, got)
	}
	// Nothing but colour and grams: no filament name in the text, only as tooltip.
	if strings.Contains(strings.SplitN(got, `title=`, 2)[0], "DEEPLEE") {
		t.Errorf("filament name leaked into the chip body: %s", got)
	}
}

// An over-booked spool must stand out instead of showing a bare negative number.
func TestStockHTMLMarksNegative(t *testing.T) {
	f := newFake(t)
	f.spools = []SmSpool{{ID: 1, InitialWeight: f64(1000), UsedWeight: 1042,
		Filament: SmFilament{Name: "Leer", ColorHex: "FFFFFF"}}}
	sm, _ := f.start()
	withLedgerAndSpoolman(t, sm)

	got := stockHTML()
	if !strings.Contains(got, `class="chip neg"`) || !strings.Contains(got, `-42 g`) {
		t.Errorf("over-booked spool renders as %q", got)
	}
}

// Multi-colour filaments become a gradient in the recorded direction; the
// fallback keeps a colourless spool visible.
func TestSwatchStyle(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    SmFilament
		want string
	}{
		{"single", SmFilament{ColorHex: "6B4423"}, "background:#6B4423"},
		{"hashed", SmFilament{ColorHex: "#6B4423"}, "background:#6B4423"},
		{"none", SmFilament{}, "background:" + swatchFallback},
		{"garbage", SmFilament{ColorHex: "red;width:99px"}, "background:" + swatchFallback},
		{"coaxial", SmFilament{ColorHex: "FF00FF", MultiColorHexes: "0000FF,FF0000,FFFF00", MultiColorDirection: "coaxial"},
			"background:linear-gradient(180deg,#0000FF,#FF0000,#FFFF00)"},
		{"longitudinal", SmFilament{MultiColorHexes: "0000FF,FF0000", MultiColorDirection: "longitudinal"},
			"background:linear-gradient(90deg,#0000FF,#FF0000)"},
		// A single entry is no gradient -- CSS needs two stops.
		{"multi with one", SmFilament{ColorHex: "AABBCC", MultiColorHexes: "0000FF"}, "background:#AABBCC"},
	} {
		if got := swatchStyle(tc.f); got != tc.want {
			t.Errorf("%s: swatchStyle = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestStockHTMLWithoutSpoolman(t *testing.T) {
	withLedgerAndSpoolman(t, nil)
	if got := stockHTML(); !strings.Contains(got, "SPOOLMAN_URL") {
		t.Errorf("without Spoolman the section renders %q", got)
	}
}
