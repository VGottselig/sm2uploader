package main

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The four fixtures in testdata/ are the real key lines of four Snapmaker Orca
// 2.3.4 files from 2026-07-30. They happen to cover all four slot indices with
// exactly one used slot each -- and Ivana proves the index parallelism of
// filament_cost (11.99, not the 13.59 of the other three).
func TestParseRealSampleTails(t *testing.T) {
	cases := []struct {
		file      string
		slot      int
		rawG      float64
		wantG     int
		costPerKg float64
		colour    string
		preset    string
		printTime string
	}{
		{"Assembly_0.2mm_3h42m", 0, 37.57, 38, 13.59, "#D8C6A1", "DEEPLEE Basic PLA Beige", "3h 41m 38s"},
		{"ElefantMobile_0.2mm_4h25m", 1, 45.93, 46, 13.59, "#F19CB4", "DEEPLEE PLA PRO Rosa", "4h 24m 54s"},
		{"Ivana_0.2mm_36m36s", 2, 1.98, 2, 11.99, "#FFFFFF", "DEEPLEE PLA+ Weiß", "36m 36s"},
		{"ElefantMobile_0.2mm_2h14m", 3, 18.84, 19, 13.59, "#6E8394", "DEEPLEE Basic PLA Blau-Grau", "2h 13m 34s"},
	}

	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			use := parseFixture(t, c.file)
			if !use.HasBlock {
				t.Fatal("HasBlock = false, want true")
			}
			if len(use.Slots) != 1 {
				t.Fatalf("got %d used slots, want 1: %+v", len(use.Slots), use.Slots)
			}
			s := use.Slots[0]
			if s.Slot != c.slot {
				t.Errorf("Slot = %d, want %d", s.Slot, c.slot)
			}
			if s.GcodeG != c.wantG {
				t.Errorf("GcodeG = %d, want %d (raw %.2f)", s.GcodeG, c.wantG, c.rawG)
			}
			if s.CostPerKg != c.costPerKg {
				t.Errorf("CostPerKg = %v, want %v", s.CostPerKg, c.costPerKg)
			}
			if s.Colour != c.colour {
				t.Errorf("Colour = %q, want %q", s.Colour, c.colour)
			}
			if s.Preset != c.preset {
				t.Errorf("Preset = %q, want %q", s.Preset, c.preset)
			}
			if s.Material != "PLA" || s.Vendor != "DEEPLEE" {
				t.Errorf("Material/Vendor = %q/%q, want PLA/DEEPLEE", s.Material, s.Vendor)
			}
			if s.Density != 1.24 || s.Diameter != 1.75 {
				t.Errorf("Density/Diameter = %v/%v, want 1.24/1.75", s.Density, s.Diameter)
			}
			if use.PrintTime != c.printTime {
				t.Errorf("PrintTime = %q, want %q", use.PrintTime, c.printTime)
			}
			if math.Abs(use.TotalG-c.rawG) > 0.001 {
				t.Errorf("TotalG = %v, want %v", use.TotalG, c.rawG)
			}
		})
	}
}

// Consistency of the G-Code itself: used[g] == used[cm3] * density, and
// total filament cost == sum(used_g * cost_per_kg / 1000).
func TestRealSampleInternalConsistency(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.tail.gcode"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures found: %v", err)
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".tail.gcode")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			keys, err := readTailKeys(bytes.NewReader(raw), int64(len(raw)), 256<<10)
			if err != nil {
				t.Fatal(err)
			}
			cm3 := parseNumList(keys["filament used [cm3]"])
			grams := parseNumList(keys["filament used [g]"])
			dens := parseNumList(keys["filament_density"])
			costs := parseNumList(keys["filament_cost"])
			var sumCost float64
			for i := range grams {
				if grams[i] <= 0 {
					continue
				}
				if want := cm3[i] * dens[i]; math.Abs(grams[i]-want) > 0.02 {
					t.Errorf("slot %d: used[g] = %v, cm3*density = %v", i, grams[i], want)
				}
				sumCost += grams[i] * costs[i] / 1000
			}
			total := parseFloat(keys["total filament cost"])
			if math.Abs(sumCost-total) > 0.005 {
				t.Errorf("cost cross-check: computed %.4f, G-Code says %.2f", sumCost, total)
			}
		})
	}
}

// A semicolon inside a quoted preset name must not split the array -- the
// reason parseStrList is quote-aware instead of a plain strings.Split.
func TestParseSemicolonInsidePresetName(t *testing.T) {
	src := `; filament used [g] = 0.00, 12.50
; filament used [mm] = 0.00, 4200.00
; filament_settings_id = "Plain PLA";"Weird; Brand PLA"
; filament_colour = #112233;#445566
; filament_type = PLA;PETG
; filament_vendor = ACME;ACME
; filament_density = 1.24,1.27
; filament_diameter = 1.75,1.75
; filament_cost = 10.00,20.00
`
	use := mustParse(t, src)
	if len(use.Slots) != 1 {
		t.Fatalf("got %d slots, want 1", len(use.Slots))
	}
	if got := use.Slots[0].Preset; got != "Weird; Brand PLA" {
		t.Errorf("Preset = %q, want %q", got, "Weird; Brand PLA")
	}
	if got := use.Slots[0].Material; got != "PETG" {
		t.Errorf("Material = %q, want PETG", got)
	}
}

// Several used slots at once, unquoted preset names, colours without "#".
func TestParseMultipleSlotsUnquoted(t *testing.T) {
	src := `; filament used [g] = 10.40, 0.00, 3.50
; filament used [mm] = 3500.00, 0.00, 1180.00
; filament_settings_id = Alpha PLA;Beta PLA;Gamma PLA
; filament_colour = 112233;445566;aabbcc
; filament_type = PLA;PLA;PLA
; filament_vendor = ACME;ACME;ACME
; filament_density = 1.24,1.24,1.24
; filament_diameter = 1.75,1.75,1.75
; filament_cost = 10.00,20.00,30.00
; total filament used [g] = 13.90
; total filament cost = 0.21
`
	use := mustParse(t, src)
	if len(use.Slots) != 2 {
		t.Fatalf("got %d slots, want 2: %+v", len(use.Slots), use.Slots)
	}
	if use.Slots[0].Slot != 0 || use.Slots[0].GcodeG != 10 || use.Slots[0].Preset != "Alpha PLA" {
		t.Errorf("slot[0] = %+v", use.Slots[0])
	}
	if use.Slots[1].Slot != 2 || use.Slots[1].GcodeG != 4 || use.Slots[1].Preset != "Gamma PLA" {
		t.Errorf("slot[1] = %+v", use.Slots[1])
	}
	if use.Slots[1].Colour != "#AABBCC" {
		t.Errorf("Colour = %q, want #AABBCC", use.Slots[1].Colour)
	}
	// filament_cost present per slot -> no single-slot derivation
	if use.Slots[0].CostPerKg != 10 || use.Slots[1].CostPerKg != 30 {
		t.Errorf("CostPerKg = %v/%v, want 10/30", use.Slots[0].CostPerKg, use.Slots[1].CostPerKg)
	}
}

// A slot below 0.5 g rounds to 0 but still gets a row -- it really was printed,
// it just books nothing.
func TestParseRoundsToWholeGramsKeepingTinySlots(t *testing.T) {
	src := `; filament used [g] = 0.40, 0.00
; filament used [mm] = 130.00, 0.00
; filament_settings_id = "Tiny PLA";"Other"
; filament_colour = #010203;#040506
; filament_type = PLA;PLA
; filament_vendor = ACME;ACME
; filament_density = 1.24,1.24
; filament_diameter = 1.75,1.75
; filament_cost = 10.00,10.00
`
	use := mustParse(t, src)
	if len(use.Slots) != 1 {
		t.Fatalf("got %d slots, want 1", len(use.Slots))
	}
	if use.Slots[0].GcodeG != 0 {
		t.Errorf("GcodeG = %d, want 0", use.Slots[0].GcodeG)
	}
}

// Luban / .nc / laser files have no consumption block: HasBlock is false and no
// slots -- the caller writes a row without any booking.
func TestParseWithoutConsumptionBlock(t *testing.T) {
	src := ";Filament used: 1.23m\nG1 X1 Y1\nG1 X2 Y2 E0.4\nM104 S0\n"
	use := mustParse(t, src)
	if use.HasBlock {
		t.Error("HasBlock = true, want false")
	}
	if len(use.Slots) != 0 {
		t.Errorf("got %d slots, want 0", len(use.Slots))
	}
}

// The HEADER_BLOCK at the top uses "key: value" and repeats filament_density.
// Only "=" lines count, so the lower block must win.
func TestParseIgnoresHeaderBlockColonSyntax(t *testing.T) {
	src := `; HEADER_BLOCK_START
; total layer number: 56
; filament_density: 9.99,9.99
; HEADER_BLOCK_END
G1 X1 Y1 E0.2
; filament used [g] = 5.00, 0.00
; filament_settings_id = "A";"B"
; filament_colour = #111111;#222222
; filament_type = PLA;PLA
; filament_vendor = ACME;ACME
; filament_density = 1.24,1.24
; filament_diameter = 1.75,1.75
; filament_cost = 10.00,10.00
`
	use := mustParse(t, src)
	if len(use.Slots) != 1 {
		t.Fatalf("got %d slots, want 1", len(use.Slots))
	}
	if use.Slots[0].Density != 1.24 {
		t.Errorf("Density = %v, want 1.24 (from the '=' block)", use.Slots[0].Density)
	}
}

// The config dump length is not constant. If the block sits farther from the end
// than the first window, the window has to grow instead of reporting HasBlock=false.
func TestParseGrowsTailWindow(t *testing.T) {
	var b strings.Builder
	b.WriteString(`; filament used [g] = 0.00, 45.93
; filament used [mm] = 0.00, 15398.04
; filament_settings_id = "A";"DEEPLEE PLA PRO Rosa"
; filament_colour = #111111;#F19CB4
; filament_type = PLA;PLA
; filament_vendor = ACME;DEEPLEE
; filament_density = 1.24,1.24
; filament_diameter = 1.75,1.75
; filament_cost = 13.59,13.59
`)
	// ~300 KB of trailing config-dump-like padding pushes the block out of the
	// 256 KB window.
	for i := 0; b.Len() < 300<<10; i++ {
		fmt.Fprintf(&b, "; custom_gcode_line_%d = M117 padding padding padding padding\n", i)
	}
	use := mustParse(t, b.String())
	if !use.HasBlock {
		t.Fatal("HasBlock = false -- the tail window did not grow")
	}
	if len(use.Slots) != 1 || use.Slots[0].GcodeG != 46 {
		t.Fatalf("slots = %+v, want one slot with 46 g", use.Slots)
	}
	if use.Slots[0].Preset != "DEEPLEE PLA PRO Rosa" {
		t.Errorf("Preset = %q", use.Slots[0].Preset)
	}
}

// The uploader streams the very same reader afterwards, so the parser must
// rewind it.
func TestParseSeeksBackToStart(t *testing.T) {
	r := bytes.NewReader([]byte("G1 X1\n; filament used [g] = 1.00\n; filament_settings_id = \"A\"\n"))
	if _, err := ParseGcodeUse(r); err != nil {
		t.Fatal(err)
	}
	pos, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatal(err)
	}
	if pos != 0 {
		t.Errorf("reader at %d, want 0", pos)
	}
}

// Without filament_cost (other slicer) a single used slot derives the price per
// kg from the totals; several slots leave it at 0.
func TestParseDerivesCostForSingleSlot(t *testing.T) {
	src := `; filament used [g] = 0.00, 45.93
; filament used [mm] = 0.00, 15398.04
; total filament used [g] = 45.93
; total filament cost = 0.62
; filament_settings_id = "A";"B"
; filament_colour = #111111;#222222
; filament_type = PLA;PLA
; filament_vendor = ACME;ACME
; filament_density = 1.24,1.24
; filament_diameter = 1.75,1.75
`
	use := mustParse(t, src)
	if len(use.Slots) != 1 {
		t.Fatalf("got %d slots, want 1", len(use.Slots))
	}
	if got := use.Slots[0].CostPerKg; math.Abs(got-13.499) > 0.01 {
		t.Errorf("CostPerKg = %v, want ~13.50 (0.62/45.93*1000)", got)
	}
}

// TestParseFullRealFiles runs the parser over the complete multi-MB originals,
// not just the extracted tails -- this is what exercises the tail window against
// a real ~24 KB block sitting behind millions of bytes of body G-Code. The files
// live outside the repo (~9 MB), so point SM2_GCODE_SAMPLES at them to run it:
//
//	SM2_GCODE_SAMPLES=/home/gad/sm2uploader-build/gcode-samples go test -run FullReal
func TestParseFullRealFiles(t *testing.T) {
	dir := os.Getenv("SM2_GCODE_SAMPLES")
	if dir == "" {
		t.Skip("SM2_GCODE_SAMPLES not set")
	}
	want := map[string]struct {
		slot   int
		grams  int
		cost   float64
		preset string
	}{
		"Assembly_0.2mm_3h42m":      {0, 38, 13.59, "DEEPLEE Basic PLA Beige"},
		"ElefantMobile_0.2mm_4h25m": {1, 46, 13.59, "DEEPLEE PLA PRO Rosa"},
		"Ivana_0.2mm_36m36s":        {2, 2, 11.99, "DEEPLEE PLA+ Weiß"},
		"ElefantMobile_0.2mm_2h14m": {3, 19, 13.59, "DEEPLEE Basic PLA Blau-Grau"},
	}
	for name, w := range want {
		t.Run(name, func(t *testing.T) {
			fh, err := os.Open(filepath.Join(dir, name+".gcode"))
			if err != nil {
				t.Skip(err)
			}
			defer fh.Close()
			use, err := ParseGcodeUse(fh)
			if err != nil {
				t.Fatal(err)
			}
			if !use.HasBlock || len(use.Slots) != 1 {
				t.Fatalf("HasBlock=%v, slots=%+v", use.HasBlock, use.Slots)
			}
			s := use.Slots[0]
			if s.Slot != w.slot || s.GcodeG != w.grams || s.CostPerKg != w.cost || s.Preset != w.preset {
				t.Errorf("got T%d %dg %.2f/kg %q, want T%d %dg %.2f/kg %q",
					s.Slot, s.GcodeG, s.CostPerKg, s.Preset, w.slot, w.grams, w.cost, w.preset)
			}
			// The uploader streams the same handle afterwards.
			if pos, _ := fh.Seek(0, io.SeekCurrent); pos != 0 {
				t.Errorf("file handle at %d, want 0", pos)
			}
		})
	}
}

func mustParse(t *testing.T, src string) *GcodeUse {
	t.Helper()
	use, err := ParseGcodeUse(bytes.NewReader([]byte(src)))
	if err != nil {
		t.Fatal(err)
	}
	return use
}

func parseFixture(t *testing.T, name string) *GcodeUse {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name+".tail.gcode"))
	if err != nil {
		t.Fatal(err)
	}
	use, err := ParseGcodeUse(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return use
}
