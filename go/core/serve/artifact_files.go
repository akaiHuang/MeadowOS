package serve

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/gorilla/css/scanner"
	"github.com/hey-meadow/meadow-code/go/core/session"
	"golang.org/x/net/html"
)

const artifactPreviewMax = 32 << 20
const artifactDownloadMax = 64 << 20
const artifactPreviewResourceMax = 1 << 20
const artifactPreviewResourcesMax = 4 << 20
const artifactPreviewResourceCount = 32

type artifactPreviewResource struct {
	Kind     string `json:"kind"`
	URL      string `json:"url"`
	Contents string `json:"contents"`
}

type artifactFileRequest struct {
	CWD          string `json:"cwd"`
	SessionID    string `json:"sessionId"`
	RelativePath string `json:"relativePath"`
}

// A resumed live session has a new ID, while its artifact mount follows the
// durable chat ID. Reject mixing a known live session with another workspace.
func (s *Server) artifactChatID(cwd, id string) (string, error) {
	if sess := s.session(id); sess != nil {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		if filepath.Clean(sess.cwd) != filepath.Clean(cwd) {
			return "", fmt.Errorf("artifact session belongs to another workspace")
		}
		return queueChatID(sess), nil
	}
	return id, nil
}

// Artifact reads use canonical roots; executable HTML never gets a same-origin
// inline response. The preview consumes JSON only and runs it in an opaque frame.
func openArtifactFile(body artifactFileRequest, limit int64) (*os.File, os.FileInfo, error) {
	root, rel := body.CWD, body.RelativePath
	sessionMount := false
	if rel == "/session" || strings.HasPrefix(rel, "/session/") {
		sessionMount = true
		var err error
		root, err = session.ArtifactsDir(body.CWD, body.SessionID)
		if err != nil {
			return nil, nil, err
		}
		rel = strings.TrimPrefix(strings.TrimPrefix(rel, "/session"), "/")
	}
	if filepath.IsAbs(rel) || !filepath.IsLocal(rel) {
		return nil, nil, fmt.Errorf("artifact path is outside the selected root")
	}
	// OpenRoot confines every lookup, including ancestor symlink changes during
	// the open. A prior EvalSymlinks check alone would leave a check/open race.
	confined, err := openArtifactRoot(root, sessionMount)
	if err != nil {
		return nil, nil, err
	}
	defer confined.Close()
	file, err := confined.Open(rel)
	if err != nil {
		return nil, nil, err
	}
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() > limit {
		file.Close()
		return nil, nil, fmt.Errorf("artifact must be a regular file of at most %d bytes", limit)
	}
	return file, stat, nil
}

func openArtifactRoot(path string, sessionMount bool) (*os.Root, error) {
	if !sessionMount {
		return os.OpenRoot(path)
	}
	// Anchor the chat mount in trusted app storage. Open each directory through
	// its parent handle and verify its inode, rejecting swapped mount symlinks
	// as well as ancestor symlinks to another chat or outside storage.
	primary := session.PrimaryDir()
	rel, err := filepath.Rel(primary, path)
	if err != nil || !filepath.IsLocal(rel) {
		return nil, fmt.Errorf("invalid artifact mount")
	}
	parent, err := os.OpenRoot(primary)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		before, err := parent.Lstat(part)
		if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
			parent.Close()
			return nil, fmt.Errorf("artifact mount must contain real directories")
		}
		next, err := parent.OpenRoot(part)
		parent.Close()
		if err != nil {
			return nil, err
		}
		after, err := next.Stat(".")
		if err != nil || !os.SameFile(before, after) {
			next.Close()
			return nil, fmt.Errorf("artifact mount changed during open")
		}
		parent = next
	}
	return parent, nil
}

func (s *Server) handleArtifactPreview(w http.ResponseWriter, r *http.Request) {
	var body artifactFileRequest
	if !decodeBody(w, r, &body) {
		return
	}
	var err error
	body.SessionID, err = s.artifactChatID(body.CWD, body.SessionID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !strings.EqualFold(filepath.Ext(body.RelativePath), ".html") {
		writeErr(w, http.StatusBadRequest, "complete preview supports HTML artifacts")
		return
	}
	file, stat, err := openArtifactFile(body, artifactPreviewMax)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, artifactPreviewMax+1))
	if err != nil || len(data) > artifactPreviewMax || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		writeErr(w, http.StatusBadRequest, "artifact is not bounded UTF-8 HTML")
		return
	}
	resources, previewErrors := artifactPreviewResources(body, data)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{"relativePath": body.RelativePath, "contents": string(data), "binary": false, "truncated": false, "sizeBytes": stat.Size(), "previewResources": resources, "previewErrors": previewErrors})
}

// Materialize only explicitly referenced local classic scripts and stylesheets.
// The frontend embeds these bytes into its existing opaque-origin sandbox;
// this endpoint never exposes executable same-origin URLs or grants a file token.
// Modules, dynamic fetch/import, and transitive CSS resources are not bundled.
// Each companion uses the same selected workspace/durable-chat root as the HTML.
func artifactPreviewResources(body artifactFileRequest, source []byte) ([]artifactPreviewResource, []string) {
	resources := []artifactPreviewResource{}
	problems := []string{}
	refs := []artifactPreviewResource{}
	seen := map[string]bool{}
	hasBase := false
	z := html.NewTokenizer(bytes.NewReader(source))
	for {
		if len(problems) >= 8 {
			break
		}
		kind := z.Next()
		if kind == html.ErrorToken {
			break
		}
		if kind != html.StartTagToken && kind != html.SelfClosingTagToken {
			continue
		}
		token := z.Token()
		attrs := map[string]string{}
		for _, attr := range token.Attr {
			if _, ok := attrs[attr.Key]; !ok {
				attrs[attr.Key] = attr.Val
			}
		}
		if token.Data == "base" && attrs["href"] != "" {
			hasBase = true
		}
		resource := artifactPreviewResource{}
		switch token.Data {
		case "script":
			resource.Kind, resource.URL = "script", attrs["src"]
		case "link":
			for _, rel := range strings.Fields(strings.ToLower(attrs["rel"])) {
				if rel == "stylesheet" {
					resource.Kind, resource.URL = "style", attrs["href"]
				}
			}
		}
		if resource.URL == "" {
			continue
		}
		parsed, err := url.Parse(strings.TrimSpace(resource.URL))
		if err != nil {
			problems = append(problems, "無法解析預覽資源路徑。")
			continue
		}
		if parsed.IsAbs() || parsed.Host != "" {
			continue
		} // No server-side network requests.
		if token.Data == "script" && strings.EqualFold(strings.TrimSpace(attrs["type"]), "module") {
			problems = append(problems, "內嵌預覽尚不支援本機 JavaScript 模組；原始檔案已保留。")
			continue
		}
		if attrs["integrity"] != "" {
			problems = append(problems, "內嵌預覽尚不支援帶完整性驗證的本機資源；原始檔案已保留。")
			continue
		}
		key := resource.Kind + "\x00" + resource.URL
		if seen[key] {
			continue
		}
		seen[key] = true
		if len(refs) >= artifactPreviewResourceCount {
			problems = append(problems, "預覽資源超過 32 個；原始檔案已保留。")
			break
		}
		refs = append(refs, resource)
	}
	if hasBase && len(refs) > 0 {
		return resources, append(problems, "內嵌預覽尚不支援以 base 重新定位本機資源；原始檔案已保留。")
	}
	total := 0
	for _, ref := range refs {
		parsed, _ := url.Parse(strings.TrimSpace(ref.URL))
		if parsed.Path == "" || strings.ContainsAny(parsed.Path, "\\\x00") || strings.HasPrefix(parsed.Path, "/") {
			problems = append(problems, fmt.Sprintf("無法預覽資源 %q：需要檔案旁的相對路徑。", ref.URL))
			continue
		}
		request := body
		// Preserve the virtual /session mount; openArtifactFile confines the
		// cleaned relative name in exactly the same canonical root as the HTML.
		mount := ""
		rel := filepath.ToSlash(body.RelativePath)
		if strings.HasPrefix(rel, "/session/") {
			mount = "/session/"
			rel = strings.TrimPrefix(rel, mount)
		}
		resolved := path.Join(path.Dir(rel), parsed.Path)
		if !filepath.IsLocal(filepath.FromSlash(resolved)) {
			problems = append(problems, fmt.Sprintf("無法預覽資源 %q：路徑超出目前範圍。", ref.URL))
			continue
		}
		request.RelativePath = mount + resolved
		file, _, err := openArtifactFile(request, artifactPreviewResourceMax)
		if err != nil {
			problems = append(problems, fmt.Sprintf("無法載入預覽資源 %q：檔案不存在、超過 1 MiB 或不在允許範圍。", ref.URL))
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(file, artifactPreviewResourceMax+1))
		file.Close()
		if readErr != nil || len(data) > artifactPreviewResourceMax || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			problems = append(problems, fmt.Sprintf("無法載入預覽資源 %q：需要完整 UTF-8 文字檔。", ref.URL))
			continue
		}
		total += len(data)
		if total > artifactPreviewResourcesMax {
			problems = append(problems, "預覽資源合計超過 4 MiB；原始檔案已保留。")
			break
		}
		if ref.Kind == "style" && artifactPreviewCSSNeedsResources(string(data)) {
			problems = append(problems, fmt.Sprintf("預覽樣式 %q 另有本機資源或匯入；目前尚未支援，原始檔案已保留。", ref.URL))
			continue
		}
		ref.Contents = string(data)
		resources = append(resources, ref)
	}
	return resources, problems
}

func artifactPreviewCSSNeedsResources(source string) bool {
	scan := scanner.New(source)
	for {
		token := scan.Next()
		if token.Type == scanner.TokenEOF {
			return false
		}
		if token.Type == scanner.TokenError {
			return true
		}
		if token.Type == scanner.TokenAtKeyword && strings.EqualFold(token.Value, "@import") {
			return true
		}
		if token.Type != scanner.TokenURI {
			continue
		}
		start, end := strings.Index(token.Value, "("), strings.LastIndex(token.Value, ")")
		if start < 0 || end <= start {
			return true
		}
		value := strings.Trim(strings.TrimSpace(token.Value[start+1:end]), "\"'")
		parsed, err := url.Parse(value)
		if err != nil || (!parsed.IsAbs() && parsed.Host == "") {
			return true
		}
	}
}

func (s *Server) handleArtifactDownload(w http.ResponseWriter, r *http.Request) {
	var body artifactFileRequest
	if !decodeBody(w, r, &body) {
		return
	}
	var err error
	body.SessionID, err = s.artifactChatID(body.CWD, body.SessionID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	file, stat, err := openArtifactFile(body, artifactDownloadMax)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(body.RelativePath)}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", fmt.Sprint(stat.Size()))
	_, _ = io.Copy(w, io.LimitReader(file, artifactDownloadMax))
}
