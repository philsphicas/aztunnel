# Scenario: Kubernetes-to-Kubernetes Connectivity

Connect services across two Kubernetes clusters without VPN or network
peering. Each cluster runs an aztunnel sidecar — one as a listener, one as
a sender — and they meet in the middle through Azure Relay.

```
┌── Cluster A ──────────────┐       ┌──────────────┐       ┌── Cluster B ──────────────┐
│                           │       │              │       │                           │
│  ┌──────┐  ┌───────────┐  │       │ Azure Relay  │       │  ┌───────────┐  ┌──────┐  │
│  │ app  │  │ aztunnel  │  │       │              │       │  │ aztunnel  │  │ app  │  │
│  │      │──│ sender    │══════════│              │══════════│ listener  │──│      │  │
│  │      │  │ (sidecar) │  │  WSS  │              │  WSS  │  │ (sidecar) │  │ :80  │  │
│  └──────┘  └───────────┘  │       │              │       │  └───────────┘  └──────┘  │
│                           │       │              │       │                           │
└───────────────────────────┘       └──────────────┘       └───────────────────────────┘
```

## What you'll set up

| Component    | Cluster                                   | Configuration | Guide                                                   |
| ------------ | ----------------------------------------- | ------------- | ------------------------------------------------------- |
| **Listener** | Cluster B (where the target service runs) | K8s sidecar   | [Listener: K8s sidecar](listener-kubernetes-sidecar.md) |
| **Sender**   | Cluster A (where the client runs)         | K8s sidecar   | [Sender: K8s sidecar](sender-kubernetes-sidecar.md)     |

## Prerequisites

- Two Kubernetes clusters (e.g., two kind clusters)
- An Azure Relay namespace with a hybrid connection
- SAS keys (listener + sender) or Entra ID credentials

## 1. Set up the listener in Cluster B

Cluster B has the service you want to reach. Deploy it with an aztunnel
listener sidecar:

```sh
# Create listener credentials in Cluster B
kubectl --context kind-cluster-b create secret generic aztunnel-listener-creds \
  --from-literal=AZTUNNEL_RELAY_NAME=my-relay-ns \
  --from-literal=AZTUNNEL_KEY_NAME=listen-policy \
  --from-literal=AZTUNNEL_KEY='<listener-key>'

# Deploy the service + listener
kubectl --context kind-cluster-b apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: service-b
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
        - cross-cluster
        - --allow
        - "localhost:80"
      envFrom:
        - secretRef:
            name: aztunnel-listener-creds
EOF
```

## 2. Set up the sender in Cluster A

Cluster A has the application that needs to call the service in Cluster B.
Deploy it with an aztunnel sender sidecar that port-forwards to the remote
service:

```sh
# Create sender credentials in Cluster A
kubectl --context kind-cluster-a create secret generic aztunnel-sender-creds \
  --from-literal=AZTUNNEL_RELAY_NAME=my-relay-ns \
  --from-literal=AZTUNNEL_KEY_NAME=send-policy \
  --from-literal=AZTUNNEL_KEY='<sender-key>'

# Deploy the client app + sender
kubectl --context kind-cluster-a apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: app-a
spec:
  containers:
    - name: app
      image: curlimages/curl:latest
      command: ["sleep", "infinity"]

    - name: aztunnel
      image: ghcr.io/philsphicas/aztunnel:latest   # use aztunnel:dev for local builds
      imagePullPolicy: IfNotPresent
      command:
        - aztunnel
        - relay-sender
        - port-forward
        - localhost:80
        - --hyco
        - cross-cluster
        - --bind
        - "127.0.0.1:8080"
      envFrom:
        - secretRef:
            name: aztunnel-sender-creds
EOF
```

The sender forwards port 8080 in the pod to `localhost:80` on the listener
side (which is nginx in Cluster B).

## 3. Test the connection

From the app container in Cluster A:

```sh
kubectl --context kind-cluster-a exec app-a -c app -- curl -s http://localhost:8080
```

This request flows: app → aztunnel sender → Azure Relay → aztunnel listener → nginx.

## kind demo

A complete working example with two kind clusters:

```sh
# Pull aztunnel image
docker pull ghcr.io/philsphicas/aztunnel:latest

# Create two clusters and load the image
kind create cluster --name cluster-a
kind create cluster --name cluster-b
kind load docker-image ghcr.io/philsphicas/aztunnel:latest --name cluster-a
kind load docker-image ghcr.io/philsphicas/aztunnel:latest --name cluster-b

# Create credentials in both clusters
kubectl --context kind-cluster-b create secret generic aztunnel-listener-creds \
  --from-literal=AZTUNNEL_RELAY_NAME=my-relay-ns \
  --from-literal=AZTUNNEL_KEY_NAME=listen-policy \
  --from-literal=AZTUNNEL_KEY='<listener-key>'

kubectl --context kind-cluster-a create secret generic aztunnel-sender-creds \
  --from-literal=AZTUNNEL_RELAY_NAME=my-relay-ns \
  --from-literal=AZTUNNEL_KEY_NAME=send-policy \
  --from-literal=AZTUNNEL_KEY='<sender-key>'

# Deploy listener + nginx in Cluster B
kubectl --context kind-cluster-b apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: service-b
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
        - cross-cluster
        - --allow
        - "localhost:80"
      envFrom:
        - secretRef:
            name: aztunnel-listener-creds
EOF

# Deploy sender + curl client in Cluster A
kubectl --context kind-cluster-a apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: app-a
spec:
  containers:
    - name: app
      image: curlimages/curl:latest
      command: ["sleep", "infinity"]
    - name: aztunnel
      image: ghcr.io/philsphicas/aztunnel:latest   # use aztunnel:dev for local builds
      imagePullPolicy: IfNotPresent
      command:
        - aztunnel
        - relay-sender
        - port-forward
        - localhost:80
        - --hyco
        - cross-cluster
        - --bind
        - "127.0.0.1:8080"
      envFrom:
        - secretRef:
            name: aztunnel-sender-creds
EOF

# Wait for pods to be ready
kubectl --context kind-cluster-b wait --for=condition=Ready pod/service-b --timeout=60s
kubectl --context kind-cluster-a wait --for=condition=Ready pod/app-a --timeout=60s

# Test: curl from Cluster A reaches nginx in Cluster B
kubectl --context kind-cluster-a exec app-a -c app -- curl -s http://localhost:8080
```

## SOCKS5 variant

If the app in Cluster A needs to reach multiple services in Cluster B, use
SOCKS5 instead of port-forward:

```yaml
- name: aztunnel
  command:
    - aztunnel
    - relay-sender
    - socks5-proxy
    - --hyco
    - cross-cluster
    - --bind
    - "127.0.0.1:1080"
  envFrom:
    - secretRef:
        name: aztunnel-sender-creds
```

Then the app can reach any allowed target via the SOCKS5 proxy at
`localhost:1080`:

```sh
curl --socks5h localhost:1080 http://localhost:80       # nginx
curl --socks5h localhost:1080 http://localhost:9999     # echo server
```

## Bidirectional connectivity

If both clusters need to call each other, set up two hybrid connections —
one for each direction:

| Hybrid connection | Listener  | Sender    |
| ----------------- | --------- | --------- |
| `a-to-b`          | Cluster B | Cluster A |
| `b-to-a`          | Cluster A | Cluster B |

Each cluster runs both a listener sidecar and a sender sidecar, on different
hybrid connections.
