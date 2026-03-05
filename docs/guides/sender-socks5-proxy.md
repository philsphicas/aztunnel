# Sender: SOCKS5 Proxy

Run a local SOCKS5 proxy that forwards connections through Azure Relay. Unlike
port-forward (which maps one local port to one remote target), SOCKS5 lets you
reach **any** target the listener allows — you choose the destination at
connection time.

```
┌──────────┐       ┌───────────────────┐       ┌──────────┐
│  curl    │       │  aztunnel         │       │          │──▶ 10.0.0.5:80
│  browser ├──────▶│  socks5-proxy     │══WSS══│ listener │──▶ 10.0.0.6:443
│  app     │ :1080 │                   │       │          │──▶ 10.0.0.7:8080
└──────────┘       └───────────────────┘       └──────────┘
```

## Prerequisites

- A running aztunnel listener on the remote side (see
  [listener guides](README.md#listener-remote-side))
- Azure Relay **Sender** credentials (SAS key or Entra ID)

## Basic usage

Start the SOCKS5 proxy:

```sh
aztunnel relay-sender socks5-proxy \
  --relay my-relay-ns \
  --hyco my-tunnel \
  --bind 127.0.0.1:1080
```

The proxy listens on `127.0.0.1:1080` and forwards each connection through
Azure Relay to the listener, which dials the requested target.

## DNS resolution: `--socks5` vs `--socks5h`

This matters. SOCKS5 clients can resolve DNS **locally** or pass the hostname
to the **remote** side. This affects what the listener sees and how the
allowlist matches.

| curl flag                 | DNS resolution                    | What the listener sees     | Allowlist must contain                      |
| ------------------------- | --------------------------------- | -------------------------- | ------------------------------------------- |
| `--socks5h` (recommended) | **Remote** (listener resolves)    | hostname, e.g. `mydb:5432` | `mydb:5432` (hostname match)                |
| `--socks5`                | **Local** (your machine resolves) | IP, e.g. `10.0.0.5:5432`   | `10.0.0.5:5432` or CIDR like `10.0.0.0/8:*` |

**Use `--socks5h` in most cases.** The listener is on the remote network and
can resolve internal hostnames that your workstation can't. The `h` stands
for "host resolution at the proxy."

```sh
# Recommended — listener resolves the hostname
curl --socks5h 127.0.0.1:1080 http://internal-dashboard:3000

# Also works — your machine resolves the IP first
curl --socks5 127.0.0.1:1080 http://10.0.0.5:8080/health
```

### How the allowlist interacts with DNS

The listener matches the target string **exactly as received** — no DNS
resolution is done during allowlist checking:

- **Hostname targets** (from `--socks5h`) match hostname allowlist entries
  literally. `mydb:5432` matches `--allow "mydb:5432"` but does **not**
  match `--allow "10.0.0.0/8:*"`, even if `mydb` resolves to `10.0.0.5`.
- **IP targets** (from `--socks5` or using IPs directly) match CIDR
  allowlist entries. `10.0.0.5:5432` matches `--allow "10.0.0.0/8:*"` but
  does **not** match `--allow "mydb:5432"`.

> **Watch out for `localhost`**: If a client resolves `localhost` locally, it
> may send `127.0.0.1` or `[::1]` depending on the platform. An allowlist
> with `--allow "127.0.0.1:80"` won't match `[::1]:80`. Use `--socks5h`
> so the listener receives the hostname `localhost` instead of a resolved IP.

If your allowlist uses CIDR ranges, clients must send IPs (use `--socks5`).
If your allowlist uses hostnames, clients must send hostnames (use
`--socks5h`). You can mix both in the same allowlist:

```sh
aztunnel relay-listener \
  --allow "internal-dashboard:3000" \
  --allow "10.0.0.0/8:*"
```

### Other clients

| Client          | Remote DNS (like `--socks5h`)              | Local DNS (like `--socks5`) |
| --------------- | ------------------------------------------ | --------------------------- |
| **curl**        | `--socks5h`                                | `--socks5`                  |
| **Firefox**     | Check "Proxy DNS when using SOCKS v5"      | Uncheck it                  |
| **Chrome**      | `socks5://` (default behavior)             | N/A (always proxies DNS)    |
| **ssh + nc**    | `nc -x proxy:1080 %h %p` (passes hostname) | N/A                         |
| **proxychains** | `proxy_dns` in config (default)            | `proxy_dns_old` or disabled |

Or set it as the default proxy:

```sh
export ALL_PROXY=socks5h://127.0.0.1:1080
curl http://10.0.0.5:8080/health
```

## Using with a browser

Configure your browser to use the SOCKS5 proxy for accessing internal web
apps, dashboards, and admin panels. See the DNS resolution table above for
how to configure proxy DNS in each browser.

**Firefox** (recommended — supports per-connection SOCKS5 without OS-level
settings):

1. Settings → Network Settings → Manual proxy configuration
2. SOCKS Host: `127.0.0.1`, Port: `1080`
3. Select SOCKS v5
4. Check "Proxy DNS when using SOCKS v5" (recommended — lets the listener
   resolve internal hostnames)

**Chrome** (command line):

```sh
google-chrome --proxy-server="socks5://127.0.0.1:1080"
```

**System-wide** (macOS):

```sh
networksetup -setsocksfirewallproxy Wi-Fi 127.0.0.1 1080
# To disable:
networksetup -setsocksfirewallproxystate Wi-Fi off
```

## Using with SSH (dynamic forwarding)

You can use SOCKS5 as an alternative to ProxyCommand when you want one proxy
for multiple SSH targets:

```sh
ssh -o ProxyCommand="nc -x 127.0.0.1:1080 %h %p" user@10.0.0.5
```

> For a single SSH target, [ProxyCommand mode](sender-ssh-proxycommand.md) is
> simpler — it doesn't need a running proxy process.

## Ephemeral port

If you don't care which port the proxy binds to, omit `--bind` or use
port `0`. The OS picks an available port automatically:

```sh
aztunnel relay-sender socks5-proxy --relay my-relay-ns --hyco my-tunnel
```

aztunnel logs the assigned port:

```
level=INFO msg="socks5-proxy listening" bind=127.0.0.1:52431
```

If you're running it as a [bgtask](https://github.com/philsphicas/bgtask),
`bgtask ls` shows the listening port automatically:

```sh
bgtask run --name proxy -- aztunnel relay-sender socks5-proxy
bgtask ls
# → proxy  running  :52431  aztunnel relay-sender socks5-proxy
```

## Sharing the proxy on the network (`--gateway`)

By default, the proxy binds to `127.0.0.1` (localhost only). To share it
with other machines on your network:

```sh
aztunnel relay-sender socks5-proxy \
  --relay my-relay-ns \
  --hyco my-tunnel \
  --bind 0.0.0.0:1080
  # or: --gateway --bind :1080
```

> **Security**: `--gateway` exposes the proxy to your entire network. Only
> use this on trusted networks or behind a firewall.

## When to use SOCKS5 vs port-forward

|                    | SOCKS5 proxy                          | Port forward                               |
| ------------------ | ------------------------------------- | ------------------------------------------ |
| **Targets**        | Any target the listener allows        | One fixed target                           |
| **Client support** | Needs SOCKS5-aware client             | Any TCP client                             |
| **Best for**       | Browsing, exploring, multiple targets | Database access, single-service forwarding |
| **Overhead**       | Slightly more (SOCKS5 handshake)      | Minimal                                    |

Use SOCKS5 when you don't know all the targets ahead of time, or when you
need to reach many services through one tunnel. Use port-forward when you
want a specific `localhost:port → remote:port` mapping.

## Environment variables

Set credentials once so you don't need flags on every invocation:

```sh
export AZTUNNEL_RELAY_NAME=my-relay-ns
export AZTUNNEL_HYCO_NAME=my-tunnel
export AZTUNNEL_KEY_NAME=send-policy
export AZTUNNEL_KEY='<your-sas-key>'

aztunnel relay-sender socks5-proxy --bind 127.0.0.1:1080
```

## Debugging

Increase the log level to see connection details:

```sh
aztunnel relay-sender socks5-proxy --relay my-relay-ns --hyco my-tunnel --log-level debug
```

Each SOCKS5 CONNECT request logs the target address. Common issues:

| Symptom                            | Likely cause                                           |
| ---------------------------------- | ------------------------------------------------------ |
| `connection refused` on proxy port | Proxy isn't running or bound to a different address    |
| `allowlist rejected`               | The listener's `--allow` doesn't include the target    |
| `no active listener`               | Listener isn't running or wrong hybrid connection      |
| Client hangs                       | Client may not support SOCKS5, or DNS resolution issue |
