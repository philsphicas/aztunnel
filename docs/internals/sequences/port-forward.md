# Port-forward rendezvous

Wire-level sequence for a single port-forward rendezvous from cold
state:

1. The Listener opens a control WebSocket to the Relay
   (`action=listen`) and waits.
2. A Client opens a TCP connection to the local Sender.
3. The Sender opens a fresh rendezvous WebSocket to Azure Relay.
4. The Listener picks up the accept message and dials back.
5. The Relay emits 101 to both halves in parallel.
6. The first byte echoes through the established bridge.
7. The connection tears down when the Client closes.

## Topology

```mermaid
flowchart LR
    Client(Client)
    Sender(Sender)
    Listener(Listener)
    Target(Target)
    subgraph Relay["Azure Relay namespace"]
        direction TB
        RNs["RNs<br/>sender's<br/>relay node"]
        RNl["RNl<br/>listener's control<br/>relay node"]
    end
    Client ---|"lane c"| Sender
    Sender ---|"lane s"| Relay
    Relay ---|"lane l"| Listener
    Listener ---|"lane t"| Target
    style Relay fill:none,stroke:#999
```

Two relay nodes inside the namespace serve this rendezvous: `RNl`
holds the listener's warm control channel; `RNs` is the node the
sender dialled and is where the rendezvous itself happens. The
hop from `RNl` to `RNs` is relay-internal — invisible to clients
and modelled as zero cost. Lane `l` covers both the control
channel (Listener ↔ `RNl`, via FE VIP) and the listener's
rendezvous dial (Listener → `RNs` direct, hostname comes in the
accept message); the two sub-paths have similar RTTs from any
given host and are treated as one lane in the algebra.

## Sequence

```mermaid
sequenceDiagram
    autonumber
    participant Client as Client<br/>(echo)
    participant Snd as Sender<br/>aztunnel relay-sender
    box transparent Azure Relay
    participant RNs as RNs<br/>(sender's relay node,<br/>via FE VIP)
    participant RNl as RNl<br/>(listener's control<br/>relay node, via FE VIP)
    end
    participant Lst as Listener<br/>aztunnel relay-listener
    participant Tgt as Target<br/>(echo server)

    rect rgb(245, 245, 245)
    Note over Lst,RNl: Phase 0 — Listener control WS already warm.<br/>Established once per listener process, not paid per rendezvous.
    Note over Lst: DNS lookup for FE VIP hostname (uncached)
    Lst  ->>  RNl: TCP SYN [+l↑]
    RNl  ->>  Lst: TCP SYN+ACK [+l↓]
    Lst  ->>  RNl: TLS 1.3 ClientHello (key_share = secp384r1) [+l↑]
    RNl  ->>  Lst: TLS 1.3 ServerHello + encrypted flight [+l↓]
    Lst  ->>  RNl: TLS Finished + WS GET<br/>(GET /$hc/{path}?sb-hc-action=listen&sb-hc-token=…) [+l↑]
    Note over RNl: SAS-token validation (action=listen)
    RNl  ->>  Lst: HTTP 101 Switching Protocols [+l↓]
    Note over Lst: Control channel idle thereafter — listener pings every ~30 s.
    end

    rect rgb(255, 255, 224)
    Note over Client,Snd: Phase 1 — Client connects to Sender (local)
    Client  ->>  Snd: TCP SYN [+c↑]
    Snd  -->> Client: SYN+ACK [+c↓]
    Client  ->>  Snd: ACK + first-byte write "Y" [+c↑]
    Note over Snd: accept() returns → Phase 2 starts.<br/>First byte sits in Snd's kernel buffer until the bridge is up (Phase 6).
    end

    rect rgb(255, 250, 205)
    Note over Snd,RNs: Phase 2 — Sender cold-dials RNs (via FE VIP)
    Note over Snd: DNS lookup for FE VIP hostname (uncached)
    Snd  ->>  RNs: TCP SYN [+s↑]
    RNs  ->>  Snd: TCP SYN+ACK [+s↓]
    Snd  ->>  RNs: TLS 1.3 ClientHello (key_share = secp384r1) [+s↑]
    RNs  ->>  Snd: TLS 1.3 ServerHello + encrypted flight [+s↓]
    Snd  ->>  RNs: TLS Finished + WS GET<br/>(?sb-hc-action=connect&sb-hc-token=…) [+s↑]
    Note over RNs: SAS-token validation (action=connect)
    Note over RNs,RNl: RNs has the WS GET but HOLDS the 101.<br/>It dispatches an accept message to the control plane,<br/>then waits for Lst to complete its rendezvous.
    end

    rect rgb(240, 248, 255)
    Note over RNs,Lst: Phase 3 — Accept routing (relay control plane)
    RNs  -->> RNl: relay-internal accept routing (not on wire)
    Note over RNl: MatchMake dispatch (find matching listener for path)
    RNl  ->>  Lst: accept message on warm control WS [+l↓]<br/>{"accept":{"address":"wss://RNs-hostname.…?sb-hc-action=accept&id=XYZ"…}}
    end

    rect rgb(240, 255, 240)
    Note over Lst,RNs: Phase 4 — Listener rendezvous to RNs (direct, no FE)
    Note over Lst: DNS lookup for RNs hostname (uncached) + dial init
    Lst  ->>  RNs: TCP SYN [+l↑]
    RNs  ->>  Lst: TCP SYN+ACK [+l↓]
    Lst  ->>  RNs: TLS 1.3 ClientHello [+l↑]
    RNs  ->>  Lst: TLS 1.3 ServerHello + encrypted flight [+l↓]
    Lst  ->>  RNs: TLS Finished + WS GET (?sb-hc-action=accept&id=XYZ) [+l↑]
    Note over RNs,RNl: RNs now has both halves of the bridge.<br/>It emits 101 to both sides in parallel (Phase 5).
    end

    rect rgb(207, 232, 255)
    Note over Snd,Lst: Phase 5 — Dual 101 emission
    par dual 101 emission
        RNs  ->>  Lst: HTTP 101 (listener bridge ready) [+l↓]
    and
        RNs  ->>  Snd: HTTP 101 (sender bridge ready) [+s↓]
    end
    Note over Snd,Lst: Both 101s triggered by the same event (Lst's WS GET arriving).
    end

    rect rgb(224, 255, 224)
    Note over Client,Tgt: Phase 6 — First-byte echo
    Snd  ->>  RNs: WS DataFrame "Y" (reading buffered byte from kernel) [+s↑]
    RNs  ->>  Lst: WS DataFrame "Y" [+l↓]
    Lst  ->>  Tgt: write "Y" [+t↑]
    Tgt  -->> Lst: echo "Y" [+t↓]
    Lst  ->>  RNs: WS DataFrame "Y" [+l↑]
    RNs  ->>  Snd: WS DataFrame "Y" [+s↓]
    Snd  -->> Client: write "Y" back to Client [+c↓]
    end

    rect rgb(255, 240, 240)
    Note over Client,Lst: Phase 7 — Client-initiated teardown
    Client  ->>  Snd: close local TCP socket [+c↑]
    Snd  ->>  RNs: WS close + TCP FIN [+s↑]
    RNs  --x Snd: relay cleanup [+s↓]
    RNs  --x Lst: relay cleanup [+l↓]
    Note over Snd: Sender bridge ended with cause=local_close
    Note over Lst: Listener bridge ended with cause=peer_close
    end
```

## Critical-path hop counts

| Segment                                            | Lives in  | Hops on critical path     |
| -------------------------------------------------- | --------- | ------------------------- |
| Client TCP open + first-byte write (loopback, ≈ 0) | `c`       | 3 c                       |
| **Sender cold dial, WS GET arrives at RNs**        | `s`       | **5 s**                   |
| Accept message to listener                         | `l`       | 1 l                       |
| **Listener rendezvous, WS GET arrives at RNs**     | `l`       | **5 l**                   |
| RNs → Snd 101 (parallel with RNs → Lst 101)        | `s`       | 1 s                       |
| **Client open → Sender has 101**                   | `c,s,l`   | **3 c + 6 s + 6 l**       |
| First-byte echo through the bridge back to Client  | `c,s,l,t` | 1 c + 2 s + 2 l + 2 t     |
| **Client open → first byte echoed at Client**      | `c,s,l,t` | **4 c + 8 s + 8 l + 2 t** |

(Each TLS 1.3 handshake is 1 RTT — secp384r1 in the initial
`key_share` lets Azure Relay skip the HelloRetryRequest. The 5
sender-side hops in Phase 2 are SYN↑, SYN+ACK↓, ClientHello↑,
ServerHello↓, Finished+WSGET↑. Phase 4 has the same shape on
lane `l`.)

The Client ↔ Sender hops on lane `c` are on the dependency chain
but are loopback in this topology, so each is ≈ 0 ms.

## Key facts

1. **TLS 1.3 handshake is 1 RTT** because aztunnel's TLS dialer
   forces `secp384r1` in the initial `key_share`
   (`internal/relay/client.go`). Azure Relay accepts secp384r1
   without a HelloRetryRequest. A vanilla Go TLS client offers
   X25519 first and pays an extra RTT.
2. **The two 101s in Phase 5 are parallel.** RNs writes both as
   soon as it has both halves of the bridge. Neither blocks the
   other.
3. **Listener rendezvous bypasses the FE VIP.** The accept
   message hands the listener a relay node hostname; the listener
   dials that hostname directly. This is why Phase 4 is "lane
   l, direct, no FE".
4. **Every fresh rendezvous pays a DNS lookup.** Go's stdlib
   `net.Resolver` has no built-in cache, so every handler entry
   (`handleListen`, `handleConnect`, `handleAccept`) pays a
   fresh A+AAAA resolution. `DelayProfile.DNSLookup` models
   this cost.
5. **Bridge forwarding is modelled as a pipelined delay.** Each
   WS message is stamped with `arriveBy = now + (S + L)` on the
   read side and forwarded after that deadline, with multiple
   messages allowed in flight at once. A single echo pays
   `2·(S+L)`; a stream of back-to-back messages pays roughly
   `(S+L)` to fill the pipe. Stop-and-wait would have penalised
   throughput-bound tests by `N·(S+L)`, which is not how real
   TCP behaves.

## Calibration in tests

`mockrelay`'s `DelayProfile` parameterises this diagram:

| Diagram element                            | DelayProfile field                             |
| ------------------------------------------ | ---------------------------------------------- |
| Lane `s` one-way (Phase 2, Phase 5 101)    | `SLatency`                                     |
| Lane `l` one-way (Phase 0, 3, 4, 5 101)    | `LLatency`                                     |
| Per-handler DNS lookup (Phase 0, 2, 4)     | `DNSLookup`                                    |
| SAS-token validation (Phase 0 and Phase 2) | `AuthInternal`                                 |
| Accept-message dispatch (Phase 3)          | `MatchMakeInternal`                            |
| Phase 6 bridge forwarding                  | `SLatency + LLatency` (pipelined, per message) |

Pass `mockrelay/server.DelayProfileDefault` via
`server.WithDelayProfile(...)` to make the mock pay wire-faithful
costs.
