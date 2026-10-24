package provider

import (
	"fmt"
	"regexp"
	"strings"
)

// ProviderTab is the configuration surface used by both Meadow CLI and
// Meadow Desktop.
type ProviderTab string

const (
	ProviderTabKeys     ProviderTab = "keys"
	ProviderTabAccounts ProviderTab = "accounts"
)

var (
	providerSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	providerEnvPattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// ProviderDescriptor is the native, value-safe provider catalog contract.
// It intentionally contains credential variable names, never credential
// values.
type ProviderDescriptor struct {
	Slug          string      `json:"slug"`
	Label         string      `json:"label"`
	Description   string      `json:"description"`
	AuthType      string      `json:"auth_type"`
	Tab           ProviderTab `json:"tab"`
	APIKeyEnvVars []string    `json:"api_key_env_vars"`
	BaseURLEnvVar string      `json:"base_url_env_var"`
	SignupURL     string      `json:"signup_url"`
	Order         int         `json:"order"`
}

// TabForAuthType maps account-based authentication to the Accounts surface.
// All other schemes, including AWS/Vertex SDK configuration and virtual
// providers, remain on the Keys/configuration surface.
func TabForAuthType(authType string) ProviderTab {
	switch strings.TrimSpace(authType) {
	case "oauth_device_code", "oauth_external", "oauth_minimax",
		"external_process", "copilot":
		return ProviderTabAccounts
	default:
		return ProviderTabKeys
	}
}

// ProviderCatalog returns an independent copy of the native built-in provider
// universe in the same order on every surface.
func ProviderCatalog() []ProviderDescriptor {
	out := make([]ProviderDescriptor, len(builtinProviderCatalog))
	for index, entry := range builtinProviderCatalog {
		out[index] = cloneProviderDescriptor(entry)
	}
	return out
}

// ProviderCatalogBySlug returns independent descriptor copies keyed by
// canonical slug.
func ProviderCatalogBySlug() map[string]ProviderDescriptor {
	out := make(map[string]ProviderDescriptor, len(builtinProviderCatalog))
	for _, entry := range builtinProviderCatalog {
		out[entry.Slug] = cloneProviderDescriptor(entry)
	}
	return out
}

// LookupProvider returns one independent descriptor copy.
func LookupProvider(slug string) (ProviderDescriptor, bool) {
	slug = strings.TrimSpace(slug)
	for _, entry := range builtinProviderCatalog {
		if entry.Slug == slug {
			return cloneProviderDescriptor(entry), true
		}
	}
	return ProviderDescriptor{}, false
}

func cloneProviderDescriptor(entry ProviderDescriptor) ProviderDescriptor {
	entry.APIKeyEnvVars = append([]string(nil), entry.APIKeyEnvVars...)
	if entry.APIKeyEnvVars == nil {
		entry.APIKeyEnvVars = []string{}
	}
	return entry
}

// ValidateProviderCatalog checks the cross-surface catalog invariants.
func ValidateProviderCatalog(entries []ProviderDescriptor) error {
	if len(entries) == 0 {
		return fmt.Errorf("provider catalog is empty")
	}
	seen := make(map[string]struct{}, len(entries))
	for index, entry := range entries {
		if entry.Order != index {
			return fmt.Errorf(
				"provider %q order is %d, want %d",
				entry.Slug,
				entry.Order,
				index,
			)
		}
		if !providerSlugPattern.MatchString(entry.Slug) {
			return fmt.Errorf("provider %q has an invalid slug", entry.Slug)
		}
		if _, exists := seen[entry.Slug]; exists {
			return fmt.Errorf("provider slug %q is duplicated", entry.Slug)
		}
		seen[entry.Slug] = struct{}{}
		if strings.TrimSpace(entry.Label) == "" {
			return fmt.Errorf("provider %q has an empty label", entry.Slug)
		}
		if strings.TrimSpace(entry.Description) == "" {
			return fmt.Errorf("provider %q has an empty description", entry.Slug)
		}
		if strings.TrimSpace(entry.AuthType) == "" {
			return fmt.Errorf("provider %q has an empty auth type", entry.Slug)
		}
		if entry.Tab != ProviderTabKeys && entry.Tab != ProviderTabAccounts {
			return fmt.Errorf("provider %q has invalid tab %q", entry.Slug, entry.Tab)
		}
		if want := TabForAuthType(entry.AuthType); entry.Tab != want {
			return fmt.Errorf(
				"provider %q tab is %q, want %q for auth type %q",
				entry.Slug,
				entry.Tab,
				want,
				entry.AuthType,
			)
		}
		for _, name := range entry.APIKeyEnvVars {
			if !providerEnvPattern.MatchString(name) {
				return fmt.Errorf(
					"provider %q has invalid credential environment name %q",
					entry.Slug,
					name,
				)
			}
		}
		if entry.BaseURLEnvVar != "" &&
			!providerEnvPattern.MatchString(entry.BaseURLEnvVar) {
			return fmt.Errorf(
				"provider %q has invalid base URL environment name %q",
				entry.Slug,
				entry.BaseURLEnvVar,
			)
		}
	}
	return nil
}

var builtinProviderCatalog = []ProviderDescriptor{
	{Slug: "nous", Label: "Nous Research", Description: "Nous Research — Meadow model family", AuthType: "oauth_device_code", Tab: ProviderTabAccounts, APIKeyEnvVars: []string{"NOUS_API_KEY"}, SignupURL: "https://nousresearch.com/", Order: 0},
	{Slug: "fireworks", Label: "Fireworks AI", Description: "Fireworks AI — OpenAI-compatible direct model API", AuthType: "api_key", Tab: ProviderTabKeys, APIKeyEnvVars: []string{"FIREWORKS_API_KEY"}, SignupURL: "https://app.fireworks.ai/settings/users/api-keys", Order: 1},
	{Slug: "openrouter", Label: "OpenRouter", Description: "OpenRouter — unified API for 200+ models", AuthType: "api_key", Tab: ProviderTabKeys, APIKeyEnvVars: []string{"OPENROUTER_API_KEY"}, SignupURL: "https://openrouter.ai/keys", Order: 2},
	{Slug: "moa", Label: "Mixture of Agents", Description: "Mixture of Agents (named presets; aggregator acts after reference models)", AuthType: "virtual", Tab: ProviderTabKeys, APIKeyEnvVars: []string{}, Order: 3},
	{Slug: "novita", Label: "NovitaAI", Description: "NovitaAI — AI-native cloud for builders and agents", AuthType: "api_key", Tab: ProviderTabKeys, APIKeyEnvVars: []string{"NOVITA_API_KEY"}, BaseURLEnvVar: "NOVITA_BASE_URL", SignupURL: "https://novita.ai/settings/key-management", Order: 4},
	{Slug: "lmstudio", Label: "LM Studio", Description: "LM Studio (Local desktop app with built-in model server)", AuthType: "api_key", Tab: ProviderTabKeys, APIKeyEnvVars: []string{"LM_API_KEY"}, BaseURLEnvVar: "LM_BASE_URL", Order: 5},
	{Slug: "anthropic", Label: "Anthropic", Description: "Anthropic (Claude models via API key or Claude Code)", AuthType: "api_key", Tab: ProviderTabKeys, APIKeyEnvVars: []string{"ANTHROPIC_API_KEY", "ANTHROPIC_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"}, BaseURLEnvVar: "ANTHROPIC_BASE_URL", Order: 6},
	{Slug: "openai-codex", Label: "OpenAI Codex", Description: "OpenAI Codex (Codex CLI via ChatGPT subscription or API key)", AuthType: "oauth_external", Tab: ProviderTabAccounts, APIKeyEnvVars: []string{}, Order: 7},
	{Slug: "openai-api", Label: "OpenAI API", Description: "OpenAI API (api.openai.com, API key)", AuthType: "api_key", Tab: ProviderTabKeys, APIKeyEnvVars: []string{"OPENAI_API_KEY"}, BaseURLEnvVar: "OPENAI_BASE_URL", Order: 8},
	{Slug: "alibaba", Label: "Qwen Cloud", Description: "Qwen Cloud / DashScope (Qwen + multi-provider)", AuthType: "api_key", Tab: ProviderTabKeys, APIKeyEnvVars: []string{"DASHSCOPE_API_KEY"}, BaseURLEnvVar: "DASHSCOPE_BASE_URL", SignupURL: "https://modelstudio.console.alibabacloud.com/", Order: 9},
	{Slug: "xai-oauth", Label: "xAI Grok OAuth (SuperGrok / Premium+)", Description: "xAI Grok OAuth (SuperGrok / Premium+ subscription)", AuthType: "oauth_external", Tab: ProviderTabAccounts, APIKeyEnvVars: []string{}, Order: 10},
	{Slug: "xiaomi", Label: "Xiaomi MiMo", Description: "Xiaomi MiMo (MiMo-V2.5 and V2 models: pro, omni, flash)", AuthType: "api_key", Tab: ProviderTabKeys, APIKeyEnvVars: []string{"XIAOMI_API_KEY"}, BaseURLEnvVar: "XIAOMI_BASE_URL", SignupURL: "https://platform.xiaomimimo.com", Order: 11},
	{Slug: "tencent-tokenhub", Label: "Tencent TokenHub", Description: "Tencent TokenHub (Hy3 Preview via tokenhub.tencentmaas.com)", AuthType: "api_key", Tab: ProviderTabKeys, APIKeyEnvVars: []string{"TOKENHUB_API_KEY"}, BaseURLEnvVar: "TOKENHUB_BASE_URL", Order: 12},
	{Slug: "nvidia", Label: "NVIDIA NIM", Description: "NVIDIA NIM — accelerated inference", AuthType: "api_key", Tab: ProviderTabKeys, APIKeyEnvVars: []string{"NVIDIA_API_KEY"}, BaseURLEnvVar: "NVIDIA_BASE_URL", SignupURL: "https://build.nvidia.com/", Order: 13},
	{Slug: "copilot", Label: "GitHub Copilot", Description: "GitHub Copilot (Uses GITHUB_TOKEN or gh auth token)", AuthType: "api_key", Tab: ProviderTabKeys, APIKeyEnvVars: []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"}, BaseURLEnvVar: "COPILOT_API_BASE_URL", Order: 14},
	{Slug: "copilot-acp", Label: "GitHub Copilot ACP", Description: "GitHub Copilot ACP (Spawns copilot --acp --stdio)", AuthType: "external_process", Tab: ProviderTabAccounts, APIKeyEnvVars: []string{}, Order: 15},
	{Slug: "huggingface", Label: "HuggingFace", Description: "HuggingFace Inference API", AuthType: "api_key", Tab: ProviderTabKeys, APIKeyEnvVars: []string{"HF_TOKEN"}, BaseURLEnvVar: "HF_BASE_URL", SignupURL: "https://huggingface.co/settings/tokens", Order: 16},
	{Slug: "gemini", Label: "Google AI Studio", Description: "Google AI Studio (Native Gemini API)", AuthType: "api_key", Tab: ProviderTabKeys, APIKeyEnvVars: []string{"GOOGLE_API_KEY", "GEMINI_API_KEY"}, BaseURLEnvVar: "GEMINI_BASE_URL", SignupURL: "https://aistudio.google.com/app/apikey", Order: 17},
	{Slug: "vertex", Label: "Google Vertex AI", Description: "Google Vertex AI (Gemini via GCP; OAuth2 service account or ADC, GCP billing/quotas)", AuthType: "vertex", Tab: ProviderTabKeys, APIKeyEnvVars: []string{}, Order: 18},
	{Slug: "deepseek", Label: "DeepSeek", Description: "DeepSeek — native DeepSeek API", AuthType: "api_key", Tab: ProviderTabKeys, APIKeyEnvVars: []string{"DEEPSEEK_API_KEY"}, BaseURLEnvVar: "DEEPSEEK_BASE_URL", SignupURL: "https://platform.deepseek.com/", Order: 19},
	{Slug: "xai", Label: "xAI", Description: "xAI Grok (Direct API)", AuthType: "api_key", Tab: ProviderTabKeys, APIKeyEnvVars: []string{"XAI_API_KEY"}, BaseURLEnvVar: "XAI_BASE_URL", SignupURL: "https://console.x.ai/", Order: 20},
	{Slug: "zai", Label: "Z.AI (GLM)", Description: "Z.AI / GLM — Zhipu AI models", AuthType: "api_key", Tab: ProviderTabKeys, APIKeyEnvVars: []string{"GLM_API_KEY", "ZAI_API_KEY", "Z_AI_API_KEY"}, BaseURLEnvVar: "GLM_BASE_URL", SignupURL: "https://z.ai/", Order: 21},
	{Slug: "kimi-coding", Label: "Kimi / Kimi Coding Plan", Description: "Kimi Coding Plan (api.kimi.com & Moonshot API)", AuthType: "api_key", Tab: ProviderTabKeys, APIKeyEnvVars: []string{"KIMI_API_KEY", "KIMI_CODING_API_KEY"}, BaseURLEnvVar: "KIMI_BASE_URL", SignupURL: "https://platform.moonshot.cn/", Order: 22},
	{Slug: "kimi-coding-cn", Label: "Kimi / Moonshot (China)", Description: "Kimi / Moonshot China (Domestic direct API)", AuthType: "api_key", Tab: ProviderTabKeys, APIKeyEnvVars: []string{"KIMI_CN_API_KEY"}, SignupURL: "https://platform.moonshot.cn/", Order: 23},
	{Slug: "stepfun", Label: "StepFun Step Plan", Description: "StepFun Step Plan (Agent / coding models via Step Plan API)", AuthType: "api_key", Tab: ProviderTabKeys, APIKeyEnvVars: []string{"STEPFUN_API_KEY"}, BaseURLEnvVar: "STEPFUN_BASE_URL", SignupURL: "https://platform.stepfun.com/", Order: 24},
	{Slug: "minimax", Label: "MiniMax", Description: "MiniMax (Global direct API)", AuthType: "api_key", Tab: ProviderTabKeys, APIKeyEnvVars: []string{"MINIMAX_API_KEY"}, BaseURLEnvVar: "MINIMAX_BASE_URL", SignupURL: "https://www.minimax.io/", Order: 25},
	{Slug: "minimax-oauth", Label: "MiniMax (OAuth)", Description: "MiniMax via OAuth browser flow — no API key required", AuthType: "oauth_minimax", Tab: ProviderTabAccounts, APIKeyEnvVars: []string{}, SignupURL: "https://api.minimax.io/", Order: 26},
	{Slug: "minimax-cn", Label: "MiniMax (China)", Description: "MiniMax China (Domestic direct API)", AuthType: "api_key", Tab: ProviderTabKeys, APIKeyEnvVars: []string{"MINIMAX_CN_API_KEY"}, BaseURLEnvVar: "MINIMAX_CN_BASE_URL", SignupURL: "https://www.minimaxi.com/", Order: 27},
	{Slug: "ollama-cloud", Label: "Ollama Cloud", Description: "Ollama Cloud (Cloud-hosted open models, ollama.com)", AuthType: "api_key", Tab: ProviderTabKeys, APIKeyEnvVars: []string{"OLLAMA_API_KEY"}, BaseURLEnvVar: "OLLAMA_BASE_URL", SignupURL: "https://ollama.com/settings", Order: 28},
	{Slug: "arcee", Label: "Arcee AI", Description: "Arcee AI (Trinity models, direct API)", AuthType: "api_key", Tab: ProviderTabKeys, APIKeyEnvVars: []string{"ARCEEAI_API_KEY"}, BaseURLEnvVar: "ARCEE_BASE_URL", SignupURL: "https://chat.arcee.ai/", Order: 29},
	{Slug: "gmi", Label: "GMI Cloud", Description: "GMI Cloud — multi-model direct API (slash-form model IDs)", AuthType: "api_key", Tab: ProviderTabKeys, APIKeyEnvVars: []string{"GMI_API_KEY"}, BaseURLEnvVar: "GMI_BASE_URL", SignupURL: "https://www.gmicloud.ai/", Order: 30},
	{Slug: "kilocode", Label: "Kilo Code", Description: "Kilo Code (Kilo Gateway API)", AuthType: "api_key", Tab: ProviderTabKeys, APIKeyEnvVars: []string{"KILOCODE_API_KEY"}, BaseURLEnvVar: "KILOCODE_BASE_URL", Order: 31},
	{Slug: "opencode-zen", Label: "OpenCode Zen", Description: "OpenCode Zen (Curated models, pay-as-you-go)", AuthType: "api_key", Tab: ProviderTabKeys, APIKeyEnvVars: []string{"OPENCODE_ZEN_API_KEY"}, BaseURLEnvVar: "OPENCODE_ZEN_BASE_URL", SignupURL: "https://opencode.ai/auth", Order: 32},
	{Slug: "opencode-go", Label: "OpenCode Go", Description: "OpenCode Go (Open models subscription)", AuthType: "api_key", Tab: ProviderTabKeys, APIKeyEnvVars: []string{"OPENCODE_GO_API_KEY"}, BaseURLEnvVar: "OPENCODE_GO_BASE_URL", SignupURL: "https://opencode.ai/auth", Order: 33},
	{Slug: "bedrock", Label: "AWS Bedrock", Description: "AWS Bedrock (Claude, Nova, Llama, DeepSeek; IAM or API key)", AuthType: "aws_sdk", Tab: ProviderTabKeys, APIKeyEnvVars: []string{}, Order: 34},
	{Slug: "azure-foundry", Label: "Azure Foundry", Description: "Microsoft Foundry - OpenAI-compatible endpoint (user-supplied base URL)", AuthType: "api_key", Tab: ProviderTabKeys, APIKeyEnvVars: []string{"AZURE_FOUNDRY_API_KEY"}, BaseURLEnvVar: "AZURE_FOUNDRY_BASE_URL", SignupURL: "https://ai.azure.com/", Order: 35},
	{Slug: "qwen-oauth", Label: "Qwen OAuth (Portal)", Description: "Qwen OAuth (Reuses local Qwen CLI login)", AuthType: "oauth_external", Tab: ProviderTabAccounts, APIKeyEnvVars: []string{"QWEN_API_KEY"}, Order: 36},
	{Slug: "alibaba-coding-plan", Label: "Alibaba Cloud (Coding Plan)", Description: "Alibaba Cloud Coding Plan (Dedicated coding tier)", AuthType: "api_key", Tab: ProviderTabKeys, APIKeyEnvVars: []string{"ALIBABA_CODING_PLAN_API_KEY", "DASHSCOPE_API_KEY"}, BaseURLEnvVar: "ALIBABA_CODING_PLAN_BASE_URL", SignupURL: "https://help.aliyun.com/zh/model-studio/", Order: 37},
	{Slug: "custom", Label: "custom", Description: "custom (direct API)", AuthType: "api_key", Tab: ProviderTabKeys, APIKeyEnvVars: []string{}, Order: 38},
	{Slug: "deepinfra", Label: "DeepInfra", Description: "DeepInfra — 100+ open models, pay-per-use", AuthType: "api_key", Tab: ProviderTabKeys, APIKeyEnvVars: []string{"DEEPINFRA_API_KEY"}, BaseURLEnvVar: "DEEPINFRA_BASE_URL", SignupURL: "https://deepinfra.com/dash/api_keys", Order: 39},
	{Slug: "upstage", Label: "Upstage Solar", Description: "Upstage (Solar API)", AuthType: "api_key", Tab: ProviderTabKeys, APIKeyEnvVars: []string{"UPSTAGE_API_KEY"}, BaseURLEnvVar: "UPSTAGE_BASE_URL", SignupURL: "https://console.upstage.ai/api-keys", Order: 40},
}
