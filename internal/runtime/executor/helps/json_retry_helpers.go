package helps

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// DeleteJSONField removes a top-level or nested JSON field from a payload.
func DeleteJSONField(body []byte, key string) []byte {
	if key == "" || len(body) == 0 {
		return body
	}
	updated, err := sjson.DeleteBytes(body, key)
	if err != nil {
		return body
	}
	return updated
}

// ParseRetryDelay extracts the retry delay from a Google API 429 error response.
func ParseRetryDelay(errorBody []byte) (*time.Duration, error) {
	details := gjson.GetBytes(errorBody, "error.details")
	if details.Exists() && details.IsArray() {
		for _, detail := range details.Array() {
			if detail.Get("@type").String() != "type.googleapis.com/google.rpc.RetryInfo" {
				continue
			}
			retryDelay := detail.Get("retryDelay").String()
			if retryDelay == "" {
				continue
			}
			duration, err := time.ParseDuration(retryDelay)
			if err != nil {
				return nil, fmt.Errorf("failed to parse duration")
			}
			return &duration, nil
		}

		for _, detail := range details.Array() {
			if detail.Get("@type").String() != "type.googleapis.com/google.rpc.ErrorInfo" {
				continue
			}
			quotaResetDelay := detail.Get("metadata.quotaResetDelay").String()
			if quotaResetDelay == "" {
				continue
			}
			duration, err := time.ParseDuration(quotaResetDelay)
			if err == nil {
				return &duration, nil
			}
		}
	}

	message := gjson.GetBytes(errorBody, "error.message").String()
	if message != "" {
		re := regexp.MustCompile(`after\s+(\d+)s\.?`)
		if matches := re.FindStringSubmatch(message); len(matches) > 1 {
			seconds, err := strconv.Atoi(matches[1])
			if err == nil {
				duration := time.Duration(seconds) * time.Second
				return &duration, nil
			}
		}
		reHuman := regexp.MustCompile(`after\s+((?:\d+h)?(?:\d+m)?(?:\d+s)?)\.?`)
		if matches := reHuman.FindStringSubmatch(strings.ToLower(message)); len(matches) > 1 {
			duration, err := time.ParseDuration(matches[1])
			if err == nil && duration > 0 {
				return &duration, nil
			}
		}
	}

	return nil, fmt.Errorf("no RetryInfo found")
}

// ParseAntigravityQuota extracts structured per-model quota information from a
// Google API 429 (cloudcode-pa) error body: metadata.model, the absolute
// quotaResetTimeStamp, a relative reset delay (retryDelay preferred, else
// quotaResetDelay), and the ErrorInfo reason code. Returns ok=false when no
// recognizable ErrorInfo/RetryInfo detail is present.
func ParseAntigravityQuota(errorBody []byte) (cliproxyexecutor.QuotaDetail, bool) {
	var detail cliproxyexecutor.QuotaDetail
	found := false
	details := gjson.GetBytes(errorBody, "error.details")
	if !details.Exists() || !details.IsArray() {
		return detail, false
	}
	for _, d := range details.Array() {
		switch d.Get("@type").String() {
		case "type.googleapis.com/google.rpc.ErrorInfo":
			if reason := strings.TrimSpace(d.Get("reason").String()); reason != "" {
				detail.ReasonCode = reason
				found = true
			}
			if model := strings.TrimSpace(d.Get("metadata.model").String()); model != "" {
				detail.Model = model
				found = true
			}
			if ts := strings.TrimSpace(d.Get("metadata.quotaResetTimeStamp").String()); ts != "" {
				if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
					detail.ResetAt = parsed
					found = true
				}
			}
			if detail.ResetDelay == nil {
				if qrd := strings.TrimSpace(d.Get("metadata.quotaResetDelay").String()); qrd != "" {
					if dur, err := time.ParseDuration(qrd); err == nil {
						detail.ResetDelay = &dur
						found = true
					}
				}
			}
		case "type.googleapis.com/google.rpc.RetryInfo":
			// retryDelay takes priority for the relative hint regardless of order.
			if rd := strings.TrimSpace(d.Get("retryDelay").String()); rd != "" {
				if dur, err := time.ParseDuration(rd); err == nil {
					detail.ResetDelay = &dur
					found = true
				}
			}
		}
	}
	return detail, found
}
