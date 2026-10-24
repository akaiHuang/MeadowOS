package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/scanner"
	"go/token"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Pure in-process parsing cannot interrupt Go's parser halfway through a file.
// Bound its input to 1 MiB in appendSourceDiagnostics, bound concurrent workers,
// and let cancellation/timeouts return immediately without sharing report state.
// A cancelled worker only finishes parsing its bounded data; it executes no source.
var nativeSourceDiagnosticSlots = make(chan struct{}, 2)

func runNativeSourceDiagnostics(ctx context.Context, ext, path, source string, report *sourceDiagnosticReport) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	select {
	case nativeSourceDiagnosticSlots <- struct{}{}:
	case <-ctx.Done():
		report.Reason = "diagnostic request timed out or was cancelled; no validation result"
		return
	}
	observed := make(chan sourceDiagnosticReport, 1)
	go func() {
		defer func() { <-nativeSourceDiagnosticSlots }()
		var parsed sourceDiagnosticReport
		if ext == ".json" {
			parsed = parseJSONSourceDiagnostics(source)
		} else {
			parsed = parseGoSourceDiagnostics(path, source)
		}
		observed <- parsed
	}()
	select {
	case parsed := <-observed:
		if ctx.Err() != nil {
			report.Reason = "diagnostic request timed out or was cancelled; no validation result"
			return
		}
		report.Status, report.Diagnostics, report.Truncated, report.Parser = parsed.Status, parsed.Diagnostics, parsed.Truncated, parsed.Parser
	case <-ctx.Done():
		report.Reason = "diagnostic request timed out or was cancelled; no validation result"
	}
}

func parseJSONSourceDiagnostics(source string) sourceDiagnosticReport {
	report := sourceDiagnosticReport{Status: "checked", Parser: "Go encoding/json " + runtime.Version(), Diagnostics: []sourceDiagnostic{}}
	if !utf8.ValidString(source) {
		for i := 0; i < len(source); {
			r, size := utf8.DecodeRuneInString(source[i:])
			if r == utf8.RuneError && size == 1 {
				line, column := jsonSourceDiagnosticPosition(source, i+1)
				report.Diagnostics = append(report.Diagnostics, sourceDiagnostic{Code: "JSON_UTF8", Line: line, Column: column, Message: "invalid UTF-8 encoding"})
				return report
			}
			i += size
		}
	}
	// RawMessage retains JSON numbers without float conversion (1e1000 is valid),
	// and Unmarshal rejects a second value or trailing non-whitespace.
	var value json.RawMessage
	if err := json.Unmarshal([]byte(source), &value); err != nil {
		offset := len(source) + 1
		if syntax, ok := err.(*json.SyntaxError); ok {
			offset = int(syntax.Offset)
		}
		line, column := jsonSourceDiagnosticPosition(source, offset)
		report.Diagnostics = append(report.Diagnostics, sourceDiagnostic{Code: "JSON_SYNTAX", Line: line, Column: column, Message: boundedSourceDiagnosticMessage(err.Error())})
	}
	return report
}

func jsonSourceDiagnosticPosition(source string, offset int) (int, int) {
	index := max(0, min(offset-1, len(source)))
	prefix := source[:index]
	line := strings.Count(prefix, "\n") + 1
	column := utf8.RuneCountInString(prefix[strings.LastIndex(prefix, "\n")+1:]) + 1
	return line, column
}

func parseGoSourceDiagnostics(path, source string) sourceDiagnosticReport {
	report := sourceDiagnosticReport{Status: "checked", Parser: "Go go/parser " + runtime.Version(), Diagnostics: []sourceDiagnostic{}}
	// Supplying source data prevents ParseFile from reading path. No imports or
	// project configuration are resolved and the AST is never compiled/executed.
	_, err := parser.ParseFile(token.NewFileSet(), path, source, parser.AllErrors|parser.SkipObjectResolution)
	if err == nil {
		return report
	}
	if errors, ok := err.(scanner.ErrorList); ok {
		// Scanner ordering also follows claimed //line positions. Order the
		// bounded report by physical source bytes before truncating it.
		sort.SliceStable(errors, func(i, j int) bool { return errors[i].Pos.Offset < errors[j].Pos.Offset })
		for _, syntax := range errors {
			if len(report.Diagnostics) >= 20 {
				report.Truncated = true
				break
			}
			// //line directives affect Pos.Line/Column; use the byte offset to
			// report the physical saved-file location instead of a claimed path.
			index := max(0, min(syntax.Pos.Offset, len(source)))
			prefix := source[:index]
			line, column := strings.Count(prefix, "\n")+1, len(prefix)-strings.LastIndex(prefix, "\n")
			report.Diagnostics = append(report.Diagnostics, sourceDiagnostic{Code: "GO_SYNTAX", Line: line, Column: column, Message: boundedSourceDiagnosticMessage(syntax.Msg)})
		}
	} else {
		report.Diagnostics = append(report.Diagnostics, sourceDiagnostic{Code: "GO_SYNTAX", Line: 1, Column: 1, Message: boundedSourceDiagnosticMessage(fmt.Sprint(err))})
	}
	return report
}

func boundedSourceDiagnosticMessage(message string) string {
	runes := []rune(message)
	if len(runes) > 500 {
		return string(runes[:500]) + "…"
	}
	return message
}
