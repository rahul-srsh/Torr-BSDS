# Torr-BSDS

A simplified onion routing network built on AWS ECS Fargate in Go. Routes traffic through encrypted multi-hop circuits via guard, relay, and exit nodes coordinated by a directory server. Experiments measure latency vs. privacy tradeoffs, throughput scaling with relay count, and circuit recovery under node failure using Locust and CloudWatch.

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

TODO

## Testing

```bash
for dir in directory-server guard-node relay-node exit-node client; do
  (cd $dir && go test ./...)
done
```
