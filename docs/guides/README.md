# aztunnel Guides

Guides for deploying and using aztunnel in common scenarios. Each guide is
self-contained — pick the one that matches what you're trying to do.

> **New to aztunnel?** Start with the [README](../../README.md) for
> installation and the [Azure Relay Setup Guide](../azure-setup.md) for
> provisioning your relay namespace.

## How the guides are organized

aztunnel has two sides: a **listener** (runs where the target service lives)
and a **sender** (runs where you are). Each side can be deployed in several
ways. The configuration guides below cover each deployment mode independently.
The scenario guides pair a listener mode with a sender mode to solve a
concrete problem end-to-end.

## Configuration guides

### Listener (remote side)

| Guide                                                | Description                                                    |
| ---------------------------------------------------- | -------------------------------------------------------------- |
| [Kubernetes sidecar](listener-kubernetes-sidecar.md) | Run the listener as a sidecar container alongside your service |
| [systemd unit](listener-systemd.md)                  | Run the listener as a systemd service on a VM                  |
| [Ad-hoc](listener-ad-hoc.md)                         | Run the listener in the foreground for testing and demos       |

### Sender (your side)

| Guide                                              | Description                                                                 |
| -------------------------------------------------- | --------------------------------------------------------------------------- |
| [SSH ProxyCommand](sender-ssh-proxycommand.md)     | Use `relay-sender connect` as an SSH ProxyCommand for transparent tunneling |
| [SOCKS5 proxy](sender-socks5-proxy.md)             | Run a local SOCKS5 proxy for dynamic target selection                       |
| [Port forward](sender-port-forward.md)             | Bind a local port and forward to a fixed remote target                      |
| [Kubernetes sidecar](sender-kubernetes-sidecar.md) | Run the sender as a sidecar container alongside your app                    |
| [Ad-hoc](sender-ad-hoc.md)                         | Run the sender in the foreground for testing and demos                      |
| [Azure Arc](sender-arc.md)                         | Connect to Arc-enrolled machines through automatically provisioned relays   |

## Scenario guides

Each scenario pairs a listener configuration with a sender configuration and
walks through the end-to-end setup.

| Scenario                                                            | Listener                                      | Sender                                      | Description                                                              |
| ------------------------------------------------------------------- | --------------------------------------------- | ------------------------------------------- | ------------------------------------------------------------------------ |
| [SSH into a private VM](scenario-ssh-private-vm.md)                 | [systemd](listener-systemd.md)                | [ProxyCommand](sender-ssh-proxycommand.md)  | SSH to a VM with no public IP, using `~/.ssh/config` for seamless access |
| [Browse internal web apps](scenario-browse-internal-apps.md)        | [K8s sidecar](listener-kubernetes-sidecar.md) | [SOCKS5](sender-socks5-proxy.md)            | Access dashboards and internal HTTP APIs from your laptop                |
| [kubectl to a private cluster](scenario-kubectl-private-cluster.md) | [K8s sidecar](listener-kubernetes-sidecar.md) | [port forward](sender-port-forward.md)      | Reach a private Kubernetes API server without a VPN                      |
| [VNet gateway](scenario-vnet-gateway.md)                            | [K8s sidecar](listener-kubernetes-sidecar.md) | [SOCKS5](sender-socks5-proxy.md)            | One listener as a tunnel gateway to an entire network                    |
| [Kubernetes-to-Kubernetes](scenario-k8s-to-k8s.md)                  | [K8s sidecar](listener-kubernetes-sidecar.md) | [K8s sidecar](sender-kubernetes-sidecar.md) | Cross-cluster connectivity without VPN or peering                        |
| [Multi-hop tunneling](scenario-multi-hop.md)                        | [systemd](listener-systemd.md)                | [SOCKS5](sender-socks5-proxy.md)            | Chain aztunnel with SSH jump hosts for complex topologies                |

## [FAQ](faq.md)

Common questions about aztunnel, including bgtask for background tasks,
Entra ID vs SAS keys, hybrid connection design, and monitoring.
