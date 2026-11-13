# unifi-power-controller

Remote power control for PoE-powered lab nodes, via the UniFi Network API.
Switches a node's PoE port **on**, **off**, or **cycles** it, and plugs into
[Tinkerbell](https://tinkerbell.org) as a BMC so netboot workflows get full
power management of nodes that have no BMC.

Written in Go on top of
[ubiquiti-community/go-unifi](https://github.com/ubiquiti-community/go-unifi).
It is intended to replace the embedded Python script in
[`homelab-apps/apps/node-power`](https://github.com/conallob/homelab-apps/pull/24).

## Interfaces

| Endpoint | Purpose |
| --- | --- |
| `POST /nodes/{name}/power` `{"action":"on\|off\|cycle"}` | Same contract as the Python service → `204` |
| `GET /nodes/{name}/power` | `{"state":"on\|off"}` |
| `POST /rpc` | bmclib/Rufio `rpc` provider (HMAC-signed); see [docs/tinkerbell.md](docs/tinkerbell.md) |
| `GET /healthz` | Liveness/readiness |

## Quick start

```bash
cp config.example.yaml config.yaml          # edit switch MAC, nodes, url
export UNIFI_API_KEY=...                    # or unifi.apiKeyFile
go run ./cmd/unifi-power-controller -config config.yaml
curl -X POST localhost:8080/nodes/lab-node-1/power -d '{"action":"cycle"}'
```

Kubernetes reference manifests are in [`deploy/kubernetes.yaml`](deploy/kubernetes.yaml)
and a `Dockerfile` is provided; no image is published yet.

## Requirements and behaviour

* A **UniFi OS** console and an API key. go-unifi's API-key mode speaks the UniFi OS
  API (`X-Api-Key` header, `/proxy/network/…` paths). Classic self-hosted controllers
  are not supported.
* `on`/`off` rewrite the switch's `port_overrides` (other ports' overrides are
  preserved); `cycle` uses UniFi's `power-cycle` port command.
* PoE can only cut power: Rufio's `soft` shutdown is rejected, and `setBootDevice`
  is acknowledged as a no-op. Nodes must network-boot by default.
* `minOffTime` (default 5s) keeps PoE off long enough for a node to really lose power
  when Tinkerbell does off → on back to back.

## Migrating from the Python script

The REST contract is unchanged, so the swap is a Deployment/ConfigMap change.
Differences worth knowing: the Go service uses `X-Api-Key` and the
`/proxy/network` path prefix required by UniFi OS; the Python script used
`X-API-KEY` against bare `/api/s/...` paths, which a UniFi OS console does not
serve. Node→port mapping moves from `NODE_PORT_MAP` into `config.yaml`.

## Development

```bash
go test -race ./...
```

Tests include a wire-level test driving the real go-unifi client against a fake
UniFi OS controller, so the headers, paths and payloads are asserted, not assumed.
