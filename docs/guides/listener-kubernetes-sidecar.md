# Listener: Kubernetes Sidecar

Run the aztunnel listener as a sidecar container alongside your service. The
listener accepts connections from Azure Relay and forwards them to the service
running in the same pod (via `localhost`).

```
┌─── Pod ───────────────────────────┐
│                                   │
│  ┌───────────┐   ┌─────────────┐  │       ┌──────────┐
│  │  your     │◄──│  aztunnel   │◄═══WSS═══│  sender  │
│  │  service  │   │  listener   │  │       │          │
│  │  :80      │   │  (sidecar)  │  │       └──────────┘
│  └───────────┘   └─────────────┘  │
│                                   │
└───────────────────────────────────┘
```

## Prerequisites

- A [kind](https://kind.sigs.k8s.io/) cluster (or any Kubernetes cluster)
- An Azure Relay namespace with a hybrid connection
  ([setup guide](../azure-setup.md))
- SAS keys or Entra ID credentials for the relay
- The `aztunnel` container image

## Container image

The aztunnel image is published to GitHub Container Registry:

```
ghcr.io/philsphicas/aztunnel:latest
```

For kind clusters, pull and load the image:

```sh
docker pull ghcr.io/philsphicas/aztunnel:latest
kind load docker-image ghcr.io/philsphicas/aztunnel:latest --name my-cluster
```

To build locally instead:

```sh
make docker                                       # builds aztunnel:dev
kind load docker-image aztunnel:dev --name my-cluster
```

## Create credentials

Store your SAS credentials (or any auth env vars) in a Kubernetes Secret:

```sh
kubectl create secret generic aztunnel-creds \
  --from-literal=AZTUNNEL_RELAY_NAME=my-relay-ns \
  --from-literal=AZTUNNEL_KEY_NAME=listen-policy \
  --from-literal=AZTUNNEL_KEY='<your-sas-key>'
```

> **Production**: Use Entra ID with
> [Workload Identity](https://learn.microsoft.com/en-us/azure/aks/workload-identity-overview)
> instead of SAS keys. The listener picks up credentials automatically via
> `DefaultAzureCredential` — no secret needed.

## Configuration with ConfigMap

Store non-secret configuration in a ConfigMap so you don't need to pass
relay and hybrid connection names as command-line arguments:

```sh
kubectl create configmap aztunnel-config \
  --from-literal=AZTUNNEL_RELAY_NAME=my-relay-ns \
  --from-literal=AZTUNNEL_HYCO_NAME=my-tunnel
```

With this approach, the Secret only holds the SAS key and key name:

```sh
kubectl create secret generic aztunnel-creds \
  --from-literal=AZTUNNEL_KEY_NAME=listen-policy \
  --from-literal=AZTUNNEL_KEY='<your-sas-key>'
```

Then reference both the ConfigMap and Secret in your pod spec:

```yaml
- name: aztunnel
  image: ghcr.io/philsphicas/aztunnel:latest
  command:
    - aztunnel
    - relay-listener
    - --allow
    - "localhost:80"
  envFrom:
    - configMapRef:
        name: aztunnel-config
    - secretRef:
        name: aztunnel-creds
```

With this pattern, the command line only needs the `--allow` flags — everything
else comes from the environment. The Secret only holds the SAS key and key name.

## Pod spec

Here's a pod with nginx and an aztunnel listener sidecar. The listener
forwards connections to `localhost:80` (nginx in the same pod):

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: my-service
spec:
  containers:
    - name: nginx
      image: nginx:alpine
      ports:
        - containerPort: 80

    - name: aztunnel
      image: ghcr.io/philsphicas/aztunnel:latest # use aztunnel:dev for local builds
      imagePullPolicy: IfNotPresent
      command:
        - aztunnel
        - relay-listener
        - --hyco
        - my-tunnel
        - --allow
        - "localhost:80"
        - --log-level
        - info
      envFrom:
        - secretRef:
            name: aztunnel-creds
```

Apply it:

```sh
kubectl apply -f my-service.yaml
```

## Allowlist patterns

The `--allow` flags control which targets the listener will forward to.
Inside a pod, your service is reachable at `localhost` or `127.0.0.1`:

| Pattern                         | What it allows                                            |
| ------------------------------- | --------------------------------------------------------- |
| `localhost:80`                  | Only port 80 on localhost                                 |
| `127.0.0.1:80`                  | Same, by IP                                               |
| `localhost:80`, `localhost:443` | Ports 80 and 443                                          |
| `127.0.0.1:*`                   | Any port on the loopback (useful for multi-port services) |

If your service also needs to be reachable by pod IP (e.g., from a sender
that resolves DNS), add the pod IP or CIDR:

```yaml
command:
  - aztunnel
  - relay-listener
  - --hyco
  - my-tunnel
  - --allow
  - "localhost:80"
  - --allow
  - "10.244.0.0/16:80" # pod CIDR — adjust for your cluster
```

> **Security**: Always specify an allowlist. Without `--allow`, the listener
> forwards to any target, which is rarely what you want in a sidecar.

## Multiple services in one pod

If your pod runs multiple services (e.g., an app on port 8080 and a metrics
endpoint on port 9090), allow both:

```yaml
- name: aztunnel
  image: ghcr.io/philsphicas/aztunnel:latest # use aztunnel:dev for local builds
  command:
    - aztunnel
    - relay-listener
    - --hyco
    - my-tunnel
    - --allow
    - "localhost:8080"
    - --allow
    - "localhost:9090"
  envFrom:
    - secretRef:
        name: aztunnel-creds
```

The sender selects the target at connection time (via port-forward target
argument or SOCKS5 destination).

## Gateway pattern: forwarding to other pods

A listener doesn't have to be a sidecar — it can also act as a gateway to
services elsewhere in the cluster. Deploy it as a standalone pod and allow
the relevant cluster IPs or DNS names:

```yaml
- name: aztunnel
  image: ghcr.io/philsphicas/aztunnel:latest # use aztunnel:dev for local builds
  command:
    - aztunnel
    - relay-listener
    - --hyco
    - my-tunnel
    - --allow
    - "10.96.0.0/12:*" # cluster service CIDR
    - --allow
    - "10.244.0.0/16:*" # pod CIDR
  envFrom:
    - secretRef:
        name: aztunnel-creds
```

See the [VNet gateway scenario](scenario-vnet-gateway.md) for a full
walkthrough.

## Verifying the listener

Check that the sidecar is running and connected to Azure Relay:

```sh
kubectl logs my-service -c aztunnel
```

You should see:

```
level=INFO msg="control channel connected" entityPath=my-tunnel
```

## Connecting from the sender side

Once the listener is running, connect from the sender side using any sender
mode:

```sh
# Port forward
aztunnel relay-sender port-forward --relay my-relay-ns --hyco my-tunnel --bind 127.0.0.1:8080 localhost:80
curl http://127.0.0.1:8080

# SOCKS5 proxy
aztunnel relay-sender socks5-proxy --relay my-relay-ns --hyco my-tunnel --bind 127.0.0.1:1080
curl --socks5h 127.0.0.1:1080 http://localhost:80
```

See the [sender guides](README.md#sender-your-side) for detailed configuration of each sender mode.

## kind quick-start

A complete example using kind:

```sh
# Create a cluster and load the image
kind create cluster --name demo
docker pull ghcr.io/philsphicas/aztunnel:latest
kind load docker-image ghcr.io/philsphicas/aztunnel:latest --name demo

# Create credentials
kubectl --context kind-demo create secret generic aztunnel-creds \
  --from-literal=AZTUNNEL_RELAY_NAME=my-relay-ns \
  --from-literal=AZTUNNEL_KEY_NAME=listen-policy \
  --from-literal=AZTUNNEL_KEY='<key>'

# Deploy a pod with nginx + aztunnel sidecar
kubectl --context kind-demo apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: listener
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
        - my-tunnel
        - --allow
        - "localhost:80"
      envFrom:
        - secretRef:
            name: aztunnel-creds
EOF

# Verify
kubectl --context kind-demo logs listener -c aztunnel
```

Then from your workstation (with sender credentials):

```sh
export AZTUNNEL_RELAY_NAME=my-relay-ns
export AZTUNNEL_KEY_NAME=send-policy
export AZTUNNEL_KEY='<sender-key>'

aztunnel relay-sender port-forward --hyco my-tunnel --bind 127.0.0.1:8080 localhost:80
# In another terminal:
curl http://127.0.0.1:8080
```
