# Listener: systemd Unit

Run the aztunnel listener as a systemd service on a Linux VM. This is the
standard production deployment for VMs — the listener starts on boot,
restarts on failure, and authenticates via managed identity.

## Prerequisites

- A Linux VM with outbound internet access (no public IP required)
- An Azure Relay namespace with a hybrid connection
  ([setup guide](../azure-setup.md))
- Entra ID with managed identity (recommended) or SAS keys

## Install aztunnel

```sh
sudo mkdir -p /opt/aztunnel
sudo curl -fsSL \
  https://github.com/philsphicas/aztunnel/releases/latest/download/aztunnel-linux-amd64 \
  -o /opt/aztunnel/aztunnel
sudo chmod +x /opt/aztunnel/aztunnel
sudo useradd --system --no-create-home --shell /usr/sbin/nologin aztunnel
```

## Environment file

Store deployment-specific values in an env file. The systemd unit reads
from this file so it never needs to change per deployment:

**`/opt/aztunnel/env`**:

```sh
AZTUNNEL_RELAY_NAME=my-relay-ns
AZTUNNEL_HYCO_NAME=my-tunnel
```

For SAS keys, add:

```sh
AZTUNNEL_KEY_NAME=listen-policy
AZTUNNEL_KEY=<your-sas-key>
```

> **Production**: Use Entra ID with managed identity instead of SAS keys.
> The env file only needs the relay and hybrid connection names.

## systemd unit

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
    --allow "localhost:22" \
    --log-level info
Restart=on-failure
RestartSec=5s

# Memory limit — aztunnel auto-tunes GOMEMLIMIT to 90% of this value
MemoryMax=256M

# Security hardening
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

Enable and start:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now aztunnel-listener
```

## Customizing the allowlist

The `--allow` flags in `ExecStart` control what the listener can reach. Adjust
for your use case:

| Use case                 | `--allow` value                  |
| ------------------------ | -------------------------------- |
| SSH only                 | `127.0.0.1:22`, `localhost:22`   |
| SSH + web app            | `127.0.0.1:22`, `127.0.0.1:8080` |
| Any local port           | `127.0.0.1:*`                    |
| Gateway to local network | `10.0.0.0/24:*`                  |

Edit the unit file and reload:

```sh
sudo systemctl daemon-reload
sudo systemctl restart aztunnel-listener
```

## Memory management

The `MemoryMax=256M` directive sets a cgroup memory limit. aztunnel detects
this and automatically sets `GOMEMLIMIT` to 90% of the limit (via
[automemlimit](https://github.com/KimMachineGun/automemlimit)), so the Go
garbage collector makes informed decisions and avoids OOM kills.

For busier deployments, increase `MemoryMax`:

```ini
MemoryMax=512M    # handles more concurrent connections
```

To override the auto-tuning ratio:

```sh
# In /opt/aztunnel/env:
AUTOMEMLIMIT=0.8    # use 80% instead of 90%
```

## Monitoring

Check status and logs:

```sh
# Service status
sudo systemctl status aztunnel-listener

# Follow logs
sudo journalctl -u aztunnel-listener -f

# Recent errors only
sudo journalctl -u aztunnel-listener -p err --since "1 hour ago"
```

### Prometheus metrics

Add `--metrics-addr` to expose metrics:

```ini
ExecStart=/opt/aztunnel/aztunnel relay-listener \
    --allow "127.0.0.1:22" \
    --metrics-addr :9090
```

Scrape `http://<vm-ip>:9090/metrics` from Prometheus. See the
[README](../../README.md#metrics) for the full list of available metrics.

## Automated deployment with cloud-init

For VM provisioning (ARM, Bicep, Terraform), use cloud-init to install
aztunnel at boot. See the
[Azure Relay Setup Guide](../azure-setup.md#2a-deploy-a-listener-on-an-azure-vm-with-managed-identity)
for a complete cloud-init example with managed identity.

The key idea: the systemd unit is generic (same on every VM), and the env
file at `/opt/aztunnel/env` is the only thing that changes per deployment.
This makes it easy to template in infrastructure-as-code tools.
