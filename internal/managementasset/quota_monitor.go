package managementasset

import _ "embed"

//go:embed quota_monitor.html
var quotaMonitorHTML []byte

// QuotaMonitorHTML returns a copy of the embedded read-only quota monitor page.
func QuotaMonitorHTML() []byte {
	return append([]byte(nil), quotaMonitorHTML...)
}
