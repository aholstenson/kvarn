# kvarn

A Go application that orchestrates VMs (local, EC2, GCP, Azure) and runs a runner agent on those VMs. The orchestrator manages VM lifecycle and dispatches work; the runner executes commands on VMs. Communication between components uses ConnectRPC.

## Build

```sh
buf generate
go build ./cmd/kvarn
```

## Usage

Run the orchestrator:

```sh
./kvarn orchestrator --addr :8080
```

Run the runner:

```sh
./kvarn runner --addr :9090
```
