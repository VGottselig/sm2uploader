package main

// cache.go keeps uploads that never reached the printer: offline, printer busy,
// a declined touchscreen authorization, errors in the printer's file store. One
// file pair per entry (<id>.data plus <id>.yaml) instead of an index -- a restart
// loses nothing and the directory stays readable by hand.
//
// Nothing is retried automatically: a failed upload shows up in the uploads
// table (interleaved with the ledger rows by time) and is sent again by hand
// from there.

import (
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

// CacheEntry is the record next to a cached G-code file. Reason is the first
// failure and is never overwritten; Error is the last manual attempt.
type CacheEntry struct {
	ID     string    `yaml:"id"`
	File   string    `yaml:"file"`
	Time   time.Time `yaml:"time"`
	Size   int64     `yaml:"size"`
	Reason string    `yaml:"reason,omitempty"`
	Error  string    `yaml:"error,omitempty"`
}

var (
	cacheMu  sync.Mutex
	cacheMap = map[string]*CacheEntry{}
	cacheSeq int
)

// CacheEnabled reports whether failed uploads are kept at all.
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
		log.Printf("cache entry: %s [%s]", e.File, humanReadableSize(e.Size))
	}
	if len(cacheMap) > 0 {
		log.Printf("Cache: %d upload(s) not on the printer, in %s", len(cacheMap), CacheDir)
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

// CacheLen reports how many failed uploads are waiting.
func CacheLen() int {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	return len(cacheMap)
}

// CacheList returns copies of all entries, newest first.
func CacheList() []CacheEntry {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	out := make([]CacheEntry, 0, len(cacheMap))
	for _, e := range cacheMap {
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

// CacheRemove drops an entry after it reached the printer.
func CacheRemove(id string) {
	cacheMu.Lock()
	delete(cacheMap, id)
	cacheMu.Unlock()
	os.Remove(cacheDataPath(id))
	os.Remove(cacheMetaPath(id))
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

// cacheFromUpload copies the upload into the cache. The reader is rewound first:
// the failed upload already consumed it.
func cacheFromUpload(f io.ReadSeeker, name string) (*CacheEntry, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(CacheDir, 0o755); err != nil {
		return nil, err
	}
	e := &CacheEntry{ID: newCacheID(), File: filepath.Base(name), Time: time.Now()}
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
