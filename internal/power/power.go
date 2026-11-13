// Package power switches PoE on individual switch ports via the UniFi Network
// API, using github.com/ubiquiti-community/go-unifi.
package power

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ubiquiti-community/go-unifi/unifi"
)

// PoE modes understood by the UniFi controller.
const (
	poeAuto = "auto"
	poeOff  = "off"
)

// State is the PoE power state of a node's switch port.
type State string

const (
	StateOn  State = "on"
	StateOff State = "off"
	// StateUnknown means the switch could not be queried.
	StateUnknown State = "unknown"
)

// NodeState is one node's power state as of a Snapshot.
type NodeState struct {
	Node      string
	SwitchMAC string
	State     State
}

// ErrUnknownNode is returned for a node name that is not configured.
var ErrUnknownNode = errors.New("unknown node")

// Target identifies the switch port a node is wired to.
type Target struct {
	SwitchMAC string // lower-case, colon separated
	Port      int64  // 1-based port index, as shown in the UniFi UI
}

// API is the subset of *unifi.ApiClient used here. Declared as an interface
// so tests can substitute a fake controller.
type API interface {
	GetDeviceByMAC(ctx context.Context, site, mac string) (*unifi.Device, error)
	UpdateDevice(ctx context.Context, site string, d *unifi.Device) (*unifi.Device, error)
	ExecuteCmd(ctx context.Context, site, mgr string, cmd unifi.Cmd) (any, error)
}

var _ API = (*unifi.ApiClient)(nil)

// Controller maps node names to switch ports and drives their PoE state.
type Controller struct {
	api   API
	site  string
	nodes map[string]Target

	minOff time.Duration
	now    func() time.Time
	sleep  func(ctx context.Context, d time.Duration) error

	// mu guards lastOff, and serialises read-modify-write of port_overrides,
	// which is replaced wholesale on every write: concurrent updates to a
	// switch would otherwise lose each other's changes.
	mu      sync.Mutex
	lastOff map[string]time.Time
}

// Option customises a Controller.
type Option func(*Controller)

// WithMinOffTime makes On (and Cycle on an unpowered port) wait until PoE has
// been off for at least d. Tinkerbell's netboot sequence is power-off,
// set-boot-device, power-on; without a floor the off/on can land within
// milliseconds and the node's supply never actually drops out.
func WithMinOffTime(d time.Duration) Option { return func(c *Controller) { c.minOff = d } }

// New returns a Controller for the given site and node→port mapping.
func New(api API, site string, nodes map[string]Target, opts ...Option) *Controller {
	c := &Controller{
		api: api, site: site, nodes: nodes,
		now:     time.Now,
		sleep:   sleepCtx,
		lastOff: map[string]time.Time{},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// waitMinOff blocks until the node has been off for minOff. Called without
// holding mu so one node's wait doesn't stall the others.
func (c *Controller) waitMinOff(ctx context.Context, node string) error {
	c.mu.Lock()
	at, ok := c.lastOff[node]
	c.mu.Unlock()
	if !ok || c.minOff <= 0 {
		return nil
	}
	if remaining := c.minOff - c.now().Sub(at); remaining > 0 {
		return c.sleep(ctx, remaining)
	}
	return nil
}

// Nodes returns the configured node names.
func (c *Controller) Nodes() []string {
	names := make([]string, 0, len(c.nodes))
	for n := range c.nodes {
		names = append(names, n)
	}
	return names
}

// Snapshot returns the power state of every configured node, in name order.
// Each switch is queried once however many nodes it hosts. A switch that cannot
// be queried yields StateUnknown for its nodes rather than failing the whole
// snapshot.
func (c *Controller) Snapshot(ctx context.Context) []NodeState {
	names := c.Nodes()
	sort.Strings(names)

	devices := map[string]*unifi.Device{} // by switch MAC; nil = query failed
	out := make([]NodeState, 0, len(names))
	for _, n := range names {
		t := c.nodes[n]
		dev, seen := devices[t.SwitchMAC]
		if !seen {
			d, err := c.api.GetDeviceByMAC(ctx, c.site, t.SwitchMAC)
			if err != nil {
				d = nil
			}
			devices[t.SwitchMAC], dev = d, d
		}
		st := StateUnknown
		if dev != nil {
			st = StateOn
			if effectiveMode(dev, t.Port) == poeOff {
				st = StateOff
			}
		}
		out = append(out, NodeState{Node: n, SwitchMAC: t.SwitchMAC, State: st})
	}
	return out
}

// Targets returns a copy of the node→port mapping.
func (c *Controller) Targets() map[string]Target {
	m := make(map[string]Target, len(c.nodes))
	for k, v := range c.nodes {
		m[k] = v
	}
	return m
}

func (c *Controller) target(node string) (Target, error) {
	t, ok := c.nodes[node]
	if !ok {
		return Target{}, fmt.Errorf("%w %q", ErrUnknownNode, node)
	}
	return t, nil
}

// On enables PoE on the node's port. It is a no-op if already enabled.
func (c *Controller) On(ctx context.Context, node string) error {
	if _, err := c.target(node); err != nil {
		return err
	}
	if err := c.waitMinOff(ctx, node); err != nil {
		return err
	}
	return c.setMode(ctx, node, poeAuto)
}

// Off disables PoE on the node's port. It is a no-op if already disabled.
func (c *Controller) Off(ctx context.Context, node string) error {
	return c.setMode(ctx, node, poeOff)
}

// Cycle hard power-cycles the node. If PoE is currently disabled there is
// nothing to cycle, so it is simply enabled.
func (c *Controller) Cycle(ctx context.Context, node string) error {
	t, err := c.target(node)
	if err != nil {
		return err
	}
	if err := c.waitMinOff(ctx, node); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	dev, err := c.api.GetDeviceByMAC(ctx, c.site, t.SwitchMAC)
	if err != nil {
		return fmt.Errorf("get switch %s: %w", t.SwitchMAC, err)
	}
	if mode := effectiveMode(dev, t.Port); mode == poeOff {
		if err := c.applyMode(ctx, dev, t, poeAuto); err != nil {
			return err
		}
		delete(c.lastOff, node)
		return nil
	}

	port := t.Port
	if _, err := c.api.ExecuteCmd(ctx, c.site, "devmgr", unifi.Cmd{
		Command: "power-cycle",
		Mac:     t.SwitchMAC,
		PortIdx: &port,
	}); err != nil {
		return fmt.Errorf("power-cycle %s port %d: %w", t.SwitchMAC, t.Port, err)
	}
	return nil
}

// State reports whether the node's port is currently supplying PoE.
func (c *Controller) State(ctx context.Context, node string) (State, error) {
	t, err := c.target(node)
	if err != nil {
		return "", err
	}
	dev, err := c.api.GetDeviceByMAC(ctx, c.site, t.SwitchMAC)
	if err != nil {
		return "", fmt.Errorf("get switch %s: %w", t.SwitchMAC, err)
	}
	if effectiveMode(dev, t.Port) == poeOff {
		return StateOff, nil
	}
	return StateOn, nil
}

func (c *Controller) setMode(ctx context.Context, node, mode string) error {
	t, err := c.target(node)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	dev, err := c.api.GetDeviceByMAC(ctx, c.site, t.SwitchMAC)
	if err != nil {
		return fmt.Errorf("get switch %s: %w", t.SwitchMAC, err)
	}
	// "On" means any mode that supplies power (auto, pasv24, passthrough), so
	// don't clobber a deliberately chosen non-auto mode that is already on.
	if on := effectiveMode(dev, t.Port) != poeOff; on == (mode != poeOff) {
		return nil
	}
	if err := c.applyMode(ctx, dev, t, mode); err != nil {
		return err
	}
	if mode == poeOff {
		c.lastOff[node] = c.now()
	} else {
		delete(c.lastOff, node)
	}
	return nil
}

// applyMode writes a port_overrides entry for the port, preserving every
// other override on the switch.
func (c *Controller) applyMode(ctx context.Context, dev *unifi.Device, t Target, mode string) error {
	overrides := make([]unifi.DevicePortOverrides, len(dev.PortOverrides))
	copy(overrides, dev.PortOverrides)

	found := false
	for i := range overrides {
		if overrides[i].PortIDX != nil && *overrides[i].PortIDX == t.Port {
			overrides[i].PoeMode = mode
			found = true
		}
	}
	if !found {
		port := t.Port
		overrides = append(overrides, unifi.DevicePortOverrides{PortIDX: &port, PoeMode: mode})
	}

	updated := *dev
	updated.PortOverrides = overrides
	if _, err := c.api.UpdateDevice(ctx, c.site, &updated); err != nil {
		return fmt.Errorf("set poe_mode=%s on %s port %d: %w", mode, t.SwitchMAC, t.Port, err)
	}
	return nil
}

// effectiveMode returns the PoE mode for a port: the configured override if
// present, otherwise whatever the switch reports in its live port table.
func effectiveMode(dev *unifi.Device, port int64) string {
	for _, o := range dev.PortOverrides {
		if o.PortIDX != nil && *o.PortIDX == port && o.PoeMode != "" {
			return o.PoeMode
		}
	}
	for _, p := range dev.PortTable {
		if p.PortIdx == port && p.PoeMode != "" {
			return p.PoeMode
		}
	}
	return poeAuto
}

// NormalizeMAC lower-cases a MAC address and converts dashes to colons.
func NormalizeMAC(mac string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(mac), "-", ":"))
}
