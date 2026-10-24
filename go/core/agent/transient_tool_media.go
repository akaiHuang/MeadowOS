package agent

import "github.com/hey-meadow/meadow-code/go/core/provider"

// redactToolMediaCheckpointHook keeps provider-only tool pixels out of stable
// transcript callbacks without changing durable user-upload handling.
func redactToolMediaCheckpointHook(hooks KernelHooks) KernelHooks {
	if hooks.OnCheckpoint != nil {
		checkpoint := hooks.OnCheckpoint
		hooks.OnCheckpoint = func(messages []provider.Message) {
			checkpoint(provider.StripToolResultMedia(messages))
		}
	}
	return hooks
}
