package tools

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
	"unicode"
)

type presentonEndpointConfig struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

type presentonRenderResult struct {
	HTML          string
	PPTX          []byte
	Presentation  []byte
	Template      string
	SlidePreviews [][]byte
}

func (t *PresentationArtifact) presentonEndpoint(engine string) (presentonEndpointConfig, error) {
	return loadPresentonEndpoint(engine, t.PresentonURL, t.PresentonToken)
}

func loadPresentonEndpoint(engine, endpointURL, endpointToken string) (presentonEndpointConfig, error) {
	engine = strings.TrimSpace(engine)
	if engine == "" || engine == "builtin" {
		return presentonEndpointConfig{}, nil
	}
	if engine != "" && engine != "auto" && engine != "presenton" {
		return presentonEndpointConfig{}, fmt.Errorf("unknown presentation engine %q", engine)
	}
	config := presentonEndpointConfig{URL: endpointURL, Token: endpointToken}
	if config.URL == "" {
		if home, err := os.UserHomeDir(); err == nil {
			path := filepath.Join(home, "Library", "Application Support", "MeadowHarnessComparison", "presenton", "endpoint.json")
			file, err := os.Open(path)
			if err == nil {
				defer file.Close()
				if err := json.NewDecoder(io.LimitReader(file, 16*1024)).Decode(&config); err != nil {
					return config, fmt.Errorf("invalid local Presenton configuration")
				}
			} else if !os.IsNotExist(err) {
				return config, fmt.Errorf("cannot read local Presenton configuration")
			}
		}
	}
	if config.URL == "" {
		if engine == "presenton" {
			return config, fmt.Errorf("Presenton is not installed; run scripts/presenton/install.py or select builtin HTML")
		}
		return config, nil
	}
	u, err := url.Parse(config.URL)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1") {
		return config, fmt.Errorf("Presenton renderer must be a trusted localhost HTTP endpoint")
	}
	if strings.TrimSpace(config.Token) == "" {
		return config, fmt.Errorf("Presenton renderer token is missing")
	}
	config.URL = strings.TrimRight(config.URL, "/")
	return config, nil
}

func (t *PresentationArtifact) renderPresenton(ctx context.Context, config presentonEndpointConfig, deck presentationData, args map[string]any) (*presentonRenderResult, error) {
	template := strings.TrimSpace(strArg(args, "template"))
	requestWire := map[string]any{"deck": deck, "template": template, "language": strArg(args, "language"), "instructions": strArg(args, "instructions")}
	if t.markdownImport != nil {
		if len(t.markdownImport.Bindings) != len(deck.Slides) {
			return nil, fmt.Errorf("compiled Markdown slide binding mismatch")
		}
		rawDeck, err := json.Marshal(deck)
		if err != nil {
			return nil, err
		}
		var compiledDeck map[string]any
		if err := json.Unmarshal(rawDeck, &compiledDeck); err != nil {
			return nil, err
		}
		slides, _ := compiledDeck["slides"].([]any)
		for index, binding := range t.markdownImport.Bindings {
			if binding.LayoutID != deck.Slides[index].PresentonLayoutID {
				return nil, fmt.Errorf("compiled Markdown layout binding mismatch")
			}
			slide := slides[index].(map[string]any)
			slide["markdownAdapterAlias"] = binding.Alias
			slide["markdownSchemaSHA256"] = binding.SchemaSHA256
			if len(t.markdownImport.ImagePlacements) != len(deck.Slides) {
				return nil, fmt.Errorf("compiled Markdown image placement count mismatch")
			}
			if len(t.markdownImport.ImagePlacements[index]) != 0 {
				slide["markdownImageAssets"] = t.markdownImport.ImagePlacements[index]
			}
		}
		requestWire["deck"] = compiledDeck
		requestWire["import_mode"] = "markdown_v1"
		requestWire["markdown_source"] = t.markdownImport.Source
		if len(t.markdownImport.Assets) != 0 {
			requestWire["markdown_assets"] = t.markdownImport.Assets
		}
	}
	payload, err := json.Marshal(requestWire)
	if err != nil {
		return nil, err
	}
	requestLimit := 1 << 20
	if t.markdownImport != nil && len(t.markdownImport.Assets) != 0 {
		requestLimit = presentationMarkdownMaxAssetRequestBytes
	}
	if len(payload) > requestLimit {
		return nil, fmt.Errorf("presentation request exceeds its %d-byte limit", requestLimit)
	}
	raw, err := presentonPost(ctx, config, t.HTTP, "/render", payload, 96<<20, 25*time.Minute)
	if err != nil {
		return nil, err
	}
	var wire struct {
		HTML          string          `json:"html"`
		PPTX          string          `json:"pptx_base64"`
		Presentation  json.RawMessage `json:"presentation"`
		Template      string          `json:"template"`
		LayoutIDs     []string        `json:"layout_ids"`
		SlidePreviews []struct {
			Index    int    `json:"index"`
			PNG      string `json:"png_base64"`
			LayoutID string `json:"layout_id"`
		} `json:"slide_previews"`
		MarkdownImport *struct {
			SourceSHA256          string                                    `json:"source_sha256"`
			SourceByteLength      int                                       `json:"source_byte_length"`
			SlideCount            int                                       `json:"slide_count"`
			AdapterManifestSHA256 string                                    `json:"adapter_manifest_sha256"`
			TemplateLayoutsSHA256 string                                    `json:"template_layouts_sha256"`
			Assets                map[string]presentonMarkdownAssetMetadata `json:"assets"`
		} `json:"markdown_import"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("invalid Presenton response")
	}
	if len(wire.HTML) > 32<<20 || !strings.Contains(strings.ToLower(wire.HTML), "<!doctype html>") || !strings.Contains(wire.HTML, "</html>") {
		return nil, fmt.Errorf("Presenton did not return a complete bounded HTML document")
	}
	pptx, err := base64.StdEncoding.DecodeString(wire.PPTX)
	if err != nil || len(pptx) > 64<<20 {
		return nil, fmt.Errorf("invalid Presenton PPTX")
	}
	archive, err := zip.NewReader(bytes.NewReader(pptx), int64(len(pptx)))
	if err != nil {
		return nil, fmt.Errorf("Presenton PPTX is not a ZIP package")
	}
	entries := make(map[string]bool)
	for _, file := range archive.File {
		entries[file.Name] = true
	}
	if !entries["[Content_Types].xml"] || !entries["ppt/presentation.xml"] {
		return nil, fmt.Errorf("Presenton PPTX lacks required presentation parts")
	}
	if wire.Template != template || len(wire.Presentation) == 0 || string(wire.Presentation) == "null" {
		return nil, fmt.Errorf("Presenton output metadata mismatch")
	}
	if t.markdownImport != nil {
		manifest, err := loadMarkdownAdapters()
		receipt := wire.MarkdownImport
		if err != nil || receipt == nil || receipt.SourceSHA256 != t.markdownImport.Source.SHA256 || receipt.SourceByteLength != t.markdownImport.Source.ByteLength || receipt.SlideCount != len(deck.Slides) || receipt.AdapterManifestSHA256 != markdownAdapterManifestSHA256() || receipt.TemplateLayoutsSHA256 != manifest.TemplateLayoutsSHA256 {
			return nil, fmt.Errorf("Presenton did not verify the exact compiled Markdown import receipt")
		}
		if len(receipt.Assets) != len(t.markdownImport.Assets) || (len(t.markdownImport.Assets) != 0 && !reflect.DeepEqual(receipt.Assets, markdownAssetMetadata(t.markdownImport.Assets))) {
			return nil, fmt.Errorf("Presenton did not verify every exact supplied/placeholder image asset")
		}
	}
	if len(wire.SlidePreviews) != len(deck.Slides) || len(wire.LayoutIDs) != len(deck.Slides) {
		return nil, fmt.Errorf("Presenton did not return an actual PNG preview for every slide")
	}
	previews := make([][]byte, 0, len(wire.SlidePreviews))
	total := 0
	for index, preview := range wire.SlidePreviews {
		if preview.Index != index || preview.LayoutID != deck.Slides[index].PresentonLayoutID || wire.LayoutIDs[index] != deck.Slides[index].PresentonLayoutID {
			return nil, fmt.Errorf("Presenton slide preview metadata mismatch at slide %d", index+1)
		}
		if len(preview.PNG) > base64.StdEncoding.EncodedLen(1<<20) {
			return nil, fmt.Errorf("Presenton slide %d preview exceeds 1 MiB", index+1)
		}
		data, err := base64.StdEncoding.DecodeString(preview.PNG)
		if err != nil || len(data) > 1<<20 || !bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
			return nil, fmt.Errorf("Presenton slide %d preview is not a bounded PNG", index+1)
		}
		config, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil || config.Width != 1280 || config.Height != 720 {
			return nil, fmt.Errorf("Presenton slide %d preview must be a valid 1280x720 PNG", index+1)
		}
		if _, err := png.Decode(bytes.NewReader(data)); err != nil {
			return nil, fmt.Errorf("Presenton slide %d preview PNG is incomplete", index+1)
		}
		total += len(data)
		if total > 16<<20 {
			return nil, fmt.Errorf("Presenton slide previews exceed 16 MiB")
		}
		previews = append(previews, data)
	}
	return &presentonRenderResult{HTML: wire.HTML, PPTX: pptx, Presentation: append(wire.Presentation, '\n'), Template: template, SlidePreviews: previews}, nil
}

// presentonPost is shared by read-only discovery and rendering. Authentication
// stays in local configuration, redirects are never followed, and only bounded
// renderer validation diagnostics cross the model boundary.
func presentonPost(ctx context.Context, config presentonEndpointConfig, configuredClient *http.Client, route string, payload []byte, maxBytes int64, timeout time.Duration) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, config.URL+route, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("invalid local Presenton request")
	}
	req.Header.Set("Authorization", "Bearer "+config.Token)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: timeout}
	if configuredClient != nil {
		*client = *configuredClient
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("Presenton renderer request failed; check the installed local service")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 8192))
		message := fmt.Sprintf("Presenton %s failed (HTTP %d)", strings.TrimPrefix(route, "/"), response.StatusCode)
		if response.StatusCode == http.StatusBadRequest || response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusUnprocessableEntity {
			var diagnostic struct {
				Error     string `json:"error"`
				FieldPath string `json:"field_path"`
			}
			if json.Unmarshal(body, &diagnostic) == nil {
				if path := safePresentonDiagnostic(diagnostic.FieldPath, config.Token, 240); path != "" {
					message += " at " + path
				}
				if problem := safePresentonDiagnostic(diagnostic.Error, config.Token, 400); problem != "" {
					message += ": " + problem
				}
			}
		}
		return nil, fmt.Errorf("%s; input remains in tool arguments; no successful artifact was reported", message)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("cannot read Presenton response")
	}
	if int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf("Presenton %s response exceeds %d bytes", strings.TrimPrefix(route, "/"), maxBytes)
	}
	return raw, nil
}

func safePresentonDiagnostic(value, token string, maxRunes int) string {
	if token != "" {
		value = strings.ReplaceAll(value, token, "[redacted]")
	}
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > maxRunes {
		runes = runes[:maxRunes]
	}
	return string(runes)
}
