package management

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

// GetCodexEgressPool returns the egress pool state, per-route history and group stats.
// Proxy passwords are masked.
func (h *Handler) GetCodexEgressPool(c *gin.Context) {
	h.mu.Lock()
	cfg := h.cfg
	h.mu.Unlock()
	pool := helps.CodexEgressPools()
	if cfg != nil {
		pool.ApplyConfig(cfg.Codex.EgressPool)
	}
	c.JSON(http.StatusOK, pool.Snapshot())
}

// PutCodexEgressPoolConfig replaces codex.egress-pool. Proxy URLs still carrying the masked
// password ("user:***@host") keep the stored password.
func (h *Handler) PutCodexEgressPoolConfig(c *gin.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration is not available"})
		return
	}
	var value config.CodexEgressPoolConfig
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"error": "expected one JSON configuration object"})
		return
	}
	value = helps.UnmaskCodexEgressConfig(value, h.cfg.Codex.EgressPool)
	for i, p := range value.Proxies {
		if strings.Contains(p.URL, ":***@") {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("代理 %d 的密码已隐藏但找不到原记录，请重新填写完整地址", i+1)})
			return
		}
		setting, errParse := proxyutil.Parse(p.URL)
		if strings.TrimSpace(p.URL) != "" && (errParse != nil || setting.Mode != proxyutil.ModeProxy) {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("代理 %d 地址无效（支持 http/https/socks5）", i+1)})
			return
		}
	}
	value.Normalize()
	next := h.cfg.CloneForRuntime()
	next.Codex.EgressPool = value
	h.persistConfigLocked(c, next)
}

// TestCodexEgressProxy probes chatgpt.com through the configured pool proxy at "index" and
// reports the exit IP, country and Cloudflare colo. Body: {"index": 0}.
func (h *Handler) TestCodexEgressProxy(c *gin.Context) {
	var body struct {
		Index int `json:"index"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "invalid body"})
		return
	}
	h.mu.Lock()
	cfg := h.cfg
	h.mu.Unlock()
	pool := helps.CodexEgressPools()
	if cfg != nil {
		pool.ApplyConfig(cfg.Codex.EgressPool)
	}
	raw, ok := pool.RawProxyURL(body.Index)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "代理不存在"})
		return
	}
	transport := buildProxyTransport(raw)
	if transport == nil {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "无法解析代理地址"})
		return
	}
	req, _ := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, "https://chatgpt.com/cdn-cgi/trace", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	client := &http.Client{Timeout: defaultAPICallTimeout, Transport: transport}
	start := time.Now()
	resp, errDo := client.Do(req)
	latency := time.Since(start).Milliseconds()
	if errDo != nil {
		msg := errDo.Error()
		if masked := helps.MaskCodexEgressProxyURL(raw); masked != "" {
			msg = strings.ReplaceAll(msg, raw, masked)
		}
		c.JSON(http.StatusOK, gin.H{"ok": false, "latency_ms": latency, "error": msg})
		return
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("management: close egress proxy test body error: %v", errClose)
		}
	}()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	out := gin.H{"ok": resp.StatusCode == http.StatusOK, "status": resp.StatusCode, "latency_ms": latency}
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, found := strings.Cut(line, "="); found && (k == "ip" || k == "loc" || k == "colo") {
			out[k] = strings.TrimSpace(v)
		}
	}
	c.JSON(http.StatusOK, out)
}
