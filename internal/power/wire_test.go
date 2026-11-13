package power

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ubiquiti-community/go-unifi/unifi"
)

// TestWireFormat drives a real go-unifi client against a fake UniFi OS
// controller and asserts the requests that actually go out: the API-key
// header, the /proxy/network path prefix, and the PUT / cmd payloads.
func TestWireFormat(t *testing.T) {
	const mac = "0c:ea:14:64:fb:8f"
	type req struct{ method, path, key, body string }
	var (
		mu   sync.Mutex
		reqs []req
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, req{r.Method, r.URL.Path, r.Header.Get("X-Api-Key"), string(b)})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/stat/device/") {
			_, _ = w.Write([]byte(`{"meta":{"rc":"ok"},"data":[{"_id":"dev1","mac":"` + mac + `",
				"port_overrides":[{"port_idx":7,"poe_mode":"auto","name":"keep-me"}],
				"port_table":[{"port_idx":1,"poe_mode":"auto"}]}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"meta":{"rc":"ok"},"data":[]}`))
	}))
	defer srv.Close()

	client, err := unifi.New(context.Background(), &unifi.Config{BaseURL: srv.URL, APIKey: "sekret"})
	if err != nil {
		t.Fatal(err)
	}
	c := New(client, "default", map[string]Target{"lab-node-1": {SwitchMAC: mac, Port: 1}})

	mu.Lock()
	reqs = nil
	mu.Unlock()

	if err := c.Off(context.Background(), "lab-node-1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Cycle(context.Background(), "lab-node-1"); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	var put, cmd *req
	for i := range reqs {
		r := &reqs[i]
		if r.key != "sekret" {
			t.Errorf("%s %s: X-Api-Key = %q", r.method, r.path, r.key)
		}
		if !strings.HasPrefix(r.path, "/proxy/network/api/s/default/") {
			t.Errorf("unexpected path %s", r.path)
		}
		switch {
		case r.method == "PUT":
			put = r
		case r.method == "POST":
			cmd = r
		}
	}
	if put == nil || cmd == nil {
		t.Fatalf("missing PUT/POST in %+v", reqs)
	}

	if put.path != "/proxy/network/api/s/default/rest/device/dev1" {
		t.Errorf("PUT path = %s", put.path)
	}
	var patch struct {
		PortOverrides []map[string]any `json:"port_overrides"`
	}
	if err := json.Unmarshal([]byte(put.body), &patch); err != nil {
		t.Fatal(err)
	}
	got := map[float64]map[string]any{}
	for _, o := range patch.PortOverrides {
		got[o["port_idx"].(float64)] = o
	}
	if got[1]["poe_mode"] != "off" {
		t.Errorf("port 1 override = %v", got[1])
	}
	if got[7]["name"] != "keep-me" || got[7]["poe_mode"] != "auto" {
		t.Errorf("port 7 override not preserved: %v", got[7])
	}

	if cmd.path != "/proxy/network/api/s/default/cmd/devmgr" {
		t.Errorf("cmd path = %s", cmd.path)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(cmd.body), &body)
	if body["cmd"] != "power-cycle" || body["mac"] != mac || body["port_idx"] != float64(1) {
		t.Errorf("cmd body = %s", cmd.body)
	}
}
