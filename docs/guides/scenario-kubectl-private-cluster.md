# Scenario: kubectl to a Private Cluster

Access a private Kubernetes API server from your workstation — no VPN, no
public endpoint. The listener runs as a pod in the target cluster and
forwards to the API server. You run a port-forward on your laptop.

```
┌─────────────┐       ┌──────────────┐       ┌──── Target Cluster ─────────────┐
│ Workstation  │       │ Azure Relay  │       │  ┌─────────────────┐            │
│              │       │              │       │  │  aztunnel        │            │
│  kubectl ────┤──────▶│              │◀──────┤──│  listener        │            │
│  :6443       │  WSS  │              │  WSS  │  │  (sidecar)       ├───▶ :6443  │
│              │       │              │       │  └─────────────────┘   API server│
└─────────────┘       └──────────────┘       └─────────────────────────────────┘
```

## What you'll set up

| Component    | Configuration                     | Guide                                                   |
| ------------ | --------------------------------- | ------------------------------------------------------- |
| **Listener** | K8s sidecar in the target cluster | [Listener: K8s sidecar](listener-kubernetes-sidecar.md) |
| **Sender**   | Port forward on your workstation  | [Sender: port forward](sender-port-forward.md)          |

## Prerequisites

- A Kubernetes cluster with a private API server
- An Azure Relay namespace with a hybrid connection
  ([setup guide](../azure-setup.md))
- `kubectl` access to the target cluster (for initial deployment)
- Sender credentials (SAS key or Entra ID)

## 1. Deploy the listener in the target cluster

The listener needs to reach the API server. In most clusters, the API server
is available at `kubernetes.default.svc:443` or via the cluster IP.

```sh
# Create credentials
kubectl create secret generic aztunnel-creds \
  --from-literal=AZTUNNEL_RELAY_NAME=my-relay-ns \
  --from-literal=AZTUNNEL_KEY_NAME=listen-policy \
  --from-literal=AZTUNNEL_KEY='<listener-sas-key>'
```

Deploy a pod with the listener. The allowlist is locked down to the API
server endpoint only:

```sh
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: kube-tunnel
spec:
  containers:
    - name: aztunnel
      image: ghcr.io/philsphicas/aztunnel:latest
      imagePullPolicy: IfNotPresent
      command:
        - aztunnel
        - relay-listener
        - --hyco
        - kube-api
        - --allow
        - "kubernetes.default.svc:443"
      envFrom:
        - secretRef:
            name: aztunnel-creds
EOF

kubectl logs kube-tunnel -f
```

> **Alternative target addresses**: Some clusters expose the API server at
> `10.96.0.1:443` (the default ClusterIP). You can use either the DNS name
> or the IP in the allowlist. Check with: `kubectl get svc kubernetes`

## 2. Forward the API server port

On your workstation:

```sh
export AZTUNNEL_RELAY_NAME=my-relay-ns
export AZTUNNEL_KEY_NAME=send-policy
export AZTUNNEL_KEY='<sender-sas-key>'

aztunnel relay-sender port-forward \
  --hyco kube-api \
  --bind 127.0.0.1:6443 \
  kubernetes.default.svc:443
```

## 3. Configure kubectl

Point kubectl at the forwarded port. The easiest way is to modify (or
duplicate) your kubeconfig:

```sh
# Copy your existing config for the target cluster
kubectl config view --minify --flatten > /tmp/tunnel-kubeconfig.yaml
```

Edit the `server` field to point at your local forward:

```yaml
clusters:
  - cluster:
      server: https://127.0.0.1:6443
      # keep the existing certificate-authority-data
```

> **TLS**: The API server's TLS certificate won't match `127.0.0.1`. The
> preferred fix is to add the API server's CA to the kubeconfig (for kind
> clusters, the CA is already included). As a last resort for testing,
> `insecure-skip-tls-verify: true` disables certificate validation — do
> not use this in production as it allows man-in-the-middle attacks.

Use the modified kubeconfig:

```sh
export KUBECONFIG=/tmp/tunnel-kubeconfig.yaml
kubectl get nodes
```

## kind demo

A complete example using two kind clusters:

```sh
# Create the "remote" cluster
kind create cluster --name remote

# Build and load aztunnel
docker pull ghcr.io/philsphicas/aztunnel:latest
kind load docker-image ghcr.io/philsphicas/aztunnel:latest --name remote

# Create listener credentials
kubectl --context kind-remote create secret generic aztunnel-creds \
  --from-literal=AZTUNNEL_RELAY_NAME=my-relay-ns \
  --from-literal=AZTUNNEL_KEY_NAME=listen-policy \
  --from-literal=AZTUNNEL_KEY='<key>'

# Deploy the tunnel listener
kubectl --context kind-remote apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: kube-tunnel
spec:
  containers:
    - name: aztunnel
      image: ghcr.io/philsphicas/aztunnel:latest   # use aztunnel:dev for local builds
      imagePullPolicy: IfNotPresent
      command:
        - aztunnel
        - relay-listener
        - --hyco
        - kube-api
        - --allow
        - "kubernetes.default.svc:443"
      envFrom:
        - secretRef:
            name: aztunnel-creds
EOF

# From your workstation — forward the API server
export AZTUNNEL_KEY_NAME=send-policy
export AZTUNNEL_KEY='<sender-key>'
aztunnel relay-sender port-forward --relay my-relay-ns --hyco kube-api \
  --bind 127.0.0.1:6443 kubernetes.default.svc:443

# In another terminal — use kubectl through the tunnel
kubectl --server=https://127.0.0.1:6443 --insecure-skip-tls-verify get nodes
```

## Security considerations

- **Allowlist**: Lock the listener down to `kubernetes.default.svc:443`
  only — don't use a wide CIDR
- **RBAC**: The kubeconfig still controls what you can do — the tunnel
  doesn't bypass Kubernetes RBAC
- **SAS keys**: Use Entra ID + Workload Identity in production; SAS keys
  are shown here for simplicity
