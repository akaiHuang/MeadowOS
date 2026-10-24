package producthost

import (
	"fmt"
	"strings"

	harnesscore "github.com/hey-meadow/meadow-code/go/core/harness"
	"github.com/hey-meadow/meadow-code/go/core/provider"
)

// ArtifactRetrievalMode concerns curated implementation examples only. User
// attachments, explicit memory, skills and presentation contracts are separate.
type ArtifactRetrievalMode string

const (
	ArtifactRetrievalAuto     ArtifactRetrievalMode = "auto"
	ArtifactRetrievalAssisted ArtifactRetrievalMode = "assisted"
	ArtifactRetrievalOptional ArtifactRetrievalMode = "optional"
	ArtifactRetrievalOff      ArtifactRetrievalMode = "off"
)

func parseArtifactRetrievalMode(value string) (ArtifactRetrievalMode, error) {
	mode := ArtifactRetrievalMode(strings.ToLower(strings.TrimSpace(value)))
	switch mode {
	case "", ArtifactRetrievalAuto:
		return ArtifactRetrievalAuto, nil
	case ArtifactRetrievalAssisted, ArtifactRetrievalOptional, ArtifactRetrievalOff:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid artifact retrieval mode %q: use auto, assisted, optional or off", value)
	}
}

func artifactRetrievalForModel(modelID string, configured ArtifactRetrievalMode) ArtifactRetrievalMode {
	if configured != "" && configured != ArtifactRetrievalAuto {
		return configured
	}
	profile, _ := provider.ProfileForModel(modelID)
	switch profile.Family {
	case "ornith":
		return ArtifactRetrievalAssisted
	case "xai", "deepseek":
		return ArtifactRetrievalOff
	default:
		return ArtifactRetrievalOptional
	}
}

// Apply to a fresh runtime binding, never the persistent session registry:
// switching providers must both revoke and restore their default capabilities.
func bindingWithArtifactRetrieval(binding harnesscore.Binding, configured ArtifactRetrievalMode) harnesscore.Binding {
	mode := artifactRetrievalForModel(binding.ModelID, configured)
	if mode == ArtifactRetrievalOff {
		binding.DisabledTools = append(append([]string(nil), binding.DisabledTools...), "artifact_search", "artifact_read", "artifact_copy")
	}
	if binding.Registry != nil {
		binding.Registry = binding.Registry.Excluding(binding.DisabledTools)
		if _, available := binding.Registry.Get("artifact_search"); available && mode == ArtifactRetrievalAssisted {
			binding.SystemPrompt += "\n\nCurated implementation reuse preference: assess a relevant reference with artifact_search when it can help, unless the user requests independent work. The tool presents a user choice before source reuse. Respect that choice: independent development must not read or copy curated source, while copying still requires you to judge suitability, make needed adaptations, and verify the actual result. This preference never expands the task or relaxes verification."
		}
	}
	return binding
}
