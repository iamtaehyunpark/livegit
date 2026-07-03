package transport

import (
	"os"
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
		{"native", "password", false},
		// password + system = the Duo/2FA setup: `lg connect` authenticates the
		// master interactively (password + Duo), later connections multiplex.
		{"system", "password", true},
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
	if got := PersistLabel(ghostCfg("system", "", "", 22)); got != "for 8h" {
		t.Errorf("empty persist label = %q, want 'for 8h'", got)
	}
	if got := PersistLabel(ghostCfg("system", "", "2h", 22)); got != "for 2h" {
		t.Errorf("persist label = %q, want 'for 2h'", got)
	}
	if got := PersistLabel(ghostCfg("system", "", "max", 22)); got != "until the connection drops (no expiry)" {
		t.Errorf("max persist label = %q", got)
	}
}

// "max" (and the spellings users guess) must reach ssh as ControlPersist=yes;
// durations and the empty default pass through.
func TestPersistValue(t *testing.T) {
	for in, want := range map[string]string{
		"": "8h", "30m": "30m", "8h": "8h",
		"max": "yes", "MAX": "yes", "forever": "yes", "yes": "yes", "0": "yes",
	} {
		if got := persistValue(ghostCfg("system", "", in, 22)); got != want {
			t.Errorf("persistValue(%q)=%q want %q", in, got, want)
		}
	}
}

// MasterLive / StopMaster / EnsureMaster must be no-ops (never shell out) for
// native mode, so a native project never pays for ssh -O check.
func TestControlMasterNoopForNative(t *testing.T) {
	if MasterLive(ghostCfg("native", "", "8h", 22)) {
		t.Error("MasterLive should be false for native mode")
	}
	if err := StopMaster(ghostCfg("native", "", "8h", 22)); err != nil {
		t.Errorf("StopMaster native should be nil, got %v", err)
	}
	if err := EnsureMaster(ghostCfg("native", "password", "8h", 22)); err != nil {
		t.Errorf("EnsureMaster native/password should be nil, got %v", err)
	}
}

// A second-auth (Duo/OTP) challenge must never be answered with the stored
// password; only password-looking questions are. The one-time-password rows
// are the traps: they contain "password" but are second-auth challenges.
func TestPasswordLikeQuestion(t *testing.T) {
	for q, want := range map[string]bool{
		"Password: ":                      true,
		"user@host's password:":           true,
		"":                                true, // prompt text sometimes rides the banner
		"Duo two-factor login for tpark":  false,
		"Passcode or option (1-2): ":      false,
		"Verification code: ":             false,
		"Enter a passcode or select one of the following options:": false,
		"One-time password (OATH) for `tpark':":                    false,
		"Enter your one-time password:":                            false,
		"OTP Code:":                                                false,
	} {
		if got := PasswordLikeQuestion(q); got != want {
			t.Errorf("PasswordLikeQuestion(%q)=%v want %v", q, got, want)
		}
	}
}

// askpassEnv must write an executable shim that pins the project (LG_HOME) and
// re-invokes this binary, and point SSH_ASKPASS at it with force mode — no
// secrets in the script itself.
func TestAskpassEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LG_HOME", dir)
	env, err := askpassEnv()
	if err != nil {
		t.Fatal(err)
	}
	var script string
	wantForce := false
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "SSH_ASKPASS="); ok {
			script = v
		}
		if e == "SSH_ASKPASS_REQUIRE=force" {
			wantForce = true
		}
	}
	if script == "" || !wantForce {
		t.Fatalf("env missing SSH_ASKPASS / SSH_ASKPASS_REQUIRE=force: %v", env)
	}
	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"LG_HOME='" + dir + "'", "askpass \"$@\""} {
		if !strings.Contains(string(body), want) {
			t.Errorf("shim missing %q:\n%s", want, body)
		}
	}
	info, err := os.Stat(script)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("shim is not executable: %v", info.Mode())
	}
}
