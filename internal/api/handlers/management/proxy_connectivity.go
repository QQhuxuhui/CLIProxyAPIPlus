package management

import (
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// proxyTestTargetURL is the default Google reachability probe. generate_204
// returns an empty 204 response, making it a cheap connectivity check.
const proxyTestTargetURL = "https://www.google.com/generate_204"

// TestProxy checks whether a proxy can reach upstream (Google by default).
//
// Body: {"proxy-url": "...", "target": "..."} (both optional). When "proxy-url"
// is omitted the current config proxy is used; an empty proxy tests the direct
// (no-proxy) connection. "target" overrides the probe URL.
//
// The short client timeout here is an intentional connectivity-probe timeout in
// the management API, consistent with the existing management APICall timeout.
func (h *Handler) TestProxy(c *gin.Context) {
	var body struct {
		ProxyURL *string `json:"proxy-url"`
		Target   string  `json:"target"`
	}
	_ = c.ShouldBindJSON(&body)

	proxyURL := ""
	if body.ProxyURL != nil {
		proxyURL = strings.TrimSpace(*body.ProxyURL)
	} else if h.cfg != nil {
		proxyURL = strings.TrimSpace(h.cfg.ProxyURL)
	}

	target := strings.TrimSpace(body.Target)
	if target == "" {
		target = proxyTestTargetURL
	}

	var transport http.RoundTripper
	if proxyURL != "" {
		built := buildProxyTransport(proxyURL)
		if built == nil {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "无法解析代理地址（支持 http/https/socks5）"})
			return
		}
		transport = built
	} else {
		transport = directTransport()
	}

	req, errReq := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, target, nil)
	if errReq != nil {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "无效的测试目标 URL"})
		return
	}

	httpClient := &http.Client{Timeout: defaultAPICallTimeout, Transport: transport}
	start := time.Now()
	resp, errDo := httpClient.Do(req)
	latency := time.Since(start).Milliseconds()
	if errDo != nil {
		c.JSON(http.StatusOK, gin.H{
			"ok":         false,
			"proxy_url":  proxyURL,
			"target":     target,
			"latency_ms": latency,
			"error":      errDo.Error(),
		})
		return
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("management: close proxy-test response body error: %v", errClose)
		}
	}()

	c.JSON(http.StatusOK, gin.H{
		"ok":          resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusBadRequest,
		"proxy_url":   proxyURL,
		"target":      target,
		"http_status": resp.StatusCode,
		"latency_ms":  latency,
	})
}

// directTransport returns a transport that ignores any environment proxy so a
// blank proxy setting genuinely tests the direct connection.
func directTransport() http.RoundTripper {
	if base, ok := http.DefaultTransport.(*http.Transport); ok && base != nil {
		clone := base.Clone()
		clone.Proxy = nil
		return clone
	}
	return &http.Transport{Proxy: nil}
}
