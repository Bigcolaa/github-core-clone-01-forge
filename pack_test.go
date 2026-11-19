package main

import "testing"

func TestPackRoundTrip(t *testing.T) {
	objs := map[string]*GitObject{}
	for i := 0; i < 5; i++ {
		data := []byte("hello world " + string(rune('a'+i)) + "\n")
		oid := hashObject("blob", data)
		objs[oid] = &GitObject{Kind: "blob", Data: data}
	}
	pack := writePack(objs)
	got, err := parsePushPack(pack)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(objs) {
		t.Fatalf("got %d objects, want %d", len(got), len(objs))
	}
	for oid := range objs {
		if _, ok := got[oid]; !ok {
			t.Fatalf("missing oid %s", oid)
		}
	}
}
