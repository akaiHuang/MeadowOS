package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const AskUserToolName = "ask_user_question"

// Question matches the TS ask_user_question schema.
type Question struct {
	Question     string           `json:"question"`
	Header       string           `json:"header"`
	Options      []QuestionOption `json:"options"`
	MultiSelect  bool             `json:"multiSelect"`
	AllowOther   bool             `json:"allowOther"`
	Presentation string           `json:"presentation,omitempty"`
}

type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// Asker presents questions to the user and returns one answer per question.
// A nil Asker means non-interactive mode.
type Asker func(ctx context.Context, questions []Question) ([]string, error)

// AskUser is the ask_user_question tool. The interaction itself is owned by
// the frontend via the Asker callback; headless runs answer with a
// best-judgment instruction instead of blocking.
type AskUser struct {
	Ask Asker
}

func (t *AskUser) Name() string { return AskUserToolName }
func (t *AskUser) Description() string {
	return "Ask the user one or more multiple-choice questions, optionally allowing a custom answer. Use this when the user explicitly requests a questionnaire or structured interview."
}
func (t *AskUser) Kind() Kind { return KindInteract }

func (t *AskUser) Schema() map[string]any {
	return obj(map[string]any{
		"questions": map[string]any{
			"type":        "array",
			"description": "1-4 questions, each with 2-4 options.",
			"items": obj(map[string]any{
				"question": str("The question text."),
				"header":   str("Short label, max 12 chars."),
				"options": map[string]any{
					"type": "array",
					"items": obj(map[string]any{
						"label":       str("Display text for the choice."),
						"description": str("What this option means."),
					}, "label", "description"),
				},
				"multiSelect": boolean("Allow selecting multiple options."),
				"allowOther":  boolean("Show an 'Other' free-text field for answers outside the listed options."),
			}, "question", "header", "options"),
		},
	}, "questions")
}

func (t *AskUser) Execute(ctx context.Context, args map[string]any) Result {
	raw, ok := args["questions"]
	if !ok {
		return errResultf("questions parameter is required")
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return errResult(err)
	}
	var questions []Question
	if err := json.Unmarshal(encoded, &questions); err != nil {
		return errResultf("questions must be an array: %v", err)
	}
	if len(questions) == 0 || len(questions) > 4 {
		return errResultf("provide 1-4 questions")
	}

	if t.Ask == nil {
		return Result{
			LLMContent: "Non-interactive session: the user cannot answer questions. Proceed with your best judgment and state the assumption you made.",
			Display:    "skipped (non-interactive)",
		}
	}

	answers, err := t.Ask(ctx, questions)
	if err != nil {
		return Result{
			LLMContent: "User declined to answer the questions.",
			Display:    "declined",
		}
	}
	var b strings.Builder
	b.WriteString("User has provided the following answers:\n")
	for i, q := range questions {
		answer := ""
		if i < len(answers) {
			answer = answers[i]
		}
		fmt.Fprintf(&b, "\n**%s**: %s", q.Header, answer)
	}
	out := b.String()
	return Result{LLMContent: out, Display: out}
}
