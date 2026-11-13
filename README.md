# unifi-power-controller

Remote power control for PoE-powered lab nodes, via the UniFi Network API.
Switches a node's PoE port **on**, **off**, or **cycles** it, and plugs into
[Tinkerbell](https://tinkerbell.org) as a BMC so netboot workflows get full
power management of nodes that have no BMC.

Written in Go on top of
[ubiquiti-community/go-unifi](https://github.com/ubiquiti-community/go-unifi)

## Interfaces

| Endpoint | Purpose |
| --- | --- |
| `POST /nodes/{name}/power` `{"action":"on\|off\|cycle"}` | Same contract as the Python service → `204` |
| `GET /nodes/{name}/power` | `{"state":"on\|off"}` |
| `POST /rpc` | bmclib/Rufio `rpc` provider (HMAC-signed); see [docs/tinkerbell.md](docs/tinkerbell.md) |
| `GET /healthz` | Liveness/readiness |
| `GET /metrics` | Prometheus metrics: build info, nodes configured, node power state, request counters ([details](#metrics)) |

## Quick start

```bash
cp config.example.yaml config.yaml          # edit switch MAC, nodes, url
export UNIFI_API_KEY=...                    # or unifi.apiKeyFile
go run ./cmd/unifi-power-controller -config config.yaml
curl -X POST localhost:8080/nodes/lab-node-1/power -d '{"action":"cycle"}'
```

Kubernetes reference manifests are in [`deploy/kubernetes.yaml`](deploy/kubernetes.yaml)
and images are published to `ghcr.io/conallob/unifi-power-controller` on each release
(pin a version tag rather than `latest`).

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

## Releases

Releases are built by [GoReleaser](https://goreleaser.com) (`.goreleaser.yaml`) when a
`v*` tag is pushed:

```bash
git tag -a v0.1.0 -m "v0.1.0" && git push origin v0.1.0
```

That publishes a GitHub release (linux amd64/arm64 archives, `checksums.txt`,
a changelog; tags like `v0.2.0-rc.1` become pre-releases) and a multi-arch image at
`ghcr.io/conallob/unifi-power-controller:<version>`. The version, commit and build date
are embedded via `-ldflags -X` into `internal/version`:

```bash
unifi-power-controller -version
# unifi-power-controller 0.1.0 (commit 1a2b3c4..., built 2026-11-13T12:00:00Z, go1.27.4)
```

The version is also logged at startup and exported as a metric (see below). Unstamped builds
(`go run`, `go install`) fall back to the toolchain's embedded build info. To try a release
locally without publishing: `goreleaser release --snapshot --clean --skip=publish`.

## Metrics

`GET /metrics` serves Prometheus metrics (plus the standard Go runtime and process metrics).
All label values come from fixed sets (configured node names, known RPC methods and actions),
never from request input, so cardinality stays bounded however much junk is sent.

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `unifi_power_controller_build_info` | gauge (always 1) | `version`, `commit`, `date`, `goversion` | What is running |
| `unifi_power_controller_nodes_configured` | gauge | `switch` (MAC) | Nodes configured per switch |
| `unifi_power_controller_node_power_state` | gauge (state set) | `node`, `switch`, `state=on\|off\|unknown` | Exactly one `state` is 1 per node; `unknown` = the switch couldn't be queried |
| `unifi_power_controller_rpc_requests_total` | counter | `method`, `result=ok\|error\|unauthorized\|bad_request` | `POST /rpc` requests from Rufio/bmclib; unrecognised methods are `method="unknown"` |
| `unifi_power_controller_power_actions_total` | counter | `node`, `action=on\|off\|cycle`, `result=success\|error\|unknown_node` | PoE changes requested via REST **or** RPC (`reset` counts as `cycle`); unconfigured nodes are `node="unknown"` |

Counters are created at 0 for every configured node, so `increase()`/`rate()` work from the first
event. `node_power_state` is computed at scrape time with one UniFi API call per switch, cached for
5s so scrape frequency can't turn into controller load.

```promql
# Version rolled out across instances; join onto other series
up{job="unifi-power-controller"} * on(instance) group_left(version) unifi_power_controller_build_info

# Nodes by state
sum by (state) (unifi_power_controller_node_power_state)

# Node PoE is off (alert with a `for:` that suits you), or the switch is unreachable
unifi_power_controller_node_power_state{state="off"} == 1
unifi_power_controller_node_power_state{state="unknown"} == 1

# A node being power-cycled repeatedly is usually a sign of something wrong
increase(unifi_power_controller_power_actions_total{action="cycle",result="success"}[1h]) > 3

# RPC traffic from Tinkerbell, by method and result
sum by (method, result) (rate(unifi_power_controller_rpc_requests_total[5m]))

# Failed PoE changes
sum by (node) (increase(unifi_power_controller_power_actions_total{result="error"}[15m])) > 0
```

`/metrics` is unauthenticated; like the rest of the service, keep it on a `ClusterIP`.

## Development

```bash
go test -race ./...
```

Tests include a wire-level test driving the real go-unifi client against a fake
UniFi OS controller, so the headers, paths and payloads are asserted, not assumed.
