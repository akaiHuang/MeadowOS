package main

import (
	"net/http"

	"github.com/hey-meadow/meadow-code/go/core/buildinfo"
	"github.com/hey-meadow/meadow-code/go/core/modelconnection"
)

// Account status remains truthfully guest. The lab has no account lifecycle,
// watcher, commercial grants, lease or paid membership service.
func registerVMLabAccountRoutes(api *http.ServeMux, client desktopProductAuth) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		status, err := client.Status(r.Context())
		writeDesktopProductAuthResult(w, status, err)
	}
	for _, route := range []string{"GET /auth/status", "GET /auth/admission-status", "GET /entitlement/status", "GET /license/status"} {
		api.HandleFunc(route, handler)
	}
}

func serveVMLabBuildProfile(w http.ResponseWriter, r *http.Request) bool {
	if !buildinfo.VMLab || r.URL.Path != "/js/vm_lab_build.js" {
		return false
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(`export const VM_LAB_BUILD = true;
const mark=()=>{const b=document.createElement('div');b.id='vmLabBuildIndicator';b.textContent='VM 測試版 · 免 Meadow 登入 · 自備 API';b.style.cssText='position:fixed;left:12px;bottom:10px;z-index:2147483645;font:12px system-ui;background:#3a3420;color:#ffedb2;padding:5px 10px;border-radius:5px;pointer-events:none';document.body.append(b)};
if(document.readyState==='loading')document.addEventListener('DOMContentLoaded',mark,{once:true});else mark();
`))
	}
	return true
}

func desktopConnectionKinds() []map[string]string {
	result := []map[string]string{
		{"id": string(modelconnection.KindProviderAPI), "name": "其他模型供應商 API"},
		{"id": string(modelconnection.KindLocalAPI), "name": "既有本地模型 API"},
	}
	if !buildinfo.VMLab {
		result = append([]map[string]string{{"id": string(modelconnection.KindManagedAPI), "name": "Meadow API"}}, result...)
		result = append(result, map[string]string{"id": string(modelconnection.KindMeadowCore), "name": "Meadow Core"})
	}
	return result
}

// The API and its tests share the actual native capability projection.
func desktopClientCapabilities(connectionMode bool) map[string]any {
	return map[string]any{"protected": false, "modelVaultRequired": false, "authenticationRequired": !buildinfo.VMLab, "vmLab": buildinfo.VMLab, "modelConnectionMode": connectionMode}
}
