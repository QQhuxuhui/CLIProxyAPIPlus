package registry

const (
	ChatGPTWebTextModelID  = "chatgpt-web"
	ChatGPTWebImageModelID = "chatgpt-web-image"
)

// GetChatGPTWebModels returns isolated model IDs that do not collide with Codex models.
func GetChatGPTWebModels() []*ModelInfo {
	return []*ModelInfo{
		{
			ID:                         ChatGPTWebTextModelID,
			Object:                     "model",
			Created:                    1704067200,
			OwnedBy:                    "chatgpt-web",
			Type:                       "chatgpt-web",
			DisplayName:                "ChatGPT Web",
			Description:                "Authorized ChatGPT Web session through the private conversation protocol",
			SupportedGenerationMethods: []string{"chat"},
			SupportedParameters:        []string{"messages", "stream"},
			SupportedInputModalities:   []string{"TEXT"},
			SupportedOutputModalities:  []string{"TEXT"},
		},
		{
			ID:                         ChatGPTWebImageModelID,
			Object:                     "model",
			Created:                    1704067200,
			OwnedBy:                    "chatgpt-web",
			Type:                       OpenAIImageModelType,
			DisplayName:                "ChatGPT Web Image",
			Description:                "ChatGPT Web picture_v2 image generation",
			SupportedGenerationMethods: []string{"image_generation"},
			SupportedParameters:        []string{"prompt", "n", "size", "quality", "response_format"},
			SupportedInputModalities:   []string{"TEXT"},
			SupportedOutputModalities:  []string{"IMAGE"},
		},
	}
}
