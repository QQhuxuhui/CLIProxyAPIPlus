package util

// Reference copy of the pre-optimization RenameKey. It is kept verbatim so the
// optimized implementation can be proven to produce the same output. It is
// test-only and must not be used by production code.

import (
	"fmt"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func legacyRenameKey(jsonStr, oldKeyPath, newKeyPath string) (string, error) {
	value := gjson.Get(jsonStr, oldKeyPath)

	if !value.Exists() {
		return "", fmt.Errorf("old key '%s' does not exist", oldKeyPath)
	}

	interimJSON, errSet := sjson.SetRawBytes([]byte(jsonStr), newKeyPath, []byte(value.Raw))
	if errSet != nil {
		return "", fmt.Errorf("failed to set new key '%s': %w", newKeyPath, errSet)
	}

	finalJSON, errDelete := sjson.DeleteBytes(interimJSON, oldKeyPath)
	if errDelete != nil {
		return "", fmt.Errorf("failed to delete old key '%s': %w", oldKeyPath, errDelete)
	}

	return string(finalJSON), nil
}
