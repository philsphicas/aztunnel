# Sender: Port Forward

Bind a local port and forward all connections through Azure Relay to a fixed
remote target. This is the simplest sender mode — your local applications
connect to `localhost:PORT` as if the remote service were local.

```
┌──────────┐       ┌─────────────────────┐       ┌──────────┐
│  psql    │       │  aztunnel           │       │          │
│  curl    ├──────▶│  port-forward       │══WSS══│ listener │──▶ 10.0.0.5:5432
│  browser │ :5432 │  10.0.0.5:5432      │       │          │
└──────────┘       └─────────────────────┘       └──────────┘
```

## Prerequisites

- A running aztunnel listener on the remote side (see
  [listener guides](README.md#listener-remote-side))
- Azure Relay **Sender** credentials (SAS key or Entra ID)

## Basic usage

Forward local port 5432 to a remote PostgreSQL server:

```sh
aztunnel relay-sender port-forward \
  --relay my-relay-ns \
  --hyco my-tunnel \
  --bind 127.0.0.1:5432 \
  10.0.0.5:5432
```

Then connect normally:

```sh
psql -h 127.0.0.1 -p 5432 -U myuser mydb
```

## Understanding `--bind`

The `--bind` flag controls where the sender listens for local connections.
It takes an `address:port` pair:

```sh
--bind 127.0.0.1:5432     # localhost, specific port
--bind 192.168.1.100:5432  # specific interface, specific port
--bind 0.0.0.0:5432        # all interfaces, specific port
--bind :5432               # all interfaces, specific port (shorthand)
--bind 127.0.0.1:0         # localhost, OS picks the port
```

The default is `127.0.0.1:0` — localhost only, with an OS-assigned port.

### Port `0` (ephemeral port)

When the port is `0` (or omitted), the OS picks an available port
automatically. This avoids "address already in use" errors and is the
default behavior:

```sh
aztunnel relay-sender port-forward --relay my-relay-ns --hyco my-tunnel 10.0.0.5:5432
```

aztunnel logs the assigned port on startup:

```
level=INFO msg="port-forward listening" bind=127.0.0.1:52431 target=10.0.0.5:5432
```

If you're running aztunnel as a
[bgtask](https://github.com/philsphicas/bgtask), `bgtask status` and
`bgtask ls` automatically detect listening ports — no need to dig through
logs:

```sh
bgtask run --name db-tunnel -- aztunnel relay-sender port-forward 10.0.0.5:5432
bgtask status db-tunnel
# → Ports: 52431/tcp
```

## Sharing with other machines (`--gateway`)

By default, port-forward binds to `127.0.0.1`. To share the forwarded port
with other machines on your network:

```sh
aztunnel relay-sender port-forward \
  --relay my-relay-ns \
  --hyco my-tunnel \
  --gateway --bind :5432 \
  10.0.0.5:5432
```

`--gateway` changes the bind address to `0.0.0.0`.

> **Security**: This exposes the forwarded port to your entire network.
> Only use on trusted networks.

## Multiple forwards in parallel

Use [bgtask](https://github.com/philsphicas/bgtask) to run multiple
port-forward instances as named background tasks:

```sh
bgtask run --name db    -- aztunnel relay-sender port-forward --hyco my-tunnel --bind 127.0.0.1:5432 10.0.0.5:5432
bgtask run --name web   -- aztunnel relay-sender port-forward --hyco my-tunnel --bind 127.0.0.1:8080 10.0.0.5:8080
bgtask run --name redis -- aztunnel relay-sender port-forward --hyco my-tunnel --bind 127.0.0.1:6379 10.0.0.5:6379
```

Each instance opens its own WebSocket connections through the relay.

Check logs or stop individual forwards:

```sh
bgtask logs db
bgtask stop web
```

> If you need to reach many targets dynamically, consider
> [SOCKS5 proxy mode](sender-socks5-proxy.md) instead.

## Environment variables

```sh
export AZTUNNEL_RELAY_NAME=my-relay-ns
export AZTUNNEL_HYCO_NAME=my-tunnel
export AZTUNNEL_KEY_NAME=send-policy
export AZTUNNEL_KEY='<your-sas-key>'

aztunnel relay-sender port-forward --bind 127.0.0.1:5432 10.0.0.5:5432
```

## Common use cases

| Target     | Command                                            | Then use                     |
| ---------- | -------------------------------------------------- | ---------------------------- |
| PostgreSQL | `port-forward --bind 127.0.0.1:5432 10.0.0.5:5432` | `psql -h 127.0.0.1`          |
| MySQL      | `port-forward --bind 127.0.0.1:3306 10.0.0.5:3306` | `mysql -h 127.0.0.1`         |
| Redis      | `port-forward --bind 127.0.0.1:6379 10.0.0.5:6379` | `redis-cli`                  |
| HTTP API   | `port-forward --bind 127.0.0.1:8080 10.0.0.5:8080` | `curl http://127.0.0.1:8080` |
| SSH        | `port-forward --bind 127.0.0.1:2222 10.0.0.5:22`   | `ssh -p 2222 user@127.0.0.1` |

> For SSH, [ProxyCommand mode](sender-ssh-proxycommand.md) is usually more
> convenient — it integrates directly with your SSH config.

## Debugging

```sh
aztunnel relay-sender port-forward --log-level debug --bind :5432 10.0.0.5:5432
```

| Symptom                  | Likely cause                                                             |
| ------------------------ | ------------------------------------------------------------------------ |
| `address already in use` | Another process is using the port; try a different `-b` port or use `:0` |
| `allowlist rejected`     | The listener's `--allow` doesn't include `10.0.0.5:5432`                 |
| `no active listener`     | Listener isn't running or wrong hybrid connection                        |
| Connection drops         | Check `--tcp-keepalive` settings on both sides                           |
