package main

// Validates that raw zlib streams captured by parsePushPack are exact: a pack
// rebuilt from them must parse back into the identical object set. A single
// extra or missing byte would break the next object's header and fail here.

import (
	"fmt"
	"strings"
	"testing"
)

func TestPushPackRawPassThroughRoundTrip(t *testing.T) {
	original := map[string]*GitObject{}
	for i := 0; i < 6; i++ {
		data := []byte(fmt.Sprintf("object-%d-%s", i, strings.Repeat("y", i*137)))
		original[hashObjectPush("blob", data)] = &GitObject{Kind: "blob", Data: data}
	}

	// A client-style pack (freshly compressed) is what parsePushPack sees.
	pack1 := writePack(original)
	parsed, err := parsePushPack(pack1)
	if err != nil {
		t.Fatalf("parse original pack: %v", err)
	}
	if len(parsed) != len(original) {
		t.Fatalf("parsed %d objects, want %d", len(parsed), len(original))
	}
	for oid, obj := range parsed {
		if len(obj.RawZlib) == 0 {
			t.Fatalf("object %s has no captured zlib stream", oid)
		}
	}

	// Rebuild with verbatim zlib and parse again. Misaligned boundaries would
	// corrupt the following object header.
	pack2 := writePack(parsed)
	parsed2, err := parsePushPack(pack2)
	if err != nil {
		t.Fatalf("parse pass-through pack: %v", err)
	}
	if len(parsed2) != len(original) {
		t.Fatalf("pass-through parsed %d objects, want %d", len(parsed2), len(original))
	}
	for oid := range original {
		obj, ok := parsed2[oid]
		if !ok {
			t.Fatalf("object %s missing after pass-through", oid)
		}
		if string(obj.Data) != string(original[oid].Data) {
			t.Fatalf("object %s data changed", oid)
		}
		if len(obj.RawZlib) == 0 {
			t.Fatalf("object %s lost zlib on second pass", oid)
		}
	}
}
