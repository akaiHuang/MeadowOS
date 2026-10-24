//go:build !meadow_protected

package main

import (
	"os"
	"sort"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/edition"
	modelmgr "github.com/hey-meadow/meadow-code/go/core/model"
	"github.com/hey-meadow/meadow-code/go/core/provider"
)

// modelEntry is one row in the composer's model picker.
type modelEntry struct {
	Name                 string `json:"name"`
	ModelID              string `json:"modelId"`
	Delivery             string `json:"delivery,omitempty"` // api routes through Core without owning upstream weights
	Desc                 string `json:"desc,omitempty"`     // installation/catalog description
	ProductRole          string `json:"productRole,omitempty"`
	RoleLabel            string `json:"roleLabel,omitempty"`
	RoleDescription      string `json:"roleDescription,omitempty"`
	ProductionReady      bool   `json:"productionReady"`
	Active               bool   `json:"active"`
	Live                 bool   `json:"live"`                           // currently served by a running server
	Installed            bool   `json:"installed"`                      // weights exist locally, including app-bundled models
	HarnessStatus        string `json:"harnessStatus,omitempty"`        // ready | adapting
	RecommendedMemoryGiB int64  `json:"recommendedMemoryGiB,omitempty"` // total working-memory recommendation
	Multimodal           bool   `json:"multimodal"`                     // model accepts image/audio parts
}

func configuredInstalledModels() []string {
	for _, key := range []string{"MEADOW_CORE_INSTALLED_MODELS", "MEADOW_INSTALLED_MODELS"} {
		raw := strings.TrimSpace(os.Getenv(key))
		if raw == "" {
			continue
		}
		models := make([]string, 0, 4)
		for _, value := range strings.Split(raw, ",") {
			if value = strings.TrimSpace(value); value != "" {
				models = append(models, value)
			}
		}
		return models
	}
	return nil
}

// mergeModelList builds the picker rows: live-served models first (first one
// active), then the settings catalog. The catalog refers to the same physical
// model under a different id than the server's served name (a filesystem path
// vs. --served-model-name), so dedup must match on the display name as well
// as the id — id-only dedup shows the one running model twice.
func mergeModelList(served []string, settingsRaw map[string]any, fallback string, productCatalog ...[]modelmgr.ModelInfo) []modelEntry {
	return mergeModelListWithInstalled(served, nil, settingsRaw, fallback, productCatalog...)
}

func mergeModelListWithInstalled(served, installed []string, settingsRaw map[string]any, fallback string, productCatalog ...[]modelmgr.ModelInfo) []modelEntry {
	out := []modelEntry{}
	seen := map[string]bool{}
	key := func(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
	installedSet := map[string]bool{}
	for _, id := range installed {
		if info, ok := modelmgr.ModelByName(id); ok {
			id = info.ServedName
		}
		if k := key(id); k != "" {
			installedSet[k] = true
		}
	}
	isInstalled := func(id string) bool {
		if info, ok := modelmgr.ModelByName(id); ok {
			id = info.ServedName
		}
		return installedSet[key(id)] || modelmgr.ResolveModelPath(id) != ""
	}
	dup := func(ss ...string) bool {
		for _, s := range ss {
			if k := key(s); k != "" && seen[k] {
				return true
			}
		}
		return false
	}
	mark := func(ss ...string) {
		for _, s := range ss {
			if k := key(s); k != "" {
				seen[k] = true
			}
		}
	}

	providers, _ := settingsRaw["modelProviders"].(map[string]any)
	catalogMeta := func(id string) (string, string) { return catalogNameDesc(providers, id) }

	for _, id := range served {
		if key(id) == "" || dup(id) {
			continue
		}
		nm, ds := catalogMeta(id)
		if info, ok := modelmgr.ModelByName(id); ok {
			nm = info.ServedName
		}
		out = append(out, modelEntry{Name: nm, ModelID: id, Desc: ds, Active: len(out) == 0, Live: true, Installed: true})
		mark(id, nm)
	}
	if len(out) == 0 && fallback != "" {
		nm, ds := catalogMeta(fallback)
		if info, ok := modelmgr.ModelByName(fallback); ok {
			nm = info.ServedName
		}
		out = append(out, modelEntry{Name: nm, ModelID: fallback, Desc: ds, Active: true, Installed: isInstalled(fallback)})
		mark(fallback, nm)
	}
	names := make([]string, 0, len(providers))
	for n := range providers {
		names = append(names, n)
	}
	sort.Strings(names) // map order is random; keep shortcut row order stable
	for _, n := range names {
		p, ok := providers[n].(map[string]any)
		if !ok {
			continue
		}
		models, _ := p["models"].([]any)
		for _, mv := range models {
			m, ok := mv.(map[string]any)
			if !ok {
				continue
			}
			name, _ := m["name"].(string)
			id, _ := m["modelId"].(string)
			desc, _ := m["description"].(string)
			if id == "" || dup(id, name) {
				continue
			}
			out = append(out, modelEntry{Name: name, ModelID: id, Desc: desc, Installed: isInstalled(id)})
			mark(id, name)
		}
	}
	// Meadow Core owns the authoritative installed inventory. An edition may
	// ship a model that is intentionally absent from the legacy settings or
	// compile-time catalog (for example the skull 30B profile); installed
	// models must still become selectable rows rather than only acting as
	// presence markers for catalog entries.
	for _, id := range installed {
		if key(id) == "" || dup(id) {
			continue
		}
		name, desc := catalogMeta(id)
		if info, ok := modelmgr.ModelByName(id); ok {
			name = info.ServedName
		}
		if strings.TrimSpace(name) == "" {
			name = id
		}
		if strings.TrimSpace(desc) == "" {
			desc = "本機已安裝"
		}
		out = append(out, modelEntry{
			Name:      name,
			ModelID:   id,
			Desc:      desc,
			Installed: true,
		})
		mark(id, name)
	}
	// The current release catalog is code-owned. Legacy settings.json often
	// lists only one selected model; relying on it alone silently hid other
	// Meadow product tiers. Keep all compatible product rows visible even when
	// their dedicated HG pipeline is still being adapted.
	for _, catalog := range productCatalog {
		for _, info := range catalog {
			if canonical, ok := modelmgr.ModelByName(info.ServedName); ok {
				info.ServedName = canonical.ServedName
			}
			if info.ServedName == "" || dup(info.ServedName) {
				continue
			}
			present := isInstalled(info.ServedName)
			desc := "尚未安裝"
			if present {
				desc = "本機已安裝"
			}
			out = append(out, modelEntry{
				Name:      info.ServedName,
				ModelID:   info.ServedName,
				Desc:      desc,
				Installed: present,
			})
			mark(info.ServedName)
		}
	}
	for i := range out {
		info, ok := modelmgr.ModelByName(out[i].Name)
		if !ok {
			info, ok = modelmgr.ModelByName(out[i].ModelID)
		}
		if !ok {
			continue
		}
		profile, profileOK := provider.ProfileForModel(out[i].Name)
		if !profileOK {
			profile, profileOK = provider.ProfileForModel(out[i].ModelID)
		}
		if profileOK {
			out[i].Multimodal = profile.Multimodal
		}
		out[i].RecommendedMemoryGiB = info.MinRAMGiB
		if position, positioned := modelmgr.ProductPositionFor(info.ServedName); positioned {
			out[i].ProductRole = string(position.Role)
			out[i].RoleLabel = position.Label
			out[i].RoleDescription = position.CoreTasks
			out[i].ProductionReady = position.Ready
			if position.Ready {
				out[i].HarnessStatus = "ready"
			} else {
				out[i].HarnessStatus = "adapting"
			}
		}
		present := out[i].Live || out[i].Installed || isInstalled(info.ServedName)
		out[i].Installed = present
		if out[i].Desc == "" || out[i].Desc == "尚未安裝" {
			if present {
				out[i].Desc = "本機已安裝"
			} else {
				out[i].Desc = "尚未安裝"
			}
		}
	}
	// Product order is capacity-first, independent of which model happens to
	// be live or which order settings.json used. Active/live state remains on
	// its original row; only presentation and ⌘1-5 shortcuts are normalized.
	modelRank := func(entry modelEntry) int {
		name := strings.ToLower(entry.Name + " " + entry.ModelID)
		switch {
		case strings.Contains(name, "lightning") || strings.Contains(name, "lfm2"):
			return 3
		case strings.Contains(name, "12b"):
			return 12
		case strings.Contains(name, "26b"):
			return 26
		case strings.Contains(name, "27b") || strings.Contains(name, "qwen3.8"):
			return 27
		case strings.Contains(name, "30b") || strings.Contains(name, "muse") || strings.Contains(name, "nemotron"):
			return 30
		case strings.Contains(name, "35b") || strings.Contains(name, "meadow q"):
			return 35
		default:
			return 1000
		}
	}
	// Meadow Core's installed inventory is the edition allowlist, not merely a
	// presence hint. Once it is available, hide retired rows restored from old
	// settings or the compatibility catalog. This removes 26B from the shipped
	// picker without deleting its on-disk weights or legacy session metadata.
	if len(installedSet) > 0 {
		allowed := out[:0]
		for _, entry := range out {
			id := entry.ModelID
			if info, ok := modelmgr.ModelByName(id); ok {
				id = info.ServedName
			}
			if installedSet[key(id)] {
				allowed = append(allowed, entry)
			}
		}
		out = allowed
		hasActive := false
		for _, entry := range out {
			hasActive = hasActive || entry.Active
		}
		if !hasActive && len(out) > 0 {
			out[0].Active = true
		}
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("MEADOW_MODEL_EDITION")), "skull") {
		skull := make([]modelEntry, 0, 1)
		has35 := false
		for _, entry := range out {
			switch modelRank(entry) {
			case 35:
				entry.Name = edition.SkullModelDisplayName
				entry.Desc = "Huihui Ornith 1.5 35B A3B · Abliterated · MLX Q4"
				skull = append(skull, entry)
				has35 = true
			}
		}
		if !has35 {
			skull = append(skull, modelEntry{
				Name:       edition.SkullModelDisplayName,
				ModelID:    "Meadow Coder 35B",
				Desc:       "Huihui Ornith 1.5 35B A3B · Abliterated · MLX Q4",
				Installed:  isInstalled("Meadow Coder 35B"),
				Multimodal: true,
			})
		}
		out = skull
	}
	sort.SliceStable(out, func(i, j int) bool { return modelRank(out[i]) < modelRank(out[j]) })
	return out
}

// catalogNameDesc resolves a model id (filesystem path or served name) to
// its settings-catalog display name and description. Returns (id, "") when
// nothing matches — callers never surface a raw path as a label, and the
// spawn path uses the name as --served-model-name so a freshly loaded
// model serves under its brand name instead of the manager's default.
func catalogNameDesc(providers map[string]any, id string) (string, string) {
	k := func(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
	for _, pv := range providers {
		p, ok := pv.(map[string]any)
		if !ok {
			continue
		}
		models, _ := p["models"].([]any)
		for _, mv := range models {
			m, ok := mv.(map[string]any)
			if !ok {
				continue
			}
			name, _ := m["name"].(string)
			mid, _ := m["modelId"].(string)
			desc, _ := m["description"].(string)
			if name != "" && (k(mid) == k(id) || k(name) == k(id)) {
				return name, desc
			}
		}
	}
	return id, ""
}
