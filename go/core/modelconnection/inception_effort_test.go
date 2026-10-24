package modelconnection_test

import (
	"testing"

	"github.com/hey-meadow/meadow-code/go/core/modelconnection"
)

func TestInceptionEffortRequiresExplicitValidCatalogMetadata(t *testing.T) {
	catalog := &modelconnection.Catalog{
		Version: 1, SelectedConnectionID: "inception", SelectedModelID: "opaque-alias",
		Connections: []modelconnection.Connection{{
			ID: "inception", Kind: modelconnection.KindProviderAPI, Name: "Inception",
			BaseURL: "https://api.inceptionlabs.ai/v1",
			Models:  []modelconnection.Model{{ID: "opaque-alias", SupportsTools: true, ReasoningControl: "inception-effort"}},
		}},
	}
	if err := modelconnection.Validate(catalog); err != nil {
		t.Fatal(err)
	}
	withCapability := catalog.Fingerprint()
	catalog.Connections[0].Models[0].ReasoningControl = ""
	if withCapability == "" || catalog.Fingerprint() == withCapability {
		t.Fatal("explicit capability must be part of configuration identity")
	}
	catalog.Connections[0].Models[0].ReasoningControl = "guessed-from-model-name"
	if err := modelconnection.Validate(catalog); err == nil || catalog.Fingerprint() != "" {
		t.Fatal("unknown reasoning controls must fail closed")
	}
}
