package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/gosuri/uilive"
	"github.com/imroc/req/v3"
)

const (
	HTTPPort    = "8080"
	HTTPTimeout = 5
)

const (
	AuthStatusApproved = 1 + iota
	AuthStatusDenied
	AuthStatusWaiting
)

type HTTPConnector struct {
	client  *req.Client
	printer *Printer
}

func (hc *HTTPConnector) Ping(p *Printer) bool {
	if p.Sacp {
		return false
	}
	if ping(p.IP, HTTPPort, 3) {
		hc.printer = p
		return true
	}
	return false
}

func (hc *HTTPConnector) Connect() error {
	result := struct {
		Token string `json:"token"`
	}{}

	req := hc.request().
		SetResult(&result).
		SetRetryCount(3).
		SetRetryFixedInterval(1 * time.Second).
		SetRetryCondition(func(r *req.Response, err error) bool {
			if Debug {
				log.Printf("-- retrying %s -> %d, token %s", r.Request.URL, r.StatusCode, hc.printer.Token)
			}
			// A 403 here means the printer's single connection is currently held by
			// another client (busy) — NOT that the token is invalid. Do not discard
			// the (valid) token and do not fall back to a fresh touchscreen auth;
			// just fail, so a later retry with the same token succeeds once the
			// connection is free.
			return false
		})

	resp, err := req.Post(hc.URL("/connect"))
	if err != nil {
		return err
	}
	if resp.StatusCode == 200 {
		if hc.printer.Token != result.Token {
			hc.printer.Token = result.Token
		}
		tip := false
		// Never hold the printer's single connection open indefinitely: wait at
		// most 5 minutes for the touchscreen authorization, then release the
		// (pending) session and give up so other clients aren't blocked.
		deadline := time.Now().Add(5 * time.Minute)
		for {
			switch hc.checkStatus() {
			case AuthStatusApproved:
				return nil
			case AuthStatusWaiting:
				if !tip {
					tip = true
					log.Println(">>> Please tap Yes on Snapmaker touchscreen to continue (waiting up to 5 min) <<<")
				}
				if time.Now().After(deadline) {
					hc.Disconnect()
					return fmt.Errorf("timeout waiting for touchscreen authorization (5 min)")
				}
				// wait for auth on HMI
				<-time.After(2 * time.Second)
			case AuthStatusDenied:
				return fmt.Errorf("access denied")
			}
		}
		/*
			} else if resp.StatusCode == 403 && hc.printer.Token != "" {
				// token expired
				hc.printer.Token = ""
				// reconnect with no token to get new one
				return hc.Connect()
		*/
	}

	if resp.StatusCode == 403 {
		// Connection held by another client; keep the token so a retry works once free.
		return fmt.Errorf("printer busy: another client holds the connection")
	}
	return fmt.Errorf("connect error %d", resp.StatusCode)
}

func (hc *HTTPConnector) Disconnect() (err error) {
	if hc.client != nil && hc.printer.Token != "" {
		_, err = hc.request().Post(hc.URL("/disconnect"))
	}
	return
}

func (hc *HTTPConnector) SetToolTemperature(tool int, temperature int) (err error) {
	// *** NOT IMPLEMENTED ***
	err = fmt.Errorf("not implemented")
	return
}

func (hc *HTTPConnector) SetBedTemperature(tool int, temperature int) (err error) {
	// *** NOT IMPLEMENTED ***
	err = fmt.Errorf("not implemented")
	return
}

func (hc *HTTPConnector) Home() (err error) {
	// *** NOT IMPLEMENTED ***
	err = fmt.Errorf("not implemented")
	return
}

func (hc *HTTPConnector) StartPrint() (err error) {
	log.Printf("Starting print job...")
	resp, err := hc.request(120).Post(hc.URL("/start_print"))
	if err != nil {
		return err
	}
	return httpReject("start_print", resp)
}

// httpReject turns a 4xx/5xx printer response into an error. imroc/req only
// sets err on transport failures, so a rejection (printer not ready, or a
// stale/expired session answering 401 "Machine is not connected") otherwise
// passes as success -- which let a print that never started be logged as done
// and its filament booked. The printer's own message becomes the error text.
func httpReject(what string, resp *req.Response) error {
	if resp != nil && resp.IsErrorState() {
		if body := strings.TrimSpace(resp.String()); body != "" {
			return fmt.Errorf("%s rejected: HTTP %d: %s", what, resp.StatusCode, body)
		}
		return fmt.Errorf("%s rejected: HTTP %d", what, resp.StatusCode)
	}
	return nil
}

func (hc *HTTPConnector) Upload(payload *Payload) (err error) {
	log.Printf("Uploading via HTTP protocol")
	finished := make(chan empty, 1)
	defer func() {
		finished <- empty{}
	}()
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		for {
			select {
			case <-ticker.C:
				hc.checkStatus()
			case <-finished:
				if Debug {
					log.Printf("-- heartbeat stopped")
				}
				ticker.Stop()
				return
			}
		}
	}()

	w := uilive.New()
	w.Start()
	log.SetOutput(w)
	defer func() {
		w.Stop()
		log.SetOutput(LogOut)
	}()

	// Materialize the content up front so the multipart FileSize exactly matches
	// the bytes we stream. For G-Code-fixed files the processed size differs from
	// the original; using the original size (as before) produced a Content-Length
	// mismatch that left larger uploads incomplete on the printer (it kept the old
	// job active). Container RAM is ample and typical gcode is tens of MB.
	content, cerr := payload.GetContent(NoFix)
	if cerr != nil {
		log.SetOutput(LogOut)
		log.Printf("G-Code fix error: %s", cerr)
		return cerr
	}
	if !NoFix && payload.ShouldBeFix() {
		log.SetOutput(LogOut)
		log.Printf("G-Code fixed")
		log.SetOutput(w)
	}
	file := req.FileUpload{
		ParamName: "file",
		FileName:  payload.Name,
		GetFileContent: func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(content)), nil
		},
		FileSize: int64(len(content)),
	}
	r := hc.request(0)
	r.SetFileUpload(file)
	r.SetUploadCallbackWithInterval(func(info req.UploadInfo) {
		if info.FileSize > 0 {
			perc := float64(info.UploadedSize) / float64(info.FileSize) * 100.0
			log.Printf("  - HTTP sending %.1f%%", perc)
		} else {
			log.Printf("  - HTTP sending %s...", humanReadableSize(info.UploadedSize))
		}
	}, 35*time.Millisecond)

	// Luban behavior: when printing, send the file via /prepare_print with type=3DP
	// (this loads it as the active print job, including the heating sequence), then
	// call /start_print with the token only. A plain upload just stores the file.
	if payload.Print {
		r.SetFormData(map[string]string{"type": "3DP"})
		resp, perr := r.Post(hc.URL("/prepare_print"))
		if perr != nil {
			err = perr
			return
		}
		if err = httpReject("prepare_print", resp); err != nil {
			return
		}
		// The file is on the printer from here on. Recorded separately from the
		// start so the consumption ledger can tell "uploaded but not started"
		// (row stays open, nothing booked) from a failed upload (no row at all).
		payload.Uploaded = true
		log.SetOutput(LogOut)
		log.Printf("Print job prepared")
		err = hc.StartPrint()
		log.SetOutput(w)
		if err == nil {
			err = hc.verifyLoaded(payload.Name)
		}
		if err == nil {
			payload.Started = true
		}
	} else {
		resp, perr := r.Post(hc.URL("/upload"))
		if perr != nil {
			err = perr
		} else if err = httpReject("upload", resp); err == nil {
			payload.Uploaded = true
		}
	}
	return
}

// verifyLoaded confirms the printer actually loaded/started the just-uploaded
// file. If /status reports a clearly different fileName, the upload wasn't
// applied (e.g. the previous job is still active) and we return an error, so the
// caller reports a real failure instead of a false "Upload finished".
func (hc *HTTPConnector) verifyLoaded(name string) error {
	un := strings.ToLower(name)
	for i := 0; i < 3; i++ {
		time.Sleep(2 * time.Second)
		result := struct {
			FileName string `json:"fileName"`
		}{}
		resp, e := hc.request(8).SetResult(&result).Get(hc.URL("/status"))
		if e != nil || resp.StatusCode != 200 {
			continue
		}
		pn := strings.ToLower(result.FileName)
		if pn == "" {
			continue
		}
		if strings.HasPrefix(un, pn) || strings.HasPrefix(pn, un) {
			return nil // correct file loaded
		}
		return fmt.Errorf("printer is on %q, not the uploaded %q -- upload not applied (printer storage/size?)", result.FileName, name)
	}
	return nil // couldn't verify (busy/unreachable) -- don't block
}

func (hc *HTTPConnector) request(timeout ...int) *req.Request {
	to := HTTPTimeout
	if len(timeout) > 0 {
		to = timeout[0]
	}

	if hc.client == nil {
		hc.client = req.C()
		hc.client.DisableAllowGetMethodPayload()
		if Debug {
			hc.client.EnableDumpAllWithoutRequestBody()
		}
	}

	req := hc.client.SetTimeout(time.Second * time.Duration(to)).R()
	// for GET
	req.SetQueryParam("token", hc.printer.Token)
	// for POST
	req.SetFormData(map[string]string{"token": hc.printer.Token})

	return req
}

func (hc *HTTPConnector) checkStatus() (status int) {
	r, err := hc.request().Get(hc.URL("/status"))
	if Debug {
		log.Printf("-- heartbeat: %d, err(%s)", r.StatusCode, err)
	}
	if err == nil {
		switch r.StatusCode {
		case 200:
			return AuthStatusApproved
		case 204:
			return AuthStatusWaiting
			// case 401:
			// 	return AuthStatusDenied
			// case 403:
			// 	if hc.printer.Token != "" { hc.printer.Token = ""}
			// 	return AuthStatusExpired
		}
	}
	return AuthStatusDenied
}

/*
URL to make url with path
*/
func (hc *HTTPConnector) URL(path string) string {
	return fmt.Sprintf("http://%s:%s/api/v1%s", hc.printer.IP, HTTPPort, path)
}

func init() {
	Connector.RegisterHandler(&HTTPConnector{})
}
