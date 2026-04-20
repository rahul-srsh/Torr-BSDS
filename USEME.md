# Using the HopVault client

The HopVault client is a Go CLI that routes an HTTP request through the
deployed onion network and prints the destination's response body to stdout.

This document covers day-to-day usage. See [README.md](README.md) for how to
provision the infrastructure and deploy the services that the client talks to.

## Prerequisites

- Go 1.25+ (only needed if you use `go run`; a pre-built binary has no runtime
  deps)
- Network reach from your machine to the directory server and guard node
- `DIRECTORY_URL` set to the directory server's `http://<ip>:8080`

See [README.md §Quick-start](README.md#quick-start) for how to discover
`DIRECTORY_URL` and `ECHO_SERVER_URL` after deploy.

## Run it

```bash
cd client
go run . \
  --directory-url "$DIRECTORY_URL" \
  --destination-url "https://api.example.com/path"
```

The response body is written to stdout. Exit code is non-zero on failure.

## All flags

| Flag | Default | What it does |
|---|---|---|
| `--directory-url` | *(required)* | Base URL of the directory server |
| `--destination-url` | *(required)* | Full URL of the real destination |
| `--method` | `GET` | HTTP method (`GET`, `POST`, `PUT`, …) |
| `--body` | `""` | Request body (string) |
| `--hops` | `3` | `1` (guard only) or `3` (full circuit) |
| `--timeout` | `15s` | HTTP client timeout |
| `--max-retries` | `3` | Max circuit rebuild attempts on hop failure |
| `--health-check` | `false` | Enable background `/nodes` polling for proactive rebuild |

## Common patterns

### GET through the full 3-hop circuit

```bash
go run . --directory-url "$DIRECTORY_URL" \
         --destination-url "$ECHO_SERVER_URL/echo?hello=world"
```

### POST with a JSON body

```bash
go run . --directory-url "$DIRECTORY_URL" \
         --destination-url "https://api.example.com/submit" \
         --method POST \
         --body '{"key":"value"}'
```

### 1-hop mode (guard acts as exit)

Used by Experiment 1's baseline. The guard decrypts and forwards directly to
the destination instead of passing through relay + exit.

```bash
go run . --directory-url "$DIRECTORY_URL" \
         --destination-url "$ECHO_SERVER_URL/echo" \
         --hops 1
```

### Pipe the response into another tool

```bash
go run . --directory-url "$DIRECTORY_URL" \
         --destination-url "$ECHO_SERVER_URL/echo" | jq .
```

### Enable proactive failure detection

The client polls `GET $DIRECTORY_URL/nodes` every 5 s; if any hop in the
current circuit is no longer listed as healthy, it rebuilds before the next
request times out.

```bash
go run . --directory-url "$DIRECTORY_URL" \
         --destination-url "$ECHO_SERVER_URL/echo" \
         --health-check
```

### Increase the retry budget

Default is 3 circuit attempts. Raise it for experiments where relays are
being killed:

```bash
go run . --directory-url "$DIRECTORY_URL" \
         --destination-url "$ECHO_SERVER_URL/echo" \
         --max-retries 10
```

### Longer timeout for slow destinations

```bash
go run . --directory-url "$DIRECTORY_URL" \
         --destination-url "https://slow.example.com" \
         --timeout 60s
```

## Build a standalone binary

```bash
cd client
go build -o ../hopvault-client .
cd ..
./hopvault-client --directory-url "$DIRECTORY_URL" \
                  --destination-url "$ECHO_SERVER_URL/echo"
```

The binary is statically linked — copy it to any machine that has network
reach to the directory server and the guard node.

## What happens under the hood

One invocation triggers:

1. `GET $DIRECTORY_URL/circuit?hops=3` — pick a random guard + relay + exit triple
2. Fresh AES-256 session key per hop, RSA-OAEP-wrapped with that hop's public
   key and delivered via `POST /setup` on each node
3. 3-layer AES-256-GCM onion wrap of the destination HTTP request
4. `POST /onion` to the guard's public IP, which decrypts its layer and
   forwards the remainder to the relay, which forwards to the exit, which
   executes the real HTTP request against the destination
5. Return path — each hop re-encrypts the response with its session key
6. Client peels the 3 layers with (guardKey, relayKey, exitKey) in order and
   writes the destination's response body to stdout

If any hop is unreachable during setup or send, the client fetches a fresh
circuit and retries (up to `--max-retries` times) without surfacing the
failure. Each rebuild is logged to stderr:

```
[client] rebuild: attempt=2 hop=relay node=abc123... success=true ms=412
```

## Privacy property

Reading the logs on individual nodes, you will see:

- **Guard** — knows your public IP (the client's) and the relay's IP; never sees
  the destination URL
- **Relay** — knows only the guard's IP and the exit's IP; never sees your IP
  or the destination URL
- **Exit** — knows the destination URL; never sees your IP

No single node observation deanonymizes the flow. A global adversary that
controls both guard and exit on the same circuit can correlate, which is the
classic Tor threat model.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `get circuit from directory server: GET …/circuit returned 503` | Fewer than 1 guard/relay/exit is healthy. Check `aws ecs describe-services` and the CloudWatch log groups. |
| `establish session keys: setup key for node … connection refused` | Guard / relay / exit registered a private IP the client can't reach. All services need `assign_public_ip=true` (they do by default in this infra). |
| `send onion request via guard …: context deadline exceeded` | Guard is reachable but timing out forwarding to the relay. Bump `--timeout`. |
| `all 3 circuit attempts failed` | Persistent node failure. Check `HopVault-Overview` CloudWatch dashboard for task churn. |
| Response body is empty but exit code is 0 | The destination really did return an empty body. Rerun with `--method GET --destination-url $ECHO_SERVER_URL/echo` to verify the circuit itself is fine. |

## See also

- [README.md](README.md) — project overview, architecture, deploy
- [client/client.go](client/client.go) — `ExecuteRequestWithHops`,
  `BuildOnion`, `DecryptResponse` for library use
- [client/main.go](client/main.go) — flag parsing + `runClient` entry point
