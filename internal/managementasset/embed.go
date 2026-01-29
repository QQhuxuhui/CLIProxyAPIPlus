package managementasset

import (
	"embed"
)

//go:embed static/management.html
var embeddedAssets embed.FS

// GetEmbeddedManagementHTML returns the embedded management.html content.
// Returns nil if the embedded file is not available.
func GetEmbeddedManagementHTML() []byte {
	data, err := embeddedAssets.ReadFile("static/management.html")
	if err != nil {
		return nil
	}
	return data
}

// HasEmbeddedManagementHTML checks if the embedded management.html is available.
func HasEmbeddedManagementHTML() bool {
	_, err := embeddedAssets.ReadFile("static/management.html")
	return err == nil
}
