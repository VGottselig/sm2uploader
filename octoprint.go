package main

import (
	"bytes"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const (
	maxMemory = 64 << 20 // 64MB
)

const pageCSS = `
body{background:#1b1b1d;color:#e0e0e0;font:14px/1.5 system-ui,"Segoe UI",Roboto,sans-serif;margin:0;padding:18px;max-width:920px}
h1{font-size:19px;margin:0 0 2px}h1 .ver{color:#6b7280;font-size:13px;font-weight:400}
h2{font-size:13px;text-transform:uppercase;letter-spacing:.05em;color:#9ca3af;margin:22px 0 8px}
h2 .hint{text-transform:none;letter-spacing:0;font-weight:400;color:#6b7280}
.meta{color:#8b8b8b;font-size:13px;margin-bottom:16px}.meta b{color:#cbd5e1}
.upload{background:#26262a;border:1px solid #383840;border-radius:10px;padding:16px;display:flex;flex-wrap:wrap;gap:14px;align-items:center}
.upload input[type=file]{color:#ddd;font-size:13px;min-width:0;flex:1 1 200px}
.upload label{display:flex;align-items:center;gap:7px;color:#cbd5e1}
.upload button{margin-left:auto;background:#3b82f6;color:#fff;border:0;border-radius:7px;padding:9px 20px;font-size:14px;font-weight:500;cursor:pointer}
.upload button:hover{background:#2563eb}
.banner{padding:10px 14px;border-radius:7px;margin:14px 0;font-size:14px}
.banner-ok{background:#123524;color:#86efac;border:1px solid #166534}
.banner-err{background:#3b1414;color:#fca5a5;border:1px solid #991b1b}
.stats{background:#232327;border-radius:8px;padding:12px 14px;font:12px/1.5 ui-monospace,Menlo,Consolas,monospace;color:#b8b8b8;white-space:pre-wrap;overflow-x:auto;margin:0}
.log{background:#141416;border-radius:8px;padding:12px 14px;font:12px/1.55 ui-monospace,Menlo,Consolas,monospace;overflow:auto;max-height:60vh}
.log span{display:block;white-space:pre-wrap;word-break:break-word}
.log-err{color:#f87171}.log-ok{color:#4ade80}.log-info{color:#c3c7cf}
.actions{margin:14px 0}
.dl{display:inline-block;background:#374151;color:#e5e7eb;text-decoration:none;border-radius:7px;padding:9px 16px;font-size:14px;border:1px solid #4b5563}
.dl:hover{background:#4b5563}
`

var (
	fixShutoff        = true
	fixPreheat        = true
	// fixReinforceTower = true
	fixReplaceTool    = true

	// userAgent: OrcaSlicer/01.09.03.50
	// userAgent: BBL-Slicer/v01.09.03.50 (dark) Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko)
	// userAgent: PrusaSlicer/2.6.0+arm64 (3.10.2-202402201133)
	// userAgent: PrusaSlicer/2.8.0+MacOS-arm64
	reUserAgent = regexp.MustCompile(`^(\w+)/(\S+)(?:[+-].*)?$`)
)

type stats struct {
	start       time.Time
	memory      uint64
	success     uint
	failure     uint
	lastSuccess *last
	lastFailure *last
}

type last struct {
	filaname string
	size     int64
	time     time.Time
}

func (s *stats) addSuccess(filaname string, size int64) {
	s.success++
	s.lastSuccess = &last{
		filaname: normalizedFilename(filaname),
		size:     size,
		time:     time.Now(),
	}
}

func (s *stats) addFailure(filaname string, size int64) {
	s.failure++
	s.lastFailure = &last{
		filaname: normalizedFilename(filaname),
		size:     size,
		time:     time.Now(),
	}
}

func (s *stats) StatsText() string {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	s.memory = mem.Alloc

	buf := bytes.Buffer{}
	buf.WriteString("memory alloc: " + humanReadableSize(int64(s.memory)) + "\n")
	buf.WriteString("uptime: " + time.Since(s.start).String() + "\n")
	buf.WriteString(fmt.Sprintf("success: %d, failure: %d\n", s.success, s.failure))
	buf.WriteString(fmt.Sprintf("last success: %s\n - %s (%s)\n", s.lastSuccess.time.Format(time.RFC3339), s.lastSuccess.filaname, humanReadableSize(s.lastSuccess.size)))
	buf.WriteString(fmt.Sprintf("last failure: %s\n - %s (%s)\n", s.lastFailure.time.Format(time.RFC3339), s.lastFailure.filaname, humanReadableSize(s.lastFailure.size)))
	return buf.String()
}

func (s *stats) String() string {
	return s.StatsText() + "\n###LOG### (newest first)\n" + LogRing.String()
}

func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		defer func() {
			log.Printf("Request %s %s completed in %v", r.Method, r.URL.Path, time.Since(start))
		}()
		next.ServeHTTP(w, r)
	})
}

func startOctoPrintServer(listenAddr string, printer *Printer) error {
	var (
		_stats *stats
		mux    = http.NewServeMux()
	)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		protocol := "HTTP"
		if printer.Sacp {
			protocol = "SACP"
		} else if printer.Moonraker {
			protocol = "Moonraker"
		}
		banner := ""
		if ok := r.URL.Query().Get("ok"); ok != "" {
			banner = `<div class="banner banner-ok">Hochgeladen: ` + html.EscapeString(ok) + `</div>`
		} else if e := r.URL.Query().Get("err"); e != "" {
			banner = `<div class="banner banner-err">Fehler: ` + html.EscapeString(e) + `</div>`
		}
		page := `<!doctype html><html lang="de"><head><meta charset="utf-8">` +
			`<meta name="viewport" content="width=device-width,initial-scale=1">` +
			`<title>sm2uploader</title><style>` + pageCSS + `</style></head><body>` +
			`<h1>sm2uploader <span class="ver">` + html.EscapeString(Version) + `</span></h1>` +
			`<div class="meta">printer <b>` + html.EscapeString(printer.ID) + `</b> @ ` +
			html.EscapeString(printer.IP) + ` · ` + protocol + `</div>` +
			banner +
			`<form class="upload" method="POST" action="/api/files/local" enctype="multipart/form-data">` +
			`<input type="hidden" name="gui" value="1">` +
			`<input type="file" name="file" accept=".gcode,.gco,.g,.nc" required>` +
			`<label><input type="checkbox" name="print" value="true"> Druck sofort starten</label>` +
			`<button type="submit">Hochladen</button>` +
			`</form>` +
			`<div class="actions"><a class="dl" href="/download">&#8595; Aktuelle gcode herunterladen</a></div>` +
			`<h2>Status</h2><pre class="stats">` + html.EscapeString(_stats.StatsText()) + `</pre>` +
			`<h2>Log <span class="hint">(neueste oben)</span></h2>` +
			`<div class="log">` + LogRing.HTML() + `</div>` +
			`</body></html>`
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		writeResponse(w, http.StatusOK, page)
	})

	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		respVersion := `{"api": "0.1", "server": "1.2.3", "text": "OctoPrint 1.2.3/Dummy"}`
		writeResponse(w, http.StatusOK, respVersion)
	})

	// /download streams the gcode of the file currently loaded on the printer
	// (via the printer's /api/v1/print_file) back to the browser as a download.
	mux.HandleFunc("/download", func(w http.ResponseWriter, r *http.Request) {
		data, filename, err := Connector.DownloadCurrent(printer)
		if err != nil {
			http.Redirect(w, r, "/?err="+url.QueryEscape("Download fehlgeschlagen: "+err.Error()), http.StatusSeeOther)
			return
		}
		filename = strings.NewReplacer(`"`, "", "\r", "", "\n", "", "/", "_", `\`, "_").Replace(filename)
		if filename == "" {
			filename = "print.gcode"
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
		w.WriteHeader(http.StatusOK)
		w.Write(data)
	})

	mux.HandleFunc("/api/files/local", func(w http.ResponseWriter, r *http.Request) {
		// Check if request is a POST request
		if r.Method != http.MethodPost {
			methodNotAllowedResponse(w, r.Method)
			return
		}

		err := r.ParseMultipartForm(maxMemory)
		if err != nil {
			internalServerErrorResponse(w, err.Error())
			return
		}

		// Retrieve the uploaded file
		file, fd, err := r.FormFile("file")
		if err != nil {
			bedRequestResponse(w, err.Error())
			return
		}
		defer file.Close()

		// read X-Api-Key header
		apiKey := r.Header.Get("X-Api-Key")
		apiKey = testUserAgent(r.Header.Get("User-Agent"), apiKey)
		if len(apiKey) > 5 {
			argumentsFromApi(apiKey)
		}

		// Send the stream to the printer
		payload := NewPayload(file, fd.Filename, fd.Size)

		// Moonraker/Klipper devices don't need G-Code fix
		moonrakerNoFix := printer.Moonraker
		effectiveNoFix := NoFix || moonrakerNoFix
		if moonrakerNoFix && !NoFix {
			log.Printf("Moonraker device detected, skipping G-Code fix for '%s'", payload.Name)
		}

		// If output directory is specified and the file needs fixing,
		// pre-process it and save both original and fixed files to disk.
		if OutputDir != "" && payload.ShouldBeFix() && !effectiveNoFix {
			origContent, readErr := io.ReadAll(file)
			if readErr != nil {
				log.Printf("Warning: failed to read '%s' for output: %s", payload.Name, readErr)
			} else {
				fixedContent, procErr := postProcess(bytes.NewReader(origContent))
				if procErr != nil {
					log.Printf("Warning: failed to post-process '%s' for output: %s", payload.Name, procErr)
				} else {
					fixedPath, saveErr := saveToOutputDir(payload.Name, bytes.NewReader(origContent), fixedContent, true)
					if saveErr != nil {
						log.Printf("Warning: failed to save to output dir: %s", saveErr)
					} else if fixedPath != "" {
						payload.FixedFile = fixedPath
						payload.Size = int64(len(fixedContent))
						log.Printf("Saved: original -> %s/%s, fixed -> %s/%s_fixed%s",
							OutputDir, payload.Name, OutputDir, payload.Name[:len(payload.Name)-len(filepath.Ext(payload.Name))], filepath.Ext(payload.Name))
					}
				}
			}
		} else if OutputDir != "" {
			log.Printf("Skipping output save for '%s' (shouldFix=%v, nofix=%v)",
				payload.Name, payload.ShouldBeFix(), effectiveNoFix)
		}

		startRequested := StartAfterUpload || r.FormValue("print") == "true"
		if startRequested {
			log.Printf("Print start requested (print=%q)", r.FormValue("print"))
		}
		gui := r.FormValue("gui") == "1"
		if err := Connector.Upload(printer, payload, startRequested); err != nil {
			_stats.addFailure(payload.Name, payload.Size)
			if gui {
				http.Redirect(w, r, "/?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
				return
			}
			internalServerErrorResponse(w, err.Error())
			return
		}

		_stats.addSuccess(payload.Name, payload.Size)

		log.Printf("Upload finished: %s [%s]", fd.Filename, payload.ReadableSize())

		if gui {
			http.Redirect(w, r, "/?ok="+url.QueryEscape(fd.Filename), http.StatusSeeOther)
			return
		}
		// Return success response
		writeResponse(w, http.StatusOK, `{"done": true}`)
	})

	handler := LoggingMiddleware(mux)
	log.Printf("Starting OctoPrint server on %s ...", listenAddr)

	// Create a listener
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return err
	}

	_stats = &stats{
		start:   time.Now(),
		success: 0,
		failure: 0,
		lastSuccess: &last{
			filaname: "",
			size:     0,
			time:     time.Now(),
		},
		lastFailure: &last{
			filaname: "",
			size:     0,
			time:     time.Now(),
		},
	}

	log.Printf("Server started, now you can upload files to http://%s", listener.Addr().String())
	// Start the server
	return http.Serve(listener, handler)
}

func writeResponse(w http.ResponseWriter, status int, body string) {
	if has := w.Header().Get("Content-Type"); has == "" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	w.WriteHeader(status)
	w.Write([]byte(body))
}

func methodNotAllowedResponse(w http.ResponseWriter, method string) {
	log.Print("Method not allowed: ", method)
	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

func internalServerErrorResponse(w http.ResponseWriter, err string) {
	log.Print("Internal server error: ", err)
	http.Error(w, err, http.StatusInternalServerError)
}

func bedRequestResponse(w http.ResponseWriter, err string) {
	log.Print("Bad request: ", err)
	http.Error(w, err, http.StatusBadRequest)
}

func argumentsFromApi(str string) {
	if strings.TrimSpace(str) == "" {
		return
	}
	if strings.Contains(str, "nofix") {
		NoFix = true
		log.Printf("SMFix disabled via API key (nofix)")
		return
	}
	fixPreheat = !strings.Contains(str, "nopreheat")
	fixShutoff = !strings.Contains(str, "noshutoff")
	// fixReinforceTower = !strings.Contains(str, "noreinforcetower")
	fixReplaceTool = !strings.Contains(str, "noreplacetool")

	msg := []string{}
	if fixPreheat {
		msg = append(msg, "-preheat")
	} else {
		msg = append(msg, "-nopreheat")
	}
	if fixShutoff {
		msg = append(msg, "-shutoff")
	} else {
		msg = append(msg, "-noshutoff")
	}
	// if fixReinforceTower {
	// 	msg = append(msg, "-reinforcetower")
	// } else {
	// 	msg = append(msg, "-noreinforcetower")
	// }
	if fixReplaceTool {
		msg = append(msg, "-replacetool")
	} else {
		msg = append(msg, "-noreplacetool")
	}
	if len(msg) > 0 {
		log.Printf("SMFix with args: %s", strings.Join(msg, " "))
	}
}

func testUserAgent(userAgent, apiKey string) string {
	matches := reUserAgent.FindStringSubmatch(userAgent)
	if len(matches) >= 2 {
		slicerName := matches[1]
		slicerVersion := matches[2]
		if (slicerName == "PrusaSlicer" && slicerVersion >= "2.8.0") || (slicerName == "OrcaSlicer" && slicerVersion >= "2.1.1") {
			if !strings.Contains(apiKey, "nopreheat") && strings.Contains(apiKey, "preheat") {
				apiKey = strings.Replace(apiKey, "preheat", "nopreheat", -1)
			} else {
				apiKey += ";nopreheat;"
			}
		// if !strings.Contains(apiKey, "noreinforcetower") && strings.Contains(apiKey, "reinforceTower") {
		// 	apiKey = strings.Replace(apiKey, "reinforceTower", "noreinforcetower", -1)
		// } else {
		// 	apiKey += ";noreinforcetower;"
		// }
		}
	}
	return apiKey
}
