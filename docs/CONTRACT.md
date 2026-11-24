# Contratto HTTP `ouf.vertical-mock.v1`

Implementa `ouf.vertical-source.v1` e le garanzie G1-G9 di `ouf.delta-position.v1`. Le estensioni della busta sono compatibili con i consumatori della fonte v1.

## 1. Ruolo e perimetro
Il mock è un servizio HTTP **generico**. Genera a cicli le istanze di un dominio dichiarato in un file (§5) e le espone come un verticale reale.

Ha tre superfici:
- **lettura**, per il consumatore ouf (l'Ingestion, attraverso il Gateway);
- **amministrazione**, per il banco di prova delle linee V4 e V5. ouf non la usa mai;
- **sonde**, per la piattaforma.

Nel repository e nell'immagine non ci sono dati reali, indirizzi, domini o account (D-058). Gli esempi usano dati fittizi e segnaposto.

## 2. Autenticazione
- Ci sono due bearer distinti, letti da file all'avvio: uno di **lettura** e uno di **amministrazione**. Il confronto avviene a tempo costante. Due file con lo stesso valore impediscono l'avvio.
- Le risorse di lettura accettano solo il bearer di lettura, quelle di amministrazione solo il bearer di amministrazione.
- Senza credenziale valida la risposta è `401 {"code":"UNAUTHENTICATED"}`, con `WWW-Authenticate: Bearer`, senza `schemaVersion` e senza altri dati.
- Le sonde (§7) sono anonime.

## 3. Regole comuni delle risposte
- JSON UTF-8. Le date sono RFC 3339 in UTC con `Z`, con la frazione di secondo solo se serve.
- **Versione di schema in ogni risposta autenticata (D-076)**, compresi gli errori `400`, `404`, `409`, `410`, `429` e `503`:
  - nell'intestazione `X-Schema-Version`;
  - nel corpo, nel campo `schemaVersion`.
- **Quale versione:**
  - per le risorse di una collezione, la versione **corrente** della collezione al momento della risposta;
  - per il catalogo e per i comandi globali, il **digest del catalogo**: SHA-256 del JSON canonico dell'elenco `[[collection, schemaVersion], …]` ordinato per `collection`.
- **Errori:** `{"code","message","collection"?,"schemaVersion"}`. `message` è un testo fisso in inglese, senza dati.
- **JSON canonico:** RFC 8785 (JCS). Vale per tutte le impronte di questo contratto. Il vettore di controllo del task-04 resta valido (§4.2).

## 4. Risorse di lettura

### 4.1 `GET /collections`
```json
{"schemaVersion": "<digest del catalogo>",
 "collections": [{
   "collection": "edicole", "title": "Edicole (dati fittizi)",
   "schemaVersion": "<64 esadecimali>", "fieldsSha256": "<64 esadecimali>",
   "deltaModes": ["CURSOR", "WATERMARK", "SNAPSHOT"], "defaultMode": "CURSOR",
   "deletions": "EXPLICIT",
   "maxLateness": "PT10M", "retention": "P7D", "retainedFrom": "2026-11-17T22:00:00Z",
   "cycle": "PT1M"}]}
```
- `deletions`:
  - `EXPLICIT`: le cancellazioni sono eventi `DELETE` nelle modalità a delta;
  - `SNAPSHOT_ABSENCE`: nessun `DELETE` nelle modalità a delta; un oggetto cancellato sparisce solo dagli snapshot. In questo caso `SNAPSHOT` deve essere fra le `deltaModes`.
- `maxLateness`: il ritardo massimo dichiarato fra `changedAt` e la visibilità di un evento (G6).
- `retainedFrom`: il limite della compattazione. Una posizione è completa solo se non è anteriore a questo istante (G7).

### 4.2 `GET /collections/{c}/schema`
```json
{"collection": "edicole", "schemaVersion": "…", "fieldsSha256": "…",
 "fields": [{"name": "cap", "type": "string", "nullable": false, "description": "Codice di avviamento postale"}],
 "key": ["id"],
 "geometry": {"field": "geometria", "type": "Point", "crs": "EPSG:4326"}}
```
- **`type`** è un tipo base del vocabolario `ouf.value-types.v1` ammesso dal contratto della fonte: `string`, `integer`, `decimal`, `boolean`, `date`, `datetime` oppure `geometry`. `geometry` è un GeoJSON.
- **`fieldsSha256`** è lo SHA-256, in 64 cifre esadecimali minuscole, del JSON canonico dell'array `fields`:
  - l'array è ordinato per `name`;
  - ogni elemento ha esattamente le quattro chiavi `description`, `name`, `nullable` e `type`.

  Vettore di controllo: `[{"description":"Caffè","name":"id","nullable":false,"type":"string"}]` → `9d6a480a969b79b83dc6398f197b32c3c9dffa10e8cdd82dd34fa250fa632610`.
- **`schemaVersion`** è lo SHA-256 del JSON canonico di `[fields ordinati, key, geometry]`. È opaco e deterministico:
  - lo stesso schema ha sempre la stessa versione, anche dopo un ritorno a uno schema precedente;
  - qualsiasi differenza di nome, tipo, nullabilità, descrizione, chiave o geometria cambia la versione.
- **Chiave:** in v1 è sempre `["id"]`, una stringa non vuota e stabile per oggetto.

### 4.3 `GET /collections/{c}/items`
**Parametri:**
- `mode`: facoltativo, altrimenti vale `defaultMode`. Una modalità che la collezione non offre dà `400 MODE_NOT_OFFERED`;
- `limit`: da 1 a 1000, predefinito 100;
- `CURSOR`: `cursor`, un token opaco. Senza `cursor` la lettura parte dall'inizio della storia conservata. Per il primo caricamento vedi G9 e `ouf.delta-position.v1` §5;
- `WATERMARK`: la prima pagina usa `changedSince`, una data RFC 3339, e `afterId`, facoltativo. Le pagine successive usano `cursor`;
- `SNAPSHOT`: `page`, un token opaco.

Parametri incoerenti con la modalità danno `400 INVALID_POSITION`.

**Busta:**
```json
{"collection": "edicole", "schemaVersion": "…", "fieldsSha256": "…", "mode": "CURSOR",
 "items": [
   {"id": "E-000042", "revision": 7, "op": "UPSERT", "changedAt": "2026-11-24T22:15:02Z", "properties": {"id": "E-000042", "…": "…"}},
   {"id": "E-000017", "revision": 4, "op": "DELETE", "changedAt": "2026-11-24T22:15:03Z"}],
 "next": "<opaco>|null", "hasMore": false,
 "snapshotComplete": null, "snapshotId": null, "resume": null,
 "positionExpiresAt": "2026-12-01T22:15:02Z|null"}
```
- **Rappresentazione:**
  - ogni item della risposta è reso con lo schema di `schemaVersion` della busta, anche nella rilettura di una posizione vecchia. Una risposta corrisponde a uno schema;
  - `UPSERT` porta tutti i campi dello schema, compreso `id`. `null` compare solo nei campi `nullable`;
  - `DELETE` non porta `properties`.
- **Ordine nella risposta:** `changedAt`, poi `id`, poi `revision`, poi la sequenza di arrivo.
- **`CURSOR`:**
  - la posizione individua un intervallo della **sequenza di arrivo**;
  - il **primo uso** di una posizione ne fissa in modo durevole il limite superiore. Le riletture dello stesso token restituiscono quindi gli stessi eventi, nello stesso ordine e con le stesse pagine, finché la posizione è conservata (G4);
  - sull'ultima pagina `hasMore=false` e `next` è la posizione da cui parte il poll successivo. Non è mai `null`.
- **`WATERMARK`:**
  - restituisce gli eventi visibili con la coppia (`changedAt`, `id`) strettamente maggiore di (`changedSince`, `afterId`);
  - senza `afterId` comprende tutti gli `id` all'istante indicato;
  - sull'ultima pagina `next=null`. Il consumatore costruisce il poll successivo da solo, con il ritardo di sicurezza (`ouf.delta-position.v1` §4.2).
- **`SNAPSHOT`:**
  - è lo stato degli oggetti vivi in un punto della sequenza di arrivo, fissato dalla prima pagina. Gli item sono `UPSERT` con l'ultima revisione;
  - tutte le pagine hanno lo stesso `snapshotId`. Solo l'ultima ha `snapshotComplete=true`, le altre `false`;
  - l'ultima pagina porta `resume`, con un campo per ogni modalità a delta offerta. Esempio: `{"cursor":"…","watermark":{"changedSince":"…"},"snapshotAt":"…"}` (G9). Il punto dello snapshot è l'istante `snapshotAt` del mock in cui la prima pagina ha fissato il limite della sequenza;
    - **`cursor`** riparte **esattamente** dal punto dello snapshot: contiene tutti e soli gli arrivi successivi;
    - **`watermark`** è una ripresa **sicura, non esatta**: `changedSince = snapshotAt − maxLateness`, senza `afterId`. Per G6, ogni evento non ancora visibile al punto dello snapshot ha `changedAt` successivo a quell'istante. La risposta può ripetere eventi già coperti dallo snapshot. Il consumatore li scarta per chiave dell'evento (`ouf.delta-position.v1` §4.2 e §7). Con una soglia temporale una ripresa esatta non è possibile: un evento in ritardo può avere `changedAt` anteriore a eventi già visibili.
- **`positionExpiresAt`:**
  - è l'istante oltre il quale `next`, o `resume`, non sarà più completa: la data di registrazione del primo evento non ancora coperto, più la retention;
  - vale `null` se non ci sono eventi dopo la posizione. In quel caso la posizione resta valida finché non arriva un evento.

**Errori tipizzati:**

| Stato | `code` | Quando |
|---|---|---|
| 400 | `INVALID_LIMIT`, `INVALID_MODE`, `MODE_NOT_OFFERED`, `INVALID_POSITION` | Parametri non validi o token alterato |
| 404 | `UNKNOWN_COLLECTION` | Collezione assente. `schemaVersion` è il digest del catalogo |
| 410 | `CURSOR_EXPIRED` | La posizione è anteriore a `retainedFrom`, oppure appartiene a un'epoca precedente a un `reset` |
| 429 | `RATE_LIMITED` | Limite di letture o guasto simulato; c'è sempre `Retry-After` in secondi interi |
| 503 | `SOURCE_UNAVAILABLE` | Guasto simulato; `Retry-After` se noto |

## 5. Dominio configurabile
Il dominio sta in un file JSON (`OUF_MOCK_DOMAIN_FILE`), con schema `ouf.vertical-mock-domain.v1`. Il repository fornisce `examples/domain-lab.json` con due collezioni fittizie:
- `edicole`: punto, indirizzo, CAP, località, tipologia, servizi, telefono dimostrativo, data di apertura;
- `cantieri`: poligono, intervento, data di inizio e data di fine.

```json
{"schema": "ouf.vertical-mock-domain.v1",
 "collections": [{
   "collection": "edicole", "title": "Edicole (dati fittizi)", "idPrefix": "E-",
   "delta": {"modes": ["CURSOR", "WATERMARK", "SNAPSHOT"], "defaultMode": "CURSOR", "deletions": "EXPLICIT"},
   "generation": {"count": 5, "create": 50, "update": 40, "delete": 10, "duplicate": 0, "late": 0, "maxLateness": "PT10M"},
   "geometry": {"field": "geometria", "type": "Point", "crs": "EPSG:4326", "bbox": [12.0, 45.0, 12.09, 45.09]},
   "fields": [
     {"name": "id", "type": "string", "nullable": false, "description": "Identificativo naturale", "generator": {"kind": "id"}},
     {"name": "indirizzo", "type": "string", "nullable": false, "description": "Via e numero civico",
      "generator": {"kind": "template", "pattern": "{choice:Via delle Lanterne|Via dei Gelsi} {int:1..120}", "varies": "OBJECT"}},
     {"name": "geometria", "type": "geometry", "nullable": false, "description": "Posizione", "generator": {"kind": "point"}}]}]}
```

**Regole del file:**
- `collection` segue `^[a-z][a-z0-9_-]{0,62}$`. I nomi dei campi seguono `^[A-Za-z_][A-Za-z0-9_]{0,63}$`. `id` è obbligatorio e usa il generatore `id`.
- `delta.modes` non è vuoto e `defaultMode` vi appartiene. `SNAPSHOT_ABSENCE` richiede `SNAPSHOT`.
- `generation`:
  - `create`, `update` e `delete` sono pesi percentuali interi con somma 100;
  - `duplicate` e `late` sono percentuali da 0 a 100 delle operazioni del ciclo;
  - `maxLateness` non supera `OUF_MOCK_MAX_LATENESS`.
- **Generatori**, elenco chiuso:
  - `id`;
  - `choice` con `values`;
  - `template` con segnaposto `{choice:a|b}` e `{int:min..max}`;
  - `integer` con `min` e `max`;
  - `decimal` con `min`, `max` e `scale`;
  - `boolean`;
  - `date` con `from` e `to`, oppure `after` (un altro campo data) con `minDays` e `maxDays`;
  - `datetime` con `source` uguale a `CHANGED_AT` oppure `RANGE`;
  - `point` e `polygon`, entro `geometry.bbox`.

  `varies` vale `OBJECT` (stabile per oggetto) oppure `REVISION` (cambia a ogni revisione). `nullPercent` è ammesso solo sui campi `nullable`.
- **Valori deterministici:** ogni valore deriva da seme, collezione, `id`, campo d'origine e, con `REVISION`, dalla revisione. Lo stesso seme con gli stessi comandi ripete gli stessi oggetti.
- **Errori:** un file non valido impedisce l'avvio con un messaggio che indica campo e regola.
- **Rapporto con il journal:** il file **inizializza** un journal nuovo. Dopo, l'autorità è il journal. Se il file cambia:
  - `/admin/status` lo segnala con due impronte diverse;
  - per applicarlo serve `POST /admin/reset {"reloadDomain": true}`.

## 6. Amministrazione (solo banco di prova)
Tutte le risposte portano `schemaVersion` (§3). Ogni comando che produce eventi restituisce anche:
- le righe del registro atteso (§6.2) generate dal comando;
- `nextSince`.

I comandi sono atomici. Un lotto che non si può eseguire per intero fallisce con `409 TARGET_NOT_AVAILABLE` e non scrive nulla.

### 6.1 Comandi
| Comando | Corpo | Effetto |
|---|---|---|
| `POST /admin/rate` | `{"collection"?, "count"?, "create"?, "update"?, "delete"?, "duplicate"?, "late"?, "maxLateness"?, "cycle"?}` | Modifica parziale della generazione di una collezione, o di tutte. `cycle` è globale, da `PT1S` in su |
| `POST /admin/inject` | `{"collection", "kind", "id"?, "count"?, "lateness"?}` | `kind` vale:<br>• `create`, `update` o `delete`;<br>• `duplicate`: ripubblica un evento già visibile, con stessi `id`, `revision`, `changedAt` e contenuto;<br>• `late`: nuova revisione con `changedAt` = adesso, resa visibile dopo `lateness`, da `PT1S` a `maxLateness` |
| `POST /admin/schema` | `{"collection", "action", "name"?, "newName"?, "type"?, "nullable"?, "description"?, "generator"?, "version"?}` | `action` vale `add`, `remove`, `rename`, `type`, `nullable`, `describe` oppure `revert`, che richiede una `version` già vista nella collezione. Vale **subito**, per le risposte e per gli eventi nuovi. Restituisce `previousSchemaVersion`, `schemaVersion` e `fieldsSha256` |
| `POST /admin/outage` | `{"collection"?, "kind": "UNAVAILABLE\|RATE_LIMITED", "duration", "retryAfter"?}` | Guasto simulato sulle sole risorse di lettura. `duration` uguale a `PT0S` chiude subito. La generazione continua |
| `POST /admin/clock` | `{"advance": "PT6H"}` | Sposta in avanti l'orologio del mock, mai indietro. Serve a provare retention e ritardi senza attese. Disponibile solo con `OUF_MOCK_CLOCK_CONTROL=enabled`, altrimenti `404` |
| `POST /admin/reset` | `{"reloadDomain"?: false}` | Nuova epoca: storia vuota, schemi iniziali, posizioni precedenti scadute (`410`) |
| `GET /admin/status` | — | Configurazione effettiva, generazione per collezione, eventi in attesa di visibilità, guasto, testa e limite della sequenza, `retainedFrom`, scarto dell'orologio, impronte del file di dominio e del journal |

**Semantica del ritardo** (`late`, sia generato sia iniettato):
- l'evento nasce con `changedAt` = adesso e con la revisione successiva dell'oggetto, e resta **in attesa**;
- diventa visibile, cioè riceve la sua sequenza di arrivo, quando scade il ritardo. La pubblicazione avviene al più tardi al primo fra:
  - il ciclo successivo;
  - la prima richiesta autenticata successiva;
  - un `clock`.
- intanto l'oggetto può ricevere altre revisioni, visibili subito. Il consumatore vede allora `r+1` prima di `r`: è il caso «fuori ordine».

  Anche una cancellazione può precedere un aggiornamento in ritardo: il caso «tombstone prima dell'aggiornamento tardivo».

### 6.2 Registro atteso (V5)
**`GET /admin/expected?since=<seq>&limit=<1..5000>&collection=<c>?&renderAs=<schemaVersion>?`** elenca, in ordine di arrivo, gli eventi **visibili** con sequenza maggiore di `since`:
```json
{"schemaVersion": "…", "rows": [{
   "sequence": 123, "recordedAt": "…", "collection": "edicole",
   "origin": "GENERATED|INJECTED", "kind": "CREATE|UPDATE|DELETE|DUPLICATE|LATE_UPDATE",
   "lateness": "PT3M|null", "deleteVisibility": "EXPLICIT|SNAPSHOT_ONLY|null",
   "emittedSchemaVersion": "…", "renderedSchemaVersion": "…",
   "item": {"id": "…", "revision": 7, "op": "UPSERT", "changedAt": "…", "properties": {}},
   "contentSha256": "…"}],
 "nextSince": 123, "hasMore": false}
```
- **`renderAs`:**
  - rende ogni item con uno schema già visto nella collezione, per esempio quello approvato in ouf;
  - senza `renderAs` vale lo schema in vigore all'emissione;
  - una versione sconosciuta dà `400 UNKNOWN_SCHEMA_VERSION`.
- **`contentSha256`** è lo SHA-256 del JSON canonico dell'oggetto `item` così come è reso. Il consumatore ricalcola la stessa impronta sugli item ricevuti, con i valori ricevuti e senza riformattarli. Così confronta il registro con lineage, handoff e ACK (`ouf.delta-position.v1` §10).
- Gli eventi in attesa non compaiono nel registro e si contano in `/admin/status`.

**`GET /admin/expected/state?collection=<c>&renderAs=?&after=<id>?&limit=?`** dà lo stato atteso alla testa della sequenza:
- oggetti vivi: `id`, `revision`, `changedAt`, `properties`, `contentSha256`;
- oggetti cancellati ancora conservati: `id`, ultima `revision`, `deletedAt`.

Serve a confrontare lo stato corrente in UDP.

## 7. Sonde e processo
- `GET /health/live` restituisce `204`.
- `GET /health/ready` restituisce `204` quando il journal è caricato e scrivibile, altrimenti `503`.
- Le sonde hanno corpo vuoto, sono senza intestazione di schema e non risentono dei guasti simulati: la readiness descrive il processo, non la fonte simulata.
- **Arresto:** su `SIGTERM` l'arresto è ordinato. Il servizio chiude le richieste in corso, sincronizza il journal ed esce con codice 0 entro 10 secondi.
- **Scrittore unico:** un lock esclusivo nella directory dati garantisce un solo processo per directory. Una seconda istanza termina con un errore esplicito.
- **Cancello d'avvio:** il processo può non essere il PID 1. L'immagine permette il cancello d'avvio della pipeline dei moduli (`/bin/sh -c '<gate>' ouf-gated-application <comando>`, `ouf-deploy/tools/module_release/contract.py`).

## 8. Configurazione (D-056)
| Variabile | Predefinito | Note |
|---|---|---|
| `OUF_MOCK_PORT` | `8080` | Porta d'ascolto nel container |
| `OUF_MOCK_DATA_DIR` | obbligatoria | Directory del journal e del lock |
| `OUF_MOCK_DOMAIN_FILE` | obbligatoria | File di dominio (§5). L'immagine contiene l'esempio in `/usr/share/ouf-vertical-mock/examples/domain-lab.json` |
| `OUF_MOCK_READ_TOKEN_FILE`, `OUF_MOCK_ADMIN_TOKEN_FILE` | obbligatorie | File distinti, in sola lettura |
| `OUF_MOCK_SEED` | `1` | Intero a 64 bit |
| `OUF_MOCK_CYCLE` | `PT1M` | Un ciclo al minuto (D-093, punto 11) |
| `OUF_MOCK_RETENTION` | `P7D` | Storia rigiocabile (D-093, punto 11) |
| `OUF_MOCK_MAX_LATENESS` | `PT10M` | Ritardo massimo ammesso; minore della retention |
| `OUF_MOCK_READ_MIN_INTERVAL` | `PT0S` | Intervallo minimo fra letture di una collezione; sotto la soglia risponde `429` |
| `OUF_MOCK_CLOCK_CONTROL` | `disabled` | `enabled` abilita `/admin/clock`, solo in prova |

Le durate sono ISO 8601. Un valore non valido impedisce l'avvio.

## 9. Immagine e artefatto governato
- **Immagine `ouf-vertical-mock`:**
  - runtime `alpine:3.22`, con `/bin/sh` e `sleep` per il cancello;
  - utente `10008:10008`, distinto da quelli degli altri moduli;
  - binario `/usr/local/bin/ouf-vertical-mock`, comando `serve`, `EXPOSE 8080`, `STOPSIGNAL SIGTERM`;
  - nessun `VOLUME`: i mount li dichiara l'installazione.
- **CI `module-ci.yml`**, su `push`, `pull_request` e `workflow_dispatch`, con le convenzioni dei moduli:
  - etichette `org.opencontainers.image.revision` e `org.opencontainers.image.source`;
  - scansione Trivy;
  - prova del cancello;
  - nelle PR, l'artefatto `vertical-mock-governed-image-<runId>` con `image.tar.gz`, `manifest.json` (`ouf.vertical-mock-tested-image.v1`) e `SHA256SUMS`, conservato 14 giorni;
  - i campi del manifest seguono quelli di `ouf.udp-tested-image.v1`, senza le migrazioni Flyway e con `contract: "ouf.vertical-mock.v1"` e `journalFormat: "ouf.vertical-mock-journal.v1"`. `targetApplyAuthorized`, `humanBudgetGranted` e `releaseAcceptance` sono sempre `false`.

## 10. Registro della configurazione (D-065)
| Archivio | Contenuto | Segreti | Cattura e ripristino |
|---|---|---|---|
| Directory dati (`OUF_MOCK_DATA_DIR`) | Journal degli eventi e degli schemi, epoca e chiave HMAC delle posizioni | Sì: la chiave delle posizioni, che non va pubblicata | Dati di prova rigenerabili. Si possono non catturare; il ripristino è un `reset` (invalida le posizioni dei consumatori: `POSITION_LOST` voluto) |
| File dei token | Bearer di lettura e di amministrazione | Sì | Custodia dell'installazione; rotazione con riavvio. Il consumatore ouf riceve il bearer di lettura solo per riferimento (`credentialRef`) |
| File di dominio | Collezioni, campi, generatori | No | Versionato nella configurazione dell'installazione |

## 11. Versioni
- Il contratto si chiama `ouf.vertical-mock.v1`. Le aggiunte compatibili restano v1.
- Il journal ha formato `ouf.vertical-mock-journal.v1`. Una versione successiva del mock legge i journal v1, oppure rifiuta l'avvio con un errore esplicito (analogo della D-080).
