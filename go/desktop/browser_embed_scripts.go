package main

import _ "embed"

//go:embed browser_assets/webmcp_bridge.js
var desktopBrowserWebMCPScript string

//go:embed browser_assets/diagnostics.js
var desktopBrowserDiagnosticsScript string
