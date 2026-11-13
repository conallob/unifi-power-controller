package metrics

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/conallob/unifi-power-controller/internal/power"
)

type fakeSource struct {
	calls  int
	states []power.NodeState
}

func (f *fakeSource) Snapshot(context.Context) []power.NodeState {
	f.calls++
	return f.states
}

var nodes = map[string]power.Target{
	"n1": {SwitchMAC: "aa", Port: 1},
	"n2": {SwitchMAC: "aa", Port: 2},
	"n3": {SwitchMAC: "bb", Port: 1},
}

func TestBuildInfo(t *testing.T) {
	m := New(Config{Info: Info{Version: "1.2.3", Commit: "abc1234", Date: "2026-11-13T00:00:00Z"}})
	rr := httptest.NewRecorder()
	m.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	body := rr.Body.String()
	if rr.Code != 200 || !strings.Contains(body, `unifi_power_controller_build_info{commit="abc1234",date="2026-11-13T00:00:00Z",goversion="go`) || !strings.Contains(body, `version="1.2.3"} 1`) {
		t.Fatalf("unexpected exposition (code %d):\n%.600s", rr.Code, body)
	}
}

func TestNodesConfiguredAndState(t *testing.T) {
	src := &fakeSource{states: []power.NodeState{
		{Node: "n1", SwitchMAC: "aa", State: power.StateOn},
		{Node: "n2", SwitchMAC: "aa", State: power.StateOff},
		{Node: "n3", SwitchMAC: "bb", State: power.StateUnknown},
	}}
	m := New(Config{Nodes: nodes, States: src})

	if err := testutil.GatherAndCompare(m.Registry, strings.NewReader(`
# HELP unifi_power_controller_nodes_configured Number of nodes configured, by the (MAC address of the) switch they are wired to.
# TYPE unifi_power_controller_nodes_configured gauge
unifi_power_controller_nodes_configured{switch="aa"} 2
unifi_power_controller_nodes_configured{switch="bb"} 1
`), "unifi_power_controller_nodes_configured"); err != nil {
		t.Fatal(err)
	}

	if err := testutil.GatherAndCompare(m.Registry, strings.NewReader(`
# HELP unifi_power_controller_node_power_state Power state of each configured node's PoE port as a state set: for every node, exactly one of state=on|off|unknown is 1. unknown means the switch could not be queried.
# TYPE unifi_power_controller_node_power_state gauge
unifi_power_controller_node_power_state{node="n1",state="off",switch="aa"} 0
unifi_power_controller_node_power_state{node="n1",state="on",switch="aa"} 1
unifi_power_controller_node_power_state{node="n1",state="unknown",switch="aa"} 0
unifi_power_controller_node_power_state{node="n2",state="off",switch="aa"} 1
unifi_power_controller_node_power_state{node="n2",state="on",switch="aa"} 0
unifi_power_controller_node_power_state{node="n2",state="unknown",switch="aa"} 0
unifi_power_controller_node_power_state{node="n3",state="off",switch="bb"} 0
unifi_power_controller_node_power_state{node="n3",state="on",switch="bb"} 0
unifi_power_controller_node_power_state{node="n3",state="unknown",switch="bb"} 1
`), "unifi_power_controller_node_power_state"); err != nil {
		t.Fatal(err)
	}
}

func TestStateSnapshotIsCached(t *testing.T) {
	src := &fakeSource{states: []power.NodeState{{Node: "n1", SwitchMAC: "aa", State: power.StateOn}}}
	m := New(Config{Nodes: nodes, States: src, StateCacheTTL: time.Minute})
	for i := 0; i < 5; i++ { // five scrapes within the TTL
		if _, err := m.Registry.Gather(); err != nil {
			t.Fatal(err)
		}
	}
	if src.calls != 1 {
		t.Fatalf("snapshot calls = %d, want 1 (cached)", src.calls)
	}

	src2 := &fakeSource{states: src.states}
	m2 := New(Config{Nodes: nodes, States: src2, StateCacheTTL: time.Nanosecond})
	for i := 0; i < 3; i++ {
		time.Sleep(time.Millisecond)
		_, _ = m2.Registry.Gather()
	}
	if src2.calls != 3 {
		t.Fatalf("expired cache should refetch each scrape, calls = %d", src2.calls)
	}
}

func TestCountersAndBoundedLabels(t *testing.T) {
	m := New(Config{Nodes: nodes})

	m.Action("n1", "cycle", "success")
	m.Action("n1", "cycle", "success")
	m.Action("n1", "off", "error")
	m.Action("not-a-node", "on", "unknown_node")
	m.Action("attacker-supplied-1", "on", "unknown_node")
	m.RPC("setPowerState", "ok")
	m.RPC("setPowerState", "ok")
	m.RPC("getPowerState", "error")
	m.RPC("totally-made-up", "bad_request")
	m.RPC("another-made-up", "bad_request")

	for _, c := range []struct {
		got, want float64
		what      string
	}{
		{testutil.ToFloat64(m.actions.WithLabelValues("n1", "cycle")), 2, "n1 cycle attempts"},
		{testutil.ToFloat64(m.actions.WithLabelValues("n1", "off")), 1, "n1 off attempts (a failed attempt still counts)"},
		{testutil.ToFloat64(m.actionErrors.WithLabelValues("n1", "off")), 1, "n1 off errors"},
		{testutil.ToFloat64(m.actionErrors.WithLabelValues("n1", "cycle")), 0, "n1 cycle errors (successes are not errors)"},
		{testutil.ToFloat64(m.actions.WithLabelValues("n2", "on")), 0, "n2 on (pre-created at 0)"},
		{testutil.ToFloat64(m.actions.WithLabelValues("unknown", "on")), 2, "unknown node bucket"},
		{testutil.ToFloat64(m.rpc.WithLabelValues("setPowerState", "ok")), 2, "setPowerState ok"},
		{testutil.ToFloat64(m.rpc.WithLabelValues("getPowerState", "error")), 1, "getPowerState error"},
		{testutil.ToFloat64(m.rpc.WithLabelValues("unknown", "bad_request")), 2, "unknown method bucket"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.what, c.got, c.want)
		}
	}

	// Unknown-node requests are attempts but not errors.
	if n := testutil.ToFloat64(m.actionErrors.WithLabelValues("n1", "on")); n != 0 {
		t.Errorf("unexpected error count %v", n)
	}

	// Junk input must not have created new series: (3 nodes + "unknown") x 3
	// actions attempts, 3 nodes x 3 actions errors.
	if n := testutil.CollectAndCount(m.actions); n != 4*3 {
		t.Errorf("power_actions_total series = %d, want %d", n, 4*3)
	}
	if n := testutil.CollectAndCount(m.actionErrors); n != 3*3 {
		t.Errorf("power_action_errors_total series = %d, want %d", n, 3*3)
	}
	if n := testutil.CollectAndCount(m.rpc); n != 4*2+3 {
		t.Errorf("rpc_requests_total series = %d, want %d", n, 4*2+3)
	}
}
