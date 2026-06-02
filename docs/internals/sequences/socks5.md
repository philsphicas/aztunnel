# SOCKS5 rendezvous

Wire-level sequence for a SOCKS5 rendezvous from cold state:

1. The Listener opens a control WebSocket to the Relay
   (`action=listen`) and waits.
2. A Client opens a TCP connection to the local SOCKS5 server
   (in-process on the Sender).
3. Client and Sender complete the SOCKS5 greeting and CONNECT.
4. The Sender performs the rendezvous with the Relay.
5. The Listener dials the target named in CONNECT.
6. The Listener's `ConnectResponse` returns through the bridge.
7. The Sender emits `REP=0x00` to the Client.
8. Payload flows in both directions until the Client closes.

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

The Sender hosts an in-process SOCKS5 server, so the Client speaks
SOCKS5 on lane `c`. From the Relay's perspective the rendezvous is
identical to a port-forward — only the local protocol differs.
Inside the namespace, `RNl` holds the listener's warm control
channel and `RNs` is the node the sender dialled; the listener
also dials `RNs` directly (hostname comes in the accept message).
The relay-internal `RNs → RNl` hop is invisible and modelled as
zero cost.

## Sequence

```mermaid
sequenceDiagram
    autonumber
    participant Client as Client<br/>(SOCKS5)
    participant Snd as Sender<br/>aztunnel socks5-proxy
    box transparent Azure Relay
    participant RNs as RNs<br/>(sender's relay node,<br/>via FE VIP)
    participant RNl as RNl<br/>(listener's control<br/>relay node, via FE VIP)
    end
    participant Lst as Listener<br/>aztunnel relay-listener
    participant Tgt as Target<br/>(echo server)

    rect rgb(245, 245, 245)
    Note over Lst,RNl: Phase 0 — Listener control WS already warm.<br/>Established once per listener process, not paid per SOCKS5 CONNECT.
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
    Note over Client,Snd: Phase 1 — Local SOCKS5 setup
    Client  ->>  Snd: TCP connect to local SOCKS5 port [+c↑]
    Snd  -->> Client: TCP open completes locally [+c↓]
    Client  ->>  Snd: greeting 05 01 00 (no-auth offer) [+c↑]
    Snd  -->> Client: method selection 05 00 (no-auth accepted) [+c↓]
    Client  ->>  Snd: CONNECT 05 01 00 ATYP DST.ADDR DST.PORT [+c↑]
    Note over Client,Snd: Target parsed by socks5.Handshake.<br/>bridge_id minted after target is known.<br/>NO REP yet — sender holds REP until listener accepts.
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

    rect rgb(240, 230, 255)
    Note over Client,Tgt: Phase 6 — Envelope, target dial, REP
    Snd  ->>  RNs: ConnectEnvelope JSON {target, bridge_id} [+s↑]
    RNs  ->>  Lst: ConnectEnvelope delivered [+l↓]
    Lst  ->>  Tgt: TCP connect to target [+t↑]
    Tgt  -->> Lst: target connect succeeds [+t↓]
    Lst  ->>  RNs: ConnectResponse {ok:true, listener_id} [+l↑]
    RNs  ->>  Snd: ConnectResponse delivered [+s↓]
    Snd  -->> Client: SOCKS5 REP=0x00 (CONNECT complete) [+c↓]
    Note over Client,Snd: CONNECT completes here.<br/>Client may write payload only after REP=0.
    end

    rect rgb(224, 255, 224)
    Note over Client,Tgt: Phase 7 — Payload echo
    Client  ->>  Snd: write payload [+c↑]
    Snd  ->>  RNs: WS binary frame [+s↑]
    RNs  ->>  Lst: WS binary frame [+l↓]
    Lst  ->>  Tgt: write payload [+t↑]
    Tgt  -->> Lst: echo payload [+t↓]
    Lst  ->>  RNs: WS binary frame [+l↑]
    RNs  ->>  Snd: WS binary frame [+s↓]
    Snd  -->> Client: write payload back to client [+c↓]
    end

    rect rgb(255, 240, 240)
    Note over Client,Lst: Phase 8 — Client-initiated teardown
    Client  ->>  Snd: close local socket [+c↑]
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
| Local TCP open before SOCKS5                       | `c`       | 2 c                       |
| SOCKS5 greeting (offer + selection)                | `c`       | 2 c                       |
| CONNECT request                                    | `c`       | 1 c                       |
| **Relay rendezvous, Snd SYN → Snd 101**            | `s,l`     | **6 s + 6 l**             |
| Envelope to listener (ConnectEnvelope outbound)    | `s,l`     | s + l                     |
| Target dial                                        | `t`       | 2 t                       |
| ConnectResponse back to sender                     | `s,l`     | s + l                     |
| REP to client                                      | `c`       | 1 c                       |
| **Cold start → REP visible to client**             | `c,s,l,t` | **6 c + 8 s + 8 l + 2 t** |
| Payload one-way through bridge                     | `c,s,l,t` | c + s + l + t             |
| **Payload echo (Client → Tgt → Client after REP)** | `c,s,l,t` | **2 c + 2 s + 2 l + 2 t** |

(`6 c` covers: TCP open `2 c` + greeting `2 c` + CONNECT `1 c` +
REP `1 c`. Loopback hops are ≈ 0 ms but are listed for
completeness.)

The Phase 6 `s + l + 2 t + s + l = 2 s + 2 l + 2 t` between
"Sender's 101 arrives" and "REP returns to the client" is the
ConnectEnvelope / target-dial / ConnectResponse round-trip on the
freshly-built bridge — this is the SOCKS5-specific cost on top of
the rendezvous.

## Key facts

1. **TLS 1.3 handshake is 1 RTT** because aztunnel's TLS dialer
   forces `secp384r1` in the initial `key_share`
   (`internal/relay/client.go`). Azure Relay accepts secp384r1
   without a HelloRetryRequest. A vanilla Go TLS client offers
   X25519 first and pays an extra RTT.
2. **The 30 s SOCKS5 handshake deadline is local-only.**
   `handleSOCKS5` sets a read deadline, runs `socks5.Handshake`,
   then clears it before relay work starts
   (`internal/sender/socks5proxy.go`). A slow client can stall
   only the local handshake window, not the bridge.
3. **Bridge ids start after CONNECT reveals the target.** The
   Sender mints the bridge id after `socks5.Handshake` returns
   the target, then binds it into the logger. The relay
   rendezvous and target dial both run under the same bridge id.
4. **No-auth and CONNECT-only.** `Handshake` accepts only method
   `0x00` and writes `0xFF` if no acceptable method is offered.
   Non-CONNECT commands return `REP=0x07`
   (`internal/sender/socks5/socks5.go`).
5. **Target failure surfaces as a non-zero REP.** If `Lst → Tgt`
   fails, the Listener replies with `ConnectResponse{ok:false}`
   and the Sender translates that to the appropriate SOCKS5 REP
   code (`internal/listener/listener.go`,
   `internal/sender/socks5proxy.go`).

## Calibration in tests

`mockrelay`'s `DelayProfile` parameterises the rendezvous phases:

| Diagram element                            | DelayProfile field                             |
| ------------------------------------------ | ---------------------------------------------- |
| Lane `s` one-way (Phase 2, Phase 5 101)    | `SLatency`                                     |
| Lane `l` one-way (Phase 0, 3, 4, 5 101)    | `LLatency`                                     |
| Per-handler DNS lookup (Phase 0, 2, 4)     | `DNSLookup`                                    |
| SAS-token validation (Phase 0 and Phase 2) | `AuthInternal`                                 |
| Entra-token validation (Phase 0, Phase 2)  | `EntraValidate`                                |
| Accept-message dispatch (Phase 3)          | `MatchMakeInternal`                            |
| Phase 6 and Phase 7 bridge forwarding      | `SLatency + LLatency` (pipelined, per message) |

The relay charges one token-validation cost per token-bearing leg,
selected by the inbound token's shape: `EntraValidate` for an Entra
(JWT) bearer token, `AuthInternal` for a SAS token.

Phase 6 (envelope / target dial / REP) and Phase 7 (payload) ride
the established bridge, which is delay-modelled as a pipelined
delay: each WS message pays one `S + L` end-to-end propagation,
with multiple messages allowed in flight at once. A single echo
pays `2·(S+L)`; a streaming download pays roughly one `S+L` to
fill the pipe — not N times that.
