// Package agentclient contains the presentation-only clients for the protected
// Meadow product. This file deliberately speaks only the private agentd HTTP
// protocol over daemontransport's authenticated Unix socket. Do not add Agent,
// provider, tool, session, model, runtime, Core URL, or process imports here.
package agentclient

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/buildinfo"
	"github.com/hey-meadow/meadow-code/go/core/daemontransport"
	editioncfg "github.com/hey-meadow/meadow-code/go/core/edition"
	modelmgr "github.com/hey-meadow/meadow-code/go/core/model"
	"github.com/hey-meadow/meadow-code/go/core/productauth"
	"github.com/hey-meadow/meadow-code/go/core/productsecurity"
)

const (
	defaultProtectedModel = "Meadow Coder 35B"
	maxControlBody        = 16 << 20
	maxEventFrame         = 64 << 20
)

type repeatedFlag []string

func (values *repeatedFlag) String() string { return strings.Join(*values, ",") }

func (values *repeatedFlag) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("value must not be empty")
	}
	*values = append(*values, value)
	return nil
}

type protectedShell struct {
	transport *daemontransport.Client
	stdout    io.Writer
	stderr    io.Writer
	approval  string
	json      bool
}

type protectedRuntimeStartupDependencies struct {
	ensureAuth   func(context.Context, string) error
	newClient    func() (*daemontransport.Client, error)
	authorize    func(context.Context, *daemontransport.Client, string) error
	startRuntime func(context.Context, string, string) (*daemontransport.Client, error)
}

func prepareProtectedRuntime(ctx context.Context, workspace, approval, requirement string) (*daemontransport.Client, error) {
	return prepareProtectedRuntimeWith(ctx, workspace, approval, requirement, protectedRuntimeStartupDependencies{
		ensureAuth:   ensureAuthDaemon,
		newClient:    daemontransport.NewClient,
		authorize:    requireProtectedCapability,
		startRuntime: startProtectedRuntimeAfterAuthorization,
	})
}

func prepareProtectedRuntimeWith(ctx context.Context, workspace, approval, requirement string, dependencies protectedRuntimeStartupDependencies) (*daemontransport.Client, error) {
	if dependencies.ensureAuth == nil || dependencies.newClient == nil || dependencies.authorize == nil || dependencies.startRuntime == nil {
		return nil, errors.New("protected authorization startup is incomplete")
	}
	if err := dependencies.ensureAuth(ctx, workspace); err != nil {
		return nil, err
	}
	authClient, err := dependencies.newClient()
	if err != nil {
		return nil, err
	}
	if err := dependencies.authorize(ctx, authClient, requirement); err != nil {
		return nil, err
	}
	return dependencies.startRuntime(ctx, workspace, approval)
}

func startProtectedRuntimeAfterAuthorization(ctx context.Context, workspace, approval string) (*daemontransport.Client, error) {
	for attempt := 0; attempt < 50; attempt++ {
		if err := ensureProtectedDaemon(ctx, workspace, "", approval); err != nil {
			return nil, err
		}
		client, err := daemontransport.NewClient()
		if err != nil {
			return nil, err
		}
		var capabilities map[string]any
		err = daemonAuthDoWithClient(ctx, client, http.MethodGet, "/agent/client/capabilities", nil, &capabilities)
		if err == nil {
			return client, nil
		}
		if !strings.Contains(err.Error(), "auth_only_route_denied") {
			return nil, err
		}
		if err := sleepContext(ctx, 100*time.Millisecond); err != nil {
			return nil, err
		}
	}
	return nil, errors.New("protected runtime did not replace the authentication-only daemon")
}

func protectedModelRequirement(modelID string) (string, error) {
	requirement, ok := modelmgr.ProductAccessRequirementForModel(modelID)
	if !ok || (requirement.Admission != productauth.AdmissionGeneral && requirement.Admission != productauth.AdmissionPro) {
		return "", errors.New("unknown_capability: model is not in the protected product catalog")
	}
	return requirement.Admission, nil
}

type shellSession struct {
	SessionID string `json:"sessionId"`
	ChatID    string `json:"chatId,omitempty"`
}

type wireEvent struct {
	ID   int64
	Type string
	Data map[string]any
}

// RunProtectedCLI is shared by the meadow and meadow-client protected entry
// points. It contains no direct Core/provider/runtime endpoint flags.
func RunProtectedCLI(args []string, stdout, stderr io.Writer) int {
	if err := productsecurity.VerifyCurrentBundle(); err != nil {
		fmt.Fprintln(stderr, "Meadow product integrity check failed:", err)
		return 1
	}
	if len(args) > 0 && args[0] == "update" {
		workspace, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(stderr, "Meadow Agent update service is unavailable")
			return 1
		}
		return runUpdateCLI(args[1:], stdout, stderr, workspace, func(ctx context.Context, workspace string) error {
			return ensureProtectedDaemon(ctx, workspace, "", "")
		})
	}
	if len(args) > 0 && args[0] == "model" {
		return runProtectedModelCommand(args[1:], stdout, stderr)
	}
	if len(args) > 0 && (args[0] == "/goodAI" || args[0] == "/badAI") {
		profile, _ := editioncfg.Normalize(args[0])
		if err := editioncfg.Persist(profile); err != nil {
			fmt.Fprintln(stderr, "Meadow edition:", err)
			return 1
		}
		fmt.Fprintln(stdout, "Meadow edition updated. The next protected session will use the selected product profile.")
		return 0
	}

	flags := flag.NewFlagSet("meadow", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var images repeatedFlag
	workspaceShort := flags.String("C", "", "workspace root")
	workspaceLong := flags.String("dir", "", "workspace root")
	taskFlag := flags.String("task", "", "user task to run headless")
	modelFlag := flags.String("model", "", "signed logical Meadow model ID")
	resumeFlag := flags.String("resume", "", "durable chat id")
	approvalFlag := flags.String("approval", "default", "plan|default|auto-edit|yolo")
	jsonFlag := flags.Bool("json", false, "print typed events as JSON")
	researchFlag := flags.Bool("research", false, "use the research run mode")
	twoPassFlag := flags.Bool("two-pass", false, "request two-pass execution")
	flags.Var(&images, "image", "attach a local image (repeatable)")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	workspace, err := protectedWorkspace(*workspaceShort, *workspaceLong)
	if err != nil {
		fmt.Fprintln(stderr, "FATAL:", err)
		return 1
	}
	approval, err := protectedApproval(*approvalFlag)
	if err != nil {
		fmt.Fprintln(stderr, "FATAL:", err)
		return 2
	}
	modelID := strings.TrimSpace(*modelFlag)
	if modelID == "" {
		modelID = defaultProtectedModel
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	requiredCapability, err := protectedModelRequirement(modelID)
	if err != nil {
		fmt.Fprintln(stderr, "FATAL:", err)
		return 1
	}
	if *researchFlag || *twoPassFlag {
		requiredCapability = productauth.AdmissionPro
	}
	transport, err := prepareProtectedRuntime(ctx, workspace, approval, requiredCapability)
	if err != nil {
		fmt.Fprintln(stderr, "FATAL:", err)
		return 1
	}
	shell := &protectedShell{transport: transport, stdout: stdout, stderr: stderr, approval: approval, json: *jsonFlag}
	if err := shell.activateModel(ctx, modelID); err != nil {
		fmt.Fprintln(stderr, "FATAL:", err)
		return 1
	}
	session, err := shell.openSession(ctx, workspace, strings.TrimSpace(*resumeFlag))
	if err != nil {
		fmt.Fprintln(stderr, "FATAL:", err)
		return 1
	}
	if err := shell.post(ctx, "/session/"+url.PathEscape(session.SessionID)+"/model", map[string]any{"modelId": modelID}, nil); err != nil {
		fmt.Fprintln(stderr, "FATAL:", err)
		return 1
	}
	if err := shell.post(ctx, "/session/"+url.PathEscape(session.SessionID)+"/approval", map[string]any{"mode": approval}, nil); err != nil {
		fmt.Fprintln(stderr, "FATAL:", err)
		return 1
	}

	task := strings.TrimSpace(*taskFlag)
	if positional := strings.TrimSpace(strings.Join(flags.Args(), " ")); positional != "" {
		if task == "" {
			task = positional
		} else {
			task += " " + positional
		}
	}
	options := map[string]any{"twoPass": *twoPassFlag, "mode": "auto"}
	if *researchFlag {
		options["mode"] = "research"
	}
	if task != "" || len(images) > 0 {
		prompt, promptErr := protectedPrompt(task, images)
		if promptErr != nil {
			fmt.Fprintln(stderr, "FATAL:", promptErr)
			return 1
		}
		if err := shell.runTurn(ctx, session.SessionID, prompt, options, nil); err != nil {
			fmt.Fprintln(stderr, "FATAL:", err)
			return 1
		}
		return 0
	}
	return shell.runInteractive(ctx, session.SessionID, options)
}

func (shell *protectedShell) runInteractive(ctx context.Context, sessionID string, options map[string]any) int {
	lines := make(chan string)
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Buffer(make([]byte, 4096), maxEventFrame)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()
	fmt.Fprintln(shell.stdout, "Meadow CLI ready. During a run, use /guide, /queue or /cancel.")
	for {
		fmt.Fprint(shell.stdout, "> ")
		select {
		case <-ctx.Done():
			return 0
		case line, ok := <-lines:
			if !ok {
				return 0
			}
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if buildinfo.DesktopPackageMode != "portable" && (line == "/goodAI" || line == "/badAI") {
				profile, _ := editioncfg.Normalize(line)
				if err := editioncfg.Persist(profile); err != nil {
					fmt.Fprintln(shell.stderr, "Meadow edition:", err)
					continue
				}
				fmt.Fprintln(shell.stdout, "Edition saved for the next protected session.")
				continue
			}
			if line == "/cancel" {
				_ = shell.post(ctx, "/session/"+url.PathEscape(sessionID)+"/cancel", map[string]any{}, nil)
				continue
			}
			prompt := []map[string]any{{"type": "text", "text": line}}
			if err := shell.runTurn(ctx, sessionID, prompt, options, lines); err != nil {
				fmt.Fprintln(shell.stderr, "Meadow:", err)
			}
		}
	}
}

func (shell *protectedShell) runTurn(ctx context.Context, sessionID string, prompt []map[string]any, options map[string]any, lines <-chan string) error {
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	events := make(chan wireEvent, 64)
	streamErr := make(chan error, 1)
	go func() { streamErr <- shell.stream(turnCtx, sessionID, events) }()
	promptErr := make(chan error, 1)
	go func() {
		promptErr <- shell.post(turnCtx, "/session/"+url.PathEscape(sessionID)+"/prompt", map[string]any{
			"prompt": prompt, "options": options,
		}, nil)
	}()

	promptReturned := false
	for {
		select {
		case <-ctx.Done():
			_ = shell.post(context.Background(), "/session/"+url.PathEscape(sessionID)+"/cancel", map[string]any{}, nil)
			return ctx.Err()
		case err := <-promptErr:
			promptReturned = true
			if err != nil {
				return err
			}
		case err := <-streamErr:
			if err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			if promptReturned {
				return nil
			}
		case event := <-events:
			done, err := shell.handleEvent(ctx, sessionID, event, lines)
			if err != nil {
				return err
			}
			if done {
				return nil
			}
		case line, ok := <-lines:
			if lines == nil || !ok {
				lines = nil
				continue
			}
			line = strings.TrimSpace(line)
			switch {
			case line == "/cancel":
				if err := shell.post(ctx, "/session/"+url.PathEscape(sessionID)+"/cancel", map[string]any{}, nil); err != nil {
					return err
				}
			case strings.HasPrefix(line, "/guide "):
				if err := shell.post(ctx, "/session/"+url.PathEscape(sessionID)+"/steer", map[string]any{"text": strings.TrimSpace(strings.TrimPrefix(line, "/guide "))}, nil); err != nil {
					fmt.Fprintln(shell.stderr, "Meadow guide:", err)
				}
			default:
				text := strings.TrimSpace(strings.TrimPrefix(line, "/queue "))
				if text != "" {
					if err := shell.post(ctx, "/session/"+url.PathEscape(sessionID)+"/queue", map[string]any{
						"prompt": []map[string]any{{"type": "text", "text": text}}, "options": options,
					}, nil); err != nil {
						fmt.Fprintln(shell.stderr, "Meadow queue:", err)
					} else {
						fmt.Fprintln(shell.stdout, "[queued]")
					}
				}
			}
		}
	}
}

func (shell *protectedShell) handleEvent(ctx context.Context, sessionID string, event wireEvent, lines <-chan string) (bool, error) {
	if shell.json {
		return event.Type == "turn_completed", json.NewEncoder(shell.stdout).Encode(map[string]any{
			"id": event.ID, "type": event.Type, "data": event.Data,
		})
	}
	switch event.Type {
	case "session_update":
		update, _ := event.Data["update"].(map[string]any)
		kind := textValue(update["sessionUpdate"])
		switch kind {
		case "agent_message_chunk":
			fmt.Fprint(shell.stdout, nestedText(update["content"]))
		case "agent_thought_chunk":
			fmt.Fprint(shell.stderr, nestedText(update["content"]))
		case "tool_call_draft", "tool_call_draft_progress":
			fmt.Fprint(shell.stderr, textValue(update["argumentsDelta"]))
		case "tool_call":
			fmt.Fprintf(shell.stderr, "\n[tool %s] %s\n", textValue(update["title"]), compactJSON(update["rawInput"]))
		case "tool_call_update":
			fmt.Fprintf(shell.stderr, "[tool %s %s] %s\n", textValue(update["title"]), textValue(update["status"]), compactJSON(update["rawOutput"]))
		}
	case "permission_request":
		return false, shell.resolvePermission(ctx, sessionID, event.Data, lines)
	case "user_input_request":
		return false, shell.resolveUserInput(ctx, sessionID, event.Data, lines)
	case "inference_progress":
		fmt.Fprintf(shell.stderr, "[model %s]\n", compactJSON(event.Data))
	case "guidance":
		fmt.Fprintf(shell.stderr, "[guidance %s]\n", compactJSON(event.Data))
	case "stream_error":
		return false, errors.New(textValue(event.Data["message"]))
	case "turn_completed":
		fmt.Fprintln(shell.stdout)
		return true, nil
	}
	return false, nil
}

func (shell *protectedShell) resolvePermission(ctx context.Context, sessionID string, data map[string]any, lines <-chan string) error {
	requestID := textValue(data["requestId"])
	tool, _ := data["toolCall"].(map[string]any)
	option := "reject"
	switch shell.approval {
	case "yolo":
		option = "approve_always"
	case "auto-edit":
		option = "approve"
	case "plan":
		option = "reject"
	default:
		fmt.Fprintf(shell.stderr, "\nAllow %s %s? [y/N/a] ", textValue(tool["title"]), compactJSON(tool["rawInput"]))
		if lines != nil {
			select {
			case answer, ok := <-lines:
				if ok {
					switch strings.ToLower(strings.TrimSpace(answer)) {
					case "y", "yes":
						option = "approve"
					case "a", "always":
						option = "approve_always"
					}
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return shell.post(ctx, "/session/"+url.PathEscape(sessionID)+"/permission/"+url.PathEscape(requestID), map[string]any{
		"outcome": map[string]any{"outcome": "selected", "optionId": option},
	}, nil)
}

func (shell *protectedShell) resolveUserInput(ctx context.Context, sessionID string, data map[string]any, lines <-chan string) error {
	requestID := textValue(data["requestId"])
	questions, _ := data["questions"].([]any)
	answers := make(map[string]any, len(questions))
	for index, raw := range questions {
		question, _ := raw.(map[string]any)
		fmt.Fprintf(shell.stderr, "\n%s\n", textValue(question["question"]))
		for optionIndex, rawOption := range anyValues(question["options"]) {
			option, _ := rawOption.(map[string]any)
			fmt.Fprintf(shell.stderr, "  %d. %s\n", optionIndex+1, textValue(option["label"]))
		}
		if lines == nil {
			answers["q"+strconv.Itoa(index)] = ""
			continue
		}
		fmt.Fprint(shell.stderr, "> ")
		select {
		case answer, ok := <-lines:
			if ok {
				answers["q"+strconv.Itoa(index)] = strings.TrimSpace(answer)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return shell.post(ctx, "/session/"+url.PathEscape(sessionID)+"/user-input/"+url.PathEscape(requestID), map[string]any{"answers": answers}, nil)
}

func (shell *protectedShell) openSession(ctx context.Context, workspace, chatID string) (shellSession, error) {
	var session shellSession
	path := "/session"
	body := map[string]any{"cwd": workspace, "sessionScope": "single"}
	if chatID != "" {
		path = "/session/resume"
		body = map[string]any{"cwd": workspace, "chatId": chatID}
	}
	if err := shell.post(ctx, path, body, &session); err != nil {
		return shellSession{}, err
	}
	if strings.TrimSpace(session.SessionID) == "" {
		return shellSession{}, errors.New("Meadow Agent returned no session id")
	}
	return session, nil
}

func (shell *protectedShell) activateModel(ctx context.Context, modelID string) error {
	return shell.post(ctx, "/agent/model", map[string]any{"model": modelID}, nil)
}

func (shell *protectedShell) stream(ctx context.Context, sessionID string, output chan<- wireEvent) error {
	request, err := shell.transport.NewRequest(ctx, http.MethodGet, "/session/"+url.PathEscape(sessionID)+"/events", nil)
	if err != nil {
		return err
	}
	response, err := shell.transport.HTTP.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return protectedResponseError(response)
	}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), maxEventFrame)
	var frame strings.Builder
	flush := func() error {
		if frame.Len() == 0 {
			return nil
		}
		var envelope struct {
			ID   int64          `json:"id"`
			Type string         `json:"type"`
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal([]byte(frame.String()), &envelope); err != nil {
			return fmt.Errorf("decode Meadow Agent event: %w", err)
		}
		frame.Reset()
		select {
		case output <- wireEvent{ID: envelope.ID, Type: envelope.Type, Data: envelope.Data}:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if err := flush(); err != nil {
				return err
			}
		case strings.HasPrefix(line, "data:"):
			if frame.Len() > 0 {
				frame.WriteByte('\n')
			}
			frame.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := flush(); err != nil {
		return err
	}
	return scanner.Err()
}

func (shell *protectedShell) post(ctx context.Context, path string, body, output any) error {
	request, err := shell.transport.NewRequest(ctx, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	response, err := shell.transport.HTTP.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return protectedResponseError(response)
	}
	if output == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxControlBody))
		return nil
	}
	return json.NewDecoder(io.LimitReader(response.Body, maxControlBody)).Decode(output)
}

func ensureProtectedDaemon(ctx context.Context, workspace, modelID, approval string) error {
	binary, err := daemontransport.BundledDaemonPath()
	if err != nil {
		return err
	}
	startup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err = daemontransport.EnsureDaemon(startup, daemontransport.LaunchConfig{
		BinaryPath: binary, WorkspaceRoot: workspace, ModelID: modelID,
		ApprovalMode: approval, HarnessMode: "codex-style",
	})
	return err
}

func runProtectedModelCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && isModelVaultCommand(args[0]) {
		return runModelVaultCLI(args, os.Stdin, stdout, stderr, "", func(ctx context.Context, workspace string) error {
			_, err := prepareProtectedRuntime(ctx, workspace, "default", productauth.AdmissionGeneral)
			return err
		})
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(stdout, "Usage: meadow model catalog|list|status|install|remove|import|activate|unload")
		return 0
	}
	action := args[0]
	flags := flag.NewFlagSet("meadow model "+action, flag.ContinueOnError)
	flags.SetOutput(stderr)
	workspaceFlag := flags.String("C", "", "workspace root")
	asJSON := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	workspace, err := protectedWorkspace(*workspaceFlag, "")
	if err != nil {
		fmt.Fprintln(stderr, "Meadow model:", err)
		return 1
	}
	modelID := strings.TrimSpace(strings.Join(flags.Args(), " "))
	if action == "activate" && modelID == "" {
		fmt.Fprintln(stderr, "Meadow model: activate requires a model")
		return 2
	}
	requirement := productauth.AdmissionGeneral
	if modelID != "" {
		requirement, err = protectedModelRequirement(modelID)
		if err != nil {
			fmt.Fprintln(stderr, "Meadow model:", err)
			return 1
		}
	}
	transport, err := prepareProtectedRuntime(context.Background(), workspace, "default", requirement)
	if err != nil {
		fmt.Fprintln(stderr, "Meadow model:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	method, path, body := http.MethodGet, "/agent/model", any(nil)
	switch action {
	case "status":
	case "activate":
		method, body = http.MethodPost, map[string]any{"model": modelID}
	case "unload":
		method, body = http.MethodDelete, map[string]any{"model": modelID}
	default:
		fmt.Fprintf(stderr, "Meadow model: unknown protected command %q\n", action)
		return 2
	}
	request, err := transport.NewRequest(ctx, method, path, body)
	if err != nil {
		fmt.Fprintln(stderr, "Meadow model:", err)
		return 1
	}
	response, err := transport.HTTP.Do(request)
	if err != nil {
		fmt.Fprintln(stderr, "Meadow model:", err)
		return 1
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(response.Body, maxControlBody))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		fmt.Fprintln(stderr, "Meadow model:", strings.TrimSpace(string(payload)))
		return 1
	}
	if *asJSON {
		fmt.Fprintln(stdout, strings.TrimSpace(string(payload)))
		return 0
	}
	var decoded any
	if json.Unmarshal(payload, &decoded) == nil {
		pretty, _ := json.MarshalIndent(decoded, "", "  ")
		fmt.Fprintln(stdout, string(pretty))
	} else {
		fmt.Fprintln(stdout, strings.TrimSpace(string(payload)))
	}
	return 0
}

func protectedPrompt(text string, imagePaths []string) ([]map[string]any, error) {
	blocks := make([]map[string]any, 0, len(imagePaths)+1)
	if strings.TrimSpace(text) != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": strings.TrimSpace(text)})
	}
	for _, path := range imagePaths {
		payload, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read image %s: %w", path, err)
		}
		if len(payload) > 100*1024 {
			return nil, fmt.Errorf("image %s exceeds the protected 100 KB CLI limit", path)
		}
		mediaType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
		if mediaType == "" {
			mediaType = http.DetectContentType(payload)
		}
		blocks = append(blocks, map[string]any{
			"type": "image_url", "image_url": map[string]any{
				"url": "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(payload),
			},
		})
	}
	if len(blocks) == 0 {
		return nil, errors.New("prompt is empty")
	}
	return blocks, nil
}

func protectedWorkspace(short, long string) (string, error) {
	workspace := strings.TrimSpace(short)
	if workspace == "" {
		workspace = strings.TrimSpace(long)
	}
	if workspace == "" {
		var err error
		workspace, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	absolute, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace is not a directory: %s", absolute)
	}
	return absolute, nil
}

func protectedApproval(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		value = "default"
	}
	switch value {
	case "plan", "default", "auto-edit", "yolo":
		return value, nil
	default:
		return "", fmt.Errorf("invalid approval mode %q", value)
	}
}

func protectedResponseError(response *http.Response) error {
	payload, _ := io.ReadAll(io.LimitReader(response.Body, maxControlBody))
	message := strings.TrimSpace(string(payload))
	if message == "" {
		message = response.Status
	}
	return fmt.Errorf("Meadow Agent returned %s: %s", response.Status, message)
}

func compactJSON(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	payload, _ := json.Marshal(value)
	return string(payload)
}

func textValue(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}

func nestedText(value any) string {
	if object, ok := value.(map[string]any); ok {
		return textValue(object["text"])
	}
	return textValue(value)
}

func anyValues(value any) []any {
	values, _ := value.([]any)
	return values
}
