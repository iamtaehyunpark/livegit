package config

import "testing"

func mapper() *PathMapper {
	c := &Config{LocalRoot: "/Users/me/proj"}
	c.Source.RemoteRoot = "/home/u/proj"
	return NewPathMapper(c)
}

func TestRel(t *testing.T) {
	for in, want := range map[string]string{
		"":           "",
		"/":          "",
		".":          "",
		"foo":        "foo",
		"/foo/":      "foo",
		"foo/bar":    "foo/bar",
		"./foo/bar/": "foo/bar",
		"a//b":       "a/b",
	} {
		if got := Rel(in); got != want {
			t.Errorf("Rel(%q)=%q want %q", in, got, want)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	m := mapper()
	rel := "pkg/file.go"
	if got := m.RelToLocal(rel); got != "/Users/me/proj/pkg/file.go" {
		t.Fatalf("RelToLocal=%q", got)
	}
	if got := m.RelToRemote(rel); got != "/home/u/proj/pkg/file.go" {
		t.Fatalf("RelToRemote=%q", got)
	}
	back, err := m.LocalToRel("/Users/me/proj/pkg/file.go")
	if err != nil || back != rel {
		t.Fatalf("LocalToRel=%q err=%v", back, err)
	}
	back, err = m.RemoteToRel("/home/u/proj/pkg/file.go")
	if err != nil || back != rel {
		t.Fatalf("RemoteToRel=%q err=%v", back, err)
	}
}

func TestEscapeRejected(t *testing.T) {
	m := mapper()
	if _, err := m.LocalToRel("/etc/passwd"); err == nil {
		t.Error("expected escape error for local")
	}
	if _, err := m.RemoteToRel("/home/u/other/x"); err == nil {
		t.Error("expected escape error for remote")
	}
}

func TestRootRel(t *testing.T) {
	m := mapper()
	if got := m.RelToLocal(""); got != "/Users/me/proj" {
		t.Errorf("root local=%q", got)
	}
	if got := m.RelToRemote(""); got != "/home/u/proj" {
		t.Errorf("root remote=%q", got)
	}
}
