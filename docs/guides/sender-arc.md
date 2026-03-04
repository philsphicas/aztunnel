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
    StrictHostKeyChecking no
    UserKnownHostsFile /dev/null
    Hostname localhost
    ProxyCommand aztunnel arc connect --resource-id %n --port %p
```

Then connect using the resource ID directly:

```sh
ssh -p 22 user@/subscriptions/SUB/resourceGroups/RG/providers/Microsoft.HybridCompute/machines/myVM
```

`%n` passes the original hostname (the resource ID) to aztunnel, and `%p`
passes the port. `StrictHostKeyChecking no` is needed because the hostname
doesn't match a real DNS name.

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

## arc port-forward

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
testing. This uses `docker exec` to install the Arc agent inside the
kind node's container:

```sh
KIND_CLUSTER_NAME="arctest"
RESOURCE_GROUP="arctest-rg"
SUBSCRIPTION_ID="$(az account show -o json | jq -r .id)"
ACCESS_TOKEN="$(az account get-access-token -o json | jq -r .accessToken)"

# Create the kind cluster
kind create cluster --name "$KIND_CLUSTER_NAME"

# Install SSH + Arc agent inside the node
docker exec -i "${KIND_CLUSTER_NAME}-control-plane" bash <<SCRIPT
apt-get update && apt-get install -y wget openssh-server sudo
systemctl start ssh
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
RESOURCE_ID="/subscriptions/$SUBSCRIPTION_ID/resourceGroups/$RESOURCE_GROUP/providers/Microsoft.HybridCompute/machines/${KIND_CLUSTER_NAME}-control-plane"

ssh -o ProxyCommand="aztunnel arc connect --resource-id $RESOURCE_ID" root@arctest
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

Start the SSH SOCKS proxy as a background task:

```sh
bgtask run --name socks-over-arc -- \
  ssh -D 1080 -N \
    -o ProxyCommand="aztunnel arc connect --resource-id /subscriptions/SUB/resourceGroups/RG/providers/Microsoft.HybridCompute/machines/myVM" \
    user@myVM
```

Or with the wildcard SSH config from above:

```sh
bgtask run --name socks-over-arc -- \
  ssh -D 1080 -N -p 22 \
    user@/subscriptions/SUB/resourceGroups/RG/providers/Microsoft.HybridCompute/machines/myVM
```

Then use the proxy to reach anything on the VM's network:

```sh
curl --socks5h 127.0.0.1:1080 http://10.0.0.5:8080/api/health
curl --socks5h 127.0.0.1:1080 http://internal-db:5432
```

This is a quick way to get network access without provisioning a relay
namespace or deploying a listener — all you need is an Arc-enrolled VM
with SSH.

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
