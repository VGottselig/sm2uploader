package main

import (
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

// Row status values. Deliberately no separate "cancelled" state: a row
// corrected back to 0 g is indistinguishable from one that was never booked.
const (
	StatusOpen    = "open"
	StatusBooked  = "booked"
	StatusFailure = "failed"
)

// UploadRow is one (upload x filament slot) pair -- our own truth. Spoolman has
// no booking history (only used_weight/first_used/last_used), so without this
// ledger neither "80 -> 60 = -20" nor a repeatable cancellation is possible.
type UploadRow struct {
	ID       string    `yaml:"id"`
	UploadID string    `yaml:"upload_id"` // groups the slots of one job
	Time     time.Time `yaml:"time"`
	File     string    `yaml:"file"`
	Size     int64     `yaml:"size"`

	Slot     int     `yaml:"slot"` // tool index, -1 without filament data
	Preset   string  `yaml:"preset,omitempty"`
	Colour   string  `yaml:"colour,omitempty"`
	Material string  `yaml:"material,omitempty"`
	Vendor   string  `yaml:"vendor,omitempty"`
	Density  float64 `yaml:"density,omitempty"`
	Diameter float64 `yaml:"diameter,omitempty"`

	CostPerKg float64 `yaml:"cost_per_kg,omitempty"` // from the G-Code, used when creating a spool
	GcodeG    int     `yaml:"gcode_g"`               // immutable value from the G-Code
	GcodeMM   float64 `yaml:"gcode_mm,omitempty"`
	PrintTime string  `yaml:"print_time,omitempty"`

	BookedG         int        `yaml:"booked_g"`                    // what Spoolman actually holds for this row
	RemainingAfterG *int       `yaml:"remaining_after_g,omitempty"` // frozen with the booking
	SpoolID         int        `yaml:"spool_id,omitempty"`
	Status          string     `yaml:"status"`
	BookedAt        *time.Time `yaml:"booked_at,omitempty"`
	Error           string     `yaml:"error,omitempty"`

	// NoFilament marks Luban/.nc/laser/CNC uploads: timestamp and file name
	// only, no booking (no guessing from E moves).
	NoFilament bool `yaml:"no_filament,omitempty"`
}

// ledgerDoc is the on-disk shape, kept separate from Ledger so the mutex and
// the path never take part in (un)marshalling.
type ledgerDoc struct {
	Rows []*UploadRow `yaml:"rows"`
}

// Ledger is the upload ledger, held in memory and written back completely after
// every change. Every HTTP request runs in its own goroutine (http.Serve), so an
// Orca upload appending a row and a "book" click changing one really can happen
// at the same time -- hence the mutex around read-modify-write.
type Ledger struct {
	mu   sync.Mutex
	path string
	rows []*UploadRow
}

// TheLedger is the process-wide ledger, set up in main().
var TheLedger *Ledger

var uploadSeq atomic.Uint64

// newUploadID returns an ID that groups all slots of one upload.
func newUploadID(t time.Time) string {
	return fmt.Sprintf("%s-%d", t.Format("20060102-150405"), uploadSeq.Add(1))
}

// LoadLedger reads the ledger once at startup. A missing file is a fresh start;
// a broken file is reported and treated as empty rather than aborting the
// uploader (the printer must keep working).
func LoadLedger(path string) *Ledger {
	l := &Ledger{path: path}
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("Ledger %s not readable: %s", path, err)
		}
		return l
	}
	var doc ledgerDoc
	if err := yaml.Unmarshal(b, &doc); err != nil {
		log.Printf("Ledger %s is damaged, starting empty: %s", path, err)
		return l
	}
	l.rows = doc.Rows
	log.Printf("Ledger %s: %d rows", path, len(l.rows))
	return l
}

// Append adds rows and writes the file.
func (l *Ledger) Append(rows ...*UploadRow) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rows = append(l.rows, rows...)
	return l.saveLocked()
}

// Update applies fn to the row with the given ID and writes the file.
func (l *Ledger) Update(id string, fn func(*UploadRow)) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.rows {
		if r.ID == id {
			fn(r)
			return l.saveLocked()
		}
	}
	return fmt.Errorf("row %q not found", id)
}

// Get returns a copy of one row.
func (l *Ledger) Get(id string) (UploadRow, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.rows {
		if r.ID == id {
			return *r, true
		}
	}
	return UploadRow{}, false
}

// Recent returns copies of the n newest rows, newest first. Copies, because the
// caller renders them while other goroutines may be writing.
func (l *Ledger) Recent(n int) []UploadRow {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]UploadRow, 0, n)
	for i := len(l.rows) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, *l.rows[i])
	}
	return out
}

// Len reports how many rows are kept (everything is kept; only the GUI limits
// what it renders).
func (l *Ledger) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.rows)
}

// saveLocked writes the whole ledger atomically: temp file, fsync, rename.
// LocalStorage.Save() (localstorage.go) uses os.WriteFile -- truncate then
// write, which is not atomic. A crash mid-write would destroy a consumption
// ledger, so that pattern is deliberately not reused here.
func (l *Ledger) saveLocked() error {
	if l.path == "" {
		return nil
	}
	b, err := yaml.Marshal(ledgerDoc{Rows: l.rows})
	if err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, l.path)
}
