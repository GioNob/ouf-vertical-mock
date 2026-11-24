# Registro della configurazione (D-065)

| Archivio | Contenuto | Segreti | Cattura e ripristino |
|---|---|---|---|
| Directory dati (`OUF_MOCK_DATA_DIR`) | Journal degli eventi e degli schemi, epoca e chiave HMAC delle posizioni | Sì: la chiave delle posizioni, che non va pubblicata | Dati di prova rigenerabili. Si possono non catturare; il ripristino è un `reset` (invalida le posizioni dei consumatori: `POSITION_LOST` voluto) |
| File dei token | Bearer di lettura e di amministrazione | Sì | Custodia dell'installazione; rotazione con riavvio. Il consumatore ouf riceve il bearer di lettura solo per riferimento (`credentialRef`) |
| File di dominio | Collezioni, campi, generatori | No | Versionato nella configurazione dell'installazione |
