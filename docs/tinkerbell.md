# Power management for Tinkerbell nodes

Tinkerbell controls machine power through **Rufio**, which talks to BMCs using
[bmclib](https://github.com/bmc-toolbox/bmclib). PoE-powered nodes (a Raspberry
Pi CM4 on a PoE switch port, say) have no BMC, but bmclib ships a generic
**`rpc` provider**: it sends JSON over HTTP to a "consumer" URL of your choice.
`unifi-power-controller` is that consumer. It translates Rufio's requests into
UniFi PoE changes on the switch port the node is plugged into, so the switch
*is* the BMC.

```
Workflow (bootMode: netboot)
   │  creates
   ▼
Rufio Job ──► Rufio Machine ──► bmclib rpc provider ──HTTP POST /rpc──► unifi-power-controller
 off → PXE → on                 (HMAC-signed)                                │
                                                                             ▼
                                                    UniFi Network API: port_overrides / power-cycle
                                                                             │
                                                                             ▼
                                                          Switch PoE port ──► node
```

## What gets translated

| Rufio / bmclib request | Effect on the switch port |
| --- | --- |
| `setPowerState` `on` | `poe_mode: auto` |
| `setPowerState` `off` | `poe_mode: off` |
| `setPowerState` `cycle`, `reset` | UniFi `power-cycle` port command (PoE enabled → off → on). If the port is currently off, it is simply enabled. |
| `setPowerState` `soft` | **Rejected** (501). A PoE port can cut power but cannot ask an OS to shut down. |
| `getPowerState` | `on` / `off` from the port's PoE mode |
| `setBootDevice` | **Acknowledged, does nothing.** There is no BMC to program a boot device. |
| `ping` | `pong` (bmclib pings when it opens a connection) |

Two consequences worth understanding:

1. **Nodes must network-boot by default.** Tinkerbell's netboot workflow asks
   for "boot once from PXE"; we can't honour that, so we say "ok" and rely on
   the node already trying the network (Smee then decides whether to serve an
   install environment or tell it to boot locally). For a CM4, set the
   bootloader EEPROM `BOOT_ORDER` so network boot is tried first / on a blank
   disk.
2. **Off → on is rate-limited on purpose.** The netboot sequence is
   *hard-off, set-boot-device, power-on*, which can land within milliseconds.
   A board whose supply never actually drops out does not reboot. `minOffTime`
   (default `5s`) makes the service hold a power-on until PoE has been off for
   at least that long. Raise it if your nodes need longer to discharge.

## 1. Prerequisites on the UniFi side

* A **UniFi OS** console (UDM, UCG, Cloud Key Gen2+, …). API keys are a UniFi OS
  feature; classic self-hosted controllers are not supported.
* An API key created in the Network application (Settings → Control Plane →
  Integrations). The key acts with the permissions of the admin who made it;
  it needs to be able to edit device settings.
* *Recommended:* in the UniFi UI, give each node's port an explicit override
  once (e.g. set the PoE mode to "Auto" and save). The service rewrites the
  switch's whole `port_overrides` list preserving every other port, but only
  sets `port_idx` and `poe_mode` on a port that has no override yet.
* The switch's MAC address and the **1-based port number** each node uses, as
  shown in the UniFi UI.

## 2. Deploy the service

See [`config.example.yaml`](../config.example.yaml) and
[`deploy/kubernetes.yaml`](../deploy/kubernetes.yaml). Create two secrets: the
UniFi API key, and a random shared secret for HMAC:

```bash
openssl rand -hex 32   # the RPC HMAC secret
```

Run a single replica; it serialises writes to a switch's `port_overrides` in
process.

The node name in `nodes:` must equal what Rufio sends as `host`, which is the
Machine's `spec.connection.host`. That is how a request for `lab-node-1` finds
port 1.

## 3. Register each node with Rufio

### Tinkerbell with `bmc.tinkerbell.org/v1alpha1` (Machine + `bmcRef`)

The HMAC secret is a Kubernetes Secret with a `secret` key, referenced from the
Machine. With `rpc` options set, `authSecretRef` is not used and no other BMC
provider is tried.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: unifi-power-hmac
  namespace: tinkerbell
type: Opaque
stringData:
  secret: "<the openssl rand output>"
---
apiVersion: bmc.tinkerbell.org/v1alpha1
kind: Machine
metadata:
  name: lab-node-1
  namespace: tinkerbell
spec:
  connection:
    host: lab-node-1                 # sent as "host"; must match nodes: in config
    insecureTLS: false
    providerOptions:
      rpc:
        consumerURL: http://unifi-power-controller.unifi-power.svc.cluster.local/rpc
        # signature:
        #   includedPayloadHeaders: ["X-BMCLIB-Timestamp"]   # and signedHeaders: in our config
        hmac:
          secrets:
            sha256:
              - name: unifi-power-hmac
                namespace: tinkerbell
```

Then point the node's `Hardware` at it:

```yaml
apiVersion: tinkerbell.org/v1alpha1
kind: Hardware
metadata:
  name: lab-node-1
  namespace: tinkerbell
spec:
  bmcRef:
    apiGroup: bmc.tinkerbell.org
    kind: Machine
    name: lab-node-1
  # ... interfaces, metadata, etc.
```

A Workflow that sets `bootOptions.bootMode: netboot` (with
`toggleAllowNetboot: true`) now makes the Tinkerbell controller create a Rufio
Job that runs **off → one-time PXE boot → on** against that Machine; the
service turns those into PoE off, a no-op acknowledgement, and PoE on.

### Tinkerbell with `v1alpha2` (BMC inline in `Hardware`)

There is no separate Machine; the same `rpc` options live under the Hardware's
`spec.bmc.connection`. The service is unchanged. Credentials/HMAC secrets are
referenced the way the v1alpha2 `Hardware` docs describe; check your Tinkerbell
version's `docs/technical/v1alpha2/hardware.yaml`.

```yaml
spec:
  bmc:
    connection:
      host: lab-node-1
      providerOptions:
        rpc:
          consumerURL: http://unifi-power-controller.unifi-power.svc.cluster.local/rpc
```

## 4. Test it without Tinkerbell

The REST interface is handy for smoke tests and runs without HMAC:

```bash
kubectl port-forward svc/unifi-power-controller 8080:80
curl -X POST localhost:8080/nodes/lab-node-1/power -d '{"action":"cycle"}'
curl localhost:8080/nodes/lab-node-1/power        # {"state":"on"}
```

To exercise the exact thing Rufio sends (if HMAC is on, add the signature
header; if you'd rather not, test on a throwaway instance with no `rpc:`
secrets):

```bash
curl -s localhost:8080/rpc -d '{"id":1,"host":"lab-node-1","method":"getPowerState"}'
# {"id":1,"host":"lab-node-1","result":"on"}
```

## Security notes

* `/rpc` and the REST endpoints can cut power to your nodes. Keep the Service
  `ClusterIP`, don't put it behind a public Gateway, and set `rpc.hmacSHA256Files`.
  The REST API has no authentication of its own; protect it with a NetworkPolicy
  or by not exposing it (`kubectl port-forward` is fine).
* HMAC authenticates the request body (and any `signedHeaders`). It does not by
  itself prevent replay of a captured request; sign the timestamp header and
  keep the service on a trusted network.
* The API key can edit anything its owner can in the Network app. Use a
  dedicated, least-privileged local admin to create it.

## Troubleshooting

* **`ping` fails when Rufio opens the Machine** — check `consumerURL` is
  reachable from the Rufio pod and that the HMAC secret matches (401 in the
  service log: `rpc: bad or missing signature`).
* **`unknown node "…"`** — the Machine's `connection.host` isn't a key under
  `nodes:`.
* **502 / `set poe_mode… `** — the UniFi call failed; the log has the controller's
  error. Confirm `unifi.url` is the UniFi OS console and the API key works.
* **Node doesn't reboot on cycle** — try a larger `minOffTime`, and confirm the
  node is actually powered by that port's PoE.
