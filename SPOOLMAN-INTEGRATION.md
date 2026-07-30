# Spoolman-Integration in sm2uploader — Vorabklärung

Input für `/flow-next:plan`. Branch: `feat/spoolman` (Basis `22c305c` = deployter Stand).
Stand: 29.07.2026. G-Code-Format siehe [`GCODE-FILAMENT.md`](GCODE-FILAMENT.md).

---

## 1. Anforderung (vom Anwender)

- Upload-Tabelle in der sm2uploader-GUI (`http://docker:8844`), **neueste oben**.
- Spalten u. a. **Filament im Auftrag** und **Filament verbraucht** — „verbraucht" = was bei
  Spoolman tatsächlich verrechnet wurde.
- „verbraucht" ist **editierbar**; eine Änderung fließt **nach Spoolman zurück**.
  Beispiel: steht 80, Anwender ändert auf 60 → 20 g werden bei Spoolman zurückgebucht.
- Bequeme Möglichkeit, einen **kompletten Auftrag zu buchen oder zu stornieren**.
- Vor dem Buchen prüfen, ob das Filament in Spoolman **hinterlegt** ist — wenn nicht, erst anlegen.
- **Upload + Start** → automatisch verbrauchen. **Upload ohne Start** → nur eintragen.

Spoolman läuft bereits: `ghcr.io/donkie/spoolman:latest`, v0.25.0, Host-Port **7912**,
intern **8000**, SQLite, Container `spoolman` im Netz `dockervolumes_home_net`.

---

## 2. Entschieden

| # | Thema | Entscheidung |
|---|---|---|
| 1 | Repo / Branch | dieses Repo, Branch `feat/spoolman` |
| 2 | Netzwerk | `sm2uploader` kommt ins `home_net`, URL `http://spoolman:8000` via Env `SPOOLMAN_URL` |
| 3 | Tabelle | **eine Zeile pro (Upload × Slot)**, flach, neueste oben |
| 4 | Zuordnung Filament→Rolle | automatisch über den Orca-Preset-Namen, gepflegt als **`extra`-Feld an der Spoolman-Rolle**; bei mehreren Treffern die **zuletzt benutzte, nicht archivierte** Rolle |
| 5 | Fehlendes Filament | **automatisch anlegen** — Filament *und* (bei Bedarf) Rolle, mit 1000 g Nettogewicht und allen aus dem G-Code bekannten Werten |
| 6 | Buchungseinheit | **Gramm** (`use_weight`), eine Dezimalstelle, Zeitstempel in `Europe/Berlin`. Dichte/Durchmesser beim Anlegen aus dem G-Code übernehmen, damit Spoolman und Orca gleich rechnen |
| 7 | Spalten | Zeit, Datei, Tool, Filament, **G-Code g (readonly)**, **gebucht g (editierbar)**, Druckzeit, Kosten, Status. Die Menge steht bewusst **zweimal**: links der unveränderliche Wert aus dem G-Code, rechts der bei Spoolman verbuchte. „Buchen" = rechten Wert auf den linken setzen |
| 8 | Buchungszeitpunkt | **sofort**, sobald Upload *und* Startbefehl erfolgreich durch sind. Schlägt der Start fehl, wird **nicht** gebucht — die Zeile bleibt offen |
| 9 | Korrektur / Storno | **kein eigener Storno-Zustand.** Pro Zeile gibt es einen Wert; jede Änderung schickt die **Differenz zum bereits gebuchten Betrag** an Spoolman (gebucht 120, korrigiert auf 50 → `use_weight: +70`). „Stornieren" = Wert auf 0, „buchen" = Wert auf den G-Code-Wert. Bewusst in Kauf genommen: eine auf 0 korrigierte Zeile ist nicht von einer nie gebuchten unterscheidbar |
| 10 | Doppel-Upload | alle Uploads **gleich** behandeln — keine Wiederholungserkennung, keine Markierung. Reiner Upload bucht nichts |
| 11 | Offene Zeilen | **keine** Erinnerung, kein Zähler. Am Display manuell gestartete Drucke bucht der Anwender über den Button der Zeile |
| 12 | Rollen | genau **eine Rolle pro Sorte** → Zuordnung ist eindeutig, **kein** Rollen-Dropdown in der Tabelle, nur der Gramm-Wert ist editierbar. Rollentausch pflegt der Anwender in Spoolman (auf 1000 g stellen) |
| 13 | Restmengen-Warnung | Bedarf gegen `remaining_weight` prüfen und warnen — **nur bei Upload + Start**, nicht bei reinem Upload. Nie blockieren |
| 14 | Abgebrochene Drucke | rein **manuelle** Korrektur des Werts. Kein Fortschritts-Poll am Drucker |
| 15 | Retention | **alles** in der Ledger-Datei behalten, in der GUI nur die **10** neuesten Zeilen rendern |
| 16 | Zugriffsschutz | `:8844` bleibt **ohne Auth** (LAN-intern, einheitlich zu Spoolman und dem übrigen Stack). Zustandsänderungen ausschließlich per **POST**, damit kein Link und kein Browser-Prefetch bucht |
| 17 | Dateien ohne Verbrauchsblock | Luban, `.nc`, Laser/CNC bekommen eine **Zeile ohne Buchung**: Zeitstempel und Dateiname, Filamentspalten leer, Buchen-Button deaktiviert. Kein Rechnen aus den E-Bewegungen |
| 18 | Restmengen-Spalte | zusätzliche Spalte mit der Restmenge der Rolle **nach dieser Buchung**, im Ledger festgehalten — ändert sich nur, wenn diese Zeile korrigiert wird. Noch **nicht** gebuchte Zeilen zeigen den aktuellen Stand der Rolle (ihre Buchung ist 0, also derselbe Wert); mit dem Buchen friert die Zahl ein. Färbung: unter der Schwelle **orange**, unter 0 **rot und fett**. Schwelle per Env `SPOOLMAN_LOW_G`, Standard **100 g** |

---

## 3. Verifizierte Fakten (Grundlage der Entscheidungen)

**Spoolman-API** (geprüft gegen die laufende Instanz, `/api/v1/openapi.json`):

- Gebucht wird immer auf einen **Spool** (physische Rolle), nie auf ein **Filament** (Typ).
- `PUT /spool/{id}/use` nimmt `use_weight` **oder** `use_length` (nicht beides, sonst 400).
- **Negative Werte funktionieren.** `spoolman/database/spool.py:277` (`use_weight_safe`) rechnet
  `used_weight + weight`, mit `else_=0.0`. → Storno und Korrektur sind negative Deltas.
  ⚠️ Das Clamping bei 0 ist **stumm** — zu viel zurückbuchen erzeugt keinen Fehler.
- `PATCH /spool/{id}` kann `used_weight` **absolut** setzen (`minimum: 0`), Alternative zum Delta.
- Spoolman hat **keine Buchungshistorie** — nur `used_weight`, `first_used`, `last_used`.
  → Wer „welcher Auftrag hat wie viel gebucht" wissen will, muss es selbst führen.
- ⚠️ **`remaining_weight` ist bei Spoolman auf ≥ 0 geklemmt**: `api/v1/models.py:328` rechnet
  `max(initial_weight - used_weight, 0)`, und das Feld ist mit `ge=0` deklariert. Eine überbuchte
  Rolle ist daran also **nicht** erkennbar. → Restmenge selbst aus `initial_weight - used_weight`
  rechnen (Rückfall auf `filament.weight`, wenn die Rolle kein eigenes Nettogewicht hat — dieselbe
  Reihenfolge nutzt Spoolman intern), damit negative Werte sichtbar werden.
- `PUT /spool/{id}/use` **liefert den aktualisierten Spool zurück** → die neue Restmenge lässt sich
  direkt aus der Buchungsantwort rechnen und einfrieren, ohne zusätzlichen `GET`.
- Eigene Felder sind anlegbar: `GET/POST /field/{entity_type}/{key}` (für das Preset-Mapping).
- Weitere relevante Endpunkte: `GET/POST /filament`, `GET/POST /spool`, `GET /vendor`,
  `PUT /spool/{id}/measure`, `GET /external/filament`.

**Infrastruktur:**

- `sm2uploader` hängt in `dockervolumes_default`, `spoolman` in `dockervolumes_home_net` →
  sie sehen sich **aktuell nicht**. Muss vor der ersten Buchung gelöst werden (Entscheidung 2).
- Compose-Command von `sm2uploader` setzt **kein** `-output` → `OutputDir` ist leer, G-Code wird
  **nicht** auf Platte gehalten. Folge: kein Backfill alter Uploads, und der Parser muss im
  Upload-Pfad laufen.
- Persistenz-Ort ist das Named Volume `sm2_data:/data` (übersteht Image-Rebuilds).
  `-knownhosts /data/hosts.yaml` liegt schon dort.
- GUI ist ein einziger Go-String in `octoprint.go:136-153`; das Log aktualisiert sich per
  Polling auf `/log` (`octoprint.go:166`). Die Tabelle gehört in dasselbe Muster
  (inline CSS in `pageCSS`, keine externen Assets).
- Upload-Handler: `octoprint.go:172-262`. Der Multipart-`file` ist ein `ReadSeeker`.

---

## 4. Technische Leitlinien

**G-Code parsen**

- **Vor** SMFix, aus dem hochgeladenen Original (`octoprint.go:186`).
- Orca schreibt den Statistikblock **am Dateiende** → nur den Tail lesen (`Seek` vom Ende,
  z. B. 64 KB), danach `Seek(0,0)`. Keine 66-MB-Datei in den RAM ziehen.
- Arrays sind **index-parallel**, Index `i` = Tool `T{i}`; nur Slots mit Wert > 0 wurden gedruckt.
- **Delimiter sind uneinheitlich**: Zahlen-Arrays per Komma, String-Arrays per **Semikolon**,
  `filament_settings_id` zusätzlich in Anführungszeichen. Details in `GCODE-FILAMENT.md`.

**Ledger (eigene Wahrheit, in `/data`)**

Ohne eigenen Datensatz ist „80 → 60 = −20" und ein idempotenter Storno nicht möglich.
Pro Zeile mindestens: Upload-ID (klammert die Slots eines Auftrags), Zeitstempel, Dateiname,
Dateigröße, Slot-Index/Tool, Preset-Name, Farbe, Typ, Dichte, Durchmesser, `gcode_g`, `gcode_mm`,
`booked_g`, `remaining_after_g` (Restmenge nach dieser Buchung, aus der Antwort des `use`-Aufrufs),
`spool_id`, Status (`offen` | `gebucht` | `fehler`), Buchungszeitpunkt, Fehlertext.

**Regeln**

- Alle Spoolman-Writes sind **Deltas gegen `booked_g`** — nie absolute Werte aus der Tabelle.
- **Spoolman offline darf den Druck nicht blockieren.** Buchung erst nach erfolgreichem Upload;
  Fehler nur loggen, Zeile auf `fehler` + Retry. Die Antwort an Orca bleibt unberührt.
- Nur **erfolgreiche** Uploads erzeugen eine Zeile.
- **Auto-Anlage**: vor dem Anlegen über den Namen deduplizieren; `filament_vendor` aus dem G-Code
  ist der **Preset**-Vendor (steht auf „Snapmaker", nicht auf dem echten Hersteller) — bewusst so
  übernehmen oder leer lassen; Leergewicht (Tare) ist unbekannt → **leer lassen**, nicht raten;
  selbst angelegte Einträge erkennbar markieren.
- **Kein Verbrauchsblock** (Luban, CNC/Laser, `.nc`) → Zeile ohne Filamentdaten, Buchung deaktiviert.
- **Abgebrochene Drucke**: bewusst über die manuelle Korrektur abgedeckt (Stop-Button existiert in
  HA). Kein Fortschritts-Poll.
- **Auth**: `:8844` und `:7912` sind im LAN ohne Schutz. Die neuen Endpunkte ändern Inventar →
  ausschließlich POST, kein Zustandswechsel per GET (CSRF/Prefetch).
- **Retention**: alles behalten, aber nur die 10 neuesten Zeilen rendern.

**Tests** (Repo hat bisher keine): Unit-Tests für den Parser — Komma- vs. Semikolon-Delimiter,
quoted Strings, **Semikolon im Filamentnamen**, 1 vs. 4 Slots, fehlende `filament used`-Zeilen,
Konsistenz-Check `used[g] = used[cm3] × density`.

**Deploy-Kette** (unverändert, plus zwei Ergänzungen)

1. Build im `golang:1.24`-Container mit `go build -buildvcs=false ./...`
   (ohne das Flag: „error obtaining VCS status: exit status 128").
2. Binary nach `/home/gad/dockervolumes/sm2uploader/sm2uploader-patched`.
3. **Neu:** im Compose `networks: [home_net]` und `SPOOLMAN_URL=http://spoolman:8000`.
4. `docker compose build sm2uploader && docker compose up -d sm2uploader`.

**Upstream:** Das ist ein **Fork-Feature**. Nicht für einen PR an `macdylan/sm2uploader`
vorgesehen und getrennt von den sauberen PR-Branches `fix-fixed-gcode-filesize` (#35) und
`fix-persist-token-on-connect` (#36) halten.

---

## 5. Offene Punkte

### 🔴 Zurückgestellt — blockiert die Umsetzung

**Hersteller-Feld beim automatischen Anlegen.** Der G-Code liefert in `filament_vendor` den
*Preset*-Hersteller („Snapmaker"), nicht den echten (DEEPLEE steckt nur im Preset-Namen).
Bevor das entschieden wird, prüft der Anwender die **Orca-Filament-Presets** und bearbeitet die
Definitionen gegebenenfalls; danach wird ein neuer G-Code gegengeprüft.

**Bis dahin nicht implementieren.**

Beim Prüfen in Orca relevant, weil diese Werte in Spoolman übernommen werden:

- `filament_vendor` — steht er im Preset richtig, entfällt jede Heuristik
- `filament_settings_id` (Preset-Name) — Schlüssel für die Zuordnung zur Spoolman-Rolle,
  muss eindeutig und stabil sein
- `filament_density` und `filament_diameter` — stimmen sie nicht, rechnen Orca und Spoolman
  auseinander

### Sonst alles geklärt

Die Punkte 1–17 in Abschnitt 2 sind entschieden. Offen ist **nur** das Hersteller-Feld oben —
sobald das geklärt ist, kann geplant und implementiert werden.
