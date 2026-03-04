# Scenario: Browse Internal Web Apps

Access internal dashboards, admin panels, and HTTP APIs from your laptop
through a SOCKS5 proxy. The listener runs alongside the web app (as a
Kubernetes sidecar or on a host), and you run a SOCKS5 proxy locally.

```
┌─────────────┐       ┌──────────────┐       ┌────────────────────────┐
│ Workstation  │       │ Azure Relay  │       │  ┌──────────────────┐  │
│              │       │              │       │  │ internal app     │  │
│  browser ────┤──────▶│              │◀──────┤──│ :3000            │  │
│  :1080       │  WSS  │              │  WSS  │  │                  │  │
│  (SOCKS5)    │       │              │       │  │ aztunnel listener│  │
│              │       │              │       │  └──────────────────┘  │
└─────────────┘       └──────────────┘       └────────────────────────┘
```

## What you'll set up

| Component    | Configuration                     | Guide                                                   |
| ------------ | --------------------------------- | ------------------------------------------------------- |
| **Listener** | K8s sidecar (or ad-hoc on a host) | [Listener: K8s sidecar](listener-kubernetes-sidecar.md) |
| **Sender**   | SOCKS5 proxy on your workstation  | [Sender: SOCKS5 proxy](sender-socks5-proxy.md)          |

## Prerequisites

- A running internal web app you want to reach
- An Azure Relay namespace with a hybrid connection
  ([setup guide](../azure-setup.md))
- Sender credentials (SAS key or Entra ID)

## 1. Deploy the listener

### Kubernetes sidecar

Deploy the listener alongside your web app:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: dashboard
spec:
  containers:
    - name: grafana
      image: grafana/grafana:latest
      ports:
        - containerPort: 3000

    - name: aztunnel
      image: ghcr.io/philsphicas/aztunnel:latest # use aztunnel:dev for local builds
      imagePullPolicy: IfNotPresent
      command:
        - aztunnel
        - relay-listener
        - --hyco
        - internal-apps
        - --allow
        - "localhost:3000"
      envFrom:
        - secretRef:
            name: aztunnel-creds
```

### Ad-hoc on a host

If the app runs directly on a host:

```sh
aztunnel relay-listener \
  --relay my-relay-ns \
  --hyco internal-apps \
  --allow "localhost:3000"
```

## 2. Start the SOCKS5 proxy on your workstation

```sh
export AZTUNNEL_RELAY_NAME=my-relay-ns
export AZTUNNEL_KEY_NAME=send-policy
export AZTUNNEL_KEY='<sender-key>'

aztunnel relay-sender socks5-proxy --hyco internal-apps -b 127.0.0.1:1080
```

## 3. Browse through the proxy

### Firefox (recommended)

1. Settings → Network Settings → Manual proxy configuration
2. SOCKS Host: `127.0.0.1`, Port: `1080`, SOCKS v5
3. Navigate to `http://localhost:3000`

### curl

```sh
curl --socks5h 127.0.0.1:1080 http://localhost:3000
```

### Chrome

```sh
google-chrome --proxy-server="socks5://127.0.0.1:1080"
```

## Multiple web apps

If you have several internal apps, allow them all in the listener:

```yaml
command:
  - aztunnel
  - relay-listener
  - --hyco
  - internal-apps
  - --allow
  - "localhost:3000" # Grafana
  - --allow
  - "localhost:8080" # Admin panel
  - --allow
  - "localhost:9090" # Prometheus
```

Then access each one through the same SOCKS5 proxy:

```sh
curl --socks5h 127.0.0.1:1080 http://localhost:3000   # Grafana
curl --socks5h 127.0.0.1:1080 http://localhost:8080   # Admin panel
curl --socks5h 127.0.0.1:1080 http://localhost:9090   # Prometheus
```

## kind demo

```sh
# Create cluster and load image
kind create cluster --name apps
docker pull ghcr.io/philsphicas/aztunnel:latest
kind load docker-image ghcr.io/philsphicas/aztunnel:latest --name apps

# Create credentials
kubectl --context kind-apps create secret generic aztunnel-creds \
  --from-literal=AZTUNNEL_RELAY_NAME=my-relay-ns \
  --from-literal=AZTUNNEL_KEY_NAME=listen-policy \
  --from-literal=AZTUNNEL_KEY='<listener-key>'

# Deploy nginx + aztunnel listener
kubectl --context kind-apps apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: web-app
spec:
  containers:
    - name: nginx
      image: nginx:alpine
      ports:
        - containerPort: 80
    - name: aztunnel
      image: ghcr.io/philsphicas/aztunnel:latest   # use aztunnel:dev for local builds
      imagePullPolicy: IfNotPresent
      command:
        - aztunnel
        - relay-listener
        - --hyco
        - internal-apps
        - --allow
        - "localhost:80"
      envFrom:
        - secretRef:
            name: aztunnel-creds
EOF

# On your workstation — start SOCKS5 proxy
export AZTUNNEL_KEY_NAME=send-policy
export AZTUNNEL_KEY='<sender-key>'
aztunnel relay-sender socks5-proxy --relay my-relay-ns --hyco internal-apps -b 127.0.0.1:1080

# Test
curl --socks5h 127.0.0.1:1080 http://localhost:80
```
