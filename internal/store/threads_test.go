package store

import (
	"reflect"
	"testing"
)

func TestThreadsGroupAndOrderWithoutMutatingInput(t *testing.T) {
	a := rec(1, "newest", "personal", 1, "rules")
	b := rec(3, "oldest", "otp", 1, "rules")
	a.ThreadID, b.ThreadID = 9, 9
	b.Address = "different address in same thread"
	c := rec(2, "other thread, same sender", "personal", 1, "rules")
	c.ThreadID = 10
	d := a
	d.ID = 4 // same timestamp, tie broken by ID
	input := []Record{a, c, d, b}
	before := append([]Record(nil), input...)
	got := Threads(input)
	if len(got) != 2 || got[0].Key != "thread:9" || got[1].Key != "thread:10" {
		t.Fatalf("unexpected threads: %+v", got)
	}
	var ids []int64
	for _, r := range got[0].Records {
		ids = append(ids, r.ID)
	}
	if !reflect.DeepEqual(ids, []int64{3, 1, 4}) {
		t.Fatalf("chronological IDs = %v", ids)
	}
	if !reflect.DeepEqual(input, before) {
		t.Fatal("input was mutated")
	}
}

func TestThreadsMissingIDs(t *testing.T) {
	a := rec(1, "a", "personal", 1, "rules")
	b := rec(2, "b", "personal", 1, "rules")
	b.ThreadID = -1
	c := rec(3, "anonymous", "personal", 1, "rules")
	c.Address, c.Person = "", ""
	d := c
	d.ID = 4
	e := a
	e.ID, e.ThreadID = 5, 9
	got := Threads([]Record{a, b, c, d, e})
	if len(got) != 4 {
		t.Fatalf("got %d threads, want 4", len(got))
	}
	for _, thread := range got {
		if thread.Key == ThreadKey(a) && len(thread.Records) != 2 {
			t.Fatal("same fallback address should group")
		}
	}
	if ThreadKey(a) == ThreadKey(e) {
		t.Fatal("fallback address merged with a known thread")
	}
	if ThreadKey(c) == ThreadKey(d) {
		t.Fatal("anonymous records merged")
	}
	if Threads([]Record{c})[0].Label() != "Unknown sender" {
		t.Fatal("missing fallback label")
	}
	if len(Threads(nil)) != 0 {
		t.Fatal("empty input should have no threads")
	}
}
