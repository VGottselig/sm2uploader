package main

import (
	"bytes"
	"io"
	"math"
	"strconv"
	"strings"
)

// Snapmaker Orca / OrcaSlicer writes the consumption statistics and the full
// config dump as comment lines at the very END of the file, using "key = value".
// The HEADER_BLOCK at the top of the file uses "key: value" and repeats a few of
// the same keys (filament_density) -- only the "=" syntax is read, so the lower
// (complete) block always wins.
//
// Measured on four real 2026-07-30 files from Snapmaker Orca 2.3.4: the block
// spans the last ~23.6 KB. Its length is not constant (custom start/end G-Code
// ends up in the config dump), so the tail window is grown until all required
// keys were seen instead of silently computing from half the data.
var gcodeTailWindows = []int64{256 << 10, 1 << 20, 4 << 20}

// SlotUse is the filament consumption of one filament slot of one job.
// Index i of every filament_* array corresponds to tool T{i}.
type SlotUse struct {
	Slot      int
	Preset    string // filament_settings_id -- the most specific identity
	Colour    string // filament_colour, "#RRGGBB"
	Material  string // filament_type
	Vendor    string // filament_vendor
	Density   float64
	Diameter  float64
	CostPerKg float64
	GcodeG    int     // rounded ONCE here; whole grams everywhere downstream
	GcodeMM   float64 // filament length, informational
}

// GcodeUse is what a single uploaded file tells us about filament use.
// HasBlock is false for files without the Orca consumption block (Luban, .nc,
// laser/CNC) -- those get a ledger row without any booking.
type GcodeUse struct {
	Slots     []SlotUse
	PrintTime string // "3h 41m 38s"
	TotalG    float64
	HasBlock  bool
}

// ParseGcodeUse reads the consumption block from the tail of rs and always
// seeks back to the start, so the caller can hand the same reader on to the
// uploader. It must run on the ORIGINAL upload, before SMFix.
func ParseGcodeUse(rs io.ReadSeeker) (*GcodeUse, error) {
	size, err := rs.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	defer rs.Seek(0, io.SeekStart)

	var keys map[string]string
	for _, window := range gcodeTailWindows {
		if keys, err = readTailKeys(rs, size, window); err != nil {
			return nil, err
		}
		if hasRequiredKeys(keys) || window >= size {
			break
		}
	}

	use := &GcodeUse{
		PrintTime: strings.TrimSpace(keys["estimated printing time (normal mode)"]),
		TotalG:    parseFloat(keys["total filament used [g]"]),
	}
	if !hasRequiredKeys(keys) {
		return use, nil
	}
	use.HasBlock = true

	var (
		usedG     = parseNumList(keys["filament used [g]"])
		usedMM    = parseNumList(keys["filament used [mm]"])
		presets   = parseStrList(keys["filament_settings_id"])
		colours   = parseStrList(keys["filament_colour"])
		materials = parseStrList(keys["filament_type"])
		vendors   = parseStrList(keys["filament_vendor"])
		densities = parseNumList(keys["filament_density"])
		diameters = parseNumList(keys["filament_diameter"])
		costs     = parseNumList(keys["filament_cost"])
	)

	for i, g := range usedG {
		// Unused slots are 0. Note a slot below 0.5 g stays in the list with
		// GcodeG == 0 -- it was really printed, it just books nothing.
		if g <= 0 {
			continue
		}
		use.Slots = append(use.Slots, SlotUse{
			Slot:      i,
			Preset:    at(presets, i),
			Colour:    normalizeColour(at(colours, i)),
			Material:  at(materials, i),
			Vendor:    at(vendors, i),
			Density:   atNum(densities, i),
			Diameter:  atNum(diameters, i),
			CostPerKg: atNum(costs, i),
			GcodeG:    int(math.Round(g)),
			GcodeMM:   atNum(usedMM, i),
		})
	}

	// filament_cost is present in Snapmaker Orca (verified). Should another
	// slicer omit it, derive it for the unambiguous single-slot case from the
	// totals; with several slots the split is unknown, so leave it at 0.
	if len(use.Slots) == 1 && use.Slots[0].CostPerKg == 0 {
		totalCost := parseFloat(keys["total filament cost"])
		if totalCost > 0 && use.TotalG > 0 {
			use.Slots[0].CostPerKg = totalCost / use.TotalG * 1000
		}
	}

	return use, nil
}

// hasRequiredKeys reports whether the tail window caught the whole block.
// filament_settings_id sits *after* filament used [g] in the file, so a window
// that reaches the grams also reaches the identities.
func hasRequiredKeys(keys map[string]string) bool {
	if keys == nil {
		return false
	}
	_, g := keys["filament used [g]"]
	_, id := keys["filament_settings_id"]
	return g && id
}

// readTailKeys reads the last `window` bytes and collects every "; key = value"
// comment line into a map (last occurrence wins).
func readTailKeys(rs io.ReadSeeker, size, window int64) (map[string]string, error) {
	offset := size - window
	partial := true
	if offset <= 0 {
		offset, partial = 0, false
	}
	if _, err := rs.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	buf, err := io.ReadAll(io.LimitReader(rs, size-offset))
	if err != nil {
		return nil, err
	}

	keys := make(map[string]string, 64)
	lines := bytes.Split(buf, []byte("\n"))
	if partial && len(lines) > 0 {
		lines = lines[1:] // first line of the window is cut in half
	}
	for _, raw := range lines {
		line := strings.TrimSpace(string(raw))
		if !strings.HasPrefix(line, ";") {
			continue
		}
		line = strings.TrimSpace(strings.TrimLeft(line, ";"))
		// " = " only: this skips the HEADER_BLOCK's "key: value" duplicates.
		parts := strings.SplitN(line, " = ", 2)
		if len(parts) != 2 {
			continue
		}
		keys[strings.TrimSpace(parts[0])] = parts[1]
	}
	return keys, nil
}

// parseNumList splits a comma-separated number array. Orca writes
// "37.57, 0.00, 0.00, 0.00" -- with spaces after the comma.
func parseNumList(s string) []float64 {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	fields := strings.Split(s, ",")
	out := make([]float64, 0, len(fields))
	for _, f := range fields {
		out = append(out, parseFloat(f))
	}
	return out
}

// parseStrList splits a semicolon-separated string array, honouring double
// quotes: filament_settings_id is quoted and a preset name may itself contain a
// semicolon, which a naive Split would tear apart.
func parseStrList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var (
		out   []string
		cur   strings.Builder
		inQuo bool
	)
	for _, r := range s {
		switch {
		case r == '"':
			inQuo = !inQuo
		case r == ';' && !inQuo:
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	return append(out, strings.TrimSpace(cur.String()))
}

func parseFloat(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return v
}

// normalizeColour returns the colour as "#RRGGBB" (uppercase) or "".
func normalizeColour(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if !strings.HasPrefix(s, "#") {
		s = "#" + s
	}
	return strings.ToUpper(s)
}

func at(list []string, i int) string {
	if i < 0 || i >= len(list) {
		return ""
	}
	return list[i]
}

func atNum(list []float64, i int) float64 {
	if i < 0 || i >= len(list) {
		return 0
	}
	return list[i]
}
