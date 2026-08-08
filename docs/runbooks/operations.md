# ProxyCore Operations Runbook

## Apply and rollback

The worker renders a revision, stages it, validates it, promotes it, reloads
the target service, and checks health. A failed health check must result in a
rollback or an explicit recovery-required job.

The worker listens for PostgreSQL job notifications and wakes immediately when
an apply is committed. It also reconciles the durable queue every 5 minutes by
default (`WORKER_RECONCILIATION_INTERVAL_MS`). Jobs are claimed with a lease
and are re-queued after the lease expires, so a missed notification or a
restart does not discard desired state or an outstanding apply.

Saving a zone or DNS record creates its revision and queues an immediate
combined CoreDNS/Nginx apply. Consecutive saves are intentionally not batched;
inspect `/api/status` when several edits are made in succession.

Inspect current jobs and status through the authenticated API:

```sh
curl -b cookies.txt http://localhost:3000/api/status
```

Do not edit CoreDNS or Nginx files while an apply is running. Preserve the
candidate checksum and correlation id when reporting an incident.

## Service-control boundary

Only `control` receives the Docker Engine socket. `web` and `worker` do not.
The worker sends a fixed JSON-lines operation over `/run/proxycore/control.sock`;
there is no arbitrary command, shell, or directive field. If the helper socket
is unavailable, stop applying and investigate the helper rather than granting
the worker a socket.

## Master-key rotation

1. Stop mutations and take a verified backup of the PostgreSQL volume.
2. Keep the old key available only in a protected operator environment.
3. Read each encrypted secret through the old key and write a new ciphertext
   with the new 32-byte key in one controlled migration.
4. Verify certificate private keys and provider credentials can be decrypted.
5. Update `PROXYCORE_MASTER_KEY_BASE64`, restart control-plane services, and
   remove the old key from process environments.

Never log either key or decrypted secret values.

## Cloudflare credential rotation

Create a new token scoped to the required zone and `_acme-challenge` TXT
operations, update the encrypted provider secret, issue a staging challenge,
then revoke the old token. Ordinary DNS records must not be modified by the
DNS-01 adapter.

## Certificate incidents

- A renewal failure keeps the last active certificate.
- If expiry is approaching, use staging first and inspect challenge reachability.
- HTTP-01 requires public port 80 to reach the challenge handler.
- DNS-01 requires propagation of only `_acme-challenge` TXT records.
- Never delete the active certificate during cleanup.
- On apply, the worker materializes certificate PEM/key and Basic Auth htpasswd
  files into the Nginx candidate directory under the shared `candidates` volume.
  Nginx config references those absolute candidate paths; inspect them under
  `/var/lib/proxycore/candidates/<revisionId>/nginx/` when debugging `nginx -t`.

## Retention

Retention removes old or oversized operational artifacts only. It must keep
desired state, the active applied revision, and active certificate keys.
