package main

// cache.go keeps the G-code the uploads table needs at hand: uploads that never
// reached the printer (offline, printer busy, a declined touchscreen
// authorization, errors in the printer file store) and, for the rows that did
// reach it, a copy so the very same file can be sent again. One file pair per
// entry (<id>.data plus <id>.yaml) instead of an index -- a restart loses nothing
// and the directory stays readable by hand.
//
// Nothing is retried automatically. A failed upload shows up in the uploads table
// with an Upload button, a printed row with a kept copy offers "Print again".
// Both live exactly as long as the table shows them -- see retention.go.

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// CacheDir is set from -cache / CACHE_DIR in main(); empty disables the cache.
var CacheDir string

// Entry kinds. A pending entry never reached the printer. A held entry is the
// copy of a file that did; it is only kept so a visible row can print it again.
const (
	CacheKindPending = "pending"
	CacheKindHeld    = "held"
)

// CacheEntry is the record next to a cached G-code file. Reason is the first
// failure and is never overwritten; Error is the last manual attempt.
type CacheEntry struct {
	ID     string    `yaml:"id"`
	File   string    `yaml:"file"`
	Time   time.Time `yaml:"time"`
	Size   int64     `yaml:"size"`
	Kind   string    `yaml:"kind,omitempty"` // empty = pending (files from before the kinds)
	Reason string    `yaml:"reason,omitempty"`
	Error  string    `yaml:"error,omitempty"`
}

// Held reports whether the file behind the entry already reached the printer.
func (e *CacheEntry) Held() bool { return e.Kind == CacheKindHeld }

var (
	cacheMu  sync.Mutex
	cacheMap = map[string]*CacheEntry{}
	cacheSeq int
)

// CacheEnabled reports whether files are kept at all.
func CacheEnabled() bool { return CacheDir != "" }

func cacheDataPath(id string) string { return filepath.Join(CacheDir, id+".data") }
func cacheMetaPath(id string) string { return filepath.Join(CacheDir, id+".yaml") }

// idSeq returns the trailing number of an entry id ("20261018-162312-6" -> 6).
func idSeq(id string) int {
	i := strings.LastIndex(id, "-")
	if i < 0 {
		return 0
	}
	n, _ := strconv.Atoi(id[i+1:])
	return n
}

// CacheLoad reads the cache directory once at startup.
func CacheLoad() {
	if !CacheEnabled() {
		return
	}
	ents, err := os.ReadDir(CacheDir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("Cache: %s", err)
		}
		return
	}
	cacheMu.Lock()
	defer cacheMu.Unlock()
	for _, de := range ents {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".yaml") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(CacheDir, de.Name()))
		if err != nil {
			log.Printf("Cache: %s", err)
			continue
		}
		var e CacheEntry
		if err := yaml.Unmarshal(raw, &e); err != nil || e.ID == "" || e.File == "" {
			log.Printf("Cache: skipping damaged entry %s", de.Name())
			continue
		}
		cacheMap[e.ID] = &e
		if n := idSeq(e.ID); n > cacheSeq {
			cacheSeq = n
		}
		if e.Held() {
			log.Printf("cache entry: %s [%s] kept for a repeat print", e.File, humanReadableSize(e.Size))
		} else {
			log.Printf("cache entry: %s [%s]", e.File, humanReadableSize(e.Size))
		}
	}
	if len(cacheMap) > 0 {
		log.Printf("Cache: %d file(s) in %s", len(cacheMap), CacheDir)
	}
}

// newCacheID builds an id that sorts by time and stays unique in a second.
func newCacheID() string {
	cacheMu.Lock()
	cacheSeq++
	n := cacheSeq
	cacheMu.Unlock()
	return time.Now().Format("20060102-150405") + "-" + strconv.Itoa(n)
}

// CacheLen reports how many files are kept.
func CacheLen() int {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	return len(cacheMap)
}

// CachePending returns copies of the failed uploads, newest first. Only those
// become table rows of their own; a held copy appears as the button of the row it
// belongs to.
func CachePending() []CacheEntry {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	out := make([]CacheEntry, 0, len(cacheMap))
	for _, e := range cacheMap {
		if e.Held() {
			continue
		}
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out
}

// CacheGet looks one entry up by id.
func CacheGet(id string) (*CacheEntry, bool) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	e, ok := cacheMap[id]
	return e, ok
}

// CacheOpen opens the cached G-code for reading (and seeking, the consumption
// block is read from the tail before the upload).
func CacheOpen(e *CacheEntry) (*os.File, error) {
	return os.Open(cacheDataPath(e.ID))
}

// CacheMarkHeld turns a failed entry into a kept copy once the upload finally
// reached the printer: from now on a row in the table references it and "Print
// again" reads from it.
func CacheMarkHeld(e *CacheEntry) error {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if _, ok := cacheMap[e.ID]; !ok {
		return fmt.Errorf("cache entry %s is gone", e.ID)
	}
	e.Kind = CacheKindHeld
	e.Reason = ""
	e.Error = ""
	return writeCacheMeta(e)
}

// CachePrune deletes every entry that no longer belongs to the table -- the
// second half of the retention rule. Returns the number of entries and their
// total size.
func CachePrune(keep map[string]bool) (int, int64) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	var (
		n     int
		bytes int64
	)
	for id, e := range cacheMap {
		if keep[id] {
			continue
		}
		delete(cacheMap, id)
		os.Remove(cacheDataPath(id))
		os.Remove(cacheMetaPath(id))
		n++
		bytes += e.Size
	}
	return n, bytes
}

// CacheSetReason records why the upload failed in the first place. Separate from
// CacheSetError because a later manual attempt must not overwrite it.
func CacheSetReason(e *CacheEntry, cause error) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if cause != nil {
		e.Reason = cause.Error()
	} else {
		e.Reason = ""
	}
	if err := writeCacheMeta(e); err != nil {
		log.Printf("Cache: could not write %s: %s", cacheMetaPath(e.ID), err)
	}
}

// CacheSetError records the last failed manual attempt.
func CacheSetError(e *CacheEntry, cause error) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if cause != nil {
		e.Error = cause.Error()
	} else {
		e.Error = ""
	}
	if err := writeCacheMeta(e); err != nil {
		log.Printf("Cache: could not write %s: %s", cacheMetaPath(e.ID), err)
	}
}

// writeCacheMeta writes the YAML record atomically: temp file, then rename.
func writeCacheMeta(e *CacheEntry) error {
	raw, err := yaml.Marshal(e)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(CacheDir, e.ID+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, cacheMetaPath(e.ID))
}

// cacheFromUpload copies an upload into the cache. The reader is rewound first:
// the upload already consumed it.
func cacheFromUpload(f io.ReadSeeker, name, kind string) (*CacheEntry, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(CacheDir, 0o755); err != nil {
		return nil, err
	}
	e := &CacheEntry{ID: newCacheID(), File: filepath.Base(name), Time: time.Now(), Kind: kind}
	out, err := os.Create(cacheDataPath(e.ID))
	if err != nil {
		return nil, err
	}
	n, cerr := io.Copy(out, f)
	if err == nil {
		err = cerr
	}
	if err == nil {
		err = out.Sync()
	}
	if err != nil {
		out.Close()
		os.Remove(cacheDataPath(e.ID))
		return nil, err
	}
	if err := out.Close(); err != nil {
		os.Remove(cacheDataPath(e.ID))
		return nil, err
	}
	e.Size = n
	if err := writeCacheMeta(e); err != nil {
		os.Remove(cacheDataPath(e.ID))
		os.Remove(cacheMetaPath(e.ID))
		return nil, err
	}
	cacheMu.Lock()
	cacheMap[e.ID] = e
	cacheMu.Unlock()
	return e, nil
}

// keepPrintedFile copies a successful upload into the cache, so the table can
// offer "Print again" while the row is visible. A failure is logged, never fatal:
// the print is on its way already.
func keepPrintedFile(f io.ReadSeeker, name string) string {
	if !CacheEnabled() {
		return ""
	}
	e, err := cacheFromUpload(f, name, CacheKindHeld)
	if err != nil {
		log.Printf("Keeping '%s' for a repeat print failed: %s", name, err)
		return ""
	}
	return e.ID
}
