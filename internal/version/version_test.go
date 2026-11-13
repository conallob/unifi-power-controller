package version

import (
	"runtime/debug"
	"strings"
	"testing"
)

func TestResolveKeepsLinkTimeValues(t *testing.T) {
	bi := &debug.BuildInfo{Main: debug.Module{Version: "v9.9.9"}, Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc"}}}
	v, c, d := resolve("1.2.3", "deadbeef", "2026-01-01T00:00:00Z", bi)
	if v != "1.2.3" || c != "deadbeef" || d != "2026-01-01T00:00:00Z" {
		t.Fatalf("link-time values overridden: %s %s %s", v, c, d)
	}
}

func TestResolveFallsBackToBuildInfo(t *testing.T) {
	bi := &debug.BuildInfo{
		Main: debug.Module{Version: "v1.4.0"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "abc123"},
			{Key: "vcs.time", Value: "2026-02-02T00:00:00Z"},
		},
	}
	v, c, d := resolve("dev", "unknown", "unknown", bi)
	if v != "v1.4.0" || c != "abc123" || d != "2026-02-02T00:00:00Z" {
		t.Fatalf("got %s %s %s", v, c, d)
	}
}

func TestResolveDirtyAndDevel(t *testing.T) {
	bi := &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: []debug.BuildSetting{{Key: "vcs.modified", Value: "true"}}}
	if v, _, _ := resolve("dev", "unknown", "unknown", bi); v != "dev-dirty" {
		t.Fatalf("version = %s", v)
	}
	if v, c, d := resolve("dev", "unknown", "unknown", nil); v != "dev" || c != "unknown" || d != "unknown" {
		t.Fatal("nil build info must leave defaults")
	}
}

func TestString(t *testing.T) {
	if s := String(); !strings.HasPrefix(s, "unifi-power-controller ") {
		t.Fatal(s)
	}
}
