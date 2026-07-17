package main

import (
	"html"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// logBufferSize is the number of recent log lines kept for the status page.
const logBufferSize = 80

type logEntry struct {
	t   time.Time
	msg string
}

// logRing is a fixed-size ring buffer of recent log lines.
type logRing struct {
	mu      sync.Mutex
	entries []logEntry
	max     int
}

func (lr *logRing) add(msg string) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	lr.entries = append(lr.entries, logEntry{t: time.Now(), msg: msg})
	if len(lr.entries) > lr.max {
		lr.entries = lr.entries[len(lr.entries)-lr.max:]
	}
}

// String renders the buffered lines newest-first as "timestamp, message".
func (lr *logRing) String() string {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	var b strings.Builder
	for i := len(lr.entries) - 1; i >= 0; i-- {
		e := lr.entries[i]
		b.WriteString(e.t.Format("2006-01-02 15:04:05"))
		b.WriteString(", ")
		b.WriteString(e.msg)
		b.WriteString("\n")
	}
	return b.String()
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// logClass classifies a log message for status-page coloring.
func logClass(msg string) string {
	low := strings.ToLower(msg)
	switch {
	case containsAny(low, "error", "fail", "fehler", "panic", "timeout", "refused",
		"busy", "denied", "unable", "cannot", "not connected", "not allowed", "invalid"):
		return "log-err"
	case containsAny(low, "upload finished", "prepared", "starting print",
		"print start requested", "success", "completed", "done", "server started"):
		return "log-ok"
	}
	return "log-info"
}

// HTML renders the buffered lines newest-first as colored <span> rows for the
// status page (failures red, successes green).
func (lr *logRing) HTML() string {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	var b strings.Builder
	for i := len(lr.entries) - 1; i >= 0; i-- {
		e := lr.entries[i]
		b.WriteString(`<span class="` + logClass(e.msg) + `">`)
		b.WriteString(html.EscapeString(e.t.Format("2006-01-02 15:04:05")))
		b.WriteString(", ")
		b.WriteString(html.EscapeString(e.msg))
		b.WriteString("</span>\n")
	}
	return b.String()
}

// LogRing holds the recent log lines shown on the status page.
var LogRing = &logRing{max: logBufferSize}

// logWriter tees log output to an underlying writer (stderr) and captures each
// meaningful line into LogRing. High-frequency noise (upload progress and the
// HTTP access log) is skipped so the buffer stays useful.
type logWriter struct{ under io.Writer }

func (lw logWriter) Write(p []byte) (n int, err error) {
	for _, line := range strings.Split(string(p), "\n") {
		s := strings.TrimSpace(line)
		if s == "" {
			continue
		}
		// strip the std log prefix "2006/01/02 15:04:05 " if present
		if len(s) >= 20 && s[4] == '/' && s[7] == '/' && s[10] == ' ' {
			s = s[20:]
		}
		if strings.Contains(s, "HTTP sending") || strings.HasPrefix(s, "Request ") {
			continue
		}
		LogRing.add(s)
	}
	return lw.under.Write(p)
}

// LogOut is the log destination: stderr plus the ring buffer. Used everywhere
// instead of os.Stderr so lines emitted during uploads are captured too.
var LogOut io.Writer = logWriter{under: os.Stderr}

func init() {
	log.SetOutput(LogOut)
}
