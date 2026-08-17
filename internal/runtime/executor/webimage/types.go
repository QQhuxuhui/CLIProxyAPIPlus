package webimage

const SourceFormat = "chatgpt-web-image"

// BrowserVector is the browser-state array encoded by the Sentinel SDK.
type BrowserVector []any

// PoWChallenge describes the proof-of-work portion of a Sentinel challenge.
type PoWChallenge struct {
	Required   bool   `json:"required"`
	Seed       string `json:"seed"`
	Difficulty string `json:"difficulty"`
}

// PoWOptions bounds proof generation and makes elapsed timing deterministic in tests.
type PoWOptions struct {
	MaxAttempts   int
	ElapsedMillis func() int64
}

// Credentials is a single-request snapshot of the selected Codex credential.
type Credentials struct {
	AccessToken string
	AccountID   string
	ProxyURL    string
	AuthID      string
}

// ImageResult contains one downloaded image ready for the OpenAI Images bridge.
type ImageResult struct {
	Base64Data    string
	RevisedPrompt string
	OutputFormat  string
}

// Meta describes the completed web generation without exposing the internal model.
type Meta struct {
	CreatedAt int64
}

type generationState struct {
	prompt                string
	turnTraceID           string
	parentMessageID       string
	conduitToken          string
	clientPrepareState    string
	chatRequirementsToken string
	legacyRequirements    bool
	prepareToken          string
	proofToken            string
	turnstileToken        string
	conversationID        string
	assetRefs             []string
	done                  bool
	failed                bool
}
