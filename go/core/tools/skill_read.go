package tools

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/skills"
)

const SkillReadToolName = "skill_read"

type SkillRead struct {
	catalog *skills.Catalog
}

// registerBundledSkills captures the ablation switch when the base registry is
// built. Later requests and derived registries never reread process environment.
func registerBundledSkills(registry *Registry) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MEADOW_BUNDLED_SKILLS"))) {
	case "0", "false", "off", "no":
		return
	}
	registry.Register(&SkillRead{catalog: skills.Bundled()})
}

func copyBundledSkills(registry, base *Registry) {
	if base == nil {
		return
	}
	if tool, ok := base.Get(SkillReadToolName); ok {
		registry.Register(tool)
	}
}

// BundledSkillsInstruction follows the actual tool registry, including revoked
// capabilities. It cannot advertise a skill loader unavailable to this chat.
func BundledSkillsInstruction(registry *Registry) string {
	if registry != nil {
		if tool, ok := registry.Get(SkillReadToolName); ok {
			if loader, ok := tool.(*SkillRead); ok {
				return loader.catalog.Instruction()
			}
		}
	}
	return ""
}

func (*SkillRead) Name() string { return SkillReadToolName }
func (*SkillRead) Description() string {
	return "Read one optional bundled skill chosen from the system catalog. Returns its bounded Markdown guidance; does not execute instructions or select a workflow. Use only an exact catalog name."
}
func (*SkillRead) Kind() Kind                       { return KindReadOnly }
func (*SkillRead) ParallelSafe(map[string]any) bool { return true }
func (*SkillRead) SubtaskSafe() bool                { return true }
func (*SkillRead) CompleteObservation() bool        { return true }
func (t *SkillRead) Schema() map[string]any {
	names := make([]string, 0)
	for _, entry := range t.catalog.Descriptors() {
		names = append(names, entry.Name)
	}
	schema := obj(map[string]any{"name": map[string]any{"type": "string", "enum": names,
		"description": "Exact bundled skill name from the system catalog."}}, "name")
	schema["additionalProperties"] = false
	return schema
}
func (t *SkillRead) Execute(ctx context.Context, args map[string]any) Result {
	if err := ctx.Err(); err != nil {
		return errResult(err)
	}
	for key := range args {
		if key != "name" {
			return errResultf("skill_read accepts only the name argument")
		}
	}
	name, ok := args["name"].(string)
	if !ok {
		return errResultf("skill_read requires an exact catalog name")
	}
	body, ok := t.catalog.Read(name)
	if !ok {
		return errResultf("Unknown bundled skill; use an exact name from the system catalog")
	}
	payload, _ := json.Marshal(map[string]any{"name": name,
		"source": "bundled:" + name + "/SKILL.md", "content": body})
	return Result{LLMContent: string(payload), Display: "已讀取技能：" + name}
}
