[中文说明|Chinese Readme](README.zh-cn.md)

# sm2uploader

A command-line tool for send the gcode file to Snapmaker Printers via WiFi connection.

> **This is a fork** of [macdylan/sm2uploader](https://github.com/macdylan/sm2uploader). On top of
> upstream it adds a web GUI, starting prints straight after upload, a token that survives restarts,
> and filament consumption booked to [Spoolman](https://github.com/Donkie/Spoolman).
> See [Fork additions](#fork-additions). Only two of the changes here are offered upstream
> (PRs #35 and #36); the rest is fork-only.

## Features:
- Auto discover printers (UDP broadcast, same as Snapmaker Luban)
- Upload any type of file does not depend on the head/module limit
- Simulated a OctoPrint server, so that it can be in any slicing software such as Cura/PrusaSlicer/SuperSlicer/OrcaSlicer send gcode to the printer
- Smart pre-heat for switch tools, shutoff nozzles that are no longer in use, and other optimization features for multi-extruders.
- Reinforce the prime tower to avoid it collapse for multi-filament printing
- No need to click Yes button on the touch screen every time for authorization connect
- Support Snapmaker 2 A150/250/350, J1, Artisan
- Support for multiple platforms including win/macOS/Linux/RaspberryPi

## Usage:
Download [sm2uploader](https://github.com/macdylan/sm2uploader/releases)
  - Linux/macOS: `chmod +x sm2uploader`

for Windows:
 - locate to the sm2uploader folder, and double-click `start-octoprint.bat`
 - type a port number for octoprint that you wish to listen
 - when `Server started ...` message appears, the startup was successful, do not close the cmd window, and go to the slicer software to setup a OctoPrint printer
 - use `http://127.0.0.1:(PORT NUM)` as url, click the Test Connect button, all configuration will be finished if successful.

```bash
## Discover mode
$ sm2uploader /path/to/code-file1 /path/to/code-file2
Discovering ...
Use the arrow keys to navigate: ↓ ↑ → ←
? Found 3 machines:
  ▸ A350-3DP@192.168.1.20 - Snapmaker A350
    A250-CNC@192.168.1.18 - Snapmaker A250
    J1V19@192.168.1.19 - Snapmaker-J1
Printer IP: 192.168.1.19
Printer Model: Snapmaker J1
Uploading file 'code-file1' [1.2 MB]...
  - SACP sending 100%
Upload finished.
Uploading file 'code-file2' [1.0 MB]...
  - SACP sending 100%
Upload finished.

## Use printer id
$ sm2uploader -host J1V19 /path/to/code-file1
Discovering ...
Printer IP: 192.168.1.19
Printer Model: Snapmaker J1
Uploading file 'code-file1' [1.2 MB]...
  - SACP sending 100%
Upload finished.

## OctoPrint server (CTRL-C to stop)
$ sm2uploader -octoprint 127.0.0.1:8844 -host A350
Printer IP: 192.168.1.20
Printer Model: Snapmaker 2 Model A350
Starting OctoPrint server on :8844 ...
Server started, now you can upload files to http://127.0.0.1:8844
Request GET /api/version completed in 6.334µs
  - HTTP sending 100.0%
Upload finished: model.gcode [382.2 KB]
Request POST /api/files/local completed in 951.080458ms
```

If UDP Discover can not work, use `sm2uploader -host 192.168.1.20 /file.gcode` to directly upload to printer.

If `host` in `knownhosts`, `-host printer-id` is very convenient.

Get help: `sm2uploader -h`

## Fix the "can not be opened because it is from an unidentified developer"

Solution: https://osxdaily.com/2012/07/27/app-cant-be-opened-because-it-is-from-an-unidentified-developer/

or:
`xattr -d com.apple.quarantine sm2uploader-darwin`

---

# Fork additions

Everything below only exists in this fork. All of it hangs off the OctoPrint server, so it needs
`-octoprint <addr>`; the plain command-line upload paths are untouched.

## Web GUI

Open the OctoPrint address in a browser (e.g. `http://localhost:8844`) and you get more than the
slicer endpoint:

- an **upload form** with an optional *Start print immediately* checkbox
- the **recent log**, colour-coded, refreshing itself every 3 s — no manual reload
- the **upload table** described below, refreshing every 5 s

The page is a single self-contained HTML string with inline CSS, no external assets and no build
step. It has **no authentication**: keep the port on a trusted LAN. Everything that changes state
is a POST, so no link and no browser prefetch can trigger it.

## Start prints after upload

`-start` (or `START=true`, or `print=true` from the slicer, or the GUI checkbox) uploads via
`/prepare_print` and then calls `/start_print` — the same sequence Snapmaker Luban uses, so the
heating sequence runs and the file becomes the active job. Afterwards `/status` is polled to confirm
the printer really loaded that file; if it reports a different one, the upload is reported as failed
instead of a false "Upload finished".

## Token persistence

The printer token is written to `knownhosts` as soon as it is (re)obtained, not only on a clean
shutdown, so it survives an unclean restart and does not force a fresh touchscreen authorization.
A `403` from `/connect` is treated as *printer busy* (another client holds the printer's single
connection) and keeps the valid token instead of discarding it. Waiting for the touchscreen is
capped at 5 minutes so the printer is never blocked indefinitely.

## Filament bookings to Spoolman

Every upload is parsed for its filament consumption and recorded; when a print is actually started,
the consumption is booked to Spoolman right away.

```
Time         | File                             | Filament                 | G-code g | booked g | remaining g | Print time | Cost   | Action
30 Jul 12:53 | ElefantMobile_0.2mm_4h25m.gcode  | ■ DEEPLEE PLA PRO Rosa   |       46 |     [46] |         954 | 4h 24m 54s | 0.63 € | Book · Set · Cancel
```

One row per (upload × filament slot), newest first. The amount appears **twice on purpose**: on the
left the immutable value from the G-code, on the right what Spoolman actually holds.

- **Upload + start** → booked immediately. If the upload succeeds but the start fails, the row stays
  open and nothing is booked.
- **Plain upload** → recorded only. The spool is still created if missing, so the remaining column
  has something to show, but no consumption is written.
- **No consumption block** (Luban, `.nc`, laser/CNC) → a row with the timestamp and file name only,
  not bookable. Nothing is guessed from the E moves.

The booked amount is **editable per row**, and every change is sent to Spoolman as a *delta* against
what that row already booked — booked 120, corrected to 50, sends `use_weight: -70`. `Book` copies
the G-code value over, `Cancel` sets 0. There is no separate cancelled state: a row corrected back
to 0 is indistinguishable from one that was never booked.

A missing filament is **created automatically** — vendor, filament and spool — taking density,
diameter, colour and price per kg from the G-code so Spoolman and the slicer compute alike. Self-created
entries are marked in their comment. The tare weight stays empty because the G-code does not know it,
and the net weight is assumed to be 1000 g; correct both in Spoolman if your spools differ.

The remaining weight is computed as `initial_weight − used_weight` rather than read from Spoolman's
own `remaining_weight`, which is clamped at 0 and would hide an over-booked spool. Below
`SPOOLMAN_LOW_G` it turns orange, below zero red and bold, and a warning is logged before booking —
it never blocks a print.

Cost comes from Spoolman (`spool.price` falling back to `filament.price`, over the matching net
weight) and therefore follows the *booked* amount: correct it down and the cost drops with it.
An unknown price or an unreachable Spoolman shows `0.00`.

**Spoolman is never allowed to break a print.** Booking happens only after a successful upload; a
failure is logged, flags the row and leaves the booked amount alone so a retry sends the same delta.
The response to the slicer is untouched.

### Filament ↔ spool mapping

A slot is matched to a spool by the slicer's preset name (`filament_settings_id`) in this order:

1. a spool whose custom field `sm2_preset` holds that name — most recently used one wins
2. a spool whose filament *name* equals it, which is then stamped with `sm2_preset`
3. otherwise it is created

The custom field is registered automatically on first use. Which spool a preset points to can be
re-pointed at any time by editing `sm2_preset` in Spoolman.

### The ledger

Spoolman keeps no booking history — only `used_weight`, `first_used` and `last_used` — so this fork
keeps its own record in **`uploads.yaml`** (yaml, editable and repairable by hand), next to
`knownhosts` unless `-uploads` says otherwise. Without it neither "80 → 60 = −20" nor a repeatable
cancellation would be possible.

It is written atomically (temp file, fsync, rename) and guarded by a mutex, because an upload
appending a row and a *Book* click changing one can genuinely overlap. Every row is kept forever;
the GUI renders the 10 newest.

> ⚠️ **The ledger is the only record of what was booked.** There is no reconciliation against
> Spoolman, so deleting it orphans past bookings: Spoolman still holds the grams while no row knows
> about them, and booking such a row again would double it. If you must start over, clear the ledger
> **and** the Spoolman entries together.

### Supported G-code

Consumption is read from the comment block written at the end of the file — `filament used [g]`,
`filament_settings_id`, `filament_colour`, `filament_type`, `filament_vendor`, `filament_density`,
`filament_diameter`, `filament_cost` — where index *i* of every array is tool `T{i}`.

Verified against **Snapmaker Orca 2.3.4**. OrcaSlicer and PrusaSlicer emit the same keys, but that
has not been tested here. Only the tail of the file is read (256 KB, widened while required keys are
missing), so a 60 MB G-code is not pulled into memory. Parsing runs on the original upload, before
SMFix. Grams are rounded once, at parse time, and stay whole numbers everywhere after.

Files without that block still get a row, just without filament data.

## Configuration

Every option is a flag with an environment-variable fallback; the flag wins.

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `-octoprint` | `OCTOPRINT` | — | listen address; **required** for the GUI and everything above |
| `-host` | `HOST` | — | printer id / ip / hostname |
| `-knownhosts` | `KNOWN_HOSTS` | next to the binary | printer + token store |
| `-start` | `START` | `false` | start the print after upload |
| `-spoolman` | `SPOOLMAN_URL` | — | Spoolman base url, e.g. `http://spoolman:8000`. **Empty disables bookings** — the table still records |
| `-lowg` | `SPOOLMAN_LOW_G` | `100` | low-stock threshold in grams |
| `-uploads` | `UPLOADS_FILE` | `uploads.yaml` next to `-knownhosts` | ledger file |
| `-nofix` | `NOFIX` | `false` | disable the built-in SMFix |
| `-output` | `OUTPUT_DIR` | — | also write the original and fixed G-code to disk |
| `-timeout` | `TIMEOUT` | `4s` | discovery timeout |
| `-tool1` / `-tool2` / `-bed` | `TOOL1` / `TOOL2` / `BED` | `0` | preheat temperatures |
| `-home` | `HOME` | `false` | home the printer |
| `-debug` | `DEBUG` | `false` | verbose logging |

Timestamps are `Europe/Berlin`; the zone data is embedded, so it works on images without
`/usr/share/zoneinfo`.

### Docker Compose

```yaml
services:
  sm2uploader:
    build: ./sm2uploader
    command: ["sm2uploader", "-octoprint", "0.0.0.0:8844", "-host", "192.168.1.181",
              "-knownhosts", "/data/hosts.yaml"]
    environment:
    - TZ=Europe/Berlin
    - SPOOLMAN_URL=http://spoolman:8000
    - SPOOLMAN_LOW_G=100
    networks: [default, home_net]   # home_net is where spoolman lives
    ports: ["8844:8844"]
    volumes: ["sm2_data:/data"]     # keeps hosts.yaml and uploads.yaml
```

Put the container on the same Docker network as Spoolman, or the hostname will not resolve. The
`/data` volume must be persistent — it holds both the token and the ledger.

## HTTP endpoints

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/` | the web GUI |
| `POST` | `/api/files/local` | slicer upload (OctoPrint API); honours `print=true` |
| `GET` | `/api/version` | OctoPrint handshake for slicers |
| `GET` | `/log` | recent log rows, for the page's own polling |
| `GET` | `/uploads` | the upload table, for the page's own polling |
| `POST` | `/book` | set / book / cancel one row. **POST only**, by design |

## Building and testing

```bash
go test ./...                  # unit tests
go test -race ./...            # the ledger is concurrency-sensitive
make linux-amd64               # or: all, darwin-arm64, windows-amd64, ...
go build -buildvcs=false .     # inside a container without git metadata
```

The parser tests can additionally run against complete real G-code files, which are not part of the
repo:

```bash
SM2_GCODE_SAMPLES=/path/to/gcode-samples go test -run FullReal ./...
```
