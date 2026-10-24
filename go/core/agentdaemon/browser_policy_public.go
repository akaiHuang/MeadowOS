//go:build !meadow_protected

package agentdaemon

func applyBrowserPipelinePolicy(bool) error { return nil }

func newSessionJournal(Config, bool) (*sessionJournal, error) { return nil, nil }
