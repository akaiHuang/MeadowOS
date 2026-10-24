// Package skills exposes only the small curated skill set embedded in this
// binary. It never scans a workspace, home directory, or external Codex skills.
package skills

import (
	"embed"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	MaxSkillBytes       = 8 * 1024
	maxSkillCount       = 8
	maxDescriptionBytes = 240
)

//go:embed */SKILL.md
var bundledFiles embed.FS

type Descriptor struct {
	Name        string `json:"name" yaml:"name"`
	Description string `json:"description" yaml:"description"`
}

// Catalog's contents are private and immutable after construction. Reading a
// skill creates no global "loaded" state and cannot influence another chat.
type Catalog struct {
	entries []Descriptor
	bodies  map[string]string
}

var bundledCatalog = mustBundledCatalog()

func mustBundledCatalog() *Catalog {
	catalog, err := loadCatalog(bundledFiles)
	if err != nil {
		panic("invalid bundled skill catalog: " + err.Error())
	}
	return catalog
}

func Bundled() *Catalog { return bundledCatalog }

func (c *Catalog) Descriptors() []Descriptor {
	if c == nil {
		return nil
	}
	return append([]Descriptor(nil), c.entries...)
}

func (c *Catalog) Read(name string) (string, bool) {
	if c == nil {
		return "", false
	}
	body, ok := c.bodies[name]
	return body, ok
}

// Instruction includes discovery metadata only. Full Markdown enters a chat
// exclusively through that model's explicit skill_read call.
func (c *Catalog) Instruction() string {
	if c == nil || len(c.entries) == 0 {
		return ""
	}
	var out strings.Builder
	out.WriteString("\n\nBundled skills (optional): choose whether a skill is useful for the current task. Call skill_read with its exact name to read it. Apply only relevant guidance; the user's requirements and available tool contracts govern.\n")
	for _, entry := range c.entries {
		fmt.Fprintf(&out, "- %s: %s\n", entry.Name, entry.Description)
	}
	return out.String()
}

func loadCatalog(source fs.FS) (*Catalog, error) {
	paths, err := fs.Glob(source, "*/SKILL.md")
	if err != nil || len(paths) == 0 || len(paths) > maxSkillCount {
		return nil, fmt.Errorf("expected one to %d embedded skills", maxSkillCount)
	}
	catalog := &Catalog{bodies: make(map[string]string, len(paths))}
	for _, path := range paths {
		data, err := fs.ReadFile(source, path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		if len(data) > MaxSkillBytes || !utf8.Valid(data) {
			return nil, fmt.Errorf("%s must be UTF-8 and at most %d bytes", path, MaxSkillBytes)
		}
		text := strings.ReplaceAll(string(data), "\r\n", "\n")
		if !strings.HasPrefix(text, "---\n") {
			return nil, fmt.Errorf("%s is missing YAML frontmatter", path)
		}
		frontmatter, body, ok := strings.Cut(text[4:], "\n---\n")
		if !ok || strings.TrimSpace(body) == "" {
			return nil, fmt.Errorf("%s requires frontmatter and a Markdown body", path)
		}
		var descriptor Descriptor
		decoder := yaml.NewDecoder(strings.NewReader(frontmatter))
		decoder.KnownFields(true)
		if err := decoder.Decode(&descriptor); err != nil {
			return nil, fmt.Errorf("invalid %s frontmatter: %w", path, err)
		}
		if decoder.Decode(new(any)) != io.EOF {
			return nil, fmt.Errorf("%s must contain one frontmatter document", path)
		}
		name := strings.TrimSuffix(path, "/SKILL.md")
		if descriptor.Name != name || !validName(name) {
			return nil, fmt.Errorf("%s name must equal its catalog directory ID", path)
		}
		descriptor.Description = strings.TrimSpace(descriptor.Description)
		if descriptor.Description == "" || len(descriptor.Description) > maxDescriptionBytes || strings.ContainsAny(descriptor.Description, "\n\r\x00") {
			return nil, fmt.Errorf("%s requires a short one-line description", path)
		}
		catalog.entries = append(catalog.entries, descriptor)
		catalog.bodies[name] = text
	}
	return catalog, nil
}

func validName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for _, char := range name {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
			return false
		}
	}
	return true
}
