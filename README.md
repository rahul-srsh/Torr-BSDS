# <Your Project Name>

A distributed onion-routing network implementation in Go.

## Project Structure

```
directory-server/   - Central directory service for node registration and discovery
guard-node/         - Entry point node for client connections
relay-node/         - Intermediate routing node
exit-node/          - Exit point node to the destination
client/             - Client application for connecting to the network
infra/              - Infrastructure as code (Terraform, deployment configs)
```

## Getting Started

### Build the base Docker image

```bash
docker build -f docker/Dockerfile.base -t hopvault-base .
```

### Run locally

```bash
docker run -p 8080:8080 \
  -e NODE_TYPE=guard \
  -e DIRECTORY_SERVER_URL=http://directory-server.hopvault.local:8080 \
  hopvault-base
```

### Verify health endpoint

```bash
curl http://localhost:8080/health
# {"node_type":"guard","status":"ok"}
```

### Environment variables

| Variable | Default | Description |
|---|---|---|
| `PORT` | `8080` | Port the server listens on |
| `NODE_TYPE` | `unknown` | Type of node (guard, relay, exit, directory-server, echo-server) |
| `DIRECTORY_SERVER_URL` | `http://localhost:8080` | URL of the directory server |

## Testing

```bash
# Run tests for all modules
for dir in directory-server guard-node relay-node exit-node client; do
  (cd $dir && go test ./...)
done
```