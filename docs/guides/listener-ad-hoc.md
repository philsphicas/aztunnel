# Listener: Ad-Hoc

Run the listener in the foreground for testing, demos, or quick experiments.
No systemd, no Kubernetes — just a terminal.

## Basic usage

Start the listener in the foreground:

```sh
aztunnel relay-listener \
  --relay my-relay-ns \
  --hyco my-tunnel \
  --allow "localhost:8080"
```

The listener runs until you press Ctrl+C.

## Environment variables

Set credentials once to keep commands short:

```sh
export AZTUNNEL_RELAY_NAME=my-relay-ns
export AZTUNNEL_HYCO_NAME=my-tunnel

# SAS auth
export AZTUNNEL_KEY_NAME=listen-policy
export AZTUNNEL_KEY='<your-sas-key>'

# Or just: az login (for Entra ID)
```

Then:

```sh
aztunnel relay-listener --allow "localhost:8080"
```

## Running in the background

For scripting or long-running tests, use
[bgtask](https://github.com/philsphicas/bgtask) to run the listener as a
named background task with log capture:

```sh
bgtask run --name listener -- aztunnel relay-listener --allow "localhost:8080"
```

Check logs and stop when done:

```sh
bgtask logs -f listener
# ... do your testing ...
bgtask stop listener
```

## Quick echo test

A minimal end-to-end test using `socat` as a TCP echo server:

```sh
# Terminal 1: start an echo server
socat TCP-LISTEN:9999,fork,reuseaddr EXEC:cat

# Terminal 2: start the listener
aztunnel relay-listener --relay my-relay-ns --hyco my-tunnel --allow "localhost:9999"

# Terminal 3: connect through the relay
aztunnel relay-sender port-forward --relay my-relay-ns --hyco my-tunnel --bind 127.0.0.1:19999 localhost:9999

# Terminal 4: test it
echo "hello" | nc -w 5 127.0.0.1 19999
# → hello
```

This uses the simplest possible setup — no Kubernetes, no systemd, just four
terminal windows.

## Quick HTTP test

Test with an HTTP server instead:

```sh
# Terminal 1: start a simple HTTP server
python3 -m http.server 8080

# Terminal 2: listener
aztunnel relay-listener --relay my-relay-ns --hyco my-tunnel --allow "localhost:8080"

# Terminal 3: sender
aztunnel relay-sender port-forward --relay my-relay-ns --hyco my-tunnel --bind 127.0.0.1:18080 localhost:8080

# Terminal 4: test
curl http://127.0.0.1:18080
```

## Useful flags

| Flag                | Default                      | Description                                 |
| ------------------- | ---------------------------- | ------------------------------------------- |
| `--allow`           | (none — all targets allowed) | Restrict which targets can be dialed        |
| `--max-connections` | `0` (unlimited)              | Limit concurrent connections                |
| `--connect-timeout` | `30s`                        | Timeout for dialing targets                 |
| `--log-level`       | `info`                       | Set to `debug` for connection-level details |
| `--metrics-addr`    | (disabled)                   | Expose Prometheus metrics (e.g., `:9090`)   |

## Open allowlist warning

If you don't pass `--allow`, the listener logs a warning and allows
connections to any target. This is convenient for testing but should never
be used in production:

```
level=WARN msg="no allowlist configured, all targets are permitted"
```
