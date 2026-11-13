package power

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ubiquiti-community/go-unifi/unifi"
)

type fakeAPI struct {
	dev     unifi.Device
	updated []*unifi.Device
	cmds    []unifi.Cmd
	getErr  error
	cmdErr  error
}

func (f *fakeAPI) GetDeviceByMAC(_ context.Context, _, _ string) (*unifi.Device, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	d := f.dev
	d.PortOverrides = append([]unifi.DevicePortOverrides(nil), f.dev.PortOverrides...)
	return &d, nil
}

func (f *fakeAPI) UpdateDevice(_ context.Context, _ string, d *unifi.Device) (*unifi.Device, error) {
	f.updated = append(f.updated, d)
	f.dev = *d
	return d, nil
}

func (f *fakeAPI) ExecuteCmd(_ context.Context, _, _ string, c unifi.Cmd) (any, error) {
	f.cmds = append(f.cmds, c)
	return struct{}{}, f.cmdErr
}

func i64(v int64) *int64 { return &v }

func newCtl(f *fakeAPI) *Controller {
	return New(f, "default", map[string]Target{"lab-node-1": {SwitchMAC: "aa:bb:cc:dd:ee:ff", Port: 1}})
}

func overrideMode(d *unifi.Device, port int64) (string, bool) {
	for _, o := range d.PortOverrides {
		if o.PortIDX != nil && *o.PortIDX == port {
			return o.PoeMode, true
		}
	}
	return "", false
}

func TestOffThenOnPreservesOtherOverrides(t *testing.T) {
	other := unifi.DevicePortOverrides{PortIDX: i64(7), PoeMode: "auto", Name: "keep-me"}
	f := &fakeAPI{dev: unifi.Device{PortOverrides: []unifi.DevicePortOverrides{other}}}
	c := newCtl(f)

	if err := c.Off(context.Background(), "lab-node-1"); err != nil {
		t.Fatal(err)
	}
	if m, ok := overrideMode(&f.dev, 1); !ok || m != "off" {
		t.Fatalf("port 1 override = %q,%v want off", m, ok)
	}
	if len(f.dev.PortOverrides) != 2 || f.dev.PortOverrides[0].Name != "keep-me" {
		t.Fatalf("other override not preserved: %+v", f.dev.PortOverrides)
	}

	if err := c.On(context.Background(), "lab-node-1"); err != nil {
		t.Fatal(err)
	}
	if m, _ := overrideMode(&f.dev, 1); m != "auto" {
		t.Fatalf("port 1 mode = %q want auto", m)
	}
	if len(f.dev.PortOverrides) != 2 {
		t.Fatalf("override duplicated: %+v", f.dev.PortOverrides)
	}
}

func TestIdempotentNoWrite(t *testing.T) {
	f := &fakeAPI{dev: unifi.Device{PortOverrides: []unifi.DevicePortOverrides{{PortIDX: i64(1), PoeMode: "off"}}}}
	c := newCtl(f)
	if err := c.Off(context.Background(), "lab-node-1"); err != nil {
		t.Fatal(err)
	}
	// Unconfigured port defaults to PoE on.
	f2 := &fakeAPI{}
	if err := newCtl(f2).On(context.Background(), "lab-node-1"); err != nil {
		t.Fatal(err)
	}
	if len(f.updated)+len(f2.updated) != 0 {
		t.Fatal("expected no writes")
	}
}

func TestOnKeepsNonAutoPoweredMode(t *testing.T) {
	f := &fakeAPI{dev: unifi.Device{PortOverrides: []unifi.DevicePortOverrides{{PortIDX: i64(1), PoeMode: "pasv24"}}}}
	if err := newCtl(f).On(context.Background(), "lab-node-1"); err != nil {
		t.Fatal(err)
	}
	if len(f.updated) != 0 {
		t.Fatal("must not rewrite pasv24 to auto")
	}
}

func TestCycleSendsPowerCycle(t *testing.T) {
	f := &fakeAPI{}
	if err := newCtl(f).Cycle(context.Background(), "lab-node-1"); err != nil {
		t.Fatal(err)
	}
	if len(f.cmds) != 1 {
		t.Fatalf("cmds = %d", len(f.cmds))
	}
	got := f.cmds[0]
	if got.Command != "power-cycle" || got.Mac != "aa:bb:cc:dd:ee:ff" || got.PortIdx == nil || *got.PortIdx != 1 {
		t.Fatalf("unexpected cmd %+v", got)
	}
}

func TestCycleWhenOffTurnsOn(t *testing.T) {
	f := &fakeAPI{dev: unifi.Device{PortOverrides: []unifi.DevicePortOverrides{{PortIDX: i64(1), PoeMode: "off"}}}}
	if err := newCtl(f).Cycle(context.Background(), "lab-node-1"); err != nil {
		t.Fatal(err)
	}
	if len(f.cmds) != 0 {
		t.Fatal("power-cycle on an unpowered port is pointless")
	}
	if m, _ := overrideMode(&f.dev, 1); m != "auto" {
		t.Fatalf("mode = %q want auto", m)
	}
}

func TestState(t *testing.T) {
	f := &fakeAPI{dev: unifi.Device{PortTable: []unifi.DevicePortTable{{PortIdx: 1, PoeMode: "off"}}}}
	c := newCtl(f)
	if st, err := c.State(context.Background(), "lab-node-1"); err != nil || st != StateOff {
		t.Fatalf("state = %q,%v want off", st, err)
	}
	f.dev.PortOverrides = []unifi.DevicePortOverrides{{PortIDX: i64(1), PoeMode: "auto"}}
	if st, _ := c.State(context.Background(), "lab-node-1"); st != StateOn {
		t.Fatalf("override should win over port table, got %q", st)
	}
}

func TestErrors(t *testing.T) {
	c := newCtl(&fakeAPI{})
	if err := c.On(context.Background(), "nope"); !errors.Is(err, ErrUnknownNode) {
		t.Fatalf("err = %v want ErrUnknownNode", err)
	}
	boom := errors.New("boom")
	if err := newCtl(&fakeAPI{getErr: boom}).Off(context.Background(), "lab-node-1"); !errors.Is(err, boom) {
		t.Fatalf("err = %v want wrapped boom", err)
	}
	if err := newCtl(&fakeAPI{cmdErr: boom}).Cycle(context.Background(), "lab-node-1"); !errors.Is(err, boom) {
		t.Fatalf("err = %v want wrapped boom", err)
	}
}

func TestNormalizeMAC(t *testing.T) {
	if got := NormalizeMAC(" 0C-EA-14-64-FB-8F "); got != "0c:ea:14:64:fb:8f" {
		t.Fatal(got)
	}
}

func TestMinOffTimeDelaysPowerOn(t *testing.T) {
	f := &fakeAPI{}
	now := time.Unix(1000, 0)
	var slept []time.Duration
	c := New(f, "default", map[string]Target{"n": {SwitchMAC: "aa:bb:cc:dd:ee:ff", Port: 1}}, WithMinOffTime(5*time.Second))
	c.now = func() time.Time { return now }
	c.sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); now = now.Add(d); return nil }

	ctx := context.Background()
	if err := c.Off(ctx, "n"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second) // On arrives 2s after Off
	if err := c.On(ctx, "n"); err != nil {
		t.Fatal(err)
	}
	if len(slept) != 1 || slept[0] != 3*time.Second {
		t.Fatalf("slept %v, want [3s]", slept)
	}
	if m, _ := overrideMode(&f.dev, 1); m != "auto" {
		t.Fatalf("mode = %q want auto", m)
	}

	// Off long ago: no wait.
	slept = nil
	if err := c.Off(ctx, "n"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err := c.On(ctx, "n"); err != nil {
		t.Fatal(err)
	}
	if len(slept) != 0 {
		t.Fatalf("unexpected sleep %v", slept)
	}
}

func TestMinOffTimeHonoursContextCancel(t *testing.T) {
	f := &fakeAPI{}
	c := New(f, "default", map[string]Target{"n": {SwitchMAC: "aa:bb:cc:dd:ee:ff", Port: 1}}, WithMinOffTime(time.Hour))
	if err := c.Off(context.Background(), "n"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.On(ctx, "n"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v want context.Canceled", err)
	}
	if m, _ := overrideMode(&f.dev, 1); m != "off" {
		t.Fatal("port must stay off when the wait is cancelled")
	}
}

type multiAPI struct {
	devs  map[string]*unifi.Device
	calls map[string]int
}

func (m *multiAPI) GetDeviceByMAC(_ context.Context, _, mac string) (*unifi.Device, error) {
	m.calls[mac]++
	d, ok := m.devs[mac]
	if !ok {
		return nil, errors.New("unreachable")
	}
	return d, nil
}
func (m *multiAPI) UpdateDevice(context.Context, string, *unifi.Device) (*unifi.Device, error) {
	return nil, errors.New("not used")
}
func (m *multiAPI) ExecuteCmd(context.Context, string, string, unifi.Cmd) (any, error) {
	return nil, errors.New("not used")
}

func TestSnapshot(t *testing.T) {
	api := &multiAPI{calls: map[string]int{}, devs: map[string]*unifi.Device{
		"aa": {PortOverrides: []unifi.DevicePortOverrides{{PortIDX: i64(2), PoeMode: "off"}}},
		// "bb" is deliberately missing: that switch is unreachable.
	}}
	c := New(api, "default", map[string]Target{
		"n1": {SwitchMAC: "aa", Port: 1},
		"n2": {SwitchMAC: "aa", Port: 2},
		"n3": {SwitchMAC: "bb", Port: 1},
	})
	got := c.Snapshot(context.Background())
	want := []NodeState{{"n1", "aa", StateOn}, {"n2", "aa", StateOff}, {"n3", "bb", StateUnknown}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %+v want %+v", i, got[i], want[i])
		}
	}
	if api.calls["aa"] != 1 || api.calls["bb"] != 1 {
		t.Errorf("each switch must be queried once, calls = %v", api.calls)
	}
}
