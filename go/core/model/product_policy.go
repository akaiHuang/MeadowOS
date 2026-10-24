package model

import (
	"fmt"
	"strings"

	coreauth "github.com/hey-meadow/meadow-code/go/core/productauth"
)

// ProductRuntimeClass is the product-facing execution boundary. It belongs in
// the shared Go backend so CLI and Desktop cannot disagree about whether a
// model may mutate the workspace or execute external actions.
type ProductRuntimeClass string

const (
	ProductRuntimeGeneralAgent ProductRuntimeClass = "general-agent"
	ProductRuntimeOfficeAgent  ProductRuntimeClass = "office-agent"
	ProductRuntimeCodingAgent  ProductRuntimeClass = "coding-agent"
	ProductRuntimeChatPlan     ProductRuntimeClass = "chat-plan"
	ProductRuntimeTemplate     ProductRuntimeClass = "restricted-template"
	ProductRuntimeResearch     ProductRuntimeClass = "research-rag"
	ProductRuntimeLegacy       ProductRuntimeClass = "legacy"
)

// ProductRole is the stable product identity shown by Desktop and recorded in
// release manifests. It is deliberately more specific than "agent": the 30B
// white-collar worker and Ornith coding worker must never silently share a
// prompt, router, or tool surface just because both can edit files.
type ProductRole string

const (
	ProductRoleResearchWorker   ProductRole = "research-worker"
	ProductRoleGeneralAgent     ProductRole = "general-agent"
	ProductRoleWhiteCollarAgent ProductRole = "white-collar-agent"
	ProductRoleCodingAgent      ProductRole = "coding-agent"
	ProductRoleChatPlan         ProductRole = "chat-plan"
)

// ProductPosition is the single backend-owned description of what a public
// model is for. Frontends may translate presentation, but must not infer a
// model's capabilities from parameter count or its display name.
type ProductPosition struct {
	Role      ProductRole
	Label     string
	CoreTasks string
	Ready     bool
	Runtime   ProductRuntimeClass
}

func ProductPositionFor(modelID string) (ProductPosition, bool) {
	tier, ok := tierByName(modelID)
	if !ok {
		return ProductPosition{}, false
	}
	switch tier.ServedName {
	case tierLightning.ServedName:
		return ProductPosition{
			Role: ProductRoleResearchWorker, Label: "Research Worker",
			CoreTasks: "搜尋意圖、Meadow Browser 收集、證據抽取、RAG 與短摘要",
			Ready:     true, Runtime: ProductRuntimeResearch,
		}, true
	case tierMuse30B.ServedName:
		return ProductPosition{
			Role: ProductRoleWhiteCollarAgent, Label: "白領主力 Agent",
			CoreTasks: "辦公任務、文件、研究整合、長流程與自我迭代",
			Ready:     true, Runtime: ProductRuntimeOfficeAgent,
		}, true
	case tier27B.ServedName:
		return ProductPosition{
			Role: ProductRoleCodingAgent, Label: "Agent Coding",
			CoreTasks: "讀取 codebase、多檔修改、執行編譯與測試、依診斷迭代修正",
			Ready:     false, Runtime: ProductRuntimeCodingAgent,
		}, true
	case tier35B.ServedName:
		return ProductPosition{
			Role: ProductRoleCodingAgent, Label: "Ornith Coding Agent",
			CoreTasks: "讀 codebase、多檔修改、執行測試、除錯直到驗收",
			Ready:     true, Runtime: ProductRuntimeCodingAgent,
		}, true
	case tier26B.ServedName:
		return ProductPosition{
			Role: ProductRoleGeneralAgent, Label: "本機通用 Agent",
			CoreTasks: "本機任務執行、文件與程式製作、工具調用與驗證",
			Ready:     true, Runtime: ProductRuntimeGeneralAgent,
		}, true
	case tier12B.ServedName:
		return ProductPosition{
			Role: ProductRoleChatPlan, Label: "Chat / Plan",
			CoreTasks: "聊天與任務規劃；專屬 Agent pipeline 尚未完成",
			Ready:     false, Runtime: ProductRuntimeChatPlan,
		}, true
	default:
		return ProductPosition{}, false
	}
}

// ProductRuntimeClassFor resolves aliases before assigning the product class.
// Unknown and legacy development models keep their historical behavior; only
// public product rows receive a fail-closed product boundary.
func ProductRuntimeClassFor(modelID string) ProductRuntimeClass {
	if position, ok := ProductPositionFor(modelID); ok {
		return position.Runtime
	}
	return ProductRuntimeLegacy
}

// AllowsGeneralTaskExecution is true only for full Agent products and legacy
// development models. Chat/Plan and Research Worker products must fail closed
// on edit, execute, and destructive tools.
func AllowsGeneralTaskExecution(modelID string) bool {
	class := ProductRuntimeClassFor(modelID)
	return class == ProductRuntimeGeneralAgent || class == ProductRuntimeOfficeAgent || class == ProductRuntimeCodingAgent || class == ProductRuntimeLegacy
}

func ExecutionBlockedMessage(modelID string) string {
	info, ok := ModelByName(modelID)
	name := modelID
	if ok {
		name = info.ServedName
	}
	switch ProductRuntimeClassFor(modelID) {
	case ProductRuntimeChatPlan:
		return fmt.Sprintf("ERROR: %s is a Chat/Plan product and cannot modify files, run commands, or execute external actions. Read what is needed, then answer or present a plan.", name)
	case ProductRuntimeTemplate:
		return fmt.Sprintf("ERROR: %s cannot use the general Agent tool surface. Use its restricted DOM-to-template-to-PDF pipeline.", name)
	case ProductRuntimeResearch:
		return fmt.Sprintf("ERROR: %s is a research/RAG worker. It may search, open, extract, and cite evidence, but cannot use the general coding or workspace mutation surface.", name)
	default:
		return "ERROR: this product does not allow the requested tool execution."
	}
}

// ProductAccessRequirement is a Desktop preflight only. Agentd remains the
// authoritative admission point and must independently validate the effective
// model and lease before starting or continuing a runtime.
type ProductAccessRequirement struct {
	Admission    string
	Capabilities []string
}

// ProductAccessRequirementForModel maps public model aliases onto the closed
// product-auth vocabulary. Unknown, legacy, and uncatalogued product rows fail
// closed here; public development behavior is unchanged unless a caller opts
// into this protected-product check.
func ProductAccessRequirementForModel(modelID string) (ProductAccessRequirement, bool) {
	if !IsCurrentProductModel(modelID) {
		return ProductAccessRequirement{}, false
	}
	// The deployed v1 entitlement vocabulary is authoritative and closed.
	// model.12b is its base-model seat; the current product maps that seat to
	// the sole shipped 35B row without rewriting signed capabilities.
	return ProductAccessRequirement{Admission: coreauth.AdmissionGeneral, Capabilities: []string{coreauth.CapabilityModel12B}}, true
}

// IsCurrentProductModel accepts only explicit identifiers shipped for the
// single current Desktop model. It deliberately avoids tierByName's fuzzy size
// fallback so an unknown string containing "35B" cannot cross an auth boundary.
func IsCurrentProductModel(modelID string) bool {
	want := strings.ToLower(strings.TrimSpace(modelID))
	if want == "" {
		return false
	}
	candidates := []string{tier35B.ServedName, tier35B.DirName, tier35B.HFRepo}
	candidates = append(candidates, tier35B.ServedAliases...)
	candidates = append(candidates, tier35B.DirAliases...)
	for _, candidate := range candidates {
		if want == strings.ToLower(strings.TrimSpace(candidate)) {
			return true
		}
	}
	return false
}

func AllowsProductModel(status coreauth.Status, modelID string) bool {
	if status.Validate() != nil {
		return false
	}
	requirement, ok := ProductAccessRequirementForModel(modelID)
	if !ok {
		return false
	}
	if requirement.Admission == coreauth.AdmissionPro && (status.State != coreauth.StateAuthorizedPro || status.Plan != coreauth.PlanPro) {
		return false
	}
	if requirement.Admission == coreauth.AdmissionGeneral && !status.Authenticated {
		return false
	}
	for _, capability := range requirement.Capabilities {
		if !status.Capabilities.Enabled(capability) {
			return false
		}
	}
	return true
}
