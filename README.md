# ouf-vertical-mock

Fonte HTTP di laboratorio per provare Ingestion con dati fittizi, duplicati, ritardi, cancellazioni, guasti e cambi di schema. Il consumatore usa solo `/collections`. Il banco di prova controlla la generazione e consulta il risultato atteso sotto `/admin`. Il servizio implementa `ouf.vertical-source.v1` con le estensioni di `ouf.vertical-mock.v1`.

## Avvio locale

Occorrono Go 1.27.4 e una directory dati scrivibile riservata a un solo processo. Questo esempio per shell POSIX usa credenziali fittizie.

```sh
lab=$(mktemp -d)
umask 077
printf '%s\n' demo-reader > "$lab/read"
printf '%s\n' demo-admin > "$lab/admin"
export OUF_MOCK_DATA_DIR="$lab/data"
export OUF_MOCK_DOMAIN_FILE="$PWD/examples/domain-lab.json"
export OUF_MOCK_READ_TOKEN_FILE="$lab/read"
export OUF_MOCK_ADMIN_TOKEN_FILE="$lab/admin"
go run . serve
```

La sonda locale è `http://localhost:8080/health/ready`. La generazione inizia dopo il primo intervallo. Un banco può popolare subito le collezioni tramite `inject`. Per le prove con avanzamento del tempo, impostare `OUF_MOCK_CLOCK_CONTROL=enabled` prima dell'avvio.

## Container e cancello della pipeline

L'immagine Alpine esegue il binario come `10008:10008`. La directory dati è sempre un **bind mount in lettura e scrittura (RW) di una directory dell’host**. Il rilascio dei moduli riusa i bind del predecessore, quindi un volume nominato è escluso. La directory dell’host deve essere scrivibile da `10008:10008`. I percorsi dei file dei bearer sono mount di sola lettura. L'esempio usa i due file dimostrativi appena creati, in una directory privata.

```sh
docker build -t ouf-vertical-mock:local .
chmod 0444 "$lab/read" "$lab/admin"
sudo install -d -m 0700 -o 10008 -g 10008 "$lab/container-data"
gate='OUF_APPLICATION_GATE=0; trap "OUF_APPLICATION_GATE=1" USR1; while [ "$OUF_APPLICATION_GATE" = 0 ]; do sleep 1 & wait "$!"; done; exec "$@"'
cid=$(docker run -d --read-only --cap-drop ALL --security-opt no-new-privileges \
  --mount "type=bind,src=$lab/container-data,dst=/var/lib/ouf-vertical-mock" \
  --mount "type=bind,src=$lab/read,dst=/run/mock-read,readonly" \
  --mount "type=bind,src=$lab/admin,dst=/run/mock-admin,readonly" \
  -e OUF_MOCK_DATA_DIR=/var/lib/ouf-vertical-mock \
  -e OUF_MOCK_DOMAIN_FILE=/usr/share/ouf-vertical-mock/examples/domain-lab.json \
  -e OUF_MOCK_READ_TOKEN_FILE=/run/mock-read -e OUF_MOCK_ADMIN_TOKEN_FILE=/run/mock-admin \
  -p 127.0.0.1::8080 --entrypoint /bin/sh ouf-vertical-mock:local \
  -c "$gate" ouf-gated-application /usr/local/bin/ouf-vertical-mock serve)
sleep 3
docker kill --signal USR1 "$cid"
docker port "$cid" 8080/tcp
```

Prima di `USR1` il servizio resta in attesa. Dopo il segnale, `GET /health/ready` restituisce `204` quando il journal è caricato e scrivibile. `docker stop --time 10 "$cid"` invia `SIGTERM`. La directory dati contiene una chiave HMAC e richiede la stessa custodia dei dati di prova.

## Variabili

| Variabile | Predefinito | Uso |
|---|---|---|
| `OUF_MOCK_PORT` | `8080` | Porta interna da 1 a 65535 |
| `OUF_MOCK_DATA_DIR` | obbligatoria | Journal e lock esclusivo |
| `OUF_MOCK_DOMAIN_FILE` | obbligatoria | Percorso JSON leggibile |
| `OUF_MOCK_READ_TOKEN_FILE` | obbligatoria | File del bearer di lettura |
| `OUF_MOCK_ADMIN_TOKEN_FILE` | obbligatoria | File di un bearer diverso per i comandi |
| `OUF_MOCK_SEED` | `1` | Intero con segno a 64 bit |
| `OUF_MOCK_CYCLE` | `PT1M` | Intervallo globale, minimo `PT1S` |
| `OUF_MOCK_RETENTION` | `P7D` | Durata della storia dalla visibilità |
| `OUF_MOCK_MAX_LATENESS` | `PT10M` | Massimo configurabile per collezione, inferiore alla retention |
| `OUF_MOCK_READ_MIN_INTERVAL` | `PT0S` | Intervallo minimo fra letture riuscite di `schema` o `items` della stessa collezione |
| `OUF_MOCK_CLOCK_CONTROL` | `disabled` | `enabled` espone `POST /admin/clock` |

Le durate usano giorni, ore, minuti e secondi ISO 8601, anche con frazioni di secondo. Una configurazione invalida interrompe l'avvio. La rotazione dei bearer richiede un riavvio. [Registro della configurazione](docs/CONFIGURATION_REGISTER.md)

## Dominio e generazione

Ogni voce di `collections` dichiara nome, titolo, prefisso degli identificativi, modalità di lettura, cancellazioni, parametri di generazione e campi. La chiave è `id`. Ogni campo ha tipo, descrizione, nullabilità e generatore. L'esempio comprende edicole con geometria puntuale e cantieri con poligono. Le edicole offrono tutte le modalità e tombstone espliciti. I cantieri offrono snapshot e watermark con cancellazione per assenza. [File di esempio](examples/domain-lab.json)

I generatori ammessi sono `id`, `choice`, `template`, `integer`, `decimal`, `boolean`, `date`, `datetime`, `point` e `polygon`. `varies: OBJECT` conserva il valore per oggetto, `REVISION` include la revisione nel campionamento. Il predefinito è `OBJECT` per i campioni e `datetime` con `source: CHANGED_AT` restituisce il tempo dell'evento. `nullPercent` da 0 a 100 richiede un campo nullable. Le date relative dichiarano un altro campo data con `after`. Il dominio valida i riferimenti prima dell'avvio. Per la sintassi completa si veda il [contratto HTTP, sezione 5](docs/CONTRACT.md#5-dominio-configurabile).

Un journal nuovo acquisisce dominio, seme e ciclo. Al riavvio il journal recupera le revisioni, i ritardi in attesa, i comandi e le finestre di lettura. `/admin/status` confronta le impronte del dominio configurato e del dominio acquisito. `reset` torna agli schemi iniziali e cambia epoca. `{"reloadDomain":true}` acquisisce anche il file aggiornato.

## Lettura e amministrazione

Negli esempi impostare `base` con schema, host e porta del mock. I due bearer hanno ruoli distinti.

```sh
base='http://<mock-host>'
read_auth='Authorization: Bearer demo-reader'
admin_auth='Authorization: Bearer demo-admin'
curl -i -H "$read_auth" "$base/collections"
curl -sS -H "$read_auth" "$base/collections/edicole/schema"
curl -sS -H "$read_auth" "$base/collections/edicole/items?mode=SNAPSHOT&limit=10"
curl -sS -G -H "$read_auth" "$base/collections/edicole/items" \
  --data-urlencode mode=WATERMARK --data-urlencode 'changedSince=2026-01-01T00:00:00Z'
curl -sS -H "$admin_auth" "$base/admin/status"
post() { curl -sS -H "$admin_auth" -H 'Content-Type: application/json' --data "$2" "$base/admin/$1"; }
post rate '{"collection":"edicole","count":2,"duplicate":10,"late":20,"cycle":"PT1M"}'
post inject '{"collection":"edicole","kind":"create","id":"demo-A"}'
post inject '{"collection":"edicole","kind":"late","id":"demo-A","lateness":"PT3M"}'
post inject '{"collection":"edicole","kind":"delete","id":"demo-A"}'
post clock '{"advance":"PT3M"}'
post schema '{"collection":"edicole","action":"add","name":"aperta","type":"boolean"}'
post outage '{"collection":"edicole","kind":"RATE_LIMITED","duration":"PT30S","retryAfter":5}'
```

Il ritardo assegna subito `revision` e `changedAt`. La sequenza viene assegnata alla pubblicazione. Nell'esempio il consumatore può ricevere prima il `DELETE` e poi l'aggiornamento con revisione minore. Il generatore automatico può sostituire un'operazione con un aggiornamento tardivo e ripubblicare un evento già visibile nello stesso ciclo. I comandi applicano tutto il lotto oppure rispondono `409 TARGET_NOT_AVAILABLE`.

## Registro atteso e regole del consumatore

```sh
curl -sS -G -H "$admin_auth" "$base/admin/expected" \
  --data-urlencode collection=edicole --data-urlencode since=0 --data-urlencode limit=50
curl -sS -G -H "$admin_auth" "$base/admin/expected/state" \
  --data-urlencode collection=edicole --data-urlencode limit=50
```

Per gli eventi seguire `nextSince` finché `hasMore` è vero. `since` omesso parte dal limite della storia conservata. Per lo stato seguire `nextAfter` nel parametro `after` e conservare lo stesso `asOf` su ogni pagina. Per I3, `asOf` è l’ultima sequenza coperta dalla posizione confermata. `renderAs` accetta una versione già vista nella collezione e permette di ricalcolare `contentSha256` sull'item così come ricevuto. I tombstone nascosti dai delta compaiono nel registro con `deleteVisibility: SNAPSHOT_ONLY`.

Il consumatore approva `schemaVersion` e `fieldsSha256` prima di elaborare **ogni pagina**. Conserva la posizione candidata finché tutti gli item hanno un esito durevole, poi conferma la posizione. Un cambio di schema blocca il checkpoint. Dopo un `revert` la lettura può riprendere dalla stessa posizione. Le revisioni si confrontano all’interno della stessa epoca. Una revisione superata resta in lineage, un duplicato conserva l'effetto già applicato e un tombstone mantiene la revisione dell'oggetto ritirato.

Ogni pagina di snapshot riporta `epoch`, `snapshotId` e `snapshotAt`. Con cancellazioni esplicite lo snapshot comprende anche i `DELETE` conservati. L’ultima pagina riporta lo stesso `snapshotAt` in `resume`. `resume.cursor` comprende esattamente gli arrivi successivi. `resume.watermark.changedSince` arretra di `maxLateness` e richiede deduplica. Il `410` distingue `RETENTION` (posizione persa) da `EPOCH_CHANGED` (nuova storia, con l’epoca corrente nel corpo). Per un `429` o un `503` il consumatore mantiene la posizione e rispetta `Retry-After`. Le regole normative sono in `ouf.delta-position.v1`, sezioni 3–8. [Contratto HTTP](docs/CONTRACT.md) · [Consumatore di riferimento](consumer_test.go)

## CI e consegna

`module-ci.yml` verifica formattazione, vet, test con race detector, staticcheck, govulncheck, identità dell'immagine, cancello e scansione Trivy. L'artefatto `vertical-mock-evidence-<runId>` raccoglie i risultati disponibili anche dopo un errore. Nelle PR l'artefatto `vertical-mock-governed-image-<runId>` conserva per 14 giorni `image.tar.gz`, `manifest.json` e `SHA256SUMS`. Il manifest distingue il commit esaminato dal commit effettivamente testato e registra ID immagine, etichetta di revisione e impronta dell'archivio. L'autorizzazione al rilascio resta alla pipeline di consegna.
