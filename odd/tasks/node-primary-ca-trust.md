# Feature: node-primary-ca-trust

Let a NODE trust a self-signed / private-CA primary by loading the primary's CA from the UI, per primary origin, replaceable on every (re)enrollment.

## Specs
- S1: "el codigo deberia tener algun intercanvio y alguna validacion para que el nodo pueda validar ese certificado autofirmado, recuerda que todo se tiene que poder hacer desde la UI, si yo tengo que descargar el CA que me da el primary para ponerlo en el NODE, el node tiene que tener un apartado donde yo pueda hacer eso"
- S2: "en cada reenrolmen tiene que poderse cargar uno nuevo para esa misma ip" — enrolling again against the same primary origin accepts a new CA and replaces the previous one for that origin.
- S3: The CA is trusted only for the primary origin it was loaded for (host:port), never as a global trust store (user confirmed "si" to per-primary scope).
- S4: The trusted CA must be used by every node->primary TLS client: snapshot fetch (draft), grant exchange/confirm, acknowledgement, and continuous sync. `skipVerify` stays unsupported (design.md section 4).
- S5: The UI shows the CA fingerprint (SHA-256) so the owner can compare it with the one shown by the primary; CA download already exists on the primary (`EnrollmentTrustPanel`).
- S6: A bad or non-CA PEM is rejected with a clear error; a TLS trust failure surfaces a specific message instead of "primary snapshot unavailable".

## Tasks
- [x] T1 (S3,S5,S6) Backend: CA PEM validation + per-origin trust store. Migration 007 `primary_trust_anchors` (STRICT), `configuration/sqlite_primary_trust.go`, tests RED->GREEN; schema mirrors (Bun ledger 7, 32 tables, pgconvert SQLite-only table) updated. Full `go test ./...` green. Commit: e28baf8
- [x] T2 (S2,S4) Backend wiring: `sync/origin_trust.go` per-origin RoundTripper shared by snapshot (draft, sync, ack) and exchange clients; draft accepts optional `caPem` (validated, request-scoped, fingerprint returned as `primaryCaFingerprint`); confirm uses it for the exchange and persists it (replacing the previous CA for that origin) BEFORE the sync binding, failing closed (503). Commits: 5da2a3c (transport), 9e25c30.
- [x] T3 (S6) Draft answers 409 "primary certificate not trusted; load the primary CA" for a rejected certificate (generic "primary snapshot unavailable" kept for other failures); invalid CA -> 400 "invalid CA certificate". Commit: 9e25c30.
- [x] T4 (S1,S5) UI: NodeContinuityView has an optional PRIMARY CA field (paste or load .pem/.crt/.cer, <=64 KiB) sent with the draft; preview shows `primary ca fingerprint`; two fixed draft errors (409 untrusted / 400 invalid CA) get specific messages via a closed allowlist (other bodies still discarded). vitest: only the pre-existing EnrollmentTrustPanel date failure remains (fails on clean main too); tsc clean. Commit: c927485.
- [ ] T5 Docs: README/PRD topology + help text.

## Log
- L1 (user): "Hola mira, cuando intento unirme al primario me da este error, {error: "primary snapshot unavailable"} ... paero el primary esta publicado hace rato con cambios, incluso de di a reaplicar"
- L2 (evidence): primary log "http: TLS handshake error from 192.168.1.3: remote error: tls: bad certificate" x3 (node rejects primary cert; client built with no RootCAs, cmd/server/main.go:139).
- L3 (user): "ya va porque entonces aqui hay un punto importante, el codigo deberia tener algun intercanvio y alguna validacion para que el nodo pueda validar ese certificado autofirmado, recuerda que todo se tiene que poder hacer desde la UI, si yo tengo que descargar el CA que me da el primary para ponerlo en el NODE, el node tiene que tener un apartado donde yo pueda hacer eso"
- L4 (user): "si, igual en cada reenrolmen tiene que poderse cargar uno nuevo para esa misma ip, imagina que tuve que levantar otro primario porque el anterior se me compio y no guarde el copia, primero lo pongo como nodo, y despues como primario y en ese cambio obviamente tengo un nuevo certificado, eso quiere decir que yo puedo hacer reenrolment y cargar un nuevo certificado para el mismo server"
