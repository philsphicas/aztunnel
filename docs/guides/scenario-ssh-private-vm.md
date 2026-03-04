# Scenario: SSH into a Private VM

SSH to a VM that has **no public IP** and no inbound firewall rules. The
listener runs as a systemd service on the VM, and you use SSH ProxyCommand
on your workstation.

```
┌────────────┐       ┌────────────────────┐       ┌─────────────────┐
│ Workstation│       │    Azure Relay     │       │   Private VM    │
│            │       │                    │       │  (no public IP) │
│  ssh       │──────▶│                    │◀──────│  aztunnel       │
│            │ WSS   │                    │  WSS  │  listener       │
└────────────┘       └────────────────────┘       │  sshd :22       │
                                                  └─────────────────┘
```

Both sides connect **outbound** to Azure Relay — no inbound ports needed
on either side.

## What you'll set up

| Component    | Configuration                        | Guide                                                  |
| ------------ | ------------------------------------ | ------------------------------------------------------ |
| **Listener** | systemd unit on the VM               | [Listener: systemd](listener-systemd.md)               |
| **Sender**   | SSH ProxyCommand on your workstation | [Sender: SSH ProxyCommand](sender-ssh-proxycommand.md) |

## Prerequisites

- An Azure Relay namespace with a hybrid connection
  ([setup guide](../azure-setup.md))
- A VM with outbound internet access (no public IP required)
- Entra ID with RBAC roles (recommended) or SAS keys

## 1. Deploy the listener on the VM

Install aztunnel and run it as a systemd service. The listener only needs to
forward to the local SSH server:

**`/opt/aztunnel/env`**:

```sh
AZTUNNEL_RELAY_NAME=my-relay-ns
AZTUNNEL_HYCO_NAME=my-tunnel
```

**`/etc/systemd/system/aztunnel-listener.service`**:

```ini
[Unit]
Description=aztunnel relay listener
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=aztunnel
Group=aztunnel
EnvironmentFile=/opt/aztunnel/env
ExecStart=/opt/aztunnel/aztunnel relay-listener \
    --allow "127.0.0.1:22" \
    --allow "localhost:22"
Restart=on-failure
RestartSec=5s
MemoryMax=256M
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now aztunnel-listener
```

The allowlist is locked down to SSH on localhost only. The VM authenticates
to Azure Relay via its managed identity — no secrets to manage.

> For automated VM provisioning with cloud-init, see the
> [Azure Relay Setup Guide](../azure-setup.md#2a-deploy-a-listener-on-an-azure-vm-with-managed-identity).

## 2. Grant RBAC roles

The VM's managed identity needs **Azure Relay Listener**. Your user account
needs **Azure Relay Sender**:

```sh
HYCO_ID=$(az relay hyco show \
  --resource-group rg-aztunnel \
  --namespace-name my-relay-ns \
  --name my-tunnel \
  --query id -o json | jq -r .)

# Listener: VM's managed identity
VM_IDENTITY=$(az vm show \
  --resource-group rg-aztunnel \
  --name my-vm \
  --query identity.principalId -o json | jq -r .)

az role assignment create \
  --assignee-object-id "$VM_IDENTITY" \
  --assignee-principal-type ServicePrincipal \
  --role "Azure Relay Listener" \
  --scope "$HYCO_ID"

# Sender: your user account
az role assignment create \
  --assignee "<your-email@example.com>" \
  --role "Azure Relay Sender" \
  --scope "$HYCO_ID"
```

## 3. SSH from your workstation

Make sure you're logged in with `az login`, then SSH through the relay:

```sh
ssh -o ProxyCommand="aztunnel relay-sender connect --relay my-relay-ns --hyco my-tunnel %h:%p" azureuser@127.0.0.1
```

> The target address `127.0.0.1:22` is what the **listener** dials — it
> resolves on the VM, not on your workstation.

## 4. Add to `~/.ssh/config`

For seamless access, add an entry to your SSH config:

```
Host my-vm
    HostName 127.0.0.1
    User azureuser
    ProxyCommand aztunnel relay-sender connect --relay my-relay-ns --hyco my-tunnel %h:%p
```

Now you can just type:

```sh
ssh my-vm
scp my-vm:~/data.csv .
rsync -avz my-vm:/var/log/app/ ./logs/
```

### Multiple VMs with separate hybrid connections

If each VM has its own hybrid connection (recommended):

```
Host vm-web
    HostName 127.0.0.1
    User azureuser
    ProxyCommand aztunnel relay-sender connect --relay my-relay-ns --hyco tunnel-web %h:%p

Host vm-db
    HostName 127.0.0.1
    User azureuser
    ProxyCommand aztunnel relay-sender connect --relay my-relay-ns --hyco tunnel-db %h:%p
```

## Verifying the setup

On the VM:

```sh
sudo systemctl status aztunnel-listener
sudo journalctl -u aztunnel-listener -f
```

You should see the listener connect to Azure Relay and log incoming
connections as they arrive.

From your workstation:

```sh
# Quick connectivity test
ssh -v -o ProxyCommand="aztunnel relay-sender connect --log-level debug %h:%p" azureuser@127.0.0.1
```

The `-v` flag on SSH and `--log-level debug` on aztunnel give you full
visibility into both sides of the connection.
