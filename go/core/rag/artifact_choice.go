package rag

import (
	"context"
	"errors"
	"sync"

	"github.com/hey-meadow/meadow-code/go/core/tools"
)

const (
	ArtifactChoiceQuestion = "發現有範例，要複製還是要重新開發"
	ArtifactChoiceCopy     = "複製範例"
	ArtifactChoiceDevelop  = "重新開發"
)

// A fresh choice belongs to one foreground run. Workspace-bound subtools share
// it, while subsequent runs and other conversations must ask independently.
// It is separate from tool auto-approval: only an explicit answer grants source
// access, including a direct read/copy using a handle from older history.
type artifactChoice struct {
	mu       sync.Mutex
	ask      tools.Asker
	pending  chan struct{}
	decision string
}

func RegistryWithArtifactChoice(base *tools.Registry, ask tools.Asker) *tools.Registry {
	if base == nil {
		return base
	}
	registry := base.Select(base.Names())
	choice := &artifactChoice{ask: ask}
	for _, name := range []string{"artifact_search", "artifact_read", "artifact_copy"} {
		if tool, ok := base.Get(name); ok {
			if artifact, ok := tool.(*ArtifactTool); ok {
				bound := *artifact
				bound.choice = choice
				registry.Register(&bound)
			}
		}
	}
	return registry
}

func (c *artifactChoice) choose(ctx context.Context) (string, error) {
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		c.mu.Lock()
		if c.decision != "" {
			decision := c.decision
			c.mu.Unlock()
			return decision, nil
		}
		if waiting := c.pending; waiting != nil {
			c.mu.Unlock()
			select {
			case <-waiting:
				continue
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		if c.ask == nil {
			c.mu.Unlock()
			return "", errors.New("artifact_choice_required: source reuse needs an interactive user choice")
		}
		c.pending = make(chan struct{})
		c.mu.Unlock()
		answers, err := c.ask(ctx, []tools.Question{{
			Header: "範例選擇", Question: ArtifactChoiceQuestion, Presentation: "choice_dialog",
			Options: []tools.QuestionOption{
				{Label: ArtifactChoiceCopy, Description: "以範例為基礎，依照你的需求修改。"},
				{Label: ArtifactChoiceDevelop, Description: "不使用範例原始碼，依照你的需求重新製作。"},
			},
		}})
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		decision := ""
		if err == nil && len(answers) == 1 && (answers[0] == ArtifactChoiceCopy || answers[0] == ArtifactChoiceDevelop) {
			decision = answers[0]
		} else if err == nil {
			err = errors.New("artifact_choice_required: no valid user choice received; do not reuse source")
		}
		c.mu.Lock()
		c.decision = decision
		close(c.pending)
		c.pending = nil
		c.mu.Unlock()
		return decision, err
	}
}

func artifactChoiceGuidance(decision string) string {
	if decision == ArtifactChoiceDevelop {
		return "The user chose to develop independently for this task. Do not read, copy, or reconstruct the curated source. Implement from the user's requirements with the ordinary tools. Do not ask the same reuse question again this run."
	}
	return "The user chose to copy a suitable example for this task. Select relevant sources, inspect interfaces and license, preserve companion files, adapt to the user's requirements, and verify the delivered behavior. The choice does not prove that any example fits or works."
}
