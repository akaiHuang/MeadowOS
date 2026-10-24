package cli

import (
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hey-meadow/meadow-code/go/cli/theme"
)

func renderAskUser(
	request askUserRequestMsg,
	questionIndex, cursor int,
	selected map[int]bool,
	styles theme.Styles,
	width int,
) string {
	if questionIndex < 0 || questionIndex >= len(request.questions) {
		return ""
	}
	question := request.questions[questionIndex]
	var builder strings.Builder
	header := strings.TrimSpace(question.Header)
	if header == "" {
		header = "需要你的決定"
	}
	fmt.Fprintf(&builder, "%s  %s\n", styles.Warn.Render("?"), styles.Secondary.Render(header))
	builder.WriteString(styles.Bright.Render(question.Question) + "\n\n")
	for index, option := range question.Options {
		mark := "  "
		if question.MultiSelect && selected[index] {
			mark = "✓ "
		}
		line := mark + option.Label
		if index == cursor {
			line = styles.Selected.Render("▸ " + option.Label)
		}
		builder.WriteString(line + "\n")
		if strings.TrimSpace(option.Description) != "" {
			builder.WriteString(styles.Dim.Render("    "+option.Description) + "\n")
		}
	}
	hint := "↑↓ 選擇 · Enter 確定 · Esc 暫不回答"
	if question.MultiSelect {
		hint = "↑↓ 選擇 · Space 勾選 · Enter 確定 · Esc 暫不回答"
	}
	if len(request.questions) > 1 {
		hint += fmt.Sprintf("  (%d/%d)", questionIndex+1, len(request.questions))
	}
	builder.WriteString("\n" + styles.Dim.Render(hint))
	return styles.ToolBox.Width(min(width-2, 100)).Render(builder.String())
}

func (a *App) handleAskUserKey(message tea.KeyMsg) (tea.Model, tea.Cmd) {
	if a.pendingAsk == nil || a.askQuestion >= len(a.pendingAsk.questions) {
		a.state = stateRunning
		return a, nil
	}
	question := a.pendingAsk.questions[a.askQuestion]
	switch message.String() {
	case "up", "k":
		if a.askCursor > 0 {
			a.askCursor--
		}
	case "down", "j":
		if a.askCursor < len(question.Options)-1 {
			a.askCursor++
		}
	case " ":
		if question.MultiSelect && len(question.Options) > 0 {
			a.askSelected[a.askCursor] = !a.askSelected[a.askCursor]
		}
	case "esc", "ctrl+c":
		a.pendingAsk.resp <- askUserResponse{err: errors.New("user declined to answer")}
		a.clearAskUser()
		return a, nil
	case "enter":
		if len(question.Options) == 0 {
			return a, nil
		}
		answer := question.Options[a.askCursor].Label
		if question.MultiSelect {
			chosen := make([]string, 0, len(a.askSelected))
			for index, option := range question.Options {
				if a.askSelected[index] {
					chosen = append(chosen, option.Label)
				}
			}
			if len(chosen) > 0 {
				answer = strings.Join(chosen, ", ")
			}
		}
		a.askAnswers = append(a.askAnswers, answer)
		a.askQuestion++
		a.askCursor = 0
		a.askSelected = map[int]bool{}
		if a.askQuestion >= len(a.pendingAsk.questions) {
			a.pendingAsk.resp <- askUserResponse{answers: append([]string(nil), a.askAnswers...)}
			a.clearAskUser()
			return a, nil
		}
	default:
		return a, nil
	}
	a.layout()
	return a, nil
}

func (a *App) clearAskUser() {
	a.pendingAsk = nil
	a.askQuestion = 0
	a.askCursor = 0
	a.askSelected = nil
	a.askAnswers = nil
	a.state = stateRunning
	a.layout()
}
