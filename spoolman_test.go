package main

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeSpoolman records what the client sent so the request bodies can be checked.
type fakeSpoolman struct {
	t       *testing.T
	spools  []SmSpool
	vendors []SmVendor
	fields  []map[string]string

	posted map[string]json.RawMessage // last body per "METHOD path"
	calls  map[string]int
	uses   []float64 // every use_weight delta, in order
}

func newFake(t *testing.T) *fakeSpoolman {
	return &fakeSpoolman{t: t, posted: map[string]json.RawMessage{}, calls: map[string]int{}}
}

func (f *fakeSpoolman) start() (*Spoolman, *httptest.Server) {
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	f.t.Cleanup(srv.Close)
	return NewSpoolman(srv.URL), srv
}

func (f *fakeSpoolman) handle(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1")
	key := r.Method + " " + path
	f.calls[key]++
	body, _ := io.ReadAll(r.Body)
	if len(body) > 0 {
		f.posted[key] = body
	}
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.Method == http.MethodGet && path == "/spool":
		json.NewEncoder(w).Encode(f.spools)
	case r.Method == http.MethodGet && path == "/vendor":
		json.NewEncoder(w).Encode(f.vendors)
	case r.Method == http.MethodGet && path == "/filament":
		json.NewEncoder(w).Encode([]SmFilament{})
	case r.Method == http.MethodGet && path == "/field/spool":
		json.NewEncoder(w).Encode(f.fields)
	case r.Method == http.MethodPost && path == "/field/spool/"+presetField:
		f.fields = append(f.fields, map[string]string{"key": presetField})
		w.Write([]byte(`{}`))
	case r.Method == http.MethodPost && path == "/vendor":
		v := SmVendor{ID: 7, Name: "DEEPLEE"}
		f.vendors = append(f.vendors, v)
		json.NewEncoder(w).Encode(v)
	case r.Method == http.MethodPost && path == "/filament":
		json.NewEncoder(w).Encode(SmFilament{ID: 11, Name: "DEEPLEE PLA PRO Rosa"})
	case r.Method == http.MethodPost && path == "/spool":
		var req struct {
			Price *float64          `json:"price"`
			Extra map[string]string `json:"extra"`
		}
		json.Unmarshal(body, &req)
		sp := SmSpool{ID: 22, InitialWeight: f64(1000), UsedWeight: 0, Price: req.Price, Extra: req.Extra}
		f.spools = append(f.spools, sp) // a following GET must find it
		json.NewEncoder(w).Encode(sp)
	case r.Method == http.MethodPatch && strings.HasPrefix(path, "/spool/"):
		var patch struct {
			Extra map[string]string `json:"extra"`
		}
		json.Unmarshal(body, &patch)
		f.spools[0].Extra = patch.Extra
		json.NewEncoder(w).Encode(f.spools[0])
	case r.Method == http.MethodPut && strings.HasSuffix(path, "/use"):
		var use struct {
			UseWeight float64 `json:"use_weight"`
		}
		json.Unmarshal(body, &use)
		f.uses = append(f.uses, use.UseWeight)
		if len(f.spools) == 0 {
			f.spools = []SmSpool{{ID: 22, InitialWeight: f64(1000)}}
		}
		f.spools[0].UsedWeight += use.UseWeight
		if f.spools[0].UsedWeight < 0 { // Spoolman clamps silently at 0
			f.spools[0].UsedWeight = 0
		}
		json.NewEncoder(w).Encode(f.spools[0])
	case r.Method == http.MethodGet && path == "/setting/currency":
		// The value is double JSON encoded on purpose -- like the real API.
		json.NewEncoder(w).Encode(map[string]any{"value": `"CHF"`, "is_set": true})
	default:
		http.Error(w, `{"message":"unexpected "`+key+`"}`, http.StatusNotFound)
	}
}

func f64(v float64) *float64 { return &v }

func TestNewSpoolmanURLHandling(t *testing.T) {
	if NewSpoolman("") != nil {
		t.Error("empty URL should switch the feature off (nil)")
	}
	if NewSpoolman("   ") != nil {
		t.Error("blank URL should switch the feature off (nil)")
	}
	for _, in := range []string{"http://spoolman:8000", "http://spoolman:8000/", "http://spoolman:8000/api/v1"} {
		if got := NewSpoolman(in).base; got != "http://spoolman:8000" {
			t.Errorf("NewSpoolman(%q).base = %q", in, got)
		}
	}
}

// The mapping value arrives JSON encoded ("\"name\"") and must be decoded before
// comparing.
func TestResolveSpoolByPresetMapping(t *testing.T) {
	f := newFake(t)
	f.spools = []SmSpool{
		{ID: 1, Filament: SmFilament{Name: "something else"},
			Extra:    map[string]string{presetField: `"DEEPLEE PLA PRO Rosa"`},
			LastUsed: "2026-07-01T10:00:00Z", InitialWeight: f64(1000)},
	}
	sm, _ := f.start()

	sp, err := sm.ResolveSpool(SlotUse{Preset: "DEEPLEE PLA PRO Rosa"})
	if err != nil {
		t.Fatal(err)
	}
	if sp.ID != 1 {
		t.Errorf("spool %d, want 1", sp.ID)
	}
	if f.calls["POST /spool"] != 0 || f.calls["POST /filament"] != 0 {
		t.Error("an existing spool must not create anything")
	}
}

// Several mapped spools: the most recently used, non-archived one wins.
func TestResolveSpoolPicksNewestLastUsed(t *testing.T) {
	f := newFake(t)
	mapped := map[string]string{presetField: `"Preset A"`}
	f.spools = []SmSpool{
		{ID: 1, Extra: mapped, LastUsed: "2026-01-01T10:00:00Z"},
		{ID: 2, Extra: mapped, LastUsed: "2026-07-29T18:30:00Z"},
		{ID: 3, Extra: mapped, LastUsed: ""},
		{ID: 4, Extra: mapped, LastUsed: "2026-07-30T09:00:00Z", Archived: true},
	}
	sm, _ := f.start()

	sp, err := sm.ResolveSpool(SlotUse{Preset: "Preset A"})
	if err != nil {
		t.Fatal(err)
	}
	if sp.ID != 2 {
		t.Errorf("spool %d, want 2 (newest non-archived)", sp.ID)
	}
}

// No mapping but the same filament name: adopt the spool and remember the
// mapping, keeping other extra values.
func TestResolveSpoolByFilamentNameStampsMapping(t *testing.T) {
	f := newFake(t)
	f.spools = []SmSpool{
		{ID: 5, Filament: SmFilament{Name: "DEEPLEE PLA+ Weiß"},
			Extra: map[string]string{"note": `"keep me"`}, InitialWeight: f64(1000)},
	}
	sm, _ := f.start()

	sp, err := sm.ResolveSpool(SlotUse{Preset: "deeplee pla+ weiß"}) // case-insensitive
	if err != nil {
		t.Fatal(err)
	}
	if sp.ID != 5 {
		t.Fatalf("spool %d, want 5", sp.ID)
	}
	raw, ok := f.posted["PATCH /spool/5"]
	if !ok {
		t.Fatal("no PATCH sent -- mapping was not stored")
	}
	var patch struct {
		Extra map[string]string `json:"extra"`
	}
	if err := json.Unmarshal(raw, &patch); err != nil {
		t.Fatal(err)
	}
	if got := patch.Extra[presetField]; got != `"deeplee pla+ weiß"` {
		t.Errorf("extra[%s] = %s, want a JSON encoded string", presetField, got)
	}
	if patch.Extra["note"] != `"keep me"` {
		t.Error("existing extra values were dropped instead of merged")
	}
	if f.calls["POST /field/spool/"+presetField] != 1 {
		t.Error("the custom field should have been registered once")
	}
}

// Nothing matches: vendor, filament and spool are created from the G-Code values.
func TestResolveSpoolCreatesEverything(t *testing.T) {
	f := newFake(t)
	sm, _ := f.start()

	slot := SlotUse{
		Slot: 1, Preset: "DEEPLEE PLA PRO Rosa", Colour: "#F19CB4", Material: "PLA",
		Vendor: "DEEPLEE", Density: 1.24, Diameter: 1.75, CostPerKg: 13.59, GcodeG: 46,
	}
	sp, err := sm.ResolveSpool(slot)
	if err != nil {
		t.Fatal(err)
	}
	if sp.ID != 22 {
		t.Errorf("spool %d, want 22", sp.ID)
	}

	var fil map[string]any
	if err := json.Unmarshal(f.posted["POST /filament"], &fil); err != nil {
		t.Fatal("no filament created:", err)
	}
	if fil["name"] != slot.Preset {
		t.Errorf("name = %v", fil["name"])
	}
	if fil["density"] != 1.24 || fil["diameter"] != 1.75 {
		t.Errorf("density/diameter = %v/%v, want 1.24/1.75", fil["density"], fil["diameter"])
	}
	if fil["weight"] != 1000.0 {
		t.Errorf("weight = %v, want 1000", fil["weight"])
	}
	if fil["color_hex"] != "F19CB4" {
		t.Errorf("color_hex = %v, want F19CB4 (no leading '#')", fil["color_hex"])
	}
	if fil["price"] != 13.59 {
		t.Errorf("price = %v, want 13.59 (cost per kg at 1000 g net)", fil["price"])
	}
	if fil["vendor_id"] != 7.0 {
		t.Errorf("vendor_id = %v, want the created vendor 7", fil["vendor_id"])
	}
	if fil["spool_weight"] != nil {
		t.Error("the tare weight is unknown and must stay empty")
	}

	var spool map[string]any
	if err := json.Unmarshal(f.posted["POST /spool"], &spool); err != nil {
		t.Fatal("no spool created:", err)
	}
	if spool["filament_id"] != 11.0 {
		t.Errorf("filament_id = %v, want 11", spool["filament_id"])
	}
	if spool["initial_weight"] != 1000.0 {
		t.Errorf("initial_weight = %v, want 1000", spool["initial_weight"])
	}
	extra, _ := spool["extra"].(map[string]any)
	if extra[presetField] != `"DEEPLEE PLA PRO Rosa"` {
		t.Errorf("extra[%s] = %v, want a JSON encoded string", presetField, extra[presetField])
	}
}

func TestResolveSpoolWithoutPreset(t *testing.T) {
	f := newFake(t)
	sm, _ := f.start()
	if _, err := sm.ResolveSpool(SlotUse{Preset: "  "}); err == nil {
		t.Error("a slot without a preset name should be an error")
	}
}

// Corrections are negative deltas; the silent clamp at 0 is Spoolman's.
func TestUseSendsDeltaAndReturnsUpdatedSpool(t *testing.T) {
	f := newFake(t)
	f.spools = []SmSpool{{ID: 3, InitialWeight: f64(1000), UsedWeight: 120}}
	sm, _ := f.start()

	sp, err := sm.Use(3, -70)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	json.Unmarshal(f.posted["PUT /spool/3/use"], &body)
	if body["use_weight"] != -70.0 {
		t.Errorf("use_weight = %v, want -70", body["use_weight"])
	}
	if _, ok := body["use_length"]; ok {
		t.Error("use_length must not be sent alongside use_weight (400)")
	}
	if sp.UsedWeight != 50 {
		t.Errorf("used_weight = %v, want 50", sp.UsedWeight)
	}
	rem, ok := RemainingG(sp)
	if !ok || rem != 950 {
		t.Errorf("RemainingG = %v (%v), want 950", rem, ok)
	}
}

// An over-booked spool must show up as negative -- Spoolman's own
// remaining_weight is clamped at 0 and would hide it.
func TestRemainingGGoesNegative(t *testing.T) {
	sp := &SmSpool{InitialWeight: f64(1000), UsedWeight: 1200}
	rem, ok := RemainingG(sp)
	if !ok {
		t.Fatal("ok = false")
	}
	if rem != -200 {
		t.Errorf("RemainingG = %v, want -200", rem)
	}
}

// Net weight: spool.initial_weight, falling back to filament.weight.
func TestRemainingGFallsBackToFilamentWeight(t *testing.T) {
	sp := &SmSpool{Filament: SmFilament{Weight: f64(750)}, UsedWeight: 250}
	rem, ok := RemainingG(sp)
	if !ok || rem != 500 {
		t.Errorf("RemainingG = %v (%v), want 500", rem, ok)
	}
	if _, ok := RemainingG(&SmSpool{UsedWeight: 10}); ok {
		t.Error("without any net weight there is no remaining weight")
	}
}

// spool.price -> filament.price, spool.initial_weight -> filament.weight.
// The API chains neither, the client has to.
func TestPricePerGFallbackChain(t *testing.T) {
	f := newFake(t)
	f.spools = []SmSpool{
		{ID: 1, Price: f64(20), InitialWeight: f64(1000)},
		{ID: 2, Filament: SmFilament{Price: f64(13.59), Weight: f64(1000)}},
		{ID: 3, InitialWeight: f64(1000)}, // no price anywhere
	}
	sm, _ := f.start()

	if got := sm.PricePerG(1); math.Abs(got-0.02) > 1e-9 {
		t.Errorf("PricePerG(1) = %v, want 0.02", got)
	}
	if got := sm.PricePerG(2); math.Abs(got-0.01359) > 1e-9 {
		t.Errorf("PricePerG(2) = %v, want 0.01359 (filament fallback)", got)
	}
	if got := sm.PricePerG(3); got != 0 {
		t.Errorf("PricePerG(3) = %v, want 0", got)
	}
	if got := sm.PricePerG(999); got != 0 {
		t.Errorf("PricePerG(unknown) = %v, want 0", got)
	}
}

// The currency setting is double JSON encoded.
func TestCurrencyUnwrapsDoubleEncoding(t *testing.T) {
	f := newFake(t)
	sm, _ := f.start()
	if got := sm.Currency(); got != "CHF" {
		t.Errorf("Currency = %q, want CHF", got)
	}
	// second call is cached
	if got := sm.Currency(); got != "CHF" {
		t.Errorf("cached Currency = %q", got)
	}
	if f.calls["GET /setting/currency"] != 1 {
		t.Errorf("currency fetched %d times, want 1", f.calls["GET /setting/currency"])
	}
}

// Spoolman down: the cost column shows 0 instead of taking the page with it.
func TestPricePerGWithUnreachableServer(t *testing.T) {
	sm := NewSpoolman("http://127.0.0.1:1")
	if got := sm.PricePerG(1); got != 0 {
		t.Errorf("PricePerG = %v, want 0", got)
	}
	if got := sm.Currency(); got != "EUR" {
		t.Errorf("Currency = %q, want the EUR default", got)
	}
	if _, ok := sm.RemainingByPreset("whatever"); ok {
		t.Error("RemainingByPreset should report false")
	}
}

// 5xx is retried, 4xx is not.
func TestDoRetriesServerErrors(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) < 3 {
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	sm := NewSpoolman(srv.URL)
	if _, err := sm.Spools(); err != nil {
		t.Fatalf("retry did not recover: %v", err)
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("%d attempts, want 3", got)
	}

	hits.Store(0)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, `{"message":"nope"}`, http.StatusBadRequest)
	}))
	defer bad.Close()
	if _, err := NewSpoolman(bad.URL).Spools(); err == nil {
		t.Error("400 should be an error")
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("%d attempts on 400, want 1 (no retry)", got)
	}
}

func TestExtraStringDecoding(t *testing.T) {
	cases := map[string]string{
		`"DEEPLEE PLA PRO Rosa"`: "DEEPLEE PLA PRO Rosa",
		`"with \"quotes\""`:      `with "quotes"`,
		`plain`:                  "plain", // tolerate a non-encoded value
	}
	for raw, want := range cases {
		if got := extraString(map[string]string{presetField: raw}, presetField); got != want {
			t.Errorf("extraString(%s) = %q, want %q", raw, got, want)
		}
	}
	if got := extraString(nil, presetField); got != "" {
		t.Errorf("missing key returned %q", got)
	}
}
