package shell

import (
	"testing"

	"github.com/taehyun/lg/internal/config"
)

func testCfg() *config.Config {
	c := &config.Config{}
	c.SourceTriggers.Patterns = []string{"^conda activate", "^source .*/bin/activate", "^poetry shell"}
	c.SourceTriggers.ExitCommandMap = map[string]string{
		"^conda activate":         "conda deactivate",
		"^source .*/bin/activate": "deactivate",
		"^poetry shell":           "exit",
	}
	c.SourceTriggers.DirectoryMarkers = []string{".venv", "node_modules"}
	c.SourceTriggers.AlwaysSourcePatterns = []string{"^python "}
	c.ReadonlyCommands = []string{"cat", "ls", "grep"}
	return c
}

func TestTriggerPatterns(t *testing.T) {
	e := NewTriggerEngine(testCfg())
	cases := []struct {
		cmd  string
		want bool
		via  EnteredVia
	}{
		{"conda activate ml", true, "conda"},
		{"source .venv/bin/activate", true, "venv"},
		{"poetry shell", true, "poetry"},
		{"python train.py", true, "always"},
		{"ls -la", false, ""},
		{"echo hi", false, ""},
	}
	for _, c := range cases {
		d := e.Evaluate(c.cmd, "", false, nil)
		if d.Enter != c.want {
			t.Errorf("%q enter=%v want %v", c.cmd, d.Enter, c.want)
		}
		if c.want && d.Via != c.via {
			t.Errorf("%q via=%q want %q", c.cmd, d.Via, c.via)
		}
	}
}

func TestTriggerDirMarker(t *testing.T) {
	e := NewTriggerEngine(testCfg())
	// presence reports marker NOT on ghost => should trigger.
	absent := func(relDir, marker string) bool { return false }
	d := e.Evaluate("pytest", "proj", false, absent)
	if !d.Enter || d.Via != "dir:.venv" {
		t.Errorf("expected dir trigger, got enter=%v via=%q", d.Enter, d.Via)
	}
	// present on ghost => no trigger.
	present := func(relDir, marker string) bool { return true }
	if d := e.Evaluate("pytest", "proj", false, present); d.Enter {
		t.Errorf("unexpected trigger when marker present")
	}
	// readonly command in a marker dir must NOT trigger (stays local, §5.6).
	if d := e.Evaluate("ls -la", "proj", true, absent); d.Enter {
		t.Errorf("readonly command should not trigger dir-marker entry, got via=%q", d.Via)
	}
}

func TestIsExit(t *testing.T) {
	e := NewTriggerEngine(testCfg())
	if !e.IsExit("conda", "conda deactivate") {
		t.Error("conda deactivate should exit")
	}
	if !e.IsExit("venv", "deactivate") {
		t.Error("deactivate should exit venv")
	}
	if e.IsExit("conda", "ls") {
		t.Error("ls should not exit")
	}
}

func TestRouter(t *testing.T) {
	cfg := testCfg()
	r := NewRouter(cfg, NewTriggerEngine(cfg))
	if r.Classify("cat foo") != ClassReadonly {
		t.Error("cat should be readonly")
	}
	if r.Classify("python train.py") != ClassAlwaysSource {
		t.Error("python should be always-source")
	}
	if r.Classify("echo hi") != ClassGeneral {
		t.Error("echo should be general")
	}
}
