package managementasset

import _ "embed"

// ECharts is pinned to Apache ECharts 5.6.0. Licensing notices accompany the asset.
//
//go:embed vendor/echarts-5.6.0.min.js
var echartsJavaScript []byte

//go:embed vendor/echarts-LICENSE.txt
var echartsLicense []byte

//go:embed vendor/echarts-NOTICE.txt
var echartsNotice []byte

// EChartsJavaScript includes the upstream license and notice in binary distributions.
func EChartsJavaScript() []byte {
	out := make([]byte, 0, len(echartsJavaScript)+len(echartsLicense)+len(echartsNotice)+16)
	out = append(out, "/*\n"...)
	out = append(out, echartsLicense...)
	out = append(out, echartsNotice...)
	out = append(out, "\n*/\n"...)
	return append(out, echartsJavaScript...)
}
