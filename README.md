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
| `GET /metrics` | Prometheus metrics (Go runtime, process, and `unifi_power_controller_build_info`) |

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

The version is also logged at startup and exported as the labels of a constant-1 gauge on
`/metrics`:

```
unifi_power_controller_build_info{commit="1a2b3c4...",date="2026-11-13T12:00:00Z",goversion="go1.27.4",version="0.1.0"} 1
```

so you can alert on version skew or attach the version to other series:

```promql
up{job="unifi-power-controller"} * on(instance) group_left(version) unifi_power_controller_build_info
```

`/metrics` is unauthenticated; like the rest of the service, keep it on a `ClusterIP`. Unstamped builds (`go run`, `go install`) fall back
to the toolchain's embedded build info. To try a release locally without publishing:
`goreleaser release --snapshot --clean --skip=publish`.

## Development

```bash
go test -race ./...
```

Tests include a wire-level test driving the real go-unifi client against a fake
UniFi OS controller, so the headers, paths and payloads are asserted, not assumed.
