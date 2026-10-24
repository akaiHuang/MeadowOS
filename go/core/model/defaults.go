package model

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// modelTier is a selectable default model: the served-name id the rest of the
// system recognizes (provider registry, desktop display), the on-disk directory
// basename the weights live in, the Hugging Face repo `meadow pull` fetches it
// from, an approximate download size, and the RAM it wants.
type modelTier struct {
	ServedName    string
	ServedAliases []string
	DirName       string
	DirAliases    []string
	HFRepo        string
	HFRevision    string
	ApproxGB      int
	MinRAMGiB     int64
}

var (
	tier9B  = modelTier{ServedName: "Meadow 9B", DirName: "qwythos-9b-1m-mxfp4-mlx", DirAliases: []string{"Qwythos-9B-Claude-Mythos-5-1M-mxfp4-mlx", "qwythos-9b-mxfp4", "qwythos-9b-claude-mythos-5-1m-mlx-mxfp4"}, HFRepo: "sahilchachra/Qwythos-9B-Claude-Mythos-5-1M-mxfp4-mlx", ApproxGB: 5, MinRAMGiB: 16}
	tier26B = modelTier{
		ServedName: "Meadow 26B",
		DirName:    "gemma-4-26B-A4B-it-qat-mxfp4",
		DirAliases: []string{
			"gemma-4-26b-a4b-qat-mlx-mxfp4-dequant",
			"gemma-4-26b-a4b-qat-mxfp4",
			"gemma-4-26b-a4b-qat-mlx",
			"gemma-4-26b-a4b-qat-4bit-mlx",
		},
		HFRepo:    "mlx-community/gemma-4-26B-A4B-it-qat-mxfp4",
		ApproxGB:  15,
		MinRAMGiB: 32,
	}
	tier12B = modelTier{
		ServedName: "Meadow 12B",
		DirName:    "gemma-4-12b-it-qat-4bit-mlx-mxfp4",
		DirAliases: []string{"gemma-4-12b-it-qat-mxfp4", "gemma-4-12b-it-qat-4bit-mlx", "gemma-4-12b-it-qat-mlx"},
		HFRepo:     "mlx-community/gemma-4-12B-it-qat-mxfp4",
		ApproxGB:   6,
		MinRAMGiB:  16,
	}
	tier27B = modelTier{
		ServedName: "Meadow 27B",
		ServedAliases: []string{
			"Qwen3.8-27B",
		},
		DirName: "qwen3.8-27b-4bit-mlx",
		DirAliases: []string{
			"qwen3.8-27b-4bit",
			// Retain old development paths as resolvable aliases without
			// presenting Qwen3.6 as the current public product.
			"qwen3.6-27b-4bit-mlx",
			"qwen3.6-27b-obliterated-mlx-4bit",
		},
		HFRepo:     "mlx-community/Qwen3.8-27B-4bit",
		HFRevision: "3e6447f082e89cc7f0bc6e5441afd38dfce760ff",
		ApproxGB:   16,
		MinRAMGiB:  24,
	}
	tier35B = modelTier{
		ServedName: "Meadow Coder 35B",
		ServedAliases: []string{
			"Meadow 35B",
			"Meadow Q",
			"Ornith Coding Agent",
			"Ornith-1.0-35B",
			"Ornith-1.5-35B-A3B",
		},
		DirName: "meadow-coder-ornith-1.5-35b-mxfp4",
		DirAliases: []string{
			"meadow-coder-ornith-35b-mlx-4bit",
			"Ornith-1.0-35B-4bit",
			"ornith-1.0-35b-4bit",
		},
		HFRepo:     "ornith-ai/Ornith-1.5-35B-A3B-MLX",
		HFRevision: "1713baf2e38984a3c557195caf167759144d1d7e",
		ApproxGB:   21,
		MinRAMGiB:  32,
	}
	tierMuse30B = modelTier{
		ServedName:    "Meadow 30B",
		ServedAliases: []string{"Nemotron 3 Nano Omni 30B-A3B", "Nemotron-3-Nano-Omni-30B-A3B"},
		DirName:       "divinetribe-nemotron-3-nano-omni-30b-abliterated-mm-4bit-c4d2b9ba",
		DirAliases:    []string{"Nemotron-3-Nano-Omni-30B-Abliterated-MM-4bit", "nemotron-3-nano-omni-30b-abliterated-mm-4bit"},
		HFRepo:        "divinetribe/Nemotron-3-Nano-Omni-30B-Abliterated-MM-4bit",
		HFRevision:    "c4d2b9ba1545b77de2e01ab29107f3658e0500ab",
		ApproxGB:      20,
		MinRAMGiB:     32,
	}
	tierLightning = modelTier{
		ServedName:    "Meadow Lightning⚡️",
		ServedAliases: []string{"Meadow Lightning", "LFM2.5 2.6B", "LFM2.5-2.6B"},
		DirName:       "meadow-lightning-lfm25-2.6b-mlx-4bit",
		HFRepo:        "LiquidAI/LFM2.5-2.6B-MLX-4bit",
		HFRevision:    "bb8e02bf31d8b9ddd9682c9d1a0f993a6acacee6",
		ApproxGB:      2,
		MinRAMGiB:     8,
	}

	// releaseTiers is the public Meadow product catalog, ordered from the
	// smallest local footprint to the largest. The retired 9B/12B tiers remain
	// resolvable for existing sessions but are not product rows.
	releaseTiers = []modelTier{tier26B, tierMuse30B, tier35B}
	knownTiers   = []modelTier{tierLightning, tier9B, tier12B, tier26B, tier27B, tierMuse30B, tier35B}
)

// ModelInfo is the public, surface-agnostic description of a downloadable model.
type ModelInfo struct {
	ServedName string // "Meadow 12B"
	DirName    string // on-disk directory basename
	HFRepo     string // Hugging Face repo id
	HFRevision string // immutable Hugging Face revision; empty means main
	ApproxGB   int    // approximate download size
	MinRAMGiB  int64  // recommended minimum RAM
}

func (t modelTier) info() ModelInfo {
	return ModelInfo{ServedName: t.ServedName, DirName: t.DirName, HFRepo: t.HFRepo, HFRevision: t.HFRevision, ApproxGB: t.ApproxGB, MinRAMGiB: t.MinRAMGiB}
}

func (t modelTier) dirNames() []string {
	names := []string{t.DirName}
	names = append(names, t.DirAliases...)
	return names
}

func (t modelTier) dirNamesForBoost(boost bool) []string {
	names := t.dirNames()
	if boost {
		return names
	}
	standard := make([]string, 0, len(names))
	boostOnly := make([]string, 0, len(names))
	for _, name := range names {
		if strings.Contains(strings.ToLower(name), "mxfp4") {
			boostOnly = append(boostOnly, name)
			continue
		}
		standard = append(standard, name)
	}
	if len(standard) == 0 {
		return names
	}
	return append(standard, boostOnly...)
}

func tierByName(name string) (modelTier, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return modelTier{}, false
	}
	for _, t := range knownTiers {
		if n == strings.ToLower(t.ServedName) ||
			n == strings.ToLower(t.DirName) ||
			n == strings.ToLower(t.HFRepo) {
			return t, true
		}
		for _, alias := range t.DirAliases {
			if n == strings.ToLower(alias) {
				return t, true
			}
		}
		for _, alias := range t.ServedAliases {
			if n == strings.ToLower(alias) {
				return t, true
			}
		}
	}
	if strings.Contains(n, "lightning") || strings.Contains(n, "lfm2.5") || strings.Contains(n, "lfm-2.5") {
		return tierLightning, true
	}
	if strings.Contains(n, "muse") || strings.Contains(n, "glimmer") || strings.Contains(n, "nemotron") {
		return tierMuse30B, true
	}
	if hasModelSizeHint(n, "35") {
		return tier35B, true
	}
	if hasModelSizeHint(n, "27") {
		return tier27B, true
	}
	if hasModelSizeHint(n, "26") {
		return tier26B, true
	}
	if hasModelSizeHint(n, "12") {
		return tier12B, true
	}
	if hasModelSizeHint(n, "9") || strings.Contains(n, "qwythos") || strings.Contains(n, "claude-mythos") {
		return tier9B, true
	}
	return modelTier{}, false
}

func hasModelSizeHint(name, size string) bool {
	if name == size || name == size+"b" {
		return true
	}
	for _, sep := range []string{" ", "-", "_", ".", "/"} {
		if strings.Contains(name, sep+size+"b") || strings.Contains(name, size+"b"+sep) ||
			strings.Contains(name, sep+size+sep) {
			return true
		}
	}
	return false
}

// KnownModels returns the ordered Meadow Code product catalog.
func KnownModels() []ModelInfo {
	out := make([]ModelInfo, 0, len(releaseTiers))
	for _, t := range releaseTiers {
		out = append(out, t.info())
	}
	return out
}

// ModelByName resolves a model id to its ModelInfo, tolerating the served name
// ("Meadow 12B", case-insensitive), a bare size hint ("12b" / "26"), or the
// underlying model directory / HF repo id.
func ModelByName(name string) (ModelInfo, bool) {
	t, ok := tierByName(name)
	if !ok {
		return ModelInfo{}, false
	}
	return t.info(), true
}

// TotalRAMBytes returns the machine's physical memory in bytes via sysctl, or 0
// when it cannot be read (treated as "small" by the caller — the safe choice).
// Uses the absolute /usr/sbin/sysctl: a Finder-launched GUI app inherits a
// minimal PATH that omits /usr/sbin, so a bare "sysctl" would fail and the RAM
// would mis-read as 0 (forcing 12B on every machine).
func TotalRAMBytes() int64 {
	if n, ok := envRAMBytes(); ok {
		return n
	}
	bin := "/usr/sbin/sysctl"
	if _, err := os.Stat(bin); err != nil {
		bin = "sysctl" // fall back to PATH lookup
	}
	out, err := exec.Command(bin, "-n", "hw.memsize").Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return n
}

// DefaultModelTierForRAM returns the RAM-only fallback recommendation. It is
// intentionally conservative: chip-aware selection lives in DefaultModelTier().
func DefaultModelTierForRAM(bytes int64) string {
	return RecommendedModelForMachine(MachineProfileFor(bytes, ""))
}

// DefaultModelTier picks the default served-name id from the machine's RAM and
// Apple Silicon class.
func DefaultModelTier() string { return RecommendedModelForCurrentMachine() }

func allowedFallbackTiers(servedName string) []string {
	if allowUnsupportedModelDownloads() {
		return fallbackTiers(servedName)
	}
	allowed := allowedTierSet(CurrentMachineProfile())
	out := make([]string, 0, len(knownTiers))
	for _, name := range fallbackTiers(servedName) {
		if allowed[name] {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return []string{tierLightning.ServedName}
	}
	return out
}

// DefaultModelID resolves the model a fresh run should use: the RAM-tier model
// when its weights are present, else the OTHER tier when that is the one
// actually downloaded, else the RAM-tier id with no weights on disk (the
// preflight then guides the user to download it).
func DefaultModelID() string {
	prefer := DefaultModelTier()
	for _, name := range allowedFallbackTiers(prefer) {
		if ResolveModelPath(name) != "" {
			return name
		}
	}
	return prefer
}

func fallbackTiers(servedName string) []string {
	if tier, ok := tierByName(servedName); ok {
		servedName = tier.ServedName
	}
	switch servedName {
	case tierMuse30B.ServedName:
		return []string{tierMuse30B.ServedName, tier35B.ServedName, tier26B.ServedName, tierLightning.ServedName, tier27B.ServedName, tier12B.ServedName, tier9B.ServedName}
	case tierLightning.ServedName:
		return []string{tierLightning.ServedName, tier26B.ServedName, tierMuse30B.ServedName, tier35B.ServedName, tier27B.ServedName, tier12B.ServedName, tier9B.ServedName}
	case tier9B.ServedName:
		return []string{tierLightning.ServedName, tier26B.ServedName, tierMuse30B.ServedName, tier35B.ServedName, tier27B.ServedName, tier12B.ServedName, tier9B.ServedName}
	case tier12B.ServedName:
		return []string{tier26B.ServedName, tierLightning.ServedName, tierMuse30B.ServedName, tier35B.ServedName, tier27B.ServedName, tier12B.ServedName, tier9B.ServedName}
	case tier27B.ServedName:
		return []string{tier26B.ServedName, tierLightning.ServedName, tierMuse30B.ServedName, tier35B.ServedName, tier27B.ServedName, tier12B.ServedName, tier9B.ServedName}
	case tier26B.ServedName:
		return []string{tier26B.ServedName, tierLightning.ServedName, tierMuse30B.ServedName, tier35B.ServedName, tier27B.ServedName, tier12B.ServedName, tier9B.ServedName}
	case tier35B.ServedName:
		return []string{tier35B.ServedName, tierMuse30B.ServedName, tier26B.ServedName, tierLightning.ServedName, tier27B.ServedName, tier12B.ServedName, tier9B.ServedName}
	default:
		return []string{tierLightning.ServedName, tier26B.ServedName, tierMuse30B.ServedName, tier35B.ServedName, tier27B.ServedName, tier12B.ServedName, tier9B.ServedName}
	}
}

// modelRoots are the conventional directories that hold downloaded model
// folders, in search order: an explicit MEADOW_MODEL_DIR, then the
// `meadow pull` target (~/.meadow/models). The historical ~/Desktop/LLM
// development layout is opt-in: probing a protected macOS folder at product
// startup causes a TCC permission prompt even when the bundled model is used.
func modelRoots() []string {
	var roots []string
	if r := strings.TrimSpace(os.Getenv("MEADOW_MODEL_DIR")); r != "" {
		roots = append(roots, expandHome(r))
	}
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots, filepath.Join(home, ".meadow", "models"))
		if legacyDesktopDiscoveryEnabled() {
			roots = append(roots, filepath.Join(home, "Desktop", "LLM"))
		}
	}
	return roots
}

func legacyDesktopDiscoveryEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MEADOW_ALLOW_DESKTOP_DISCOVERY"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// DefaultPullDir is where `meadow pull` writes downloaded models: an explicit
// MEADOW_MODEL_DIR if set, else ~/.meadow/models (app-managed, not cluttering
// the Desktop).
func DefaultPullDir() string {
	if r := strings.TrimSpace(os.Getenv("MEADOW_MODEL_DIR")); r != "" {
		return expandHome(r)
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".meadow", "models")
	}
	return filepath.Join(".meadow", "models")
}

// ModelPresent reports whether a directory holds usable weights: it exists and
// contains config.json plus at least one .safetensors shard.
func ModelPresent(dir string) bool {
	if dir == "" {
		return false
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		return false
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*.safetensors")); len(m) > 0 {
		return true
	}
	return false
}

// ResolveModelPath maps a default served-name id ("Meadow 26B"/"Meadow 12B") to
// an on-disk model directory under the conventional roots, or "" when neither
// the id is a known default nor the weights are present.
func ResolveModelPath(servedName string) string {
	return ResolveModelPathVariant(servedName, false)
}

// ResolveModelPathVariant resolves the best local directory for the runtime
// tier: standard prefers non-MXFP4 Q4 folders, Boost prefers MXFP4 folders.
// Both variants fall back to any compatible alias so a partial local install
// still starts instead of failing only because the preferred flavor is absent.
func ResolveModelPathVariant(servedName string, boost bool) string {
	tier, ok := tierByName(servedName)
	if !ok {
		return ""
	}
	for _, root := range modelRoots() {
		for _, dirName := range tier.dirNamesForBoost(boost) {
			if cand := filepath.Join(root, dirName); ModelPresent(cand) {
				return cand
			}
		}
	}
	return ""
}

func ResolveBoostModelPath(servedName string) string {
	return ResolveModelPathVariant(servedName, true)
}

// DefaultModelDirName returns the on-disk directory basename a default
// served-name id expects, for download-guidance messages ("" if not a default).
func DefaultModelDirName(servedName string) string {
	if info, ok := ModelByName(servedName); ok {
		return info.DirName
	}
	return ""
}
