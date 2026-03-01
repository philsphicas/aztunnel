# Azure Relay Setup Guide

This guide walks through provisioning Azure Relay resources and deploying
aztunnel with proper authentication. It covers two auth methods:

| Method                                        | Best for                                  | How it works                                                    |
| --------------------------------------------- | ----------------------------------------- | --------------------------------------------------------------- |
| **Entra ID + Managed Identity** (recommended) | VMs, containers, CI/CD                    | Azure assigns an identity to your compute; no secrets to manage |
| **SAS keys**                                  | Quick testing, environments without Entra | Shared secret                                                   |

---

## 1. Provision Azure Relay resources

All examples use the Azure CLI. Adjust names and regions as needed.

```bash
# Variables — change these
RESOURCE_GROUP="rg-aztunnel"
LOCATION="eastus2"
RELAY_NAMESPACE="my-relay-ns"
HYCO_NAME="my-tunnel"

# Create a resource group
az group create \
  --name "$RESOURCE_GROUP" \
  --location "$LOCATION"

# Create a Relay namespace
az relay namespace create \
  --resource-group "$RESOURCE_GROUP" \
  --name "$RELAY_NAMESPACE" \
  --location "$LOCATION"

# Create a Hybrid Connection (no client auth required — aztunnel handles auth)
az relay hyco create \
  --resource-group "$RESOURCE_GROUP" \
  --namespace-name "$RELAY_NAMESPACE" \
  --name "$HYCO_NAME" \
  --requires-client-authorization false
```

Your relay endpoint (FQDN) is:

```
<RELAY_NAMESPACE>.servicebus.windows.net
```

### Hybrid connections and multiple listeners

If multiple listeners connect to the **same** hybrid connection, Azure Relay
distributes incoming connections across them in round-robin fashion. This is
useful for load-balanced services (e.g., a pool of identical web servers) but
**not** for reaching a specific host.

For the common case of accessing individual VMs, create **one hybrid connection
per VM**:

```bash
az relay hyco create --resource-group "$RESOURCE_GROUP" --namespace-name "$RELAY_NAMESPACE" --name "tunnel-vm1" --requires-client-authorization false
az relay hyco create --resource-group "$RESOURCE_GROUP" --namespace-name "$RELAY_NAMESPACE" --name "tunnel-vm2" --requires-client-authorization false
az relay hyco create --resource-group "$RESOURCE_GROUP" --namespace-name "$RELAY_NAMESPACE" --name "tunnel-vm3" --requires-client-authorization false
```

Then each VM's listener specifies its own hybrid connection name.

> **Platform limits**: A single hybrid connection supports up to 25 concurrent
> listeners. This is a hard Azure limit, not configurable. A namespace can
> contain many hybrid connections.

---

## 2. Authentication with Entra ID (recommended)

Entra ID authentication uses Azure RBAC — no shared secrets, automatic token
rotation, and fine-grained access control. aztunnel uses
[`DefaultAzureCredential`](https://learn.microsoft.com/en-us/azure/developer/go/azure-sdk-authentication),
which automatically picks up managed identities, Azure CLI sessions, workload
identity, and other credential sources.

### Azure Relay RBAC roles

| Role                     | Permissions                                 | Use for                                 |
| ------------------------ | ------------------------------------------- | --------------------------------------- |
| **Azure Relay Listener** | Accept connections on hybrid connections    | Listener side                           |
| **Azure Relay Sender**   | Send connections through hybrid connections | Sender/client side                      |
| **Azure Relay Owner**    | Full control (listen + send + manage)       | Automation, not recommended for runtime |

Always assign the **minimum required role**. The listener only needs `Azure
Relay Listener`; the sender only needs `Azure Relay Sender`.

### 2a. Deploy a listener on an Azure VM with Managed Identity

This is the recommended production setup. The VM has **no public IP** — it
sits in a private VNet with no inbound ports open. aztunnel is installed at
provisioning time via cloud-init and runs as a systemd service. The VM's
managed identity authenticates to Azure Relay automatically — no secrets to
manage.

#### Create a cloud-init file

Save this as `cloud-init-aztunnel.yaml`. It installs the aztunnel binary,
writes an environment file with deployment-specific values, and starts the
listener on first boot. The systemd unit itself is completely generic — only
the environment file changes per deployment:

```yaml
#cloud-config
write_files:
  - path: /opt/aztunnel/env
    content: |
      AZTUNNEL_RELAY_NAME=my-relay-ns
      AZTUNNEL_HYCO_NAME=my-tunnel

  - path: /etc/systemd/system/aztunnel-listener.service
    content: |
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
          --allow "10.0.0.0/8:*" \
          --allow "172.16.0.0/12:*" \
          --log-level info
      Restart=on-failure
      RestartSec=5s

      # Memory limit — aztunnel auto-tunes GOMEMLIMIT to 90% of this value
      MemoryMax=512M

      # Security hardening
      NoNewPrivileges=true
      ProtectSystem=strict
      ProtectHome=true
      PrivateTmp=true

      [Install]
      WantedBy=multi-user.target

runcmd:
  - mkdir -p /opt/aztunnel
  - curl -fsSL https://github.com/philsphicas/aztunnel/releases/latest/download/aztunnel-linux-amd64 -o /opt/aztunnel/aztunnel
  - chmod +x /opt/aztunnel/aztunnel
  - useradd --system --no-create-home --shell /usr/sbin/nologin aztunnel
  - systemctl daemon-reload
  - systemctl enable --now aztunnel-listener
```

The `AZTUNNEL_RELAY_NAME` and `AZTUNNEL_HYCO_NAME` environment variables are read
from `/opt/aztunnel/env`. The systemd unit never needs to change — only the
env file contains deployment-specific values, which makes templating in
ARM/Bicep straightforward.

#### Create the VM (no public IP)

```bash
VM_NAME="aztunnel-listener"

az vm create \
  --resource-group "$RESOURCE_GROUP" \
  --name "$VM_NAME" \
  --image Ubuntu2404 \
  --size Standard_B1s \
  --admin-username azureuser \
  --generate-ssh-keys \
  --assign-identity '[system]' \
  --public-ip-address "" \
  --custom-data cloud-init-aztunnel.yaml
```

The VM has no public IP and no inbound NSG rules. The only way to reach it is
through the relay.

#### Grant the VM's identity "Azure Relay Listener" on the hybrid connection

```bash
# Get the VM's system-assigned identity principal ID
VM_IDENTITY=$(az vm show \
  --resource-group "$RESOURCE_GROUP" \
  --name "$VM_NAME" \
  --query identity.principalId \
  --output tsv)

# Get the hybrid connection resource ID
HYCO_ID=$(az relay hyco show \
  --resource-group "$RESOURCE_GROUP" \
  --namespace-name "$RELAY_NAMESPACE" \
  --name "$HYCO_NAME" \
  --query id \
  --output tsv)

# Assign the role
az role assignment create \
  --assignee-object-id "$VM_IDENTITY" \
  --assignee-principal-type ServicePrincipal \
  --role "Azure Relay Listener" \
  --scope "$HYCO_ID"
```

After cloud-init completes (~1–2 minutes), the listener connects outbound to
Azure Relay and is ready to accept tunneled connections. The managed identity
is automatically available via the Instance Metadata Service (IMDS) — no
environment variables or config files needed.

#### Connect from your workstation

On your workstation, make sure you're logged in with `az login` and have the
**Azure Relay Sender** role:

```bash
# Grant yourself Sender access (one-time setup)
az role assignment create \
  --assignee "<your-email@example.com>" \
  --role "Azure Relay Sender" \
  --scope "$HYCO_ID"

# SSH into the VM through the relay — no public IP needed
export AZTUNNEL_RELAY_NAME="my-relay-ns"
export AZTUNNEL_HYCO_NAME="my-tunnel"
ssh -o ProxyCommand="aztunnel relay-sender connect %h:%p" azureuser@10.0.0.5
```

Or use port forwarding:

```bash
aztunnel relay-sender port-forward --hyco my-tunnel 10.0.0.5:22 -b 127.0.0.1:2222
ssh -p 2222 azureuser@127.0.0.1
```

No public IPs, no inbound firewall rules, no secrets — both sides authenticate
with Entra ID tokens automatically.

### 2b. Deploy a listener as an Azure Container Instance (VNet-integrated)

This is the lightest-weight option: a single container in a VNet subnet that
acts as a tunnel gateway to everything the VNet can reach — VMs, databases,
peered VNets, on-prem via ExpressRoute. No VM to manage, no OS patching, and
ACI bills per-second so an idle listener costs almost nothing.

#### Create a VNet and subnet for ACI

```bash
VNET_NAME="vnet-aztunnel"
ACI_SUBNET="snet-aztunnel-aci"

az network vnet create \
  --resource-group "$RESOURCE_GROUP" \
  --name "$VNET_NAME" \
  --address-prefix 10.0.0.0/16 \
  --subnet-name "$ACI_SUBNET" \
  --subnet-prefix 10.0.1.0/24

# Delegate the subnet to ACI
az network vnet subnet update \
  --resource-group "$RESOURCE_GROUP" \
  --vnet-name "$VNET_NAME" \
  --name "$ACI_SUBNET" \
  --delegations Microsoft.ContainerInstance/containerGroups
```

If you already have a VNet, create a dedicated subnet with the ACI delegation
and use that instead.

#### Deploy the container

```bash
ACI_NAME="aztunnel-listener"

az container create \
  --resource-group "$RESOURCE_GROUP" \
  --name "$ACI_NAME" \
  --image ghcr.io/philsphicas/aztunnel:latest \
  --assign-identity '[system]' \
  --vnet "$VNET_NAME" \
  --subnet "$ACI_SUBNET" \
  --command-line "aztunnel relay-listener --allow '10.0.0.0/8:*' --allow '172.16.0.0/12:*'" \
  --environment-variables \
    AZTUNNEL_RELAY_NAME="$RELAY_NAMESPACE" \
    AZTUNNEL_HYCO_NAME="$HYCO_NAME" \
  --restart-policy Always \
  --cpu 0.5 \
  --memory 0.5
```

#### Grant the container's identity "Azure Relay Listener"

```bash
ACI_IDENTITY=$(az container show \
  --resource-group "$RESOURCE_GROUP" \
  --name "$ACI_NAME" \
  --query identity.principalId \
  --output tsv)

az role assignment create \
  --assignee-object-id "$ACI_IDENTITY" \
  --assignee-principal-type ServicePrincipal \
  --role "Azure Relay Listener" \
  --scope "$HYCO_ID"
```

#### Connect from your workstation

```bash
export AZTUNNEL_RELAY_NAME="$RELAY_NAMESPACE"
ssh -o ProxyCommand="aztunnel relay-sender connect --hyco $HYCO_NAME %h:%p" user@10.0.0.5
```

The container can reach anything in the VNet (and peered VNets, on-prem
networks connected via ExpressRoute or VPN Gateway). It has no public IP and
no inbound ports — all communication goes outbound through Azure Relay.

> **Outbound access**: The container needs outbound HTTPS to
> `*.servicebus.windows.net` to connect to Azure Relay. A
> [NAT gateway](https://learn.microsoft.com/en-us/azure/nat-gateway/nat-overview)
> on the ACI subnet is recommended, as Azure is retiring default outbound
> access for VNet resources. Add one with:
>
> ```bash
> az network public-ip create --resource-group "$RESOURCE_GROUP" \
>   --name pip-nat-aztunnel --sku Standard
> az network nat gateway create --resource-group "$RESOURCE_GROUP" \
>   --name nat-aztunnel --public-ip-addresses pip-nat-aztunnel
> az network vnet subnet update --resource-group "$RESOURCE_GROUP" \
>   --vnet-name "$VNET_NAME" --name "$ACI_SUBNET" --nat-gateway nat-aztunnel
> ```

### 2c. Entra ID for development (Azure CLI)

For local development and testing, `DefaultAzureCredential` picks up your
`az login` session:

```bash
az login
export AZTUNNEL_RELAY_NAME="my-relay-ns"
export AZTUNNEL_HYCO_NAME="my-tunnel"

# As long as you have the appropriate role (Listener or Sender), it just works
aztunnel relay-listener --allow "10.0.0.5:22"
```

### 2d. Entra ID with a Service Principal

For containers or CI/CD environments without managed identity:

```bash
export AZURE_TENANT_ID="<tenant-id>"
export AZURE_CLIENT_ID="<app-id>"
export AZURE_CLIENT_SECRET="<secret>"
export AZTUNNEL_RELAY_NAME="my-relay-ns"
export AZTUNNEL_HYCO_NAME="my-tunnel"

aztunnel relay-listener
```

`DefaultAzureCredential` picks up these environment variables automatically.

---

## 3. Authentication with SAS keys

SAS (Shared Access Signature) keys are a simpler alternative when Entra ID
isn't available. They work anywhere but require manual secret management.

### SAS key scoping

Azure Relay supports access policies at two levels:

| Level                 | Scope                                   | Use case                                            |
| --------------------- | --------------------------------------- | --------------------------------------------------- |
| **Namespace**         | All hybrid connections in the namespace | Never use `RootManageSharedAccessKey` in production |
| **Hybrid Connection** | Single hybrid connection only           | ✅ Always prefer this                               |

Each policy has one or more **claims**:

| Claim      | Allows                                               |
| ---------- | ---------------------------------------------------- |
| **Listen** | Accepting connections (listener side)                |
| **Send**   | Creating connections (sender side)                   |
| **Manage** | Creating/deleting hybrid connections + Listen + Send |

**Best practice**: Create one policy per hybrid connection per role — a
`listen` policy with only the Listen claim and a `send` policy with only the
Send claim.

### Create hybrid connection–level SAS policies

```bash
# Create a Listen-only policy for the listener
az relay hyco authorization-rule create \
  --resource-group "$RESOURCE_GROUP" \
  --namespace-name "$RELAY_NAMESPACE" \
  --hybrid-connection-name "$HYCO_NAME" \
  --name "listen-policy" \
  --rights Listen

# Create a Send-only policy for the sender
az relay hyco authorization-rule create \
  --resource-group "$RESOURCE_GROUP" \
  --namespace-name "$RELAY_NAMESPACE" \
  --hybrid-connection-name "$HYCO_NAME" \
  --name "send-policy" \
  --rights Send

# Retrieve the keys
az relay hyco authorization-rule keys list \
  --resource-group "$RESOURCE_GROUP" \
  --namespace-name "$RELAY_NAMESPACE" \
  --hybrid-connection-name "$HYCO_NAME" \
  --name "listen-policy"

az relay hyco authorization-rule keys list \
  --resource-group "$RESOURCE_GROUP" \
  --namespace-name "$RELAY_NAMESPACE" \
  --hybrid-connection-name "$HYCO_NAME" \
  --name "send-policy"
```

Each policy has a **primary key** and a **secondary key** — use either one.

### Use SAS keys with aztunnel

```bash
# Listener side
export AZTUNNEL_RELAY_NAME="my-relay-ns"
export AZTUNNEL_HYCO_NAME="my-tunnel"
export AZTUNNEL_KEY_NAME="listen-policy"
export AZTUNNEL_KEY="<primary-or-secondary-key>"

aztunnel relay-listener --allow "10.0.0.5:22"

# Sender side (different terminal / machine)
export AZTUNNEL_RELAY_NAME="my-relay-ns"
export AZTUNNEL_HYCO_NAME="my-tunnel"
export AZTUNNEL_KEY_NAME="send-policy"
export AZTUNNEL_KEY="<primary-or-secondary-key>"

aztunnel relay-sender port-forward 10.0.0.5:22 -b 127.0.0.1:2222
```

---

## 4. Cleanup

```bash
# Delete everything
az group delete --name "$RESOURCE_GROUP" --yes --no-wait
```
