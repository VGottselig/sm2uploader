package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// presetField is the custom spool field that maps an Orca preset name
// (filament_settings_id) to a physical spool. GET /spool has no filter for
// extra fields, so the match is made client-side over all non-archived spools.
const presetField = "sm2_preset"

// priceCacheTTL bounds how stale the price used by the cost column may be.
const priceCacheTTL = 30 * time.Second

// SmVendor, SmFilament and SmSpool cover the parts of the Spoolman v0.25 API we
// touch. Verified against the running instance's /api/v1/openapi.json.
type SmVendor struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type SmFilament struct {
	ID       int       `json:"id"`
	Name     string    `json:"name"`
	Vendor   *SmVendor `json:"vendor"`
	Material string    `json:"material"`
	Price    *float64  `json:"price"`
	Density  float64   `json:"density"`
	Diameter float64   `json:"diameter"`
	Weight   *float64  `json:"weight"`
	ColorHex string    `json:"color_hex"`
}

type SmSpool struct {
	ID            int               `json:"id"`
	Filament      SmFilament        `json:"filament"`
	Price         *float64          `json:"price"`
	InitialWeight *float64          `json:"initial_weight"`
	UsedWeight    float64           `json:"used_weight"`
	LastUsed      string            `json:"last_used"`
	Archived      bool              `json:"archived"`
	Extra         map[string]string `json:"extra"`
}

// Spoolman is a minimal client for the Spoolman v1 API.
type Spoolman struct {
	base string
	hc   *http.Client // bookings: patient
	rhc  *http.Client // page rendering: must never hang the GUI

	mu         sync.Mutex
	currency   string
	fieldReady bool
	spools     []SmSpool // cache behind the cost/remaining columns
	spoolsAt   time.Time
}

// TheSpoolman is the process-wide client; nil when SPOOLMAN_URL is unset. The
// upload table keeps working in that case, rows just stay "offen".
var TheSpoolman *Spoolman

// NewSpoolman returns nil for an empty base URL (feature switched off).
func NewSpoolman(base string) *Spoolman {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return nil
	}
	base = strings.TrimSuffix(base, "/api/v1")
	return &Spoolman{
		base: base,
		hc:   &http.Client{Timeout: 8 * time.Second},
		rhc:  &http.Client{Timeout: 2 * time.Second},
	}
}

// ---------------------------------------------------------------- transport

// do performs one API call. Network errors and 5xx are retried twice with a
// fixed 1 s pause; a 4xx is returned straight away with the server's message.
func (s *Spoolman) do(client *http.Client, method, path string, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Second)
		}
		var rdr io.Reader
		if payload != nil {
			rdr = bytes.NewReader(payload)
		}
		req, err := http.NewRequest(method, s.base+"/api/v1"+path, rdr)
		if err != nil {
			return err
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, snippet(respBody))
			continue
		}
		if resp.StatusCode >= 400 {
			return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, snippet(respBody))
		}
		if out != nil {
			if err := json.Unmarshal(respBody, out); err != nil {
				return fmt.Errorf("%s %s: unreadable response: %w", method, path, err)
			}
		}
		return nil
	}
	return lastErr
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// ---------------------------------------------------------------- reading

// Spools lists all non-archived spools.
func (s *Spoolman) Spools() ([]SmSpool, error) {
	var out []SmSpool
	if err := s.do(s.hc, http.MethodGet, "/spool?allow_archived=false", nil, &out); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.spools, s.spoolsAt = out, time.Now()
	s.mu.Unlock()
	return out, nil
}

// Currency returns the configured currency code. The setting value is *double*
// JSON encoded ({"value": "\"EUR\""}) and has to be unwrapped twice.
func (s *Spoolman) Currency() string {
	s.mu.Lock()
	if s.currency != "" {
		defer s.mu.Unlock()
		return s.currency
	}
	s.mu.Unlock()

	cur := "EUR"
	var setting struct {
		Value string `json:"value"`
	}
	if err := s.do(s.rhc, http.MethodGet, "/setting/currency", nil, &setting); err == nil {
		var inner string
		if err := json.Unmarshal([]byte(setting.Value), &inner); err == nil && inner != "" {
			cur = inner
		} else if v := strings.Trim(setting.Value, `"`); v != "" {
			cur = v
		}
	}
	s.mu.Lock()
	s.currency = cur
	s.mu.Unlock()
	return cur
}

// cachedSpools returns the spool list for rendering. It refreshes at most every
// priceCacheTTL and keeps the previous list when Spoolman is unreachable -- the
// page must render either way.
func (s *Spoolman) cachedSpools() []SmSpool {
	s.mu.Lock()
	fresh := time.Since(s.spoolsAt) < priceCacheTTL
	cached := s.spools
	s.mu.Unlock()
	if fresh {
		return cached
	}

	var out []SmSpool
	if err := s.do(s.rhc, http.MethodGet, "/spool?allow_archived=false", nil, &out); err != nil {
		return cached // stale is better than empty
	}
	s.mu.Lock()
	s.spools, s.spoolsAt = out, time.Now()
	s.mu.Unlock()
	return out
}

// PricePerG returns the price of one gram for a spool: spool.price falls back to
// filament.price, net weight spool.initial_weight to filament.weight -- the API
// does not chain those itself. Anything missing or unreachable yields 0.
func (s *Spoolman) PricePerG(spoolID int) float64 {
	if spoolID == 0 {
		return 0
	}
	for _, sp := range s.cachedSpools() {
		if sp.ID != spoolID {
			continue
		}
		price := deref(sp.Price, deref(sp.Filament.Price, 0))
		net := deref(sp.InitialWeight, deref(sp.Filament.Weight, 0))
		if price <= 0 || net <= 0 {
			return 0
		}
		return price / net
	}
	return 0
}

// RemainingByPreset is the current remaining weight of the spool behind a preset
// name, for rows that were never booked (their booking is 0, so it is the same
// value). Read-only: it never creates anything.
func (s *Spoolman) RemainingByPreset(preset string) (float64, bool) {
	sp := pickSpool(s.cachedSpools(), preset)
	if sp == nil {
		return 0, false
	}
	return RemainingG(sp)
}

// RemainingG computes the remaining weight ourselves. Spoolman's own
// remaining_weight is clamped to >= 0 (api/v1/models.py), so an over-booked
// spool would be invisible there.
func RemainingG(sp *SmSpool) (float64, bool) {
	net := deref(sp.InitialWeight, deref(sp.Filament.Weight, 0))
	if net <= 0 {
		return 0, false
	}
	return net - sp.UsedWeight, true
}

func deref(p *float64, fallback float64) float64 {
	if p == nil {
		return fallback
	}
	return *p
}

// ---------------------------------------------------------------- resolving

// ResolveSpool finds the spool for a slot, creating filament and spool when the
// preset is not in Spoolman yet. Order: preset mapping (extra field) -> filament
// name -> auto-create.
func (s *Spoolman) ResolveSpool(slot SlotUse) (*SmSpool, error) {
	if strings.TrimSpace(slot.Preset) == "" {
		return nil, fmt.Errorf("no preset name in the G-Code -- cannot map to a spool")
	}
	spools, err := s.Spools()
	if err != nil {
		return nil, err
	}

	// 1) mapped via the extra field, newest last_used wins
	if sp := pickByExtra(spools, slot.Preset); sp != nil {
		return sp, nil
	}

	// 2) same filament name -- adopt it and remember the mapping
	if sp := pickByFilamentName(spools, slot.Preset); sp != nil {
		if err := s.stampPreset(sp, slot.Preset); err != nil {
			// Not fatal: we found the spool, only the shortcut is missing.
			log.Printf("Spoolman: could not store the preset mapping on spool %d: %s", sp.ID, err)
		}
		return sp, nil
	}

	// 3) create it
	return s.createSpool(slot)
}

// pickSpool resolves a preset to a spool without any writes (extra field first,
// then filament name).
func pickSpool(spools []SmSpool, preset string) *SmSpool {
	if sp := pickByExtra(spools, preset); sp != nil {
		return sp
	}
	return pickByFilamentName(spools, preset)
}

func pickByExtra(spools []SmSpool, preset string) *SmSpool {
	var hits []SmSpool
	for _, sp := range spools {
		if sp.Archived {
			continue
		}
		if strings.EqualFold(extraString(sp.Extra, presetField), preset) {
			hits = append(hits, sp)
		}
	}
	return newestUsed(hits)
}

func pickByFilamentName(spools []SmSpool, preset string) *SmSpool {
	var hits []SmSpool
	for _, sp := range spools {
		if sp.Archived {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(sp.Filament.Name), strings.TrimSpace(preset)) {
			hits = append(hits, sp)
		}
	}
	return newestUsed(hits)
}

// newestUsed picks the most recently used spool of several candidates.
func newestUsed(hits []SmSpool) *SmSpool {
	if len(hits) == 0 {
		return nil
	}
	sort.SliceStable(hits, func(i, j int) bool {
		return lastUsedTime(hits[i]).After(lastUsedTime(hits[j]))
	})
	sp := hits[0]
	return &sp
}

func lastUsedTime(sp SmSpool) time.Time {
	if sp.LastUsed == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, sp.LastUsed); err == nil {
			return t
		}
	}
	return time.Time{}
}

// extraString decodes one extra value. Spoolman stores every extra value JSON
// encoded, regardless of the field type: a text field comes back as "\"hello\"".
func extraString(extra map[string]string, key string) string {
	raw, ok := extra[key]
	if !ok {
		return ""
	}
	var v string
	if err := json.Unmarshal([]byte(raw), &v); err == nil {
		return v
	}
	return strings.Trim(raw, `"`)
}

// jsonString is the counterpart used when writing an extra value.
func jsonString(v string) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `""`
	}
	return string(b)
}

// ---------------------------------------------------------------- writing

// ensureField registers the custom spool field once. Without it Spoolman rejects
// an unknown key in `extra`.
func (s *Spoolman) ensureField() error {
	s.mu.Lock()
	ready := s.fieldReady
	s.mu.Unlock()
	if ready {
		return nil
	}

	var fields []struct {
		Key string `json:"key"`
	}
	if err := s.do(s.hc, http.MethodGet, "/field/spool", nil, &fields); err != nil {
		return err
	}
	for _, f := range fields {
		if f.Key == presetField {
			s.mu.Lock()
			s.fieldReady = true
			s.mu.Unlock()
			return nil
		}
	}

	body := map[string]any{"name": "sm2uploader Preset", "field_type": "text", "order": 0}
	if err := s.do(s.hc, http.MethodPost, "/field/spool/"+presetField, body, nil); err != nil {
		return err
	}
	log.Printf("Spoolman: custom field %q created", presetField)
	s.mu.Lock()
	s.fieldReady = true
	s.mu.Unlock()
	return nil
}

// stampPreset writes the preset mapping onto an existing spool. Existing extra
// values are merged, not replaced.
func (s *Spoolman) stampPreset(sp *SmSpool, preset string) error {
	if err := s.ensureField(); err != nil {
		return err
	}
	extra := make(map[string]string, len(sp.Extra)+1)
	for k, v := range sp.Extra {
		extra[k] = v
	}
	extra[presetField] = jsonString(preset)

	var updated SmSpool
	if err := s.do(s.hc, http.MethodPatch, fmt.Sprintf("/spool/%d", sp.ID),
		map[string]any{"extra": extra}, &updated); err != nil {
		return err
	}
	*sp = updated
	return nil
}

// ensureVendor returns the vendor ID for a name, creating the vendor if needed.
func (s *Spoolman) ensureVendor(name string) (int, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, nil
	}
	var vendors []SmVendor
	if err := s.do(s.hc, http.MethodGet, "/vendor", nil, &vendors); err != nil {
		return 0, err
	}
	for _, v := range vendors {
		if strings.EqualFold(strings.TrimSpace(v.Name), name) {
			return v.ID, nil
		}
	}
	var created SmVendor
	if err := s.do(s.hc, http.MethodPost, "/vendor", map[string]any{"name": name}, &created); err != nil {
		return 0, err
	}
	log.Printf("Spoolman: vendor %q created (id %d)", name, created.ID)
	return created.ID, nil
}

// autoSpoolNetG is the net weight assumed for a self-created spool. A spool swap
// is maintained by hand in Spoolman (set back to 1000 g).
const autoSpoolNetG = 1000.0

// createSpool creates filament and spool from what the G-Code knows. Density and
// diameter are carried over so Spoolman and Orca compute alike; the tare weight
// stays empty because the G-Code does not know it.
func (s *Spoolman) createSpool(slot SlotUse) (*SmSpool, error) {
	// Dedupe by name first: a filament may exist without any spool.
	var filaments []SmFilament
	if err := s.do(s.hc, http.MethodGet, "/filament?name="+url.QueryEscape(slot.Preset), nil, &filaments); err != nil {
		return nil, err
	}
	filamentID := 0
	for _, f := range filaments {
		if strings.EqualFold(strings.TrimSpace(f.Name), strings.TrimSpace(slot.Preset)) {
			filamentID = f.ID
			break
		}
	}

	if filamentID == 0 {
		vendorID, err := s.ensureVendor(slot.Vendor)
		if err != nil {
			return nil, err
		}
		body := map[string]any{
			"name":     slot.Preset,
			"material": slot.Material,
			"density":  slot.Density,
			"diameter": slot.Diameter,
			"weight":   autoSpoolNetG,
			"comment":  autoComment(),
		}
		if vendorID != 0 {
			body["vendor_id"] = vendorID
		}
		// color_hex wants RRGGBB without the leading '#'.
		if hex := strings.TrimPrefix(slot.Colour, "#"); hex != "" {
			body["color_hex"] = hex
		}
		// filament_cost is the currency per kg; with a 1000 g net weight that is
		// exactly the price of a full spool.
		if slot.CostPerKg > 0 {
			body["price"] = round2(slot.CostPerKg * autoSpoolNetG / 1000)
		}
		var created SmFilament
		if err := s.do(s.hc, http.MethodPost, "/filament", body, &created); err != nil {
			return nil, err
		}
		filamentID = created.ID
		log.Printf("Spoolman: filament %q created (id %d, %.2f g/cm3, %.2f mm)",
			slot.Preset, created.ID, slot.Density, slot.Diameter)
	}

	body := map[string]any{
		"filament_id":    filamentID,
		"initial_weight": autoSpoolNetG,
		"comment":        autoComment(),
	}
	if slot.CostPerKg > 0 {
		body["price"] = round2(slot.CostPerKg * autoSpoolNetG / 1000)
	}
	// The mapping is a bonus: if the custom field cannot be registered, create
	// the spool without it rather than not at all.
	if err := s.ensureField(); err == nil {
		body["extra"] = map[string]string{presetField: jsonString(slot.Preset)}
	} else {
		log.Printf("Spoolman: custom field %q unavailable (%s) -- spool created without the mapping", presetField, err)
	}

	var created SmSpool
	if err := s.do(s.hc, http.MethodPost, "/spool", body, &created); err != nil {
		return nil, err
	}
	log.Printf("Spoolman: spool for %q created (id %d, %.0f g)", slot.Preset, created.ID, autoSpoolNetG)
	s.invalidate()
	return &created, nil
}

func autoComment() string {
	return "created by sm2uploader " + Version
}

// Use books a delta onto a spool. Negative values work (spoolman's
// use_weight_safe adds and clamps at 0) -- that is how corrections and
// cancellations are made. The response is the updated spool, so the new
// remaining weight needs no extra GET.
//
// Caution: the clamp at 0 is silent. Refunding more than was booked reports no
// error, which is exactly why this ledger tracks booked_g itself.
func (s *Spoolman) Use(spoolID, deltaG int) (*SmSpool, error) {
	var out SmSpool
	body := map[string]any{"use_weight": float64(deltaG)}
	if err := s.do(s.hc, http.MethodPut, fmt.Sprintf("/spool/%d/use", spoolID), body, &out); err != nil {
		return nil, err
	}
	s.invalidate()
	return &out, nil
}

// invalidate drops the render cache after a write so the table shows the new
// numbers right away.
func (s *Spoolman) invalidate() {
	s.mu.Lock()
	s.spoolsAt = time.Time{}
	s.mu.Unlock()
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}
