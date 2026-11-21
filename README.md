# ouf-vertical-mock

Sorgente di laboratorio per il contratto `ouf.vertical-source.v1`. Genera edicole con geometria Point e cantieri con geometria Polygon, in EPSG:4326. Gli indirizzi, i recapiti e le attività sono fittizi. Il mock permette di provare il profilo di estrazione, lo scheduler, il delta di acquisizione e il blocco per cambio di schema (D-075, D-076, D-077).

## Avvio locale

Servono Go 1.27.4 e una directory dati scrivibile. Questo esempio per shell POSIX crea due file temporanei con credenziali fittizie. La prima generazione avviene dopo un intervallo. Con lo stesso seme, gli stessi comandi e lo stesso numero di cicli si ripetono ID, scelte e attributi sintetici. I tempi degli eventi seguono l'orologio della prova.

```sh
lab=$(mktemp -d)
umask 077
printf '%s\n' 'lettura-fittizia' > "$lab/read.token"
printf '%s\n' 'admin-fittizio' > "$lab/admin.token"
export OUF_READ_TOKEN_FILE="$lab/read.token" OUF_ADMIN_TOKEN_FILE="$lab/admin.token"
export OUF_PORT=8080 OUF_DATA_DIR="$lab/data" OUF_INTERVAL=5s
go test ./...
CGO_ENABLED=0 go build -trimpath -o "$lab/vertical" .
"$lab/vertical"
```

Per il container, `docker build -t ouf-vertical-mock:local .` crea un'immagine finale `scratch` con il solo binario statico e una directory `/data`. Montare un volume persistente scrivibile dall'UID/GID 65532, montare i due token in sola lettura e impostare le quattro variabili obbligatorie. La porta esposta all'host si sceglie al lancio. Il journal richiede un solo processo scrittore per directory dati.

## Variabili

| Variabile | Predefinito | Significato |
|---|---|---|
| `OUF_PORT` | obbligatoria | Porta TCP da 1 a 65535 |
| `OUF_DATA_DIR` | obbligatoria | Directory del journal |
| `OUF_READ_TOKEN_FILE` | obbligatoria | File con il bearer di lettura |
| `OUF_ADMIN_TOKEN_FILE` | obbligatoria | File con un bearer amministrativo distinto |
| `OUF_RETENTION` | `P7D` | Durata ISO 8601 positiva con giorni, ore, minuti o secondi interi, ad esempio `P1DT12H` |
| `OUF_COLLECTIONS` | `edicole,cantieri` | Una o entrambe le collezioni, separate da virgole |
| `OUF_SEED` | `1` | Seme intero a 64 bit con segno |
| `OUF_INTERVAL` | `60s` | Durata Go tra cicli, minimo `1ms` |
| `OUF_COUNT` | `5` | Operazioni per collezione per ciclo, da 0 a 10000 |
| `OUF_CREATE`, `OUF_UPDATE`, `OUF_DELETE` | `50`, `40`, `10` | Pesi percentuali interi, somma 100. Una collezione vuota parte con una creazione |
| `OUF_READ_DELAY` | `0s` | Attesa minima fra letture riuscite di `items` della stessa collezione. Zero disattiva il limite |

I token si caricano dai file all'avvio. Seme, collezioni, cadenza e proporzioni inizializzano un nuovo journal. A un riavvio si recuperano quelli registrati insieme allo stato e all'eventuale scadenza di un outage. Un reset riapplica la configurazione d'avvio. La retention si rilegge a ogni avvio.

## API di lettura

In un secondo terminale, con la porta scelta sopra, si possono usare i seguenti comandi. Gli esempi contengono solo i bearer fittizi del laboratorio.

```sh
base=http://localhost:8080
read_api() { curl -sS -H 'Authorization: Bearer lettura-fittizia' "$@"; }
admin_api() { curl -sS -H 'Authorization: Bearer admin-fittizio' -H 'Content-Type: application/json' "$@"; }
read_api "$base/collections"
read_api "$base/collections/edicole/schema"
read_api "$base/collections/edicole/items?limit=2"
read_api "$base/collections/cantieri/items?mode=SNAPSHOT&limit=2"
curl -sS -o /dev/null -w '%{http_code}\n' "$base/health/ready"
```

Ogni risposta riporta `X-Schema-Version`. Il JSON include anche `schemaVersion`. Per le risorse di collezione vale la versione corrente, per catalogo e comandi globali vale un digest delle versioni per collezione. Le sonde anonime `GET /health/live` e `GET /health/ready` hanno corpo vuoto. Un outage porta la readiness a 503 mentre la liveness resta a 200.

| Modalità | Come proseguire | Significato della fine |
|---|---|---|
| `CURSOR` (default) | Usare `?cursor=<next>&limit=n` | Anche con `hasMore=false`, conservare `next` per il poll successivo |
| `WATERMARK` | Partire con `?mode=WATERMARK&changedSince=<datetime>&afterId=<id>`. Durante la paginazione usare `?mode=WATERMARK&cursor=<next>&limit=n` | Sull'ultima pagina `next=null`. Per il poll successivo salvare la coppia finale `changedAt,id` confermata |
| `SNAPSHOT` | Usare `?mode=SNAPSHOT&page=<next>&limit=n` | Solo l'ultima pagina ha `snapshotComplete=true` e autorizza il confronto delle assenze |

`limit` va da 1 a 1000 (default 100). Usare il token opaco di continuazione anche in WATERMARK per distinguere eventi con la stessa coppia tempo/ID al confine di pagina. Il filtro manuale seleziona le coppie strettamente successive a `(changedSince,afterId)`. Senza `afterId` include tutti gli ID all'istante indicato. Per valorizzare `changedSince` con un offset positivo, codificare il segno più, per esempio tramite `curl -G --data-urlencode`.

Ogni lettura fissa il limite superiore della sequenza di arrivo. All'interno della lettura l'ordine è `changedAt`, `id`, `revision`, poi sequenza di arrivo per duplicati identici. Il poll CURSOR successivo apre una nuova finestra. La prima finestra aperta da un checkpoint viene registrata e può essere riletta dopo un riavvio. Un ritardo registrato dopo la chiusura compare nel poll successivo anche se la sua data precede il watermark. Con WATERMARK, per cercare questi ritardi occorre rileggere una finestra temporale sovrapposta e deduplicare.

Gli UPSERT includono tutte le proprietà, compreso `id`. Un DELETE porta solo `id`, `revision`, `op` e `changedAt`. Una risposta 429 indica i secondi di attesa in `Retry-After`; una 503 segnala l'indisponibilità della fonte. Entrambe consentono il ritentativo dalla posizione confermata.

## Impronta dei campi

1. Ordinare l'array completo `fields` per `name` crescente. I nomi ammessi sono identificatori ASCII.
2. Per ciascun oggetto emettere tutte e quattro le chiavi in ordine lessicografico: `description`, `name`, `nullable`, `type`.
3. Serializzare come JSON compatto UTF-8, senza BOM, spazi o newline finali. Conservare le stringhe Unicode senza normalizzazione, senza escape HTML e senza convertire i caratteri non ASCII in sequenze `\u`. Usare gli escape JSON canonici per virgolette, backslash e caratteri di controllo. `nullable` è un booleano JSON.
4. Calcolare SHA-256 su quei byte ed emettere 64 cifre esadecimali minuscole. Il valore coincide con `fieldsFingerprint` di `/schema`. La versione opaca comprende anche chiave e geometria.

Vettore di controllo (la riga JSON termina alla parentesi quadra):

```json
[{"description":"Caffè","name":"id","nullable":false,"type":"string"}]
```

SHA-256 atteso: `9d6a480a969b79b83dc6398f197b32c3c9dffa10e8cdd82dd34fa250fa632610`.

## Comandi di prova

Inviare POST con il token amministrativo. Ogni risposta include `schemaVersion`, gli eventi prodotti in `expected` e la posizione globale `nextSince`. Un comando di controllo può produrre un registro vuoto. I comandi restano accessibili durante un outage.

```sh
admin_api -d '{"count":6,"interval":"2s","create":50,"update":30,"delete":20,"readDelay":"0s"}' "$base/admin/rate"
admin_api -d '{"collection":"edicole","kind":"duplicate"}' "$base/admin/inject"
admin_api -d '{"collection":"edicole","kind":"late","minutes":15}' "$base/admin/inject"
admin_api -d '{"collection":"cantieri","kind":"delete"}' "$base/admin/inject"
admin_api -d '{"collection":"edicole","action":"add","name":"accessibile","type":"boolean","description":"Accesso senza gradini","nullable":false}' "$base/admin/schema"
admin_api -d '{"collection":"edicole","action":"rename","name":"accessibile","newName":"accesso"}' "$base/admin/schema"
admin_api -d '{"collection":"edicole","action":"type","name":"accesso","type":"string"}' "$base/admin/schema"
admin_api -d '{"collection":"edicole","action":"remove","name":"accesso"}' "$base/admin/schema"
admin_api -d '{"collection":"edicole","action":"revert","version":"VERSIONE_SALVATA"}' "$base/admin/schema"
admin_api -d '{"duration":"30s"}' "$base/admin/outage"
admin_api "$base/admin/expected?since=0"
```

Per `revert` sostituire il segnaposto con una versione già osservata nella stessa collezione. Il journal conserva le definizioni precedenti. Le pagine, anche storiche, espongono le proprietà secondo lo schema corrente. Il registro atteso ricostruisce invece ogni evento secondo lo schema in vigore all'emissione. Una rinomina conserva l'origine del valore e un cambio di tipo genera un valore del tipo richiesto.

`rate` accetta modifiche parziali. `inject` accetta `id` opzionale per scegliere un oggetto vivo e `count` per ripetere l'operazione (default 1). Un lotto senza bersagli sufficienti fallisce interamente con 409. Il duplicato replica la coppia ID/revisione e la data. Il ritardo produce una revisione nuova retrodatata da 1 a `minutes` minuti, entro la retention. Una durata di outage `0s` ripristina subito le letture. La generazione continua durante l'outage.

`GET /admin/expected?since=N` restituisce le registrazioni con sequenza globale maggiore di N, inclusi duplicati e tombstone, in ordine di arrivo. Ogni riga contiene `sequence`, `recordedAt`, `collection`, `schemaVersion` e `item`. Usare `nextSince` per riprendere il confronto con handoff e ACK. Omettere `since` per leggere tutta la storia conservata. `POST /admin/reset` con corpo `{}` avvia una prova vuota, azzera le sequenze, ripristina gli schemi iniziali e invalida le posizioni precedenti.

## Persistenza e consumo da Ingestion

Il file `vertical-v1.jsonl` registra un lotto per append con sync prima della risposta. All'avvio viene ricostruito l'indice degli oggetti vivi. Una riga finale interrotta viene troncata. La retention si misura da `recordedAt` per conservare anche gli arrivi tardivi. La compattazione, verificata all'avvio, ai cicli e alle richieste autenticate, sostituisce il journal con lo stato vivo al confine della retention e il suffisso di eventi conservati. Uno snapshot include anche un oggetto ancora vivo il cui ultimo evento è già scaduto.

Per applicare il §4 del contratto:

1. A ogni poll osservare versione e impronta dei campi rispetto alla pubblicazione attiva. Verificare la versione anche sulle pagine successive. Se `/schema` e la pagina osservata riportano versioni diverse, ripetere l'osservazione prima di applicare la pagina.
2. Con schema approvato, deduplicare per `(id,revision)`, conservare la revisione massima anche per un tombstone e avanzare il checkpoint solo sul prefisso consegnato e confermato. Applicare le assenze soltanto alla conclusione di uno snapshot completo.
3. Una differenza di versione **o** impronta blocca l'intera pagina. Lasciare fermo il checkpoint, registrare il blocco durevole `SCHEMA_CHANGED`, inviare una segnalazione idempotente all'Onboarding per incidente e mantenere un avviso persistente per gli utenti abilitati. I poll in blocco osservano lo schema e aggiornano il conteggio dell'incidente anche dopo un riavvio.
4. Al ritorno allo schema approvato, oppure dopo revisione approvata in THS e nuova pubblicazione, riprendere dal checkpoint fermo, chiudere l'incidente e risolvere l'avviso.
5. Un 410 `CURSOR_EXPIRED` richiede un blocco distinto `POSITION_LOST`. Il recupero passa per una nuova lettura completa governata. L'azzeramento automatico della posizione farebbe perdere la continuità degli ACK.

Licenza Apache-2.0.
