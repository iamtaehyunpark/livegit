package transport

import (
	"strings"
	"testing"

	"github.com/iamtaehyunpark/livegit/internal/config"
)

// ghostCfg builds a minimal ghost config for the control-master helpers.
func ghostCfg(mode, auth, persist string, port int) *config.Config {
	c := &config.Config{}
	c.Source.Host = "gpu-1"
	c.Source.User = "u"
	c.Source.Port = port
	c.Source.SSHMode = mode
	c.Source.Auth = auth
	c.Source.ControlPersist = persist
	return c
}

func TestUsesControlMaster(t *testing.T) {
	cases := []struct {
		mode, auth string
		want       bool
	}{
		{"system", "", true},
		{"", "", true}, // empty mode defaults to system elsewhere; helper treats it as system
		{"native", "", false},
		{"system", "password", false},
	}
	for _, tc := range cases {
		if got := usesControlMaster(ghostCfg(tc.mode, tc.auth, "8h", 22)); got != tc.want {
			t.Errorf("usesControlMaster(mode=%q auth=%q)=%v want %v", tc.mode, tc.auth, got, tc.want)
		}
	}
}

func TestMasterOptsHasPathPersistKeepalive(t *testing.T) {
	got := strings.Join(masterOpts(ghostCfg("system", "", "30m", 22)), " ")
	for _, want := range []string{
		"ControlPath=" + controlPathTemplate,
		"ControlPersist=30m",
		"ServerAliveInterval=60",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("masterOpts missing %q; got: %s", want, got)
		}
	}
}

func TestMasterOptsDefaultsPersist(t *testing.T) {
	// An empty ControlPersist must not emit "ControlPersist=" (ssh would reject
	// it); it falls back to the 8h default.
	got := strings.Join(masterOpts(ghostCfg("system", "", "", 22)), " ")
	if !strings.Contains(got, "ControlPersist=8h") {
		t.Errorf("empty persist should default to 8h; got: %s", got)
	}
}

func TestPortArgs(t *testing.T) {
	if got := portArgs(ghostCfg("system", "", "8h", 22)); got != nil {
		t.Errorf("port 22 should yield no args, got %v", got)
	}
	if got := portArgs(ghostCfg("system", "", "8h", 0)); got != nil {
		t.Errorf("port 0 (default) should yield no args, got %v", got)
	}
	got := portArgs(ghostCfg("system", "", "8h", 2222))
	if strings.Join(got, " ") != "-p 2222" {
		t.Errorf("port 2222 => %v, want [-p 2222]", got)
	}
}

func TestPersistLabel(t *testing.T) {
	if got := PersistLabel(ghostCfg("system", "", "", 22)); got != "8h" {
		t.Errorf("empty persist label = %q, want 8h", got)
	}
	if got := PersistLabel(ghostCfg("system", "", "2h", 22)); got != "2h" {
		t.Errorf("persist label = %q, want 2h", got)
	}
}

// MasterLive / StopMaster / EnsureMaster must be no-ops (never shell out) for
// native/password mode, so a password project never pays for ssh -O check.
func TestControlMasterNoopForNative(t *testing.T) {
	if MasterLive(ghostCfg("native", "", "8h", 22)) {
		t.Error("MasterLive should be false for native mode")
	}
	if err := StopMaster(ghostCfg("native", "", "8h", 22)); err != nil {
		t.Errorf("StopMaster native should be nil, got %v", err)
	}
	if err := EnsureMaster(ghostCfg("system", "password", "8h", 22)); err != nil {
		t.Errorf("EnsureMaster password should be nil, got %v", err)
	}
}
