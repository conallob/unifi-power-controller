package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestBuildInfo(t *testing.T) {
	reg := NewRegistry(Info{Version: "1.2.3", Commit: "abc1234", Date: "2026-11-13T00:00:00Z"})

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, mf := range mfs {
		if mf.GetName() != "unifi_power_controller_build_info" {
			continue
		}
		found = true
		m := mf.GetMetric()[0]
		if m.GetGauge().GetValue() != 1 {
			t.Fatalf("value = %v want 1", m.GetGauge().GetValue())
		}
		got := map[string]string{}
		for _, l := range m.GetLabel() {
			got[l.GetName()] = l.GetValue()
		}
		for k, want := range map[string]string{"version": "1.2.3", "commit": "abc1234", "date": "2026-11-13T00:00:00Z"} {
			if got[k] != want {
				t.Errorf("label %s = %q want %q", k, got[k], want)
			}
		}
		if !strings.HasPrefix(got["goversion"], "go") {
			t.Errorf("goversion = %q", got["goversion"])
		}
	}
	if !found {
		t.Fatal("build_info not registered")
	}
	if n := testutil.CollectAndCount(reg, "unifi_power_controller_build_info"); n != 1 {
		t.Fatalf("series = %d want 1", n)
	}
}

func TestHandlerExposition(t *testing.T) {
	rr := httptest.NewRecorder()
	Handler(NewRegistry(Info{Version: "9.9.9", Commit: "c", Date: "d"})).ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	body := rr.Body.String()
	if rr.Code != 200 || !strings.Contains(body, `unifi_power_controller_build_info{commit="c",date="d",goversion="go`) || !strings.Contains(body, `version="9.9.9"} 1`) {
		t.Fatalf("unexpected exposition (code %d):\n%.600s", rr.Code, body)
	}
}
