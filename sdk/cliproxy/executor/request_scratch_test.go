package executor

import (
	"fmt"
	"testing"
)

func TestRequestScratchIsBoundedAndNilSafe(t *testing.T) {
	var missing *RequestScratch
	if _, ok := missing.Load("k"); ok || missing.Store("k", 1) {
		t.Fatal("nil scratch must behave as an always-miss cache")
	}
	missing.DeletePrefix("k")

	meta := WithRequestScratch(nil)
	scratch := RequestScratchFrom(meta)
	if scratch == nil || RequestScratchFrom(WithRequestScratch(meta)) != scratch {
		t.Fatal("WithRequestScratch must attach once and keep the same scratch")
	}
	for i := 0; i < maxRequestScratchEntries; i++ {
		if !scratch.Store(fmt.Sprintf("a|%d", i), i) {
			t.Fatalf("entry %d rejected below the cap", i)
		}
	}
	if scratch.Store("b|overflow", 1) {
		t.Fatal("scratch accepted an entry above the cap")
	}
	if !scratch.Store("a|0", 42) {
		t.Fatal("overwriting an existing entry must be allowed at the cap")
	}
	scratch.DeletePrefix("a|")
	if _, ok := scratch.Load("a|1"); ok {
		t.Fatal("DeletePrefix left an entry behind")
	}
	if !scratch.Store("b|1", 1) {
		t.Fatal("scratch did not free capacity after DeletePrefix")
	}
	if !IsHostPrivateMetadataKey(RequestScratchMetadataKey) || IsHostPrivateMetadataKey("session_id") {
		t.Fatal("unexpected host-private key classification")
	}
}
