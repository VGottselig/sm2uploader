package main

// The uploads table is not just a view, it is the retention rule: what it renders
// stays, everything else is deleted. That is what the table promises -- a row you
// can still see is a row you can still act on (book, retry, print again), and a
// row that is gone took its G-code copy with it.
//
// Concretely, ApplyRetention keeps:
//   - the newest uploadRowsRendered ledger rows,
//   - their G-code copies (a row without its file could not print again),
//   - every failed upload, because the table always shows all of them and each
//     one still needs its Upload button.
//
// Everything else -- older ledger rows and every file nothing points at -- is
// removed. Bookings themselves are not lost by this: Spoolman keeps the usage,
// only our own history gets shorter.
import "log"

// ApplyRetention prunes ledger and cache down to what uploadsHTML renders. Called
// at startup and after every recorded upload.
func ApplyRetention() {
	if TheLedger == nil {
		return
	}

	items := uploadItems()
	keepRows := make(map[string]bool, len(items))
	keepFiles := make(map[string]bool, len(items))
	for _, it := range items {
		if it.cach != nil {
			keepFiles[it.cach.ID] = true
			continue
		}
		keepRows[it.row.ID] = true
		if it.row.FileID != "" {
			keepFiles[it.row.FileID] = true
		}
	}

	removed, err := TheLedger.Trim(keepRows)
	if err != nil {
		// Without a written ledger the rows are still in memory; deleting their
		// files now would leave rows without a copy, so stop here.
		log.Printf("Retention: ledger not written, no files deleted: %s", err)
		return
	}
	if len(removed) > 0 {
		log.Printf("Retention: %d row(s) left the table and were dropped (newest %d are kept)",
			len(removed), uploadRowsRendered)
	}
	if n, bytes := CachePrune(keepFiles); n > 0 {
		log.Printf("Retention: %d file(s) deleted [%s]", n, humanReadableSize(bytes))
	}
}
