package tools

// WorkspaceFileState is an implementation-observed current file version.
// Missing is different from an observation failure (permission, link or IO).
// Path is a canonical model namespace, never a private host path.
type WorkspaceFileState struct {
	Path   string
	Exists bool
	SHA256 string
}

// WorkspaceFileStateObserver lets a confined file-tool adapter participate in
// native freshness guards without exposing its filesystem implementation.
type WorkspaceFileStateObserver interface {
	ObserveWorkspaceFileState(string) (WorkspaceFileState, error)
}
