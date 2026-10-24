package cli

import (
	"strconv"
	"strings"
)

func (a *App) transcriptStreaming() string {
	base := strings.TrimRight(a.streaming, "\n")
	if a.draftArguments == "" {
		return base
	}
	name := strings.TrimSpace(a.draftName)
	if name == "" {
		name = "tool_call"
	}
	header := "工具參數草稿 · " + name + "（尚未執行）"
	body := visibleToolDraft(name, a.draftArguments)
	if base == "" {
		return header + "\n\n" + body
	}
	return base + "\n\n" + header + "\n\n" + body
}

func visibleToolDraft(name, raw string) string {
	for _, field := range []string{"content", "new_string", "patch"} {
		if decoded := partialToolJSONStringField(raw, field); decoded != "" {
			return decoded
		}
		if decoded := partialTaggedToolParameter(raw, field); decoded != "" {
			return decoded
		}
	}
	return raw
}

func partialToolJSONStringField(source, field string) string {
	key := strconv.Quote(field)
	at := strings.Index(source, key)
	if at < 0 {
		return ""
	}
	at += len(key)
	for at < len(source) && isToolDraftSpace(source[at]) {
		at++
	}
	if at >= len(source) || source[at] != ':' {
		return ""
	}
	at++
	for at < len(source) && isToolDraftSpace(source[at]) {
		at++
	}
	if at >= len(source) || source[at] != '"' {
		return ""
	}
	at++
	var output strings.Builder
	for at < len(source) {
		char := source[at]
		at++
		if char == '"' {
			break
		}
		if char != '\\' {
			output.WriteByte(char)
			continue
		}
		if at >= len(source) {
			break
		}
		escaped := source[at]
		at++
		switch escaped {
		case '"', '\\', '/':
			output.WriteByte(escaped)
		case 'b':
			output.WriteByte('\b')
		case 'f':
			output.WriteByte('\f')
		case 'n':
			output.WriteByte('\n')
		case 'r':
			output.WriteByte('\r')
		case 't':
			output.WriteByte('\t')
		case 'u':
			if at+4 > len(source) {
				return output.String()
			}
			value, err := strconv.ParseUint(source[at:at+4], 16, 16)
			if err != nil {
				return output.String()
			}
			output.WriteRune(rune(value))
			at += 4
		default:
			output.WriteByte(escaped)
		}
	}
	return output.String()
}

func partialTaggedToolParameter(source, field string) string {
	marker := "<parameter=" + field + ">"
	start := strings.Index(source, marker)
	if start < 0 {
		return ""
	}
	start += len(marker)
	end := strings.Index(source[start:], "</parameter>")
	if end < 0 {
		end = len(source) - start
	}
	return strings.TrimPrefix(source[start:start+end], "\n")
}

func isToolDraftSpace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\n' || value == '\r'
}
