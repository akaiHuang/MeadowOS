package producthost

import (
	"fmt"
	"strings"

	harnesscore "github.com/hey-meadow/meadow-code/go/core/harness"
	"github.com/hey-meadow/meadow-code/go/core/prompt"
)

const conversationProfileMeadowOS = "meadow-os"

func parseConversationProfile(value string) (string, error) {
	value = strings.TrimSpace(value)
	switch value {
	case "", conversationProfileMeadowOS:
		return value, nil
	default:
		return "", fmt.Errorf("unsupported conversation profile %q", value)
	}
}

// The OS coordinator needs one native guidance/delegation capability topology
// across model families. Other products retain their family runtime selection.
// This changes the explicit product role, never provider or model identity.
func productConversationHarnessMode(profile, modelID, override string) string {
	mode := harnesscore.ResolveMode(modelID, override)
	if profile == conversationProfileMeadowOS && mode == harnesscore.ModeKernel {
		return string(harnesscore.ModeCodexStyle)
	}
	return string(mode)
}

func productConversationPrompt(profile string, opts prompt.Options) string {
	if profile == conversationProfileMeadowOS {
		return prompt.BuildMeadowOS(opts)
	}
	return prompt.Build(opts)
}
