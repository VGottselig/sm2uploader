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
| 5 | Fehlendes Filament | **automatisch anlegen** — Filament *und* (bei Bedarf) Rolle, mit 1000 g Nettogewicht und allen aus dem G-Code bekannten Werten. **Hersteller direkt aus `filament_vendor`** (steht seit der Preset-Korrektur vom 30.07.2026 korrekt auf `DEEPLEE`) — keine Heuristik. Leergewicht (Tare) bleibt leer, der G-Code kennt es nicht |
| 6 | Buchungseinheit | **Gramm** (`use_weight`), **ganze Gramm ohne Nachkommastelle** — der G-Code-Wert wird **einmal beim Parsen** gerundet, danach sind Ledger, Anzeige und Buchung durchgängig ganzzahlig (sonst erzeugt „46" bei intern gebuchten 45,9 ein Delta von +0,1). Zeitstempel in `Europe/Berlin`. Dichte/Durchmesser beim Anlegen aus dem G-Code übernehmen, damit Spoolman und Orca gleich rechnen |
| 7 | Spalten | Zeit, Datei, Tool, Filament, **G-Code g (readonly)**, **gebucht g (editierbar)**, Druckzeit, Kosten, Status. Die Menge steht bewusst **zweimal**: links der unveränderliche Wert aus dem G-Code, rechts der bei Spoolman verbuchte. „Buchen" = rechten Wert auf den linken setzen. In der Filament-Spalte ein **Farbkästchen vor dem Namen** (aus `filament_colour`, `background:#RRGGBB`, inline; keine eigene Spalte) |
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
| 19 | Rollen-Preis beim Anlegen | aus dem G-Code: **`filament_cost[i]` ist vorhanden** (verifiziert 30.07.2026, komma-getrennt je Slot, Währung pro **kg**) → `price` = `filament_cost[i]` × Nettogewicht ⁄ 1000, bei 1 kg also direkt der Wert. Fällt der Schlüssel bei anderen Slicern weg: bei **genau einem** benutzten Slot aus `total filament cost` ⁄ `total filament used [g]` × 1000 herleiten, bei mehreren Slots Preis leer lassen |
| 20 | Kosten-Spalte | rechnet aus **Spoolman-Daten**, nicht aus dem G-Code: `gebucht_g × Preis ⁄ Nettogewicht`. Preisquelle in dieser Reihenfolge: `spool.price` → `filament.price` (die API liefert **keinen** Fallback, siehe Abschnitt 3), analog Nettogewicht `spool.initial_weight` → `filament.weight`. Fehlt der Preis → Spalte leer. Währung aus `GET /setting/currency`. Folge: die Kosten folgen dem **gebuchten** Wert — korrigierst du 120 auf 50, sinken sie mit |

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
- **Kein Preis-Fallback in der API**: `api/v1/models.py:354` setzt `price=item.price` für die Rolle,
  `:219` dasselbe für das Filament — zwei getrennte Felder, Spoolman verkettet sie nicht.
  Die Reihenfolge `spool.price` → `filament.price` muss der Client selbst bilden.
- **Währung** kommt aus `GET /setting/currency` (hier `EUR`, `is_set: false`, also Default).
  ⚠️ Der Wert ist **doppelt JSON-kodiert** (`{"value": "\"EUR\""}`) und muss entpackt werden.
  Es gibt dort auch `round_prices` — brauchen wir nicht, gerundet wird selbst.
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
- Orca schreibt den Verbrauchs- und Config-Block **am Dateiende** → nur den Tail lesen (`Seek` vom
  Ende, z. B. 256 KB), danach `Seek(0,0)`. Keine 66-MB-Datei in den RAM ziehen.
  ⚠️ Die Blocklänge ist **nicht konstant** (eigenes Start-/End-G-Code landet im Config-Dump):
  gemessen ~24 KB. Also prüfen, ob alle Pflicht-Schlüssel im Tail gefunden wurden, und sonst
  weiter aufziehen — nie stillschweigend mit halben Daten weiterrechnen.
- ⚠️ **Zwei Syntaxen im selben File**: der `HEADER_BLOCK` oben nutzt `key: wert`, der Config-Dump
  unten `key = wert`. `filament_density` steht in **beiden**. Nur `=` erkennen → der untere Block
  gewinnt, und der hat alles Nötige.
- Arrays sind **index-parallel**, Index `i` = Tool `T{i}`; nur Slots mit Wert > 0 wurden gedruckt.
- `filament_colour[i]` liefert `#RRGGBB` für das Farbkästchen. Weiß (`#FFFFFF`, z. B. „PLA+ Weiß")
  braucht einen dünnen Rahmen, sonst verschwindet der Fleck auf hellem Grund — die GUI ist aber
  dunkel (`#1b1b1d`), daher unkritisch; trotzdem einen 1px-Rahmen setzen.
- **Delimiter sind uneinheitlich**: Zahlen-Arrays per Komma, String-Arrays per **Semikolon**,
  `filament_settings_id` zusätzlich in Anführungszeichen. Details in `GCODE-FILAMENT.md`.

**Ledger (eigene Wahrheit, in `/data`)**

Ohne eigenen Datensatz ist „80 → 60 = −20" und ein idempotenter Storno nicht möglich.
Pro Zeile mindestens: Upload-ID (klammert die Slots eines Auftrags), Zeitstempel, Dateiname,
Dateigröße, Slot-Index/Tool, Preset-Name, Farbe, Typ, Dichte, Durchmesser, `gcode_g`, `gcode_mm`,
`booked_g`, `remaining_after_g` (Restmenge nach dieser Buchung, aus der Antwort des `use`-Aufrufs),
`spool_id`, Status (`offen` | `gebucht` | `fehler`), Buchungszeitpunkt, Fehlertext.

**Format und Schreibweise** (Entscheidung):

- **`/data/uploads.yaml`**, geschrieben mit `gopkg.in/yaml.v3` — schon Abhängigkeit (`localstorage.go`),
  keine neue. Einheitlich zu `hosts.yaml`, im Editor lesbar und reparierbar. Bei „alles behalten"
  wächst die Datei ~300 B/Zeile (1000 Drucke ≈ 300 KB) — komplettes Neuschreiben je Änderung ist
  bei der Größe unkritisch.
- **Atomar schreiben**: in `uploads.yaml.tmp` schreiben, `fsync`, dann `os.Rename` über die Zieldatei.
  ⚠️ Das bestehende `LocalStorage.Save()` (`localstorage.go:74-78`) nutzt `os.WriteFile` — truncate
  dann schreiben, **nicht atomar**; ein Absturz mitten im Schreiben zerstört die Datei. Für ein
  Verbrauchs-Ledger vermeiden. (Optional denselben atomaren Weg auch für `hosts.yaml` nachziehen.)
- **Ein `sync.Mutex`** um Lesen-Ändern-Schreiben. Nötig, weil der OctoPrint-Server auf `http.Serve`
  läuft und **jede Anfrage in einer eigenen Goroutine** bedient: ein Orca-Upload (hängt Zeile an)
  und ein „buchen"-Klick im Browser (ändert Zeile) können echt gleichzeitig auftreten und sich
  sonst gegenseitig überschreiben. (Der bestehende `_stats`-Zähler in `octoprint.go` ist bereits
  ungeschützt — hier nicht wiederholen.)
- **Beim Start** einmal einlesen, im Speicher halten, nach jeder Änderung ganz zurückschreiben.

**Regeln**

- Alle Spoolman-Writes sind **Deltas gegen `booked_g`** — nie absolute Werte aus der Tabelle.
- **Ganze Gramm überall**: einmal beim Parsen runden, danach nur noch Ganzzahlen. Die Restmenge aus
  Spoolman kann gebrochen sein (frühere Buchungen über die Spoolman-Oberfläche) → nur für die
  Anzeige runden, nicht zurückschreiben.
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

## 5. G-Code-Gegencheck vom 30.07.2026 — alles geklärt

Vier echte Dateien aus **Snapmaker Orca 2.3.4**, nach der Preset-Korrektur erzeugt. Gesichert
unter `/home/gad/sm2uploader-build/gcode-samples/` (außerhalb des Repos, ~9 MB):
`Assembly_0.2mm_3h42m`, `ElefantMobile_0.2mm_2h14m`, `ElefantMobile_0.2mm_4h25m`,
`Ivana_0.2mm_36m36s`.

Sie decken zufällig **alle vier Slot-Indizes** ab (0, 3, 1, 2), jeweils genau einen pro Datei —
ein ideales Korpus für die Parser-Tests.

Ergebnis:

- ✅ **`filament_vendor = DEEPLEE`** in allen vier Dateien → Hersteller wird direkt übernommen,
  Entscheidung 5 braucht keine Heuristik. (Frage damit geschlossen.)
- ✅ **`filament_cost = 13.59,13.59,11.99,13.59`** ist vorhanden, komma-getrennt je Slot, Währung
  pro kg → Entscheidung 19 ohne Vorbehalt.
- ✅ **Preise sind gepflegt** (13,59 bzw. 11,99) → die Kosten-Spalte aus Entscheidung 20 hat Daten.
- ✅ Kostenformel gegengerechnet, alle vier Jobs stimmen exakt; Details in `GCODE-FILAMENT.md`.
- ⚠️ Zwei neue Parser-Fallen gefunden (nicht konstante Blocklänge, zwei Syntaxen) — in
  Abschnitt 4 und `GCODE-FILAMENT.md` festgehalten.

**Damit ist keine Entscheidung mehr offen — die Umsetzung kann geplant werden.**
