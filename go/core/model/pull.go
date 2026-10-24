package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// hfEndpoint is the Hugging Face hub. Override with MEADOW_HF_ENDPOINT (e.g. a
// mirror) — handy in regions where huggingface.co is slow/blocked, and for tests.
func hfEndpoint() string {
	if e := strings.TrimSpace(os.Getenv("MEADOW_HF_ENDPOINT")); e != "" {
		return strings.TrimRight(e, "/")
	}
	return "https://huggingface.co"
}

// hfFile is one entry from the HF repo tree API (/api/models/<repo>/tree/main).
type hfFile struct {
	Type string `json:"type"` // "file" | "directory"
	Path string `json:"path"`
	Size int64  `json:"size"`
	LFS  *struct {
		OID  string `json:"oid"` // sha256 of the content for LFS-backed files
		Size int64  `json:"size"`
	} `json:"lfs"`
}

// PullProgress is reported as a model downloads (bytes are cumulative).
type PullProgress struct {
	Model       string // served name, e.g. "Meadow 12B"
	File        string // file currently downloading
	FileIndex   int    // 1-based
	FileCount   int
	TotalDone   int64 // bytes downloaded across all files
	TotalSize   int64 // sum of all file sizes
	BytesPerSec int64
}

// pullClient has no overall timeout (a 15GB download is legitimately long);
// cancellation is via the context. A response-header timeout catches a dead
// server without clipping the body read, and Range survives redirects to the CDN.
var pullClient = &http.Client{
	Transport: &http.Transport{
		ResponseHeaderTimeout: 30 * time.Second,
		Proxy:                 http.ProxyFromEnvironment,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if rng := via[0].Header.Get("Range"); rng != "" {
			req.Header.Set("Range", rng) // HF resolve URLs redirect to a CDN; keep the resume range
		}
		return nil
	},
}

// listRepoFiles fetches the recursive file list for an HF repo.
func listRepoFiles(ctx context.Context, repo, revision string) ([]hfFile, error) {
	if strings.TrimSpace(revision) == "" {
		revision = "main"
	}
	url := hfEndpoint() + "/api/models/" + repo + "/tree/" + revision + "?recursive=1"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if tok := strings.TrimSpace(os.Getenv("HF_TOKEN")); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := pullClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing %s: HTTP %d", repo, resp.StatusCode)
	}
	var all []hfFile
	if err := json.NewDecoder(resp.Body).Decode(&all); err != nil {
		return nil, err
	}
	files := all[:0]
	for _, f := range all {
		if f.Type == "file" {
			files = append(files, f)
		}
	}
	return files, nil
}

// Pull downloads the model named by servedName into destRoot/<DirName>
// (destRoot defaults to DefaultPullDir()), resuming partial files, verifying
// LFS sha256, and reporting progress. Returns the model directory on success.
// It is safe to re-run: already-complete files are skipped.
func Pull(ctx context.Context, servedName, destRoot string, progress func(PullProgress)) (string, error) {
	info, ok := ModelByName(servedName)
	if !ok {
		return "", fmt.Errorf("unknown model %q", servedName)
	}
	if ok, reason := ModelDownloadAllowed(info.ServedName); !ok {
		return "", fmt.Errorf("%s", reason)
	}
	if destRoot == "" {
		destRoot = DefaultPullDir()
	}
	dir := filepath.Join(destRoot, info.DirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	files, err := listRepoFiles(ctx, info.HFRepo, info.HFRevision)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", fmt.Errorf("no files found in %s", info.HFRepo)
	}

	var totalSize, totalDone int64
	for _, f := range files {
		totalSize += f.Size
	}
	for i, f := range files {
		final := filepath.Join(dir, filepath.FromSlash(f.Path))
		if st, err := os.Stat(final); err == nil && st.Size() == f.Size {
			totalDone += f.Size // already complete
			continue
		}
		n, err := downloadFile(ctx, info, f, dir, i+1, len(files), totalSize, totalDone, progress)
		if err != nil {
			return "", fmt.Errorf("download %s: %w", f.Path, err)
		}
		totalDone += n
	}
	return dir, nil
}

// downloadFile downloads one repo file into dir, resuming from a prior .partial,
// verifying sha256 for LFS files, then renaming into place. Returns bytes that
// were not already on disk before this call (i.e. f.Size).
func downloadFile(ctx context.Context, info ModelInfo, f hfFile, dir string, idx, count int, totalSize, totalBefore int64, progress func(PullProgress)) (int64, error) {
	target := filepath.Join(dir, filepath.FromSlash(f.Path))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return 0, err
	}
	partial := target + ".partial"

	var offset int64
	if st, err := os.Stat(partial); err == nil {
		offset = st.Size()
	}
	if offset > f.Size { // stale/corrupt partial — start over
		_ = os.Remove(partial)
		offset = 0
	}

	revision := strings.TrimSpace(info.HFRevision)
	if revision == "" {
		revision = "main"
	}
	url := hfEndpoint() + "/" + info.HFRepo + "/resolve/" + revision + "/" + f.Path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	if tok := strings.TrimSpace(os.Getenv("HF_TOKEN")); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := pullClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	flag := os.O_CREATE | os.O_WRONLY
	switch resp.StatusCode {
	case http.StatusPartialContent: // 206 — resume accepted
		flag |= os.O_APPEND
	case http.StatusOK: // 200 — server ignored Range; restart cleanly
		offset = 0
		flag |= os.O_TRUNC
	default:
		return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	out, err := os.OpenFile(partial, flag, 0o644)
	if err != nil {
		return 0, err
	}

	buf := make([]byte, 1<<20)
	fileDone := offset
	startBytes := offset
	start := time.Now()
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				out.Close()
				return 0, werr
			}
			fileDone += int64(n)
			if progress != nil {
				var bps int64
				if el := time.Since(start).Seconds(); el > 0 {
					bps = int64(float64(fileDone-startBytes) / el)
				}
				progress(PullProgress{
					Model: info.ServedName, File: f.Path, FileIndex: idx, FileCount: count,
					TotalDone: totalBefore + fileDone, TotalSize: totalSize, BytesPerSec: bps,
				})
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			out.Close()
			return 0, rerr
		}
	}
	if err := out.Close(); err != nil {
		return 0, err
	}

	if f.LFS != nil && f.LFS.OID != "" {
		if err := verifySHA256(partial, f.LFS.OID); err != nil {
			_ = os.Remove(partial) // corrupt — force a clean re-pull next time
			return 0, err
		}
	}
	if err := os.Rename(partial, target); err != nil {
		return 0, err
	}
	return f.Size, nil
}

// verifySHA256 hashes a file and compares it to the expected hex digest.
func verifySHA256(path, want string) error {
	fh, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	h := sha256.New()
	if _, err := io.Copy(h, fh); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("sha256 mismatch (got %s, want %s)", got[:12], want[:12])
	}
	return nil
}
