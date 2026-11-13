package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad(t *testing.T) {
	t.Setenv("UNIFI_API_KEY", "")
	d := t.TempDir()
	key := write(t, d, "key", "abc\n")
	sec := write(t, d, "sec", "s3\n")
	cfg := write(t, d, "c.yaml", `
unifi: {url: "https://192.168.1.1", apiKeyFile: "`+key+`", insecureSkipVerify: true}
rpc: {hmacSHA256Files: ["`+sec+`"]}
switches: {shack: {mac: "0C-EA-14-64-FB-8F"}}
nodes:
  lab-node-1: {switch: shack, port: 1}
`)
	c, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c.UniFi.APIKey != "abc" || c.UniFi.Site != "default" || c.Listen != ":8080" {
		t.Fatalf("defaults/secrets wrong: %+v", c)
	}
	if *c.MinOffTime != 5*time.Second {
		t.Fatalf("minOffTime default = %v", *c.MinOffTime)
	}
	if len(c.RPC.SHA256Secrets) != 1 || c.RPC.SHA256Secrets[0] != "s3" {
		t.Fatalf("rpc secrets = %v", c.RPC.SHA256Secrets)
	}
	tg := c.PowerTargets()["lab-node-1"]
	if tg.SwitchMAC != "0c:ea:14:64:fb:8f" || tg.Port != 1 {
		t.Fatalf("target = %+v", tg)
	}
}

func TestEnvKeyAndExplicitZeroMinOff(t *testing.T) {
	t.Setenv("UNIFI_API_KEY", "fromenv")
	d := t.TempDir()
	cfg := write(t, d, "c.yaml", `
minOffTime: 0s
unifi: {url: "https://x"}
switches: {s: {mac: "aa:bb:cc:dd:ee:ff"}}
nodes: {n: {switch: s, port: 2}}
`)
	c, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c.UniFi.APIKey != "fromenv" || *c.MinOffTime != 0 {
		t.Fatalf("got key=%q minOff=%v", c.UniFi.APIKey, *c.MinOffTime)
	}
}

func TestLoadErrors(t *testing.T) {
	t.Setenv("UNIFI_API_KEY", "k")
	d := t.TempDir()
	base := `unifi: {url: "https://x"}
switches: {s: {mac: "aa:bb:cc:dd:ee:ff"}}
`
	for name, tc := range map[string]struct{ yaml, want string }{
		"unknown switch": {base + "nodes: {n: {switch: nope, port: 1}}", "unknown switch"},
		"bad port":       {base + "nodes: {n: {switch: s, port: 0}}", "port must be"},
		"no nodes":       {base, "at least one node"},
		"unknown field":  {base + "nodez: {}", "nodez"},
		"no url":         {"nodes: {n: {switch: s, port: 1}}", "unifi.url"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(write(t, d, "e.yaml", tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
	t.Setenv("UNIFI_API_KEY", "")
	if _, err := Load(write(t, d, "nk.yaml", base+"nodes: {n: {switch: s, port: 1}}")); err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("missing key err = %v", err)
	}
}
