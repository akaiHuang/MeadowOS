//go:build meadow_vm_lab && !meadow_protected

package modelconnection

import "testing"

func TestVMLabRejectsMembershipAndCoreAtCatalogBoundary(t *testing.T) {
	for _, kind := range []Kind{KindManagedAPI, KindMeadowCore} {
		if ValidateBaseURL(kind, "https://example.com/v1") == nil {
			t.Fatalf("accepted %s", kind)
		}
	}
	if err := ValidateBaseURL(KindProviderAPI, "https://example.com/v1"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateBaseURL(KindLocalAPI, "http://127.0.0.1:11434/v1"); err != nil {
		t.Fatal(err)
	}
}
