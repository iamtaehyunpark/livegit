package fuse

import (
	"reflect"
	"sort"
	"testing"
)

// The store itself: set/get/list/remove round-trip.
func TestXattrStoreBasics(t *testing.T) {
	var s xattrStore

	if _, ok := s.get("a.txt", "com.apple.FinderInfo"); ok {
		t.Fatal("get on empty store should miss")
	}
	s.set("a.txt", "com.apple.FinderInfo", []byte{1, 2, 3})
	s.set("a.txt", "com.apple.quarantine", []byte("q"))

	data, ok := s.get("a.txt", "com.apple.FinderInfo")
	if !ok || !reflect.DeepEqual(data, []byte{1, 2, 3}) {
		t.Fatalf("get = %v, %v", data, ok)
	}

	names := s.list("a.txt")
	sort.Strings(names)
	want := []string{"com.apple.FinderInfo", "com.apple.quarantine"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("list = %v, want %v", names, want)
	}

	if !s.remove("a.txt", "com.apple.quarantine") {
		t.Fatal("remove of existing attr should report true")
	}
	if s.remove("a.txt", "com.apple.quarantine") {
		t.Fatal("second remove should report false")
	}
}

// rename moves the attrs of a path and of everything under it; forget drops a
// whole subtree. Mirrors what RecordRename / RecordDelete do for dirs.
func TestXattrStoreRenameAndForget(t *testing.T) {
	var s xattrStore
	s.set("dir/a.txt", "attr", []byte("a"))
	s.set("dir/sub/b.txt", "attr", []byte("b"))
	s.set("dirx/c.txt", "attr", []byte("c")) // shares the prefix string but is a sibling

	s.rename("dir", "moved")

	if _, ok := s.get("dir/a.txt", "attr"); ok {
		t.Fatal("old path should have no attrs after rename")
	}
	if d, ok := s.get("moved/a.txt", "attr"); !ok || string(d) != "a" {
		t.Fatalf("moved/a.txt attr = %q, %v", d, ok)
	}
	if d, ok := s.get("moved/sub/b.txt", "attr"); !ok || string(d) != "b" {
		t.Fatalf("moved/sub/b.txt attr = %q, %v", d, ok)
	}
	if d, ok := s.get("dirx/c.txt", "attr"); !ok || string(d) != "c" {
		t.Fatalf("sibling dirx must be untouched, got %q, %v", d, ok)
	}

	s.forget("moved")
	if _, ok := s.get("moved/sub/b.txt", "attr"); ok {
		t.Fatal("forget should drop the whole subtree")
	}
	if _, ok := s.get("dirx/c.txt", "attr"); !ok {
		t.Fatal("forget must not touch the sibling")
	}
}
