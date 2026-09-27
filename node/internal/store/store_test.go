package store

import (
	"errors"
	"path/filepath"
	"testing"
)

type doc struct {
	N int    `json:"n"`
	S string `json:"s"`
}

func TestStore(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "sub", "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var d doc
	if ok, err := db.Get("b", "k", &d); ok || err != nil {
		t.Fatalf("empty get: %v %v", ok, err)
	}
	if err := db.Put("b", "k", doc{N: 1, S: "a"}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := db.Get("b", "k", &d); !ok || d.N != 1 {
		t.Fatalf("get %+v", d)
	}
	if err := Modify(db, "b", "k", func(v *doc, exists bool) error {
		if !exists {
			return errors.New("should exist")
		}
		v.N++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = Modify(db, "b", "k", func(v *doc, _ bool) error { v.N = 99; return ErrStop })
	_ = db.Put("b", "j", doc{N: 7})
	all, err := List[doc](db, "b")
	if err != nil || len(all) != 2 || all[0].N != 7 || all[1].N != 2 {
		t.Fatalf("list %+v %v", all, err)
	}
	if !db.Has("b", "j") || db.Has("b", "x") || db.Has("nob", "j") {
		t.Fatal("has")
	}
	_ = db.Delete("b", "j")
	if db.Has("b", "j") {
		t.Fatal("delete")
	}
}
