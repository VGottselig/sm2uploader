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
| 6 | Buchungseinheit | **Gramm**; Dichte/Durchmesser beim Anlegen aus dem G-Code übernehmen, damit Spoolman und Orca gleich rechnen |
| 7 | Zusatzspalten | Druckzeit und Kosten aus dem G-Code-Block mitanzeigen |

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
`booked_g`, `spool_id`, Status (`offen` | `gebucht` | `fehler`), Buchungszeitpunkt, Fehlertext.

**Regeln**

- Alle Spoolman-Writes sind **Deltas gegen `booked_g`** — nie absolute Werte aus der Tabelle.
- **Spoolman offline darf den Druck nicht blockieren.** Buchung erst nach erfolgreichem Upload;
  Fehler nur loggen, Zeile auf `fehler` + Retry. Die Antwort an Orca bleibt unberührt.
- Nur **erfolgreiche** Uploads erzeugen eine Zeile.
- **Doppel-Upload warnen**: gleicher Dateiname + gleiche Größe kurz nacheinander (typisch bei
  Retries nach `connection reset by peer`) → Hinweis „evtl. Wiederholung, schon gebucht?"
  statt stiller Zweitbuchung.
- **Auto-Anlage**: vor dem Anlegen über den Namen deduplizieren; `filament_vendor` aus dem G-Code
  ist der **Preset**-Vendor (steht auf „Snapmaker", nicht auf dem echten Hersteller) — bewusst so
  übernehmen oder leer lassen; Leergewicht (Tare) ist unbekannt → **leer lassen**, nicht raten;
  selbst angelegte Einträge erkennbar markieren.
- **Kein Verbrauchsblock** (Luban, CNC/Laser, `.nc`) → Zeile ohne Filamentdaten, Buchung deaktiviert.
- **Abgebrochene Drucke**: bewusst über die manuelle Korrektur abgedeckt (Stop-Button existiert in
  HA). Fortschritt automatisch vorbelegen wäre eine spätere Ausbaustufe.
- **„offen" muss sichtbar sein** — ein am Druckerdisplay später gestarteter Job wird sonst nie
  gebucht. Zähler/Badge in der GUI.
- **Auth**: `:8844` und `:7912` sind im LAN ohne Schutz. Die neuen Endpunkte ändern Inventar →
  ausschließlich POST, kein Zustandswechsel per GET (CSRF/Prefetch).
- **Retention**: alles behalten, aber nur die letzten ~100 Zeilen rendern.

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
