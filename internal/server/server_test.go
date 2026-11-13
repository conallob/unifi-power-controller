package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/conallob/unifi-power-controller/internal/power"
)

type fakePower struct {
	calls []string
	state power.State
	err   error
}

func (f *fakePower) rec(op, n string) error {
	f.calls = append(f.calls, op+":"+n)
	if n == "ghost" {
		return power.ErrUnknownNode
	}
	return f.err
}
func (f *fakePower) On(_ context.Context, n string) error    { return f.rec("on", n) }
func (f *fakePower) Off(_ context.Context, n string) error   { return f.rec("off", n) }
func (f *fakePower) Cycle(_ context.Context, n string) error { return f.rec("cycle", n) }
func (f *fakePower) State(_ context.Context, n string) (power.State, error) {
	return f.state, f.rec("state", n)
}

func do(h http.Handler, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestRESTPower(t *testing.T) {
	f := &fakePower{state: power.StateOn}
	h := New(Options{Power: f})

	for _, a := range []string{"on", "off", "cycle"} {
		if rr := do(h, "POST", "/nodes/lab-node-1/power", `{"action":"`+a+`"}`, nil); rr.Code != 204 {
			t.Fatalf("%s: code %d", a, rr.Code)
		}
	}
	if got := strings.Join(f.calls, ","); got != "on:lab-node-1,off:lab-node-1,cycle:lab-node-1" {
		t.Fatalf("calls = %s", got)
	}
	if rr := do(h, "POST", "/nodes/lab-node-1/power", `{"action":"explode"}`, nil); rr.Code != 400 {
		t.Fatalf("bad action code %d", rr.Code)
	}
	if rr := do(h, "POST", "/nodes/lab-node-1/power", `not json`, nil); rr.Code != 400 {
		t.Fatalf("bad json code %d", rr.Code)
	}
	if rr := do(h, "POST", "/nodes/ghost/power", `{"action":"on"}`, nil); rr.Code != 404 {
		t.Fatalf("unknown node code %d", rr.Code)
	}
	f.err = errors.New("controller down")
	if rr := do(h, "POST", "/nodes/lab-node-1/power", `{"action":"on"}`, nil); rr.Code != 502 {
		t.Fatalf("backend failure code %d", rr.Code)
	}
	f.err = nil
	rr := do(h, "GET", "/nodes/lab-node-1/power", "", nil)
	if rr.Code != 200 || strings.TrimSpace(rr.Body.String()) != `{"state":"on"}` {
		t.Fatalf("get: %d %q", rr.Code, rr.Body.String())
	}
	if rr := do(h, "GET", "/healthz", "", nil); rr.Code != 200 {
		t.Fatalf("healthz %d", rr.Code)
	}
}

func rpcCall(t *testing.T, h http.Handler, body string, hdr map[string]string) (*httptest.ResponseRecorder, rpcResponse) {
	t.Helper()
	rr := do(h, "POST", "/rpc", body, hdr)
	var resp rpcResponse
	if rr.Code == 200 {
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode %q: %v", rr.Body.String(), err)
		}
		if rr.Header().Get("Content-Length") == "" {
			t.Fatal("bmclib requires Content-Length on rpc responses")
		}
	}
	return rr, resp
}

func TestRPC(t *testing.T) {
	f := &fakePower{state: power.StateOff}
	h := New(Options{Power: f})

	cases := []struct {
		state, want string
	}{{"on", "on:n"}, {"off", "off:n"}, {"cycle", "cycle:n"}, {"reset", "cycle:n"}, {"ON", "on:n"}}
	for _, c := range cases {
		f.calls = nil
		_, resp := rpcCall(t, h, `{"id":7,"host":"n","method":"setPowerState","params":{"state":"`+c.state+`"}}`, nil)
		if resp.Error != nil || resp.ID != 7 || resp.Host != "n" {
			t.Fatalf("%s: %+v", c.state, resp)
		}
		if f.calls[0] != c.want {
			t.Fatalf("%s: calls %v want %s", c.state, f.calls, c.want)
		}
	}

	_, resp := rpcCall(t, h, `{"id":1,"host":"n","method":"getPowerState"}`, nil)
	if resp.Result != "off" {
		t.Fatalf("getPowerState = %v", resp.Result)
	}
	_, resp = rpcCall(t, h, `{"id":1,"host":"n","method":"ping"}`, nil)
	if resp.Result != "pong" {
		t.Fatalf("ping = %v", resp.Result)
	}
	_, resp = rpcCall(t, h, `{"id":1,"host":"n","method":"setBootDevice","params":{"device":"pxe"}}`, nil)
	if resp.Error != nil {
		t.Fatalf("setBootDevice: %+v", resp.Error)
	}
	_, resp = rpcCall(t, h, `{"id":1,"host":"n","method":"setPowerState","params":{"state":"soft"}}`, nil)
	if resp.Error == nil || resp.Error.Code != codeUnsupported {
		t.Fatalf("soft should be unsupported: %+v", resp)
	}
	_, resp = rpcCall(t, h, `{"id":1,"host":"ghost","method":"getPowerState"}`, nil)
	if resp.Error == nil || resp.Error.Code != codeNotFound {
		t.Fatalf("unknown host: %+v", resp)
	}
	_, resp = rpcCall(t, h, `{"id":1,"host":"n","method":"setPowerState"}`, nil)
	if resp.Error == nil || resp.Error.Code != codeInvalid {
		t.Fatalf("missing params: %+v", resp)
	}
	if rr, _ := rpcCall(t, h, `garbage`, nil); rr.Code != 400 {
		t.Fatalf("garbage code %d", rr.Code)
	}
}

func sign(body, extra, secret string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body + extra))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func TestRPCSignature(t *testing.T) {
	body := `{"id":1,"host":"n","method":"ping"}`
	h := New(Options{Power: &fakePower{}, SHA256Secrets: []string{"old", "new"}})

	if rr := do(h, "POST", "/rpc", body, nil); rr.Code != 401 {
		t.Fatalf("unsigned code %d", rr.Code)
	}
	if rr := do(h, "POST", "/rpc", body, map[string]string{"X-BMCLIB-Signature-256": sign(body, "", "wrong")}); rr.Code != 401 {
		t.Fatalf("wrong secret code %d", rr.Code)
	}
	for _, s := range []string{"old", "new"} {
		if rr := do(h, "POST", "/rpc", body, map[string]string{"X-BMCLIB-Signature-256": sign(body, "", s)}); rr.Code != 200 {
			t.Fatalf("secret %s code %d", s, rr.Code)
		}
	}
	// Rufio sends one signature per configured secret, comma separated.
	multi := sign(body, "", "rotated-out") + "," + sign(body, "", "new")
	if rr := do(h, "POST", "/rpc", body, map[string]string{"X-BMCLIB-Signature-256": multi}); rr.Code != 200 {
		t.Fatalf("multi-sig code %d", rr.Code)
	}
	// Tampered body must fail.
	if rr := do(h, "POST", "/rpc", strings.Replace(body, "ping", "getPowerState", 1), map[string]string{"X-BMCLIB-Signature-256": sign(body, "", "new")}); rr.Code != 401 {
		t.Fatalf("tampered code %d", rr.Code)
	}

	// Signed timestamp header (replay mitigation).
	hh := New(Options{Power: &fakePower{}, SHA256Secrets: []string{"s"}, SignedHeaders: []string{"X-BMCLIB-Timestamp"}})
	ts := "2026-11-11T12:00:00Z"
	ok := map[string]string{"X-BMCLIB-Timestamp": ts, "X-BMCLIB-Signature-256": sign(body, ts, "s")}
	if rr := do(hh, "POST", "/rpc", body, ok); rr.Code != 200 {
		t.Fatalf("signed header code %d", rr.Code)
	}
	bad := map[string]string{"X-BMCLIB-Timestamp": "2026-11-11T12:00:01Z", "X-BMCLIB-Signature-256": sign(body, ts, "s")}
	if rr := do(hh, "POST", "/rpc", body, bad); rr.Code != 401 {
		t.Fatalf("altered timestamp code %d", rr.Code)
	}
}

// TestRPCSignatureRufio covers the header names and payload Rufio (not
// vanilla bmclib) actually sends: X-Rufio-Signature-{256,512}, with
// X-Rufio-Timestamp's raw value appended to the body before signing.
// tinkerbell/tinkerbell's rufio/internal/controller/client.go hardcodes
// these via mergo.WithOverride, so a Machine's providerOptions.rpc can't
// opt back into bmclib's own X-BMCLIB-* defaults -- real Rufio traffic
// always looks like this.
func TestRPCSignatureRufio(t *testing.T) {
	body := `{"id":1,"host":"n","method":"ping"}`
	h := New(Options{Power: &fakePower{}, SHA256Secrets: []string{"s"}})
	ts := "2026-11-11T12:00:00Z"

	ok := map[string]string{"X-Rufio-Timestamp": ts, "X-Rufio-Signature-256": sign(body, ts, "s")}
	if rr := do(h, "POST", "/rpc", body, ok); rr.Code != 200 {
		t.Fatalf("rufio signed code %d", rr.Code)
	}

	// Altering the timestamp without re-signing must fail: it's part of the
	// signed payload, not just an informational header.
	altered := map[string]string{"X-Rufio-Timestamp": "2026-11-11T12:00:01Z", "X-Rufio-Signature-256": sign(body, ts, "s")}
	if rr := do(h, "POST", "/rpc", body, altered); rr.Code != 401 {
		t.Fatalf("rufio altered timestamp code %d", rr.Code)
	}

	// Wrong secret still fails.
	wrong := map[string]string{"X-Rufio-Timestamp": ts, "X-Rufio-Signature-256": sign(body, ts, "wrong")}
	if rr := do(h, "POST", "/rpc", body, wrong); rr.Code != 401 {
		t.Fatalf("rufio wrong secret code %d", rr.Code)
	}

	// Rotated secrets and multiple comma-separated signatures work the same
	// as the X-BMCLIB-* scheme.
	hh := New(Options{Power: &fakePower{}, SHA256Secrets: []string{"old", "new"}})
	multi := sign(body, ts, "rotated-out") + "," + sign(body, ts, "new")
	if rr := do(hh, "POST", "/rpc", body, map[string]string{"X-Rufio-Timestamp": ts, "X-Rufio-Signature-256": multi}); rr.Code != 200 {
		t.Fatalf("rufio multi-sig code %d", rr.Code)
	}
}

func TestMetricsRoute(t *testing.T) {
	// Not mounted unless configured.
	if rr := do(New(Options{Power: &fakePower{}}), "GET", "/metrics", "", nil); rr.Code != 404 {
		t.Fatalf("unconfigured /metrics code %d, want 404", rr.Code)
	}
	h := New(Options{Power: &fakePower{}, Metrics: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})})
	if rr := do(h, "GET", "/metrics", "", nil); rr.Code != 200 || rr.Body.String() != "ok" {
		t.Fatalf("/metrics: %d %q", rr.Code, rr.Body.String())
	}
	if rr := do(h, "POST", "/metrics", "", nil); rr.Code == 200 {
		t.Fatal("POST /metrics must not be served")
	}
}

type recEvent struct{ kind, a, b, c string }

type fakeRec struct{ events []recEvent }

func (f *fakeRec) RPC(method, result string) {
	f.events = append(f.events, recEvent{"rpc", method, result, ""})
}
func (f *fakeRec) Action(node, action, result string) {
	f.events = append(f.events, recEvent{"action", node, action, result})
}

func TestRecorder(t *testing.T) {
	r := &fakeRec{}
	f := &fakePower{state: power.StateOn}
	h := New(Options{Power: f, Recorder: r, SHA256Secrets: []string{"s"}})

	sig := func(body string) map[string]string {
		return map[string]string{"X-BMCLIB-Signature-256": sign(body, "", "s")}
	}
	call := func(body string) { do(h, "POST", "/rpc", body, sig(body)) }

	// REST path counts actions, including failures and unknown nodes; rejects aren't actions.
	do(h, "POST", "/nodes/n1/power", `{"action":"cycle"}`, nil)
	do(h, "POST", "/nodes/ghost/power", `{"action":"on"}`, nil)
	do(h, "POST", "/nodes/n1/power", `{"action":"explode"}`, nil)
	f.err = errors.New("controller down")
	do(h, "POST", "/nodes/n1/power", `{"action":"off"}`, nil)
	f.err = nil

	// RPC path: setPowerState maps onto the same actions; reset counts as cycle.
	call(`{"id":1,"host":"n1","method":"setPowerState","params":{"state":"reset"}}`)
	call(`{"id":1,"host":"n1","method":"getPowerState"}`)
	call(`{"id":1,"host":"n1","method":"setPowerState","params":{"state":"soft"}}`)
	call(`{"id":1,"host":"n1","method":"bogus"}`)
	do(h, "POST", "/rpc", `{"id":1,"host":"n1","method":"ping"}`, nil) // unsigned
	do(h, "POST", "/rpc", `garbage`, sig(`garbage`))

	want := []recEvent{
		{"action", "n1", "cycle", "success"},
		{"action", "ghost", "on", "unknown_node"},
		{"action", "n1", "off", "error"},
		{"action", "n1", "cycle", "success"}, {"rpc", "setPowerState", "ok", ""},
		{"rpc", "getPowerState", "ok", ""},
		{"rpc", "setPowerState", "error", ""}, // soft: unsupported, no action attempted
		{"rpc", "bogus", "error", ""},
		{"rpc", "unknown", "unauthorized", ""},
		{"rpc", "unknown", "bad_request", ""},
	}
	if len(r.events) != len(want) {
		t.Fatalf("events:\n got %v\nwant %v", r.events, want)
	}
	for i := range want {
		if r.events[i] != want[i] {
			t.Errorf("event %d = %+v want %+v", i, r.events[i], want[i])
		}
	}
}
