# Sender: Kubernetes Sidecar

Run the aztunnel sender as a sidecar container alongside your application.
The sender opens a tunnel through Azure Relay and exposes the remote service
as a local port inside the pod, so your app connects to `localhost` as if the
service were local.

```
┌── Pod (Cluster A) ────────────────────┐       ┌──────────────┐       ┌──────────┐
│                                       │       │              │       │          │
│  ┌───────┐   ┌───────────────────┐    │       │ Azure Relay  │       │ listener │
│  │ your  │──▶│ aztunnel sender   │════════════│              │══════▶│          │
│  │ app   │   │ port-forward      │    │  WSS  │              │  WSS  │          │
│  │       │   │ :8080 → remote:80 │    │       │              │       │          │
│  └───────┘   └───────────────────┘    │       │              │       │          │
│                                       │       │              │       │          │
└───────────────────────────────────────┘       └──────────────┘       └──────────┘
```

## When to use a sender sidecar

- Your app runs in Kubernetes and needs to call a service behind a relay
- You want the tunnel lifecycle managed by Kubernetes (restarts, health)
- Cross-cluster connectivity (see
  [K8s-to-K8s scenario](scenario-k8s-to-k8s.md))

## Prerequisites

- An Azure Relay namespace with a hybrid connection and a running listener
- Sender credentials (SAS key or Entra ID)
- The `ghcr.io/philsphicas/aztunnel` container image

## Container image

```
ghcr.io/philsphicas/aztunnel:latest
```

For kind clusters, pull and load:

```sh
docker pull ghcr.io/philsphicas/aztunnel:latest
kind load docker-image ghcr.io/philsphicas/aztunnel:latest --name my-cluster
```

## Create credentials

```sh
kubectl create configmap aztunnel-config \
  --from-literal=AZTUNNEL_RELAY_NAME=my-relay-ns \
  --from-literal=AZTUNNEL_HYCO_NAME=my-tunnel

kubectl create secret generic aztunnel-sender-creds \
  --from-literal=AZTUNNEL_KEY_NAME=send-policy \
  --from-literal=AZTUNNEL_KEY='<your-sas-key>'
```

## Port-forward sidecar

The most common pattern — forward a local port to a fixed remote target:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: my-app
spec:
  containers:
    - name: app
      image: my-app:latest
      env:
        - name: BACKEND_URL
          value: "http://localhost:8080"

    - name: aztunnel
      image: ghcr.io/philsphicas/aztunnel:latest
      command:
        - aztunnel
        - relay-sender
        - port-forward
        - --bind
        - "0.0.0.0:8080"
        - remote-service:80
      envFrom:
        - configMapRef:
            name: aztunnel-config
        - secretRef:
            name: aztunnel-sender-creds
```

Your app connects to `localhost:8080`, which the sender forwards through
Azure Relay to `remote-service:80` on the listener side.

> **Bind to 0.0.0.0**: Inside a pod, binding to `0.0.0.0` makes the port
> accessible to other containers in the same pod via `localhost`. This is
> different from binding on a host, where it exposes the port to the network.

## SOCKS5 sidecar

If your app needs to reach multiple targets, use the SOCKS5 proxy mode:

```yaml
- name: aztunnel
  image: ghcr.io/philsphicas/aztunnel:latest
  command:
    - aztunnel
    - relay-sender
    - socks5-proxy
    - --bind
    - "0.0.0.0:1080"
  envFrom:
    - configMapRef:
        name: aztunnel-config
    - secretRef:
        name: aztunnel-sender-creds
```

Your app uses the SOCKS5 proxy at `localhost:1080` to reach any target the
listener allows:

```sh
curl --socks5h localhost:1080 http://10.0.0.5:8080
curl --socks5h localhost:1080 http://10.0.0.6:3000
```

## Multiple remote services

To reach multiple fixed targets, run multiple sender sidecars on different
ports:

```yaml
spec:
  containers:
    - name: app
      image: my-app:latest

    - name: tunnel-api
      image: ghcr.io/philsphicas/aztunnel:latest
      command:
        - aztunnel
        - relay-sender
        - port-forward
        - --bind
        - "0.0.0.0:8080"
        - api-server:80
      envFrom:
        - configMapRef:
            name: aztunnel-config
        - secretRef:
            name: aztunnel-sender-creds

    - name: tunnel-db
      image: ghcr.io/philsphicas/aztunnel:latest
      command:
        - aztunnel
        - relay-sender
        - port-forward
        - --bind
        - "0.0.0.0:5432"
        - db-server:5432
      envFrom:
        - configMapRef:
            name: aztunnel-config
        - secretRef:
            name: aztunnel-sender-creds
```

> If you need to reach many targets, the SOCKS5 sidecar is simpler — one
> container instead of one per target.

## Verifying

```sh
kubectl logs my-app -c aztunnel
```

You should see:

```
level=INFO msg="port-forward listening" bind=0.0.0.0:8080 target=remote-service:80
```

Test from the app container:

```sh
kubectl exec my-app -c app -- curl -s http://localhost:8080
```
