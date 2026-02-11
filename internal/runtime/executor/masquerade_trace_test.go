package executor

import "testing"

func TestMasqueradeTraceStore_GetReturnsDeepCopy(t *testing.T) {
	store := NewMasqueradeTraceStore(10)
	store.SetEnabled(true)

	record := &MasqueradeTraceRecord{
		Model: "claude-sonnet",
		OriginalHeaders: map[string]string{
			"X-Test": "original",
		},
		MaskedHeaders: map[string]string{
			"X-Test": "masked",
		},
	}
	id := store.Add(record)
	if id == "" {
		t.Fatalf("expected non-empty id")
	}

	got1 := store.Get(id)
	if got1 == nil {
		t.Fatalf("expected record")
	}
	got1.OriginalHeaders["X-Test"] = "mutated"
	got1.MaskedHeaders["X-Test"] = "mutated"

	got2 := store.Get(id)
	if got2 == nil {
		t.Fatalf("expected record after mutation")
	}
	if got2.OriginalHeaders["X-Test"] != "original" {
		t.Fatalf("OriginalHeaders leaked mutation: got %q, want %q", got2.OriginalHeaders["X-Test"], "original")
	}
	if got2.MaskedHeaders["X-Test"] != "masked" {
		t.Fatalf("MaskedHeaders leaked mutation: got %q, want %q", got2.MaskedHeaders["X-Test"], "masked")
	}

	// Mutating the input record after Add should not affect stored data.
	record.OriginalHeaders["X-Test"] = "after-add-mutation"
	record.MaskedHeaders["X-Test"] = "after-add-mutation"

	got3 := store.Get(id)
	if got3 == nil {
		t.Fatalf("expected record after input mutation")
	}
	if got3.OriginalHeaders["X-Test"] != "original" {
		t.Fatalf("stored record leaked input mutation: got %q, want %q", got3.OriginalHeaders["X-Test"], "original")
	}
	if got3.MaskedHeaders["X-Test"] != "masked" {
		t.Fatalf("stored record leaked input mutation: got %q, want %q", got3.MaskedHeaders["X-Test"], "masked")
	}
}

func TestMasqueradeTraceStore_ListFullReturnsDeepCopies(t *testing.T) {
	store := NewMasqueradeTraceStore(10)
	store.SetEnabled(true)

	record := &MasqueradeTraceRecord{
		Model: "claude-sonnet",
		OriginalHeaders: map[string]string{
			"X-Test": "original",
		},
		MaskedHeaders: map[string]string{
			"X-Test": "masked",
		},
	}
	id := store.Add(record)
	if id == "" {
		t.Fatalf("expected non-empty id")
	}

	list := store.ListFull()
	if len(list) != 1 {
		t.Fatalf("expected 1 record, got %d", len(list))
	}
	list[0].OriginalHeaders["X-Test"] = "mutated"
	list[0].MaskedHeaders["X-Test"] = "mutated"

	got := store.Get(id)
	if got == nil {
		t.Fatalf("expected record after list mutation")
	}
	if got.OriginalHeaders["X-Test"] != "original" {
		t.Fatalf("OriginalHeaders leaked list mutation: got %q, want %q", got.OriginalHeaders["X-Test"], "original")
	}
	if got.MaskedHeaders["X-Test"] != "masked" {
		t.Fatalf("MaskedHeaders leaked list mutation: got %q, want %q", got.MaskedHeaders["X-Test"], "masked")
	}
}

