# Scenario: VNet Gateway

Use a single aztunnel listener as a tunnel gateway to an entire network. The
listener sits inside the network (VNet, subnet, on-prem LAN) and allows
connections to a range of targets. You run a SOCKS5 proxy on your workstation
to reach any of them dynamically.

```
┌─────────────┐       ┌──────────────┐       ┌──── Private Network ────────────┐
│ Workstation  │       │ Azure Relay  │       │                                 │
│              │       │              │       │  ┌──────────┐   ┌────────────┐  │
│  SOCKS5 ─────┤──────▶│              │◀──────┤──│ aztunnel │   │ 10.0.0.5   │  │
│  proxy       │  WSS  │              │  WSS  │  │ listener │──▶│ 10.0.0.6   │  │
│  :1080       │       │              │       │  │ (gateway)│   │ 10.0.0.7   │  │
│              │       │              │       │  └──────────┘   │ ...        │  │
└─────────────┘       └──────────────┘       └─────────────────────────────────┘
```

## What you'll set up

| Component    | Configuration                                | Guide                                                                                     |
| ------------ | -------------------------------------------- | ----------------------------------------------------------------------------------------- |
| **Listener** | Standalone pod or host in the target network | [Listener: K8s sidecar](listener-kubernetes-sidecar.md) or [systemd](listener-systemd.md) |
| **Sender**   | SOCKS5 proxy on your workstation             | [Sender: SOCKS5 proxy](sender-socks5-proxy.md)                                            |

## Key idea

Instead of one listener per service, deploy **one listener with a wide
allowlist** that can reach everything in the network. The sender uses SOCKS5
so you can choose the destination at connection time.

This trades granularity for simplicity — one tunnel covers the whole network.

## 1. Deploy the gateway listener

### Kubernetes (standalone pod)

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: gateway
spec:
  containers:
    - name: aztunnel
      image: ghcr.io/philsphicas/aztunnel:latest # use aztunnel:dev for local builds
      imagePullPolicy: IfNotPresent
      command:
        - aztunnel
        - relay-listener
        - --hyco
        - network-gateway
        - --allow
        - "10.0.0.0/8:*" # RFC 1918 private range
        - --allow
        - "172.16.0.0/12:*" # RFC 1918 private range
        - --allow
        - "192.168.0.0/16:*" # RFC 1918 private range
      envFrom:
        - secretRef:
            name: aztunnel-creds
```

### systemd (on a VM)

```ini
ExecStart=/opt/aztunnel/aztunnel relay-listener \
    --allow "10.0.0.0/8:*" \
    --allow "172.16.0.0/12:*"
```

### Azure Container Instance

See the
[Azure Relay Setup Guide](../azure-setup.md#2b-deploy-a-listener-as-an-azure-container-instance-vnet-integrated)
for deploying a VNet-integrated ACI listener that can reach everything in the
VNet and peered networks.

## 2. Start the SOCKS5 proxy

```sh
export AZTUNNEL_RELAY_NAME=my-relay-ns
export AZTUNNEL_KEY_NAME=send-policy
export AZTUNNEL_KEY='<sender-key>'

aztunnel relay-sender socks5-proxy --hyco network-gateway -b 127.0.0.1:1080
```

## 3. Access services

```sh
# Database
psql -h 10.0.0.5 -p 5432 -U myuser mydb \
  --set=PGSSLMODE=disable \
  -o "socks5://127.0.0.1:1080"

# HTTP APIs
curl --socks5h 127.0.0.1:1080 http://10.0.0.6:8080/api/health
curl --socks5h 127.0.0.1:1080 http://10.0.0.7:3000/dashboard

# SSH (via netcat through SOCKS5)
ssh -o ProxyCommand="nc -x 127.0.0.1:1080 %h %p" user@10.0.0.8

# Set as default proxy for all tools
export ALL_PROXY=socks5h://127.0.0.1:1080
curl http://10.0.0.6:8080/api/health
```

## Allowlist design

The gateway pattern uses wide CIDR ranges. Choose the scope that matches
your security needs:

| Scope              | Allowlist                         | Use case                       |
| ------------------ | --------------------------------- | ------------------------------ |
| Single subnet      | `10.0.1.0/24:*`                   | Reach one subnet, any port     |
| Restricted ports   | `10.0.0.0/8:22`, `10.0.0.0/8:443` | Any host, SSH + HTTPS only     |
| All private ranges | `10.0.0.0/8:*`, `172.16.0.0/12:*` | Broad access (common for dev)  |
| Everything         | `*`                               | No restrictions (testing only) |

> **Security**: The wider the allowlist, the more you rely on other controls
> (network segmentation, service-level auth) to limit access. In production,
> prefer tighter CIDR ranges.

## Gateway vs sidecar: when to use each

|                  | Gateway (one listener)                 | Sidecar (per-service)        |
| ---------------- | -------------------------------------- | ---------------------------- |
| **Targets**      | Many services behind one tunnel        | One specific service         |
| **Allowlist**    | Wide CIDR                              | Narrow (`localhost:PORT`)    |
| **Blast radius** | Larger — gateway can reach many hosts  | Smaller — limited to one pod |
| **Best for**     | Development, exploration, admin access | Production, least-privilege  |

## kind demo

```sh
# Create cluster with aztunnel + multiple services
kind create cluster --name gateway-demo
docker pull ghcr.io/philsphicas/aztunnel:latest
kind load docker-image ghcr.io/philsphicas/aztunnel:latest --name gateway-demo

kubectl --context kind-gateway-demo create secret generic aztunnel-creds \
  --from-literal=AZTUNNEL_RELAY_NAME=my-relay-ns \
  --from-literal=AZTUNNEL_KEY_NAME=listen-policy \
  --from-literal=AZTUNNEL_KEY='<listener-key>'

# Deploy gateway listener + two services
kubectl --context kind-gateway-demo apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: gateway
spec:
  containers:
    - name: nginx
      image: nginx:alpine
      ports:
        - containerPort: 80
    - name: echo
      image: alpine/socat:latest
      args: ["TCP-LISTEN:9999,fork,reuseaddr", "EXEC:cat"]
      ports:
        - containerPort: 9999
    - name: aztunnel
      image: ghcr.io/philsphicas/aztunnel:latest   # use aztunnel:dev for local builds
      imagePullPolicy: IfNotPresent
      command:
        - aztunnel
        - relay-listener
        - --hyco
        - gateway
        - --allow
        - "localhost:80"
        - --allow
        - "localhost:9999"
      envFrom:
        - secretRef:
            name: aztunnel-creds
EOF

# From your workstation
export AZTUNNEL_KEY_NAME=send-policy
export AZTUNNEL_KEY='<sender-key>'
aztunnel relay-sender socks5-proxy --relay my-relay-ns --hyco gateway -b 127.0.0.1:1080

# Access both services through one proxy
curl --socks5h 127.0.0.1:1080 http://localhost:80
echo "hello" | nc -x 127.0.0.1:1080 localhost 9999
```
