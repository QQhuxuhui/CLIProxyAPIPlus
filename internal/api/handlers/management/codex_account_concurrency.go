package management

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const authFileConcurrencyField = "concurrency_limit"

// GetCodexAccountConcurrencyConfig returns the switch, the default limit and the effective state.
func (h *Handler) GetCodexAccountConcurrencyConfig(c *gin.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration is not available"})
		return
	}
	value := h.cfg.Codex.AccountConcurrency
	home := h.cfg.Home.Enabled || h.authManager.HomeEnabled()
	c.JSON(http.StatusOK, gin.H{
		"enabled":       value.Enabled,
		"default-limit": value.EffectiveDefaultLimit(),
		"home_mode":     home,
		"active":        value.Enabled && !home,
	})
}

// PutCodexAccountConcurrencyConfig persists the global switch and default limit.
func (h *Handler) PutCodexAccountConcurrencyConfig(c *gin.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration is not available"})
		return
	}
	next := h.cfg.CloneForRuntime()
	value := next.Codex.AccountConcurrency
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	body := &value
	if err := decoder.Decode(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if body == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "expected a configuration object"})
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"error": "expected one JSON configuration object"})
		return
	}
	if err := value.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if value.Enabled && (next.Home.Enabled || h.authManager.HomeEnabled()) {
		c.JSON(http.StatusConflict, gin.H{"error": "Home dispatch owns scheduling; account concurrency cannot be enabled"})
		return
	}
	limit := value.EffectiveDefaultLimit()
	value.DefaultLimit = &limit
	next.Codex.AccountConcurrency = value
	h.persistConfigLocked(c, next)
}

func applyAuthFileConcurrency(entry gin.H, auth *coreauth.Auth, view coreauth.CodexAccountConcurrencyView) {
	state, ok := view.Entry(auth)
	if !ok {
		return
	}
	entry["in_flight"] = state.InFlight
	entry["concurrency_limit"] = state.Limit
	entry["concurrency_override"] = state.Override
	entry["concurrency_source"] = state.Source
}

func isAuthFileConcurrencyPatch(fields map[string]json.RawMessage) bool {
	for key := range fields {
		if rootAuthFileField(key) == authFileConcurrencyField {
			return true
		}
	}
	return false
}

// parseAuthFileConcurrencyLimit returns nil for null, which restores inheritance.
func parseAuthFileConcurrencyLimit(raw json.RawMessage) (*int, error) {
	value, errDecode := decodeAuthFileFieldValue(raw)
	if errDecode != nil {
		return nil, fmt.Errorf("%s must be a non-negative integer or null", authFileConcurrencyField)
	}
	if value == nil {
		return nil, nil
	}
	number, ok := value.(json.Number)
	if !ok {
		return nil, fmt.Errorf("%s must be a non-negative integer or null", authFileConcurrencyField)
	}
	parsed, errInt := number.Int64()
	limit := int(parsed)
	if errInt != nil || parsed < 0 || int64(limit) != parsed {
		return nil, fmt.Errorf("%s must be a non-negative integer or null", authFileConcurrencyField)
	}
	return &limit, nil
}

// patchAuthFileConcurrencyLimit writes only the limits file: the field is rejected next to any
// other field, so an account update can never be partly applied alongside it.
func (h *Handler) patchAuthFileConcurrencyLimit(c *gin.Context, name string, fields map[string]json.RawMessage) {
	raw, ok := fields[authFileConcurrencyField]
	if !ok || len(fields) != 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": authFileConcurrencyField + " must be submitted on its own"})
		return
	}
	limit, errLimit := parseAuthFileConcurrencyLimit(raw)
	if errLimit != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errLimit.Error()})
		return
	}
	var target *coreauth.Auth
	if auth, found := h.authManager.GetByID(name); found {
		target = auth
	} else {
		for _, auth := range h.authManager.List() {
			if auth.FileName == name {
				target = auth
				break
			}
		}
	}
	if target == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}
	if !coreauth.CodexAccountConcurrencyApplicable(target) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "concurrency limits apply only to codex oauth accounts"})
		return
	}
	if _, err := h.authManager.SetCodexAccountConcurrencyLimits([]string{target.ID}, limit); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to save concurrency limit: %v", err)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// PatchAuthFileFieldsBatch sets concurrency_limit for many accounts with one file write. Unknown
// or inapplicable accounts are reported in failed; the write itself is all or nothing.
func (h *Handler) PatchAuthFileFieldsBatch(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	var body map[string]json.RawMessage
	decoder := json.NewDecoder(c.Request.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	rawLimit, hasLimit := body[authFileConcurrencyField]
	rawNames, hasNames := body["names"]
	if !hasLimit || !hasNames || len(body) != 2 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "names and " + authFileConcurrencyField + " are required, and no other field is allowed"})
		return
	}
	var names []string
	if err := json.Unmarshal(rawNames, &names); err != nil || len(names) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "names must be a non-empty list"})
		return
	}
	limit, errLimit := parseAuthFileConcurrencyLimit(rawLimit)
	if errLimit != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errLimit.Error()})
		return
	}

	byName := make(map[string]*coreauth.Auth)
	accounts := h.authManager.List()
	for _, auth := range accounts {
		if auth != nil && auth.FileName != "" {
			byName[auth.FileName] = auth
		}
	}
	for _, auth := range accounts {
		if auth != nil {
			byName[auth.ID] = auth
		}
	}
	failed := make([]gin.H, 0)
	ids := make([]string, 0, len(names))
	nameByID := make(map[string]string, len(names))
	for _, rawName := range names {
		name := strings.TrimSpace(rawName)
		auth := byName[name]
		switch {
		case auth == nil:
			failed = append(failed, gin.H{"name": name, "error": "auth file not found"})
		case !coreauth.CodexAccountConcurrencyApplicable(auth):
			failed = append(failed, gin.H{"name": name, "error": "concurrency limits apply only to codex oauth accounts"})
		default:
			if _, duplicate := nameByID[auth.ID]; !duplicate {
				nameByID[auth.ID] = name
				ids = append(ids, auth.ID)
			}
		}
	}
	result, err := h.authManager.SetCodexAccountConcurrencyLimits(ids, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to save concurrency limits: %v", err)})
		return
	}
	namesFor := func(ids []string) []string {
		out := make([]string, 0, len(ids))
		for _, id := range ids {
			out = append(out, nameByID[id])
		}
		return out
	}
	status := "ok"
	if len(failed) > 0 {
		status = "partial"
	}
	c.JSON(http.StatusOK, gin.H{"status": status, "succeeded": namesFor(result.Changed), "unchanged": namesFor(result.Unchanged), "failed": failed})
}
