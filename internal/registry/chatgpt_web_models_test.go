package registry

import "testing"

func TestGetChatGPTWebModels(t *testing.T) {
	models := GetChatGPTWebModels()
	if len(models) != 2 {
		t.Fatalf("model count = %d, want 2", len(models))
	}
	if models[0].ID != ChatGPTWebTextModelID || models[0].Type != "chatgpt-web" {
		t.Fatalf("text model = %#v", models[0])
	}
	if models[1].ID != ChatGPTWebImageModelID || models[1].Type != OpenAIImageModelType {
		t.Fatalf("image model = %#v", models[1])
	}
	if models[0].ID == models[1].ID {
		t.Fatal("text and image model IDs must be distinct")
	}
}
