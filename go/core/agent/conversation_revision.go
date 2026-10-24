package agent

import (
	"strings"
	"unicode"

	"github.com/hey-meadow/meadow-code/go/core/provider"
)

// ConversationRevision captures an implicit user correction. Repeating a
// question, adding constraints to it, or explicitly rejecting the last answer
// means the next turn must review the prior answer instead of treating the text
// as an unrelated first attempt.
type ConversationRevision struct {
	Detected      bool
	Reason        string
	Attempt       int
	PriorQuestion string
	PriorAnswer   string
}

func DetectConversationRevision(task string, history []provider.Message) ConversationRevision {
	task = strings.TrimSpace(task)
	if task == "" || len(history) == 0 {
		return ConversationRevision{}
	}
	normalizedTask := normalizeConversationRevisionText(task)
	priorQuestion, priorAnswer, normalizedPrior := closestConversationExchange(normalizedTask, history)
	reason := ""
	switch {
	case normalizedTask != "" && normalizedTask == normalizedPrior && priorAnswer != "":
		reason = "repeated_question"
	case explicitConversationDissatisfaction(task):
		priorQuestion, priorAnswer = latestConversationExchange(history)
		if priorQuestion == "" || priorAnswer == "" {
			return ConversationRevision{}
		}
		reason = "explicit_dissatisfaction"
	case priorAnswer != "" && conversationTextSimilarity(normalizedTask, normalizedPrior) >= 0.55:
		reason = "strengthened_question"
	default:
		return ConversationRevision{}
	}

	attempt := 1
	for _, message := range history {
		if message.Role != "user" {
			continue
		}
		candidate := normalizeConversationRevisionText(provider.MessageText(message))
		if candidate == normalizedTask || conversationTextSimilarity(candidate, normalizedTask) >= 0.55 {
			attempt++
		}
	}
	return ConversationRevision{
		Detected: true, Reason: reason, Attempt: attempt,
		PriorQuestion: priorQuestion, PriorAnswer: priorAnswer,
	}
}

func closestConversationExchange(normalizedTask string, history []provider.Message) (string, string, string) {
	bestScore := 0.0
	bestQuestion, bestAnswer, bestNormalized := "", "", ""
	for userIndex := len(history) - 1; userIndex >= 0; userIndex-- {
		if history[userIndex].Role != "user" {
			continue
		}
		question := strings.TrimSpace(provider.MessageText(history[userIndex]))
		normalizedQuestion := normalizeConversationRevisionText(question)
		if normalizedQuestion == "" {
			continue
		}
		answer := ""
		for answerIndex := userIndex + 1; answerIndex < len(history); answerIndex++ {
			if history[answerIndex].Role == "user" {
				break
			}
			if history[answerIndex].Role == "assistant" && strings.TrimSpace(provider.MessageText(history[answerIndex])) != "" {
				answer = strings.TrimSpace(provider.MessageText(history[answerIndex]))
				break
			}
		}
		if answer == "" {
			continue
		}
		score := conversationTextSimilarity(normalizedTask, normalizedQuestion)
		if normalizedTask == normalizedQuestion {
			score = 1
		}
		if score > bestScore {
			bestScore, bestQuestion, bestAnswer, bestNormalized = score, question, answer, normalizedQuestion
			if score == 1 {
				break
			}
		}
	}
	return bestQuestion, bestAnswer, bestNormalized
}

func (r ConversationRevision) Guidance() string {
	if !r.Detected {
		return ""
	}
	if r.Reason == "repeated_question" {
		return "USER_REVISION_SIGNAL: the user asked the same question again, which means the previous answer was not satisfactory. Do not guess another lightly paraphrased answer. Re-read the previous answer, name its most likely limitation, and either (a) correct it using a substantively different interpretation or evidence path when the error is identifiable, or (b) ask exactly one targeted clarification question that separates the likely interpretations. Do not repeat the old recommendation list."
	}
	return "USER_REVISION_SIGNAL: the user repeated, strengthened, or rejected a previous request, so the prior answer was not satisfactory. Re-read the latest user wording and the previous answer. Identify the likely mismatch before answering. Do not repeat the old answer. Change at least one substantive part of the approach: interpretation, decomposition, evidence, source, calculation, or format. If the old answer is likely wrong, correct it explicitly. If one consequential detail remains unknowable, ask exactly one precise clarification question."
}

func conversationResponseRepeatsPrior(candidate, prior string) bool {
	candidate = normalizeConversationRevisionText(candidate)
	prior = normalizeConversationRevisionText(prior)
	if candidate == "" || prior == "" {
		return false
	}
	if candidate == prior {
		return true
	}
	if !conversationResponseAcknowledgesRevision(candidate) && conversationNGramContainment(candidate, prior, 2) >= 0.48 {
		// On an explicit revision turn, retaining roughly half of the previous
		// answer's phrase structure without acknowledging or diagnosing the prior
		// miss is a repeated strategy even when greetings and examples changed.
		return true
	}
	shorter, longer := len([]rune(candidate)), len([]rune(prior))
	if shorter > longer {
		shorter, longer = longer, shorter
	}
	if longer == 0 || float64(shorter)/float64(longer) < 0.72 {
		return false
	}
	return conversationTextSimilarity(candidate, prior) >= 0.86
}

func conversationResponseAcknowledgesRevision(normalized string) bool {
	for _, marker := range []string{
		"上一答", "前一個回答", "前一个回答", "之前的回覆問題", "之前的回复问题",
		"我誤解", "我误解", "我沒抓到", "我没抓到", "過於泛泛", "过于泛泛",
		"重新檢查後", "重新检查后", "previousanswer", "earlieranswer", "misunderstood",
	} {
		if strings.Contains(normalized, normalizeConversationRevisionText(marker)) {
			return true
		}
	}
	return false
}

func conversationNGramContainment(left, right string, size int) float64 {
	leftSet := conversationRuneNGrams(left, size)
	rightSet := conversationRuneNGrams(right, size)
	if len(leftSet) == 0 || len(rightSet) == 0 {
		return 0
	}
	intersection := 0
	for value := range leftSet {
		if rightSet[value] {
			intersection++
		}
	}
	denominator := len(leftSet)
	if len(rightSet) < denominator {
		denominator = len(rightSet)
	}
	return float64(intersection) / float64(denominator)
}

func latestConversationExchange(history []provider.Message) (string, string) {
	answerIndex := -1
	answer := ""
	for index := len(history) - 1; index >= 0; index-- {
		if history[index].Role == "assistant" && strings.TrimSpace(provider.MessageText(history[index])) != "" {
			answerIndex = index
			answer = strings.TrimSpace(provider.MessageText(history[index]))
			break
		}
	}
	if answerIndex < 0 {
		return "", ""
	}
	for index := answerIndex - 1; index >= 0; index-- {
		if history[index].Role == "user" && strings.TrimSpace(provider.MessageText(history[index])) != "" {
			return strings.TrimSpace(provider.MessageText(history[index])), answer
		}
	}
	return "", ""
}

func explicitConversationDissatisfaction(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	for _, marker := range []string{
		"不對", "不对", "不是這個", "不是这个", "你又", "還是錯", "还是错",
		"還是沒有", "还是没有", "我再問一次", "我再问一次", "重新回答",
		"再看一下", "再想一下", "沒有回答", "没有回答", "答非所問", "答非所问",
		"that's not", "that is not", "wrong answer", "try again", "answer again",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func normalizeConversationRevisionText(text string) string {
	var builder strings.Builder
	for _, r := range strings.ToLower(text) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

func conversationTextSimilarity(left, right string) float64 {
	if left == "" || right == "" {
		return 0
	}
	if left == right {
		return 1
	}
	leftSet := conversationRuneNGrams(left, 2)
	rightSet := conversationRuneNGrams(right, 2)
	if len(leftSet) == 0 || len(rightSet) == 0 {
		if strings.Contains(left, right) || strings.Contains(right, left) {
			return 1
		}
		return 0
	}
	intersection := 0
	union := make(map[string]bool, len(leftSet)+len(rightSet))
	for value := range leftSet {
		union[value] = true
		if rightSet[value] {
			intersection++
		}
	}
	for value := range rightSet {
		union[value] = true
	}
	return float64(intersection) / float64(len(union))
}

func conversationRuneNGrams(text string, size int) map[string]bool {
	runes := []rune(text)
	result := make(map[string]bool)
	if len(runes) < size {
		return result
	}
	for index := 0; index+size <= len(runes); index++ {
		result[string(runes[index:index+size])] = true
	}
	return result
}
