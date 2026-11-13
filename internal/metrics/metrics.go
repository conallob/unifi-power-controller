// Package metrics builds the Prometheus registry served on /metrics.
//
// Every label value is drawn from a fixed set (configured node names, known
// RPC methods, known actions), never from request input, so cardinality is
// bounded however much junk is sent to the service.
package metrics

import (
	"context"
	"net/http"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/conallob/unifi-power-controller/internal/power"
)

const namespace = "unifi_power_controller"

// Info is the build metadata exposed as labels on the build_info metric.
type Info struct {
	Version string
	Commit  string
	Date    string
}

// StateSource reports the current power state of every configured node.
type StateSource interface {
	Snapshot(ctx context.Context) []power.NodeState
}

// Config configures New.
type Config struct {
	Info Info
	// Nodes are the configured nodes; they bound the "node" label values and
	// drive nodes_configured.
	Nodes map[string]power.Target
	// States, if set, enables node_power_state.
	States StateSource
	// StateCacheTTL bounds how often a scrape triggers UniFi API calls
	// (default 5s). StateTimeout bounds one snapshot (default 10s).
	StateCacheTTL time.Duration
	StateTimeout  time.Duration
}

// Values used for the counters' "method", "action" and "result" labels.
var (
	rpcMethods = []string{"ping", "getPowerState", "setPowerState", "setBootDevice"}
	actions    = []string{"on", "off", "cycle"}
	states     = []power.State{power.StateOn, power.StateOff, power.StateUnknown}
)

// Metrics owns the registry and records request counters.
type Metrics struct {
	Registry *prometheus.Registry

	rpc          *prometheus.CounterVec
	actions      *prometheus.CounterVec
	actionErrors *prometheus.CounterVec
	known        map[string]bool
}

// New builds a registry with the Go runtime and process collectors, build_info,
// nodes_configured, node_power_state, and the request counters.
func New(cfg Config) *Metrics {
	m := &Metrics{
		Registry: prometheus.NewRegistry(),
		known:    map[string]bool{},
		rpc: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "rpc_requests_total",
			Help: "bmclib/Rufio RPC requests received on POST /rpc, by method and result " +
				"(ok, error, unauthorized, bad_request). Unrecognised methods are counted as method=\"unknown\".",
		}, []string{"method", "result"}),
		actions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "power_actions_total",
			Help: "PoE power actions requested for a node, from either the REST API or RPC, " +
				"by action (on, off, cycle). Every attempt is counted, including failures and requests for " +
				"unconfigured nodes (node=\"unknown\").",
		}, []string{"node", "action"}),
		actionErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "power_action_errors_total",
			Help: "PoE power actions that failed because the UniFi controller call failed, by action. " +
				"A subset of power_actions_total; requests for unconfigured nodes are not errors here.",
		}, []string{"node", "action"}),
	}

	perSwitch := map[string]int{}
	for name, t := range cfg.Nodes {
		m.known[name] = true
		perSwitch[t.SwitchMAC]++
		for _, a := range actions {
			// Pre-create at 0 so rate()/increase() work from the first event.
			m.actions.WithLabelValues(name, a)
			m.actionErrors.WithLabelValues(name, a)
		}
	}
	for _, a := range actions {
		m.actions.WithLabelValues("unknown", a)
	}
	for _, meth := range rpcMethods {
		for _, r := range []string{"ok", "error"} {
			m.rpc.WithLabelValues(meth, r)
		}
	}
	for _, r := range []string{"error", "unauthorized", "bad_request"} {
		m.rpc.WithLabelValues("unknown", r)
	}

	sc := &stateCollector{
		src: cfg.States, perSwitch: perSwitch, now: time.Now,
		ttl: orDefault(cfg.StateCacheTTL, 5*time.Second), timeout: orDefault(cfg.StateTimeout, 10*time.Second),
		nodesDesc: prometheus.NewDesc(namespace+"_nodes_configured",
			"Number of nodes configured, by the (MAC address of the) switch they are wired to.", []string{"switch"}, nil),
		stateDesc: prometheus.NewDesc(namespace+"_node_power_state",
			"Power state of each configured node's PoE port as a state set: for every node, exactly one of "+
				"state=on|off|unknown is 1. unknown means the switch could not be queried.",
			[]string{"node", "switch", "state"}, nil),
	}

	m.Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: namespace + "_build_info",
			Help: "A metric with a constant '1' value labeled with the version, commit and build date " +
				"the binary was built from, and the Go version it was compiled with.",
			ConstLabels: prometheus.Labels{
				"version":   cfg.Info.Version,
				"commit":    cfg.Info.Commit,
				"date":      cfg.Info.Date,
				"goversion": runtime.Version(),
			},
		}, func() float64 { return 1 }),
		m.rpc, m.actions, m.actionErrors, sc,
	)
	return m
}

// Handler serves the registry in the Prometheus exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}

// RPC records one POST /rpc request. Methods outside the known set are
// recorded as "unknown".
func (m *Metrics) RPC(method, result string) {
	if !contains(rpcMethods, method) {
		method = "unknown"
	}
	m.rpc.WithLabelValues(method, result).Inc()
}

// Action records one PoE power action with its outcome (success, error or
// unknown_node). Every call counts toward power_actions_total; only "error"
// (a failed controller call) counts toward power_action_errors_total. Nodes
// that are not configured are recorded as "unknown".
func (m *Metrics) Action(node, action, result string) {
	if !m.known[node] {
		node = "unknown"
	}
	m.actions.WithLabelValues(node, action).Inc()
	if result == "error" {
		m.actionErrors.WithLabelValues(node, action).Inc()
	}
}

// stateCollector computes node_power_state and nodes_configured at scrape
// time. Snapshots are cached briefly so scrape frequency can't translate into
// UniFi API load.
type stateCollector struct {
	src       StateSource
	perSwitch map[string]int
	ttl       time.Duration
	timeout   time.Duration
	now       func() time.Time

	nodesDesc, stateDesc *prometheus.Desc

	mu     sync.Mutex
	at     time.Time
	cached []power.NodeState
}

func (c *stateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.nodesDesc
	ch <- c.stateDesc
}

func (c *stateCollector) Collect(ch chan<- prometheus.Metric) {
	switches := make([]string, 0, len(c.perSwitch))
	for mac := range c.perSwitch {
		switches = append(switches, mac)
	}
	sort.Strings(switches)
	for _, mac := range switches {
		ch <- prometheus.MustNewConstMetric(c.nodesDesc, prometheus.GaugeValue, float64(c.perSwitch[mac]), mac)
	}
	if c.src == nil {
		return
	}
	for _, ns := range c.snapshot() {
		for _, st := range states {
			v := 0.0
			if ns.State == st {
				v = 1
			}
			ch <- prometheus.MustNewConstMetric(c.stateDesc, prometheus.GaugeValue, v, ns.Node, ns.SwitchMAC, string(st))
		}
	}
}

func (c *stateCollector) snapshot() []power.NodeState {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != nil && c.now().Sub(c.at) < c.ttl {
		return c.cached
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	c.cached, c.at = c.src.Snapshot(ctx), c.now()
	return c.cached
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}
