// Package tools implements the core Meadow tool surface (lean-go-rewrite §4 KEEP).
// Schemas are deliberately the compact shapes from compactLocalGemma4ToolSchemas
// in the TS provider — that is what the local 26B actually performs best on,
// and short schemas are free prefill budget.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/provider"
)

// Kind classifies a tool for approval-mode decisions. It mirrors the TS
// confirmationDetails.type values that the plan/auto-edit gates key on.
type Kind string

const (
	// KindReadOnly is safe in every approval mode (TS type:'info').
	KindReadOnly Kind = "read-only"
	// KindEdit mutates workspace files (TS type:'edit'); auto-approved in
	// auto-edit and yolo.
	KindEdit Kind = "edit"
	// KindExecute runs commands or performs external operations (TS type:'execute');
	// auto-approved only in yolo. Public search/fetch retrieval is KindReadOnly.
	KindExecute Kind = "execute"
	// KindInteract always needs the user (ask_user_question) — even yolo
	// does not skip it.
	KindInteract Kind = "interact"
	// KindDestructive is irreversible (rm/rmdir/shred/dd). It ALWAYS needs
	// confirmation — even yolo does not skip it — because a weak model's
	// wrong delete cannot be undone by the user re-stating the request.
	KindDestructive Kind = "destructive"
)

// Result is what one tool execution feeds back.
type Result struct {
	// SourceOperands are run-scoped observations of actual indexed text. They
	// never come from parsing model output and are not exposed as internal UI data.
	SourceOperands []SourceOperand `json:"-"`
	// SourcePassages are exact extracted text operands observed by a source
	// tool. They are private typed transport, never inferred from model prose.
	SourcePassages []EvidencePassage `json:"-"`
	// Places contains provider-observed location results for maps and durable
	// history. It is independent of model prose and only valid on success.
	Places *PlaceCollection
	// BehaviorEvidence is structured execution evidence from the browser tool.
	// Consumers must not infer assertion success from prose or Err == nil alone.
	BehaviorEvidence map[string]any
	// TaskPlanState is a typed successful plan commit. UI consumers must wait
	// for the matching call/result to cross the durable transcript boundary.
	TaskPlanState *TaskPlanState `json:"-"`
	// LLMContent goes back to the model as the tool message body.
	LLMContent string
	// ContentParts carries additional provider-native model evidence. The
	// Harness keeps LLMContent as the canonical text part and appends these
	// parts to the same tool message. Tool implementations should therefore put
	// only non-text parts here; image tools use bounded base64 data URLs rather
	// than leaking host filesystem paths to the provider.
	ContentParts []provider.MessageContentPart
	// Display is a short human-facing summary for the UI layer.
	Display string
	// Diff is an optional bounded mutation preview for user-facing clients.
	// It is execution evidence, not model guidance, and is intentionally kept
	// separate from LLMContent.
	Diff string
	// ChangedPaths reports workspace files that an execute-capable tool changed
	// indirectly. File tools already expose their target in arguments; shell
	// scripts need this explicit observation so evidence/finalization remains
	// truthful when the model creates an artifact through an interpreter.
	ChangedPaths []string
	// NoChange reports that a mutation-shaped call executed successfully but
	// left the observable workspace bytes unchanged. Runtimes must not count it
	// as mutation progress or let it reset stagnation history.
	NoChange bool
	// ProcessExited is set only by a foreground process runner after a normal
	// exit (zero or nonzero). It says nothing about success or side effects.
	// Timeouts, signals, background starts and policy violations leave it false.
	ProcessExited bool
	// FileObservations bind model-declared validation targets to bytes observed
	// unchanged before and after a normally exited foreground process. They do
	// not prove the command read those files, that no intermediate mutation
	// occurred, or that the files are correct.
	FileObservations []FileObservation
	// Err marks the call failed; LLMContent still carries the error text the
	// model should see.
	Err error
	// ReturnControl means the model deliberately started asynchronous work and
	// the runtime should end the current agent run after recording this result.
	// This is lifecycle control, not a semantic next-action decision: the model
	// chose the async tool call and a completion notification starts a fresh
	// model turn later.
	ReturnControl bool
}

// RecoveryError is a typed, model-facing tool precondition failure. It keeps
// the human error concise while giving local models a stable code, the exact
// path, and the prerequisite imposed by the tool contract. The runtime still
// does not choose the next task action.
type RecoveryError struct {
	Code           string           `json:"code"`
	Message        string           `json:"message"`
	Path           string           `json:"path,omitempty"`
	RequiredAction string           `json:"required_action,omitempty"`
	RequiredArgs   map[string]any   `json:"required_args,omitempty"`
	PreserveDraft  bool             `json:"preserve_draft,omitempty"`
	Diagnostics    []map[string]any `json:"diagnostics,omitempty"`
}

func (e *RecoveryError) Error() string {
	if e == nil {
		return "tool precondition failed"
	}
	return e.Message
}

func errResult(err error) Result {
	var recovery *RecoveryError
	if errors.As(err, &recovery) {
		payload, marshalErr := json.Marshal(recovery)
		if marshalErr == nil {
			return Result{
				LLMContent: "TOOL_ERROR " + string(payload),
				Display:    recovery.Message,
				Err:        err,
			}
		}
	}
	return Result{LLMContent: "ERROR: " + err.Error(), Display: err.Error(), Err: err}
}

func errResultf(format string, a ...any) Result {
	return errResult(fmt.Errorf(format, a...))
}

// Tool is one callable tool. Execute must be context-aware (cancellation)
// and must never panic on malformed args — the local model produces them.
type Tool interface {
	Name() string
	Description() string
	// Schema returns the JSON-schema "parameters" object.
	Schema() map[string]any
	Kind() Kind
	Execute(ctx context.Context, args map[string]any) Result
}

// RunScopedSchemaProvider marks schemas containing request-local operands.
// The model still receives the complete schema; placing it after fixed schemas
// keeps immutable tool definitions reusable by exact prompt-prefix caches.
type RunScopedSchemaProvider interface {
	RunScopedSchema() bool
}

// CompleteObservationProvider declares that a tool's independently bounded
// result is an exact contract. Head/tail excerpts would invalidate its fields
// or constraints, so context management must retain the complete observation.
// Exact duplicate addressing and whole-request context admission still apply.
// This capability comes from the registered implementation, never its name or
// the user's task. Implementations must bound their own result before return.
type CompleteObservationProvider interface {
	CompleteObservation() bool
}

// WorkspaceToolProvider rebinds a declared capability to an isolated child workspace.
// Implementations must preserve configuration and avoid widening permissions.
type WorkspaceToolProvider interface {
	ToolWorkspace() *Workspace
	ForWorkspace(*Workspace) Tool
}

// MutationPathProvider exposes every file path a single mutation call can
// change. Ordinary file tools use their top-level file_path argument; batched
// tools implement this interface so the Kernel can snapshot and roll back the
// complete transaction before execution.
type MutationPathProvider interface {
	MutationPaths(args map[string]any) []string
}

// ArgumentNormalizer lets a tool repair only its own unambiguous wire aliases
// before advertised-schema validation. It must not invent missing semantics.
type ArgumentNormalizer interface {
	NormalizeArguments(args map[string]any)
}

// ArgumentResolver expands explicit, tool-owned references before argument
// normalization, path/collision handling or authorization. Implementations
// must fail closed without side effects or partially resolving invalid input.
// The executor supplies a decoded copy; the raw model ToolCall stays unchanged.
type ArgumentResolver interface {
	ResolveArguments(args map[string]any) error
}

// EvidenceSource is one external document observed by a successful tool call.
// It is run-scoped execution evidence, not long-term memory or model guidance.
type EvidenceSource struct {
	Title             string `json:"title"`
	URL               string `json:"url"`
	Snippet           string `json:"snippet,omitempty"`
	SnippetTruncated  bool   `json:"snippetTruncated,omitempty"`
	ContentKind       string `json:"contentKind,omitempty"`
	ThumbnailURL      string `json:"thumbnailURL,omitempty"`
	FetchedAt         string `json:"fetchedAt,omitempty"`
	SourceVersion     string `json:"sourceVersion,omitempty"`
	FetchError        string `json:"fetchError,omitempty"`
	InputTruncated    bool   `json:"inputTruncated,omitempty"`
	EvidenceTruncated bool   `json:"evidenceTruncated,omitempty"`
}

// EvidenceSourceProvider lets a read/research tool expose the authoritative
// final URL and title it actually observed. ToolExecutor carries these facts
// only to later tools in the same agent run.
type EvidenceSourceProvider interface {
	EvidenceSources(args map[string]any, result Result) []EvidenceSource
}

func evidenceSourceForURL(rawURL, title string) (EvidenceSource, bool) {
	rawURL = strings.TrimSpace(rawURL)
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return EvidenceSource{}, false
	}
	title = strings.TrimSpace(title)
	if title == "" {
		title = parsed.Host
	}
	return EvidenceSource{Title: title, URL: rawURL}, true
}

type evidenceSourcesContextKey struct{}

// ContextWithEvidenceSources attaches an immutable run-scoped snapshot for a
// downstream artifact tool. Cancellation and deadlines from ctx are retained.
func ContextWithEvidenceSources(ctx context.Context, sources []EvidenceSource) context.Context {
	copySources := append([]EvidenceSource(nil), sources...)
	return context.WithValue(ctx, evidenceSourcesContextKey{}, copySources)
}

// EvidenceSourcesFromContext returns a copy so tools cannot mutate executor
// state through the context value.
func EvidenceSourcesFromContext(ctx context.Context) []EvidenceSource {
	if ctx == nil {
		return nil
	}
	sources, _ := ctx.Value(evidenceSourcesContextKey{}).([]EvidenceSource)
	return append([]EvidenceSource(nil), sources...)
}

// ReadIdentity describes one stable region returned by a read-only tool.
// The agent boundary uses it to avoid feeding unchanged content to the model
// repeatedly. Version must change whenever the underlying content may have
// changed; Key distinguishes regions of the same resource.
type ReadIdentity struct {
	Key     string
	Path    string
	Version string
}

// DeduplicableRead is implemented by read tools whose unchanged results can
// be replaced with a small status message. It deliberately lives at the tool
// contract instead of checking tool names in the agent loop.
type DeduplicableRead interface {
	ReadIdentity(args map[string]any) (ReadIdentity, error)
}

// CallClassifier lets a tool refine its Kind per call — the shell tool
// reports read-only for allowlisted inspection commands so default mode
// does not prompt for `git status`.
type CallClassifier interface {
	ClassifyCall(args map[string]any) Kind
}

// Parallelizable is an explicit opt-in for calls that may run concurrently
// with other opted-in calls from the same model turn. The default is false:
// writes, interactive tools and unknown integrations remain serialized.
// Implementations must be safe for concurrent Execute calls.
type Parallelizable interface {
	ParallelSafe(args map[string]any) bool
}

// ParallelSafe reports whether this concrete call may execute concurrently.
// Keeping the decision on the tool contract avoids name lists in the agent
// loop and lets dynamic tools such as shell classify each invocation.
func ParallelSafe(t Tool, args map[string]any) bool {
	policy, ok := t.(Parallelizable)
	return ok && policy.ParallelSafe(args)
}

// SubtaskAccessible marks tools that may be exposed inside a delegated,
// isolated read-only subtask. Absence means deny. This keeps the child surface
// capability-driven and prevents shell, writes, interaction and delegation
// from leaking into a worker simply because they exist in the parent registry.
type SubtaskAccessible interface {
	SubtaskSafe() bool
}

// SubtaskToolProvider supplies a capability-reduced view of a parent tool.
// It is used for tools such as shell where some calls are safe for a read-only
// worker but exposing the unrestricted parent instance would be unsafe.
type SubtaskToolProvider interface {
	SubtaskTool() Tool
}

// CompanionToolProvider declares tools that must remain visible whenever this
// tool is selected by a route. It is capability wiring, not action selection:
// the model still chooses whether to call the companion.
type CompanionToolProvider interface {
	CompanionTools() []string
}

// NotificationSource exposes completed asynchronous tool state at model-turn
// boundaries. Notifications contain observable facts, never a next action.
type NotificationSource interface {
	DrainNotifications() []string
}

// Notification is an observable asynchronous fact that can wake an idle
// session. ID makes delivery claimable: either an active model turn drains it
// or a host claims it for a new turn, never both.
type Notification struct {
	Source  string `json:"source"`
	ID      string `json:"id"`
	Content string `json:"content"`
	// StartedAt is operation admission provenance, not notification arrival.
	// Nil means an older adapter cannot prove which cancelled run owned it.
	StartedAt *time.Time `json:"started_at,omitempty"`
}

type NotificationEventSource interface {
	NotificationEvents() <-chan Notification
	ClaimNotification(id string) (string, bool)
}

type CloseableTool interface {
	Close() error
}

// Registry holds the active toolset in registration order.
type Registry struct {
	order    []string
	byName   map[string]Tool
	disabled map[string]bool
}

func NewRegistry(ts ...Tool) *Registry {
	r := &Registry{byName: map[string]Tool{}, disabled: map[string]bool{}}
	for _, t := range ts {
		r.Register(t)
	}
	return r
}

func (r *Registry) Register(t Tool) {
	if r.disabled[t.Name()] {
		return
	}
	if _, exists := r.byName[t.Name()]; !exists {
		r.order = append(r.order, t.Name())
	}
	r.byName[t.Name()] = t
}

func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.byName[name]
	return t, ok
}

func (r *Registry) Names() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// CompleteObservationNames derives exact-result policy from the implementations
// currently registered, including replacements under an existing tool name.
func (r *Registry) CompleteObservationNames() []string {
	if r == nil {
		return nil
	}
	var names []string
	for _, name := range r.order {
		if tool, ok := r.byName[name].(CompleteObservationProvider); ok && tool.CompleteObservation() {
			names = append(names, name)
		}
	}
	return names
}

// Select returns a registry view containing only requested registered tools,
// preserving the requested order. Tool instances are shared; the registry
// view itself is immutable after construction.
func (r *Registry) Select(names []string) *Registry {
	selected := r.EmptyView()
	if r == nil {
		return selected
	}
	visiting := map[string]bool{}
	var add func(string)
	add = func(name string) {
		if visiting[name] {
			return
		}
		visiting[name] = true
		tool, ok := r.Get(name)
		if !ok {
			return
		}
		selected.Register(tool)
		if companions, ok := tool.(CompanionToolProvider); ok {
			for _, companion := range companions.CompanionTools() {
				add(companion)
			}
		}
	}
	for _, name := range names {
		add(name)
	}
	return selected
}

// EmptyView preserves revoked capabilities when creating a reduced registry.
func (r *Registry) EmptyView() *Registry {
	out := NewRegistry()
	if r != nil {
		for name, denied := range r.disabled {
			out.disabled[name] = denied
		}
	}
	return out
}

// Excluding revokes capabilities, including later dynamic registrations and
// companion tools. Registry views are immutable while a run executes.
func (r *Registry) Excluding(names []string) *Registry {
	out := r.EmptyView()
	for _, name := range names {
		out.disabled[name] = true
	}
	if r != nil {
		for _, name := range r.Names() {
			t, _ := r.Get(name)
			out.Register(t)
		}
	}
	return out
}

// DrainNotifications collects one-shot observable state from asynchronous
// tools in registration order.
func (r *Registry) DrainNotifications() []string {
	if r == nil {
		return nil
	}
	var result []string
	for _, name := range r.order {
		tool := r.byName[name]
		if source, ok := tool.(NotificationSource); ok {
			result = append(result, source.DrainNotifications()...)
		}
	}
	return result
}

// WatchNotifications fans in completion signals from asynchronous tools. The
// returned channel closes when ctx is cancelled or every source closes.
func (r *Registry) WatchNotifications(ctx context.Context) <-chan Notification {
	output := make(chan Notification, 64)
	if r == nil {
		close(output)
		return output
	}
	var sources []NotificationEventSource
	for _, name := range r.order {
		if source, ok := r.byName[name].(NotificationEventSource); ok {
			sources = append(sources, source)
		}
	}
	if len(sources) == 0 {
		close(output)
		return output
	}
	var wg sync.WaitGroup
	wg.Add(len(sources))
	for _, source := range sources {
		source := source
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case notification, ok := <-source.NotificationEvents():
					if !ok {
						return
					}
					select {
					case output <- notification:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(output)
	}()
	return output
}

func (r *Registry) ClaimNotification(notification Notification) (string, bool) {
	if r == nil || notification.Source == "" || notification.ID == "" {
		return "", false
	}
	tool, ok := r.Get(notification.Source)
	if !ok {
		return "", false
	}
	source, ok := tool.(NotificationEventSource)
	if !ok {
		return "", false
	}
	return source.ClaimNotification(notification.ID)
}

// Close releases session-owned tool resources. Shared resources must expose
// Close through only one registered tool so they are not closed twice.
func (r *Registry) Close() error {
	if r == nil {
		return nil
	}
	var errs []error
	for _, name := range r.order {
		tool := r.byName[name]
		if closer, ok := tool.(CloseableTool); ok {
			if err := closer.Close(); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
			}
		}
	}
	return errors.Join(errs...)
}

// CallKind resolves the effective Kind for one call, honouring
// CallClassifier refinements.
func CallKind(t Tool, args map[string]any) Kind {
	if c, ok := t.(CallClassifier); ok {
		return c.ClassifyCall(args)
	}
	return t.Kind()
}

// --- small schema helpers shared by the tool files ---

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func str(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func num(desc string) map[string]any {
	return map[string]any{"type": "number", "description": desc}
}

func boolean(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

// pathArg returns the tool's file path argument, tolerating the aliases
// local models drift to (path / filename). Live failure: 26B emitted
// {"path": "snake_game.html"} for write_file and the call died with
// "path is empty" even though the intent was unambiguous.
func pathArg(args map[string]any) string {
	for _, k := range []string{"file_path", "path", "filename", "file"} {
		if v := SanitizeToolPath(strArg(args, k)); v != "" {
			return v
		}
	}
	return ""
}

// SanitizeToolPath strips the protocol junk local models glue onto filenames.
// Live 35B write_file repair kept
// "/workspace/c-zinc-revised.html\n</parameter>\n</function=write_file>"
// as the path, so the artifact landed on a newline filename and later
// visual_check / preview looked at the clean name and concluded it was lost.
func SanitizeToolPath(p string) string {
	p = strings.TrimSpace(strings.Trim(p, `"'`))
	if p == "" {
		return ""
	}
	if i := strings.IndexAny(p, "\n\r\x00"); i >= 0 {
		p = strings.TrimSpace(p[:i])
	}
	if i := strings.Index(p, "<"); i >= 0 {
		p = strings.TrimSpace(p[:i])
	}
	return p
}

// recoverProtocolLeakedFile moves a nested XML-closer save onto intended.
// Leaked write_file paths such as
// "page.html\n</parameter>\n</function=write_file>" become a directory tree
// because "</parameter>" contains a slash. The intended name then looks
// missing or "is a directory", and the model rewrites a v2 file.
func recoverProtocolLeakedFile(intended string) error {
	leaked := findProtocolLeakedFile(intended)
	if leaked == "" || leaked == intended {
		return nil
	}
	st, err := os.Stat(intended)
	switch {
	case os.IsNotExist(err):
		if err := os.MkdirAll(filepath.Dir(intended), 0o755); err != nil {
			return err
		}
		return os.Rename(leaked, intended)
	case err != nil:
		return err
	case st.IsDir():
		tmp, err := os.CreateTemp(filepath.Dir(intended), ".meadow-path-recover-*")
		if err != nil {
			return err
		}
		tmpPath := tmp.Name()
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		if err := os.Rename(leaked, tmpPath); err != nil {
			return err
		}
		if err := os.RemoveAll(intended); err != nil {
			_ = os.Rename(tmpPath, leaked)
			return err
		}
		if err := os.Rename(tmpPath, intended); err != nil {
			return err
		}
		return nil
	default:
		return nil
	}
}

func findProtocolLeakedFile(intended string) string {
	if leaked := findProtocolSuffixedPath(intended); leaked != "" {
		return leaked
	}
	st, err := os.Stat(intended)
	if err != nil || !st.IsDir() {
		return ""
	}
	var leaked []string
	_ = filepath.Walk(intended, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if strings.HasPrefix(info.Name(), ".") {
			return nil
		}
		rel, relErr := filepath.Rel(intended, path)
		if relErr != nil || !looksLikeProtocolLeakRel(rel) {
			return nil
		}
		leaked = append(leaked, path)
		return nil
	})
	if len(leaked) == 1 {
		return leaked[0]
	}
	return ""
}

func looksLikeProtocolLeakRel(rel string) bool {
	s := strings.ToLower(rel)
	return strings.Contains(s, "parameter>") ||
		strings.Contains(s, "function=") ||
		strings.Contains(s, "tool_call>")
}

func strArg(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return v
}

func intArg(args map[string]any, key string) (int, bool) {
	switch v := args[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case string:
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n, true
		}
	}
	return 0, false
}

func intSliceArg(args map[string]any, key string) []int {
	raw, ok := args[key]
	if !ok {
		return nil
	}
	var values []any
	switch typed := raw.(type) {
	case []any:
		values = typed
	case []int:
		return append([]int(nil), typed...)
	case []float64:
		result := make([]int, 0, len(typed))
		for _, value := range typed {
			result = append(result, int(value))
		}
		return result
	default:
		return nil
	}
	result := make([]int, 0, len(values))
	for _, value := range values {
		if integer, ok := intArg(map[string]any{"value": value}, "value"); ok {
			result = append(result, integer)
		}
	}
	return result
}

func boolArg(args map[string]any, key string) bool {
	v, _ := args[key].(bool)
	return v
}
