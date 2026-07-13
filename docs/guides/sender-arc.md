# Sender: Azure Arc

Connect to [Azure Arc-enrolled machines](https://learn.microsoft.com/en-us/azure/azure-arc/servers/overview)
through the relay that Azure provisions automatically. No separate relay
namespace or listener needed — the Arc agent on the target machine acts as
the listener.

```
┌────────────┐       ┌──────────────┐       ┌──────────────────┐
│ Workstation│       │ Azure Relay  │       │ Arc-enrolled VM  │
│            │       │ (auto-       │       │                  │
│  ssh ──────┤──────▶│  provisioned)│◀──────│  Arc agent       │
│            │  WSS  │              │  WSS  │  sshd :22        │
└────────────┘       └──────────────┘       └──────────────────┘
```

## Prerequisites

- The target machine must be Arc-enrolled (`Microsoft.HybridCompute/machines`)
- The `Microsoft.HybridConnectivity` resource provider must be registered
- [DefaultAzureCredential](https://learn.microsoft.com/en-us/azure/developer/go/azure-sdk-authentication)
  access to the machine's ARM resource (e.g., `az login`)
- SSH running on the target machine

## Finding your resource ID

The `--resource-id` flag takes the full ARM resource ID of the Arc machine:

```
/subscriptions/<SUB>/resourceGroups/<RG>/providers/Microsoft.HybridCompute/machines/<NAME>
```

Find it with the Azure CLI:

```sh
az connectedmachine list --resource-group my-rg -o json | jq -r '.[].id'
```

Or for a specific machine:

```sh
az connectedmachine show --resource-group my-rg --name my-vm --query id -o json | jq -r .
```

## arc connect (SSH ProxyCommand)

Use `arc connect` as an SSH ProxyCommand — same pattern as
[relay-sender connect](sender-ssh-proxycommand.md), but with Arc managing
the relay automatically:

```sh
ssh -o ProxyCommand="aztunnel arc connect --resource-id /subscriptions/SUB/resourceGroups/RG/providers/Microsoft.HybridCompute/machines/myVM" user@myVM
```

### SSH config

The most powerful pattern uses the full resource ID as the SSH hostname.
This works for any Arc machine without per-host configuration:

```
Host /subscriptions/*
    ProxyCommand aztunnel arc connect --resource-id %n --port %p
```

Then connect using the resource ID directly:

```sh
ssh user@/subscriptions/SUB/resourceGroups/RG/providers/Microsoft.HybridCompute/machines/myVM
```

`%n` passes the original hostname (the resource ID) to aztunnel, and `%p`
passes the port. No `Hostname` directive is needed — the Arc agent always
connects to `localhost:{port}` on the target machine regardless of what
hostname the SSH client uses. SSH stores the host key under the resource
ID, so each machine gets its own entry in known_hosts and you'll be warned
if a VM is reprovisioned with a different host key.

> **Optional refinements**: Add `UserKnownHostsFile ~/.ssh/arc_known_hosts`
> to keep Arc host keys separate from your regular known_hosts. Add
> `StrictHostKeyChecking accept-new` to skip the first-connect prompt.

### Per-machine aliases

For convenience, add aliases for specific machines:

```
Host arc-myvm
    User azureuser
    ProxyCommand aztunnel arc connect --resource-id /subscriptions/SUB/resourceGroups/RG/providers/Microsoft.HybridCompute/machines/myVM
```

### Using the environment variable

```sh
export AZTUNNEL_ARC_RESOURCE_ID="/subscriptions/SUB/resourceGroups/RG/providers/Microsoft.HybridCompute/machines/myVM"
ssh -o ProxyCommand="aztunnel arc connect" user@myVM
```

## Automatic Microsoft Entra ID SSH (AADSSHLoginForLinux)

If your Arc machines use the
[AADSSHLoginForLinux](https://learn.microsoft.com/en-us/azure/active-directory/devices/howto-vm-sign-in-azure-ad-linux)
extension, you normally sign in with a short-lived SSH certificate issued by
Entra ID (what `az ssh arc` does under the hood). `aztunnel arc aad-cert`
mints that certificate for you, so plain `ssh` "just works" — no `az ssh`
wrapper.

The trick is an SSH config `Match final exec` directive: it runs _during_ config
parsing (in the final pass, after the host name is canonicalized), before the
connection is made, so aztunnel can create the key pair and write a fresh
certificate before SSH authenticates.

```
Host /subscriptions/*
    StrictHostKeyChecking accept-new
    UserKnownHostsFile ~/.ssh/arc/%C/known_hosts
    IdentityFile ~/.ssh/arc/%C/id
    CertificateFile ~/.ssh/arc/%C/id-cert.pub
    Match final exec "aztunnel arc aad-cert --resource-id %n --user %r --dir ~/.ssh/arc/%C"
    ProxyCommand aztunnel arc connect --resource-id %n --port %p
```

> **Requires OpenSSH 9.6 or newer** — it hardens shell-command expansion against
> unsafe characters in connection tokens used by `Match exec`.

Then connect using your Entra ID user principal name (UPN) as the username:

```sh
ssh alice@contoso.com@/subscriptions/SUB/resourceGroups/RG/providers/Microsoft.HybridCompute/machines/myVM
```

`aztunnel arc aad-cert` prints the exact username to use (the certificate's
first principal) on stderr:

```
aztunnel: AAD SSH username: alice@contoso.com
```

### What aad-cert does

1. Generates an ephemeral RSA key pair in `--dir` (if not already there).
2. Signs in to Entra ID (interactive browser the first time) and requests a
   `token_type=ssh-cert` proof-of-possession token — the returned token _is_
   the OpenSSH certificate body. A refresh token is cached
   (`--token-cache`, default under your user config dir) for silent renewals.
3. Writes the certificate to `--cert` (default `<identity>-cert.pub`).
4. Derives the login username from the certificate's first principal and
   prints it.

On subsequent connections, if the existing certificate is still valid (at
least `--min-valid`, default 5m), aad-cert skips both the network call and the
sign-in entirely.

### Why this config

- **Per-host paths via `%C`** — `%C` is OpenSSH's connection hash (a SHA-1 over
  the local host name, the target host, port, and login user). It contains no
  slashes and is already lowercased, so it makes a clean, case-stable,
  Windows-safe directory name — unlike the raw resource ID (`%n`), which is a
  long, case-preserving path. ssh expands `%C` in _both_ the
  `IdentityFile`/`CertificateFile`/`UserKnownHostsFile` paths and in the `--dir`
  argument, so aad-cert writes the key and certificate into exactly the directory
  ssh reads from — aztunnel never reconstructs the hash itself. Each machine gets
  its own key, certificate, and `known_hosts`, and a `resource-id` marker file is
  dropped in each directory so the opaque layout stays navigable.
- **`Match final exec`, not `Match exec`** — a plain `Match exec` is evaluated in
  _both_ SSH config passes, and in the first pass the host name isn't yet
  canonicalized (lowercased), so its `%C` differs from the one ssh uses for
  `IdentityFile`. `final` runs only in the second pass, after canonicalization,
  guaranteeing its `%C` matches the paths ssh actually reads.
- **No `Hostname` directive** — the `ProxyCommand` uses `%n` (the resource ID)
  to open the relay, and ssh performs no DNS resolution when a `ProxyCommand` is
  set, so no `Hostname` is needed. Omitting it also keeps `%h` equal to the
  (lowercased) resource ID, which is what makes `%C` unique per machine — a
  shared `Hostname localhost` would collapse every machine to the same `%C`.
- **Per-host `UserKnownHostsFile` + `accept-new`** — giving each machine its own
  `%C`-named `known_hosts` file provides real trust-on-first-use across many
  machines instead of one shared file in which every host looks like the same
  endpoint.

> **Custom SSH port**: because ssh computes `%C` itself, a non-default `Port`
> directive is reflected in `%C` automatically — the `--dir ~/.ssh/arc/%C` path
> just follows along, with nothing extra to configure on the aztunnel side.

### Flags

| Flag            | Default                 | Description                                               |
| --------------- | ----------------------- | --------------------------------------------------------- |
| `--dir`         | (none)                  | This connection's key/cert dir (point at `~/.ssh/arc/%C`) |
| `--user`        | (none)                  | Login name (`%r`); selects the cached Entra account       |
| `--identity`    | (none)                  | Explicit private key path (alternative to `--dir`)        |
| `--cert`        | `<identity>-cert.pub`   | Certificate output path                                   |
| `--token-cache` | user config dir         | MSAL token cache for silent renewal                       |
| `--client-id`   | Azure CLI public client | OAuth public client ID                                    |
| `--tenant`      | `organizations`         | Entra ID tenant (use a tenant ID for guest access)        |
| `--min-valid`   | `5m`                    | Reuse an existing cert valid at least this long           |

Provide either `--dir` (recommended; writes `<dir>/id` and `<dir>/id-cert.pub`)
or an explicit `--identity` path.

### First-time sign-in

The first connection opens a browser to sign in to Entra ID. Because
`Match final exec` runs before the SSH connection, the prompt appears up front.
After that, the cached refresh token allows silent certificate renewal until it
expires.

Bind a local port for tools that don't support ProxyCommand:

```sh
aztunnel arc port-forward \
  --resource-id /subscriptions/SUB/resourceGroups/RG/providers/Microsoft.HybridCompute/machines/myVM \
  --bind 127.0.0.1:2222

# Then connect:
ssh -p 2222 user@127.0.0.1
```

This is useful for:

- GUI SSH clients that don't support ProxyCommand
- Port-forwarding non-SSH services (e.g., WAC on port 443)
- Scripts that need a stable local endpoint

## Custom port

If SSH listens on a non-standard port:

```sh
aztunnel arc connect --resource-id /subscriptions/.../machines/myVM --port 2222
aztunnel arc port-forward --resource-id /subscriptions/.../machines/myVM --port 2222 --bind :2222
```

## Service types

The `--service` flag selects which service to connect to:

| Service | Default port | Description          |
| ------- | ------------ | -------------------- |
| `SSH`   | 22           | SSH server (default) |
| `WAC`   | 443          | Windows Admin Center |

```sh
aztunnel arc port-forward --resource-id .../machines/myWinVM --service WAC --port 443 --bind :8443
```

## How Arc relay works

Unlike `relay-sender` (which uses a relay you provision yourself), `arc`
commands use the relay that Azure provisions automatically for Arc-enrolled
machines:

1. aztunnel calls the ARM API to get relay credentials for the machine
2. If the hybrid connectivity endpoint doesn't exist, aztunnel creates it
   via `EnsureHybridConnectivity`
3. aztunnel connects to the Azure Relay using the returned SAS credentials
4. The Arc agent on the target machine is already listening on that relay

Credentials are short-lived SAS tokens. For `arc port-forward`, aztunnel
refreshes credentials for each new connection to avoid expiry.

## Testing with a kind node

You can enroll a kind cluster node as an Arc-connected server for local
testing. This uses `docker exec` to install SSH, provision your SSH key,
and install the Arc agent inside the kind node's container:

```sh
KIND_CLUSTER_NAME="arctest"
RESOURCE_GROUP="arctest-rg"
SUBSCRIPTION_ID="$(az account show -o json | jq -r .id)"
ACCESS_TOKEN="$(az account get-access-token -o json | jq -r .accessToken)"
SSH_PUBKEY="$(cat ~/.ssh/id_rsa.pub)"

# Create the kind cluster
kind create cluster --name "$KIND_CLUSTER_NAME"

# Install SSH + Arc agent inside the node
docker exec -i "${KIND_CLUSTER_NAME}-control-plane" bash <<SCRIPT
apt-get update && apt-get install -y wget openssh-server sudo
mkdir -p /root/.ssh && chmod 700 /root/.ssh
echo "${SSH_PUBKEY}" > /root/.ssh/authorized_keys
chmod 600 /root/.ssh/authorized_keys
service ssh start
wget https://aka.ms/azcmagent -qO- | bash
azcmagent connect \
  -g "$RESOURCE_GROUP" \
  --subscription-id "$SUBSCRIPTION_ID" \
  --access-token "$ACCESS_TOKEN" \
  --location eastus2
SCRIPT
```

Then connect via Arc:

```sh
ssh root@/subscriptions/$SUBSCRIPTION_ID/resourceGroups/$RESOURCE_GROUP/providers/Microsoft.HybridCompute/machines/${KIND_CLUSTER_NAME}-control-plane
```

## SOCKS5 proxy over Arc SSH

If you have an Arc-enrolled VM and want to use it as a gateway to its
network, combine SSH dynamic forwarding (`-D`) with aztunnel Arc. This
gives you a SOCKS5 proxy that can reach anything the VM can reach — without
deploying an aztunnel listener or relay namespace.

```
┌─────────────┐       ┌──────────────┐       ┌──────────────────┐
│ Workstation  │       │ Azure Relay  │       │ Arc-enrolled VM  │
│              │       │ (auto-       │       │                  │
│  browser ────┤──────▶│  provisioned)│◀──────│  Arc agent       │
│  curl        │  WSS  │              │  WSS  │  sshd → network  │
│  :1080       │       │              │       │                  │
└─────────────┘       └──────────────┘       └──────────────────┘
```

Start the SSH SOCKS proxy as a background task. With the wildcard SSH
config from above, you can use the resource ID directly:

```sh
bgtask run --name socks-over-arc -- \
  ssh -D 1080 -N \
    user@/subscriptions/SUB/resourceGroups/RG/providers/Microsoft.HybridCompute/machines/myVM
```

Or without the SSH config, pass the ProxyCommand explicitly:

```sh
bgtask run --name socks-over-arc -- \
  ssh -D 1080 -N \
    -o ProxyCommand="aztunnel arc connect --resource-id /subscriptions/SUB/resourceGroups/RG/providers/Microsoft.HybridCompute/machines/myVM" \
    user@myVM
```

Then use the proxy to reach anything on the VM's network:

```sh
curl --socks5h 127.0.0.1:1080 http://10.0.0.5:8080/api/health
curl --socks5h 127.0.0.1:1080 http://internal-db:5432
```

This is a quick way to get network access without provisioning a relay
namespace or deploying a listener — all you need is an Arc-enrolled VM
with SSH.

## First-connection latency

The first `aztunnel arc connect` (or `arc port-forward`) against a
machine that has no HybridConnectivity endpoint for the requested
service will:

1. Create the endpoint and the requested service configuration (SSH by
   default, or whatever `--service` was passed) on the Arc resource.
   This step also runs when the endpoint exists but a configuration for
   the requested `--service` is missing (e.g. first time using
   `--service WAC` against a machine that previously only had SSH).
   The `--service` flag applies to both `arc connect` and
   `arc port-forward`.
2. Wait for the Arc agent on the VM to discover the new endpoint and
   register a listener on the Azure Relay hybrid connection.

Step 2 typically takes 30–90 seconds. During this window every dial
returns HTTP 404 from Azure Relay because no listener is registered yet.
aztunnel logs a clear INFO line explaining what's happening and retries
with exponential backoff until the listener appears.

For `arc connect` the dial begins immediately, so the full sequence
appears on the same invocation:

```
INFO creating Arc HybridConnectivity configuration; the Arc agent may need a moment to register a relay listener
INFO waiting for Arc agent to register a relay listener (expected after creating or updating the HybridConnectivity configuration) status=404
INFO still waiting for Arc agent to register a relay listener elapsed=18s attempts=6
INFO arc relay connected elapsed=29s attempts=8 lastStatus=404
```

For `arc port-forward` the explanatory line is emitted up front, but
the `waiting for Arc agent...` / `arc relay connected` lines only fire
when the first inbound TCP connection arrives (the dial is per
inbound connection, not per `aztunnel` invocation):

```
INFO creating Arc HybridConnectivity configuration; the first forwarded connection may wait while the Arc agent registers a relay listener
INFO arc port-forward listening bind=127.0.0.1:2222 ...
# first ssh / curl through the listener:
INFO waiting for Arc agent to register a relay listener (expected after creating or updating the HybridConnectivity configuration) status=404
INFO arc relay connected elapsed=27s attempts=7 lastStatus=404
```

If SSH itself enforces a `ConnectTimeout` shorter than this window, the
first invocation may fail — re-running it usually succeeds because the
listener stays registered.

## Debugging

```sh
aztunnel arc connect --resource-id .../machines/myVM --log-level debug
```

| Symptom                             | Likely cause                                                        |
| ----------------------------------- | ------------------------------------------------------------------- |
| `credential request failed`         | Missing RBAC on the Arc resource, or `az login` expired             |
| `ensure hybrid connectivity failed` | `Microsoft.HybridConnectivity` provider not registered              |
| `no active listener`                | Arc agent not running on the target, or SSH extension not installed |
| `connection timed out`              | Target machine offline or agent unhealthy                           |
