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

1. Open the dashboard through the host's LAN address when possible; ProxyCore
   automatically persists that address for proxied DNS answers. Set or override
   it in Network settings when the detected address is not the intended LAN,
   NAT, or public address.
2. Set the default resolver pool and any more-specific suffix pools.
3. Add a managed zone and typed records; each save queues an apply immediately.
4. In Streams/Network, issue a self-signed certificate for the proxied hostname.
5. Create a proxied record. For Compose smoke tests, point the origin at the
   demo upstream `172.30.0.10:80` (service `demo-upstream`). Configure port,
   redirects, and Basic Auth from the Record dialog as needed.
6. Review the resulting job status; use manual apply only to re-apply the
   current desired state.

Invalid desired state must never replace the last applied revision.

## Compose smoke test

```sh
# After bootstrap + network settings + zone
# 1) Issue cert for app.home.arpa in the Streams view
# 2) Add proxied A record app → any DNS value, origin 172.30.0.10:80
# 3) Wait for apply, then:
curl -vk --resolve app.home.arpa:443:127.0.0.1 https://app.home.arpa/
```

Basic Auth, when enabled, requires the username/password from the Record dialog
and client HTTPS. Auth and certificate files are written into the shared
`candidates` volume next to the rendered Nginx config.
