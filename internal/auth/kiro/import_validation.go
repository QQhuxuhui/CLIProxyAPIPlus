package kiro

import (
	"fmt"
	"strings"
)

// KiroJSONImportItem is a single account entry from JSON batch import payload.
type KiroJSONImportItem struct {
	RefreshToken string `json:"refreshToken"`
	Provider     string `json:"provider"`
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
	Region       string `json:"region"`
}

// KiroJSONImportValidation is the normalized validation result.
type KiroJSONImportValidation struct {
	Item        KiroJSONImportItem
	AccountType string
	Provider    string
}

func canonicalKiroImportProvider(provider string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "google":
		return "Google", true
	case "github":
		return "GitHub", true
	case "builderid":
		return "BuilderId", true
	case "enterprise":
		return "Enterprise", true
	default:
		return "", false
	}
}

// ValidateKiroJSONImportItem validates and normalizes one JSON import item.
func ValidateKiroJSONImportItem(item KiroJSONImportItem, index int) (*KiroJSONImportValidation, error) {
	normalized := KiroJSONImportItem{
		RefreshToken: strings.TrimSpace(item.RefreshToken),
		Provider:     strings.TrimSpace(item.Provider),
		ClientID:     strings.TrimSpace(item.ClientID),
		ClientSecret: strings.TrimSpace(item.ClientSecret),
		Region:       strings.TrimSpace(item.Region),
	}

	if normalized.RefreshToken == "" {
		return nil, fmt.Errorf("第%d条: 缺少 refreshToken", index+1)
	}
	if !strings.HasPrefix(normalized.RefreshToken, "aor") {
		return nil, fmt.Errorf("第%d条: refreshToken 格式无效（应以 aor 开头）", index+1)
	}

	hasClientID := normalized.ClientID != ""
	hasClientSecret := normalized.ClientSecret != ""
	if hasClientID != hasClientSecret {
		if hasClientID {
			return nil, fmt.Errorf("第%d条: 缺少 clientSecret（IdC 账号需要 clientId 和 clientSecret）", index+1)
		}
		return nil, fmt.Errorf("第%d条: 缺少 clientId（IdC 账号需要 clientId 和 clientSecret）", index+1)
	}

	hasClientCredentials := hasClientID && hasClientSecret
	provider := normalized.Provider
	if provider == "" {
		if hasClientCredentials {
			provider = "BuilderId"
		} else {
			provider = "Google"
		}
	}

	canonicalProvider, ok := canonicalKiroImportProvider(provider)
	if !ok {
		return nil, fmt.Errorf("第%d条: 无效的 provider %q（支持: Google, GitHub, BuilderId, Enterprise）", index+1, provider)
	}

	if hasClientCredentials && (canonicalProvider == "Google" || canonicalProvider == "GitHub") {
		return nil, fmt.Errorf("第%d条: Social provider %q 不应包含 clientId/clientSecret", index+1, canonicalProvider)
	}
	if !hasClientCredentials && (canonicalProvider == "BuilderId" || canonicalProvider == "Enterprise") {
		return nil, fmt.Errorf("第%d条: IdC provider %q 需要 clientId 和 clientSecret", index+1, canonicalProvider)
	}

	accountType := "social"
	if hasClientCredentials {
		accountType = "idc"
	}
	normalized.Provider = canonicalProvider

	return &KiroJSONImportValidation{
		Item:        normalized,
		AccountType: accountType,
		Provider:    canonicalProvider,
	}, nil
}
