# ProxyCore Bootstrap Runbook

## Preconditions

1. Install Docker Engine with Compose v2.
2. Create a private `.env` from `.env.example`.
3. Generate a 32-byte master key outside the repository:

   ```sh
   openssl rand -base64 32
   ```

4. Set `PROXYCORE_MASTER_KEY_BASE64` and change the PostgreSQL password.
5. If publishing HTTP/HTTPS, confirm ports 80 and 443 are available. HTTP/3
   additionally requires both TCP and UDP 443.

## Start

```sh
docker compose config
docker compose up -d postgres
docker compose run --rm web pnpm db:migrate
docker compose up -d
```

Open `/bootstrap` once and create the first Owner. The bootstrap endpoint must
reject every later attempt.

## First configuration

1. Set the proxy ingress IPv4/IPv6 address.
2. Set the default resolver pool and any more-specific suffix pools.
3. Add a managed zone and typed records.
4. Add certificate material or request a staging certificate.
5. Review desired state, then queue an apply.

Invalid desired state must never replace the last applied revision.
