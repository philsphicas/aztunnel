# Sender: Ad-Hoc

Run the sender in the foreground for testing and scripting. Useful for
quick experiments before setting up a permanent configuration.

## Port forward (foreground)

The simplest way to test connectivity:

```sh
aztunnel relay-sender port-forward \
  --relay my-relay-ns \
  --hyco my-tunnel \
  --bind 127.0.0.1:18080 \
  localhost:8080
```

Then in another terminal:

```sh
curl http://127.0.0.1:18080
```

Press Ctrl+C to stop.

## Background mode for scripting

Use [bgtask](https://github.com/philsphicas/bgtask) to run the sender as a
named background task with log capture:

```sh
export AZTUNNEL_RELAY_NAME=my-relay-ns
export AZTUNNEL_HYCO_NAME=my-tunnel
export AZTUNNEL_KEY_NAME=send-policy
export AZTUNNEL_KEY='<your-sas-key>'

# Start sender in background
bgtask run --name sender -- aztunnel relay-sender port-forward --bind 127.0.0.1:19999 localhost:9999
sleep 3  # wait for connection to Azure Relay

# Test
echo "hello" | nc -w 5 127.0.0.1 19999

# Cleanup
bgtask stop sender
```

## Quick connectivity test with connect mode

`relay-sender connect` bridges stdin/stdout — useful for one-shot tests
without binding a port:

```sh
echo "GET / HTTP/1.0\r\nHost: localhost\r\n\r\n" | \
  aztunnel relay-sender connect --relay my-relay-ns --hyco my-tunnel localhost:80
```

## SOCKS5 proxy (foreground)

Start a SOCKS5 proxy for exploring multiple targets:

```sh
aztunnel relay-sender socks5-proxy \
  --relay my-relay-ns \
  --hyco my-tunnel \
  --bind 127.0.0.1:1080

# In another terminal:
curl --socks5h 127.0.0.1:1080 http://10.0.0.5:8080
curl --socks5h 127.0.0.1:1080 http://10.0.0.6:3000
```

See [Sender: SOCKS5 Proxy](sender-socks5-proxy.md) for full details.

## Environment variables

Set these to avoid repeating flags:

```sh
export AZTUNNEL_RELAY_NAME=my-relay-ns
export AZTUNNEL_HYCO_NAME=my-tunnel
export AZTUNNEL_KEY_NAME=send-policy       # SAS only
export AZTUNNEL_KEY='<your-sas-key>'       # SAS only

# Now commands are short:
aztunnel relay-sender port-forward --bind :18080 localhost:8080
aztunnel relay-sender socks5-proxy --bind :1080
aztunnel relay-sender connect localhost:22
```

## Debugging flags

| Flag                   | Description                                                  |
| ---------------------- | ------------------------------------------------------------ |
| `--log-level debug`    | Log every connection, dial, and bridge event                 |
| `--metrics-addr :9090` | Expose Prometheus metrics at `http://localhost:9090/metrics` |
