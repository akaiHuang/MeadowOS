package agent

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var codexStyleArtifactClaimPath = regexp.MustCompile(`(?i)(?:/workspace/)?[a-z0-9][a-z0-9._/-]*\.[a-z0-9]{1,12}`)

var codexStyleMutationClaimMarkers = []string{
	" has been created", " has been saved", " is now on disk", " was written",
	"created ", "saved ", "wrote ", "updated ", "modified ", "generated ",
	"已建立", "建立完成", "已儲存", "已保存", "已寫入", "已修改", "已產出",
}

var codexStyleArtifactExtensions = map[string]bool{
	".c": true, ".cpp": true, ".css": true, ".csv": true, ".docx": true,
	".go": true, ".h": true, ".htm": true, ".html": true, ".java": true,
	".jpeg": true, ".jpg": true, ".js": true, ".json": true, ".jsx": true,
	".md": true, ".pdf": true, ".png": true, ".pptx": true, ".py": true,
	".rs": true, ".sh": true, ".svg": true, ".toml": true, ".ts": true,
	".tsx": true, ".txt": true, ".xlsx": true, ".xml": true, ".yaml": true,
	".yml": true, ".zsh": true,
}

// codexStyleMutationEvidenceStopHook checks only an observable postcondition:
// a final response may not claim that a named artifact was mutated unless a
// successful mutation result reports that path. It never decides whether the
// user wanted a coding workflow or which tool should run next.
func codexStyleMutationEvidenceStopHook(snapshot CodexStyleStopSnapshot) CodexStyleStopDecision {
	content := strings.TrimSpace(snapshot.Content)
	if content == "" || !codexStyleContainsMutationClaim(content) {
		return CodexStyleStopDecision{}
	}
	claimed := codexStyleClaimedPaths(content)
	if len(claimed) == 0 {
		return CodexStyleStopDecision{}
	}
	mutationClaims := make([]string, 0, len(claimed))
	missing := make([]string, 0, len(claimed))
	for _, path := range claimed {
		// Screenshots reported by a successful first-class browser verifier are
		// technical evidence owned by that tool, not files the model claimed to
		// have created through a mutation tool.
		if codexStylePathHasTechnicalEvidence(path, snapshot.TechnicalArtifacts) {
			continue
		}
		mutationClaims = append(mutationClaims, path)
		if !codexStylePathHasMutationEvidence(path, snapshot.Paths) {
			missing = append(missing, path)
		}
	}
	if len(mutationClaims) == 0 {
		return CodexStyleStopDecision{}
	}
	if len(missing) == 0 && snapshot.Mutations > 0 {
		return CodexStyleStopDecision{}
	}
	if len(missing) == 0 {
		missing = mutationClaims
	}
	return CodexStyleStopDecision{
		Continue: true,
		Message: "Completion evidence is missing: " + strings.Join(missing, ", ") +
			" 還沒產出。No successful write/edit result supports the previous claim. Perform the required mutation now, or clearly state that no file was created.",
	}
}

func codexStylePathHasTechnicalEvidence(claimed string, observedPaths []string) bool {
	normalize := func(value string) string {
		value = strings.ToLower(filepath.Clean(strings.TrimSpace(value)))
		// The artifact-path matcher intentionally may omit the optional leading
		// slash from /workspace. Restore that canonical workspace spelling before
		// applying strict path identity.
		if strings.HasPrefix(value, "workspace/") {
			value = "/" + value
		}
		return value
	}
	claim := normalize(claimed)
	claimHasDirectory := strings.Contains(claim, "/")
	for _, candidate := range observedPaths {
		observed := normalize(candidate)
		if observed == claim || (!claimHasDirectory && filepath.Base(observed) == claim) {
			return true
		}
	}
	return false
}

func codexStyleTechnicalScreenshotPaths(content string) []string {
	paths := make([]string, 0, 1)
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "-"))
		const prefix = "human_review_screenshot_path:"
		if !strings.HasPrefix(strings.ToLower(line), prefix) {
			continue
		}
		artifactPath := strings.TrimSpace(line[len(prefix):])
		if suffix := strings.LastIndex(artifactPath, " ("); suffix > 0 {
			artifactPath = strings.TrimSpace(artifactPath[:suffix])
		}
		artifactPath = strings.Trim(artifactPath, "`'\"")
		switch strings.ToLower(filepath.Ext(artifactPath)) {
		case ".jpeg", ".jpg", ".png":
			paths = append(paths, artifactPath)
		}
	}
	return codexStyleUniqueStrings(paths)
}

func codexStyleContainsMutationClaim(content string) bool {
	lower := strings.ToLower(content)
	for _, marker := range codexStyleMutationClaimMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func codexStyleClaimedPaths(content string) []string {
	matches := codexStyleArtifactClaimPath.FindAllString(content, -1)
	seen := map[string]bool{}
	paths := make([]string, 0, len(matches))
	for _, match := range matches {
		path := strings.Trim(strings.TrimSpace(match), "`'\".,;:()[]{}")
		if !codexStyleArtifactExtensions[strings.ToLower(filepath.Ext(path))] {
			continue
		}
		key := strings.ToLower(path)
		if path == "" || seen[key] {
			continue
		}
		seen[key] = true
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func codexStylePathHasMutationEvidence(claimed string, changed []string) bool {
	claim := strings.ToLower(filepath.Clean(claimed))
	claimBase := strings.ToLower(filepath.Base(claim))
	for _, candidate := range changed {
		observed := strings.ToLower(filepath.Clean(strings.TrimSpace(candidate)))
		if observed == claim || strings.HasSuffix(observed, "/"+claim) || filepath.Base(observed) == claimBase {
			return true
		}
	}
	return false
}
