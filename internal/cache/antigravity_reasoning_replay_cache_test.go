package cache

import (
	"context"
	"testing"
)

func TestAntigravityReasoningReplayStaleSnapshotCannotOverwriteNewerState(t *testing.T) {
	ClearAntigravityReasoningReplayCache()
	t.Cleanup(ClearAntigravityReasoningReplayCache)
	model := "gemini-test"
	session := "session-test"
	older := []byte(`{"type":"thought_signature","thoughtSignature":"older-signature-1234","contentIndex":1,"partIndex":0}`)
	newer := []byte(`{"type":"thought_signature","thoughtSignature":"newer-signature-1234","contentIndex":1,"partIndex":0}`)

	_, olderSnapshot, _, errOlder := GetAntigravityReasoningReplayItemsWithSnapshotRequired(context.Background(), model, session)
	if errOlder != nil {
		t.Fatal(errOlder)
	}
	_, newerSnapshot, _, errNewer := GetAntigravityReasoningReplayItemsWithSnapshotRequired(context.Background(), model, session)
	if errNewer != nil {
		t.Fatal(errNewer)
	}
	if replaced, errReplace := ReplaceAntigravityReasoningReplayItemsIfUnchanged(context.Background(), model, session, newerSnapshot, [][]byte{newer}); errReplace != nil || !replaced {
		t.Fatalf("newer replace = %v, %v", replaced, errReplace)
	}
	if replaced, errReplace := ReplaceAntigravityReasoningReplayItemsIfUnchanged(context.Background(), model, session, olderSnapshot, [][]byte{older}); errReplace != nil || replaced {
		t.Fatalf("stale replace = %v, %v, want false, nil", replaced, errReplace)
	}
	items, found, errGet := GetAntigravityReasoningReplayItemsRequired(context.Background(), model, session)
	if errGet != nil || !found || len(items) != 1 || string(items[0]) != string(newer) {
		t.Fatalf("final replay = %s, found=%v error=%v", items, found, errGet)
	}
}

func TestAntigravityReasoningReplayStaleSnapshotCannotDeleteNewerState(t *testing.T) {
	ClearAntigravityReasoningReplayCache()
	t.Cleanup(ClearAntigravityReasoningReplayCache)
	model := "gemini-test"
	session := "session-test"
	initial := []byte(`{"type":"thought_signature","thoughtSignature":"initial-signature-1234","contentIndex":1,"partIndex":0}`)
	newer := []byte(`{"type":"thought_signature","thoughtSignature":"newer-signature-1234","contentIndex":1,"partIndex":0}`)
	if !CacheAntigravityReasoningReplayItems(model, session, [][]byte{initial}) {
		t.Fatal("seed failed")
	}
	_, staleSnapshot, _, errSnapshot := GetAntigravityReasoningReplayItemsWithSnapshotRequired(context.Background(), model, session)
	if errSnapshot != nil {
		t.Fatal(errSnapshot)
	}
	if !CacheAntigravityReasoningReplayItems(model, session, [][]byte{newer}) {
		t.Fatal("newer write failed")
	}
	if deleted, errDelete := DeleteAntigravityReasoningReplayItemsIfUnchanged(context.Background(), model, session, staleSnapshot); errDelete != nil || deleted {
		t.Fatalf("stale delete = %v, %v, want false, nil", deleted, errDelete)
	}
	items, found, errGet := GetAntigravityReasoningReplayItemsRequired(context.Background(), model, session)
	if errGet != nil || !found || len(items) != 1 || string(items[0]) != string(newer) {
		t.Fatalf("final replay = %s, found=%v error=%v", items, found, errGet)
	}
}

func TestAntigravityReasoningReplayAbsentSnapshotCannotResurrectAfterWriteAndDelete(t *testing.T) {
	ClearAntigravityReasoningReplayCache()
	t.Cleanup(ClearAntigravityReasoningReplayCache)
	model := "gemini-test"
	session := "session-test"
	stale := []byte(`{"type":"thought_signature","thoughtSignature":"stale-signature-1234","contentIndex":1,"partIndex":0}`)
	newer := []byte(`{"type":"thought_signature","thoughtSignature":"newer-signature-1234","contentIndex":1,"partIndex":0}`)

	_, absentSnapshot, _, errSnapshot := GetAntigravityReasoningReplayItemsWithSnapshotRequired(context.Background(), model, session)
	if errSnapshot != nil {
		t.Fatal(errSnapshot)
	}
	if !CacheAntigravityReasoningReplayItems(model, session, [][]byte{newer}) {
		t.Fatal("newer write failed")
	}
	DeleteAntigravityReasoningReplayItem(model, session)
	if replaced, errReplace := ReplaceAntigravityReasoningReplayItemsIfUnchanged(context.Background(), model, session, absentSnapshot, [][]byte{stale}); errReplace != nil || replaced {
		t.Fatalf("stale resurrection = %v, %v, want false, nil", replaced, errReplace)
	}
}
