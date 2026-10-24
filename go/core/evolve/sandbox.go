package evolve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/tools"
)

// sandbox is deliberately independent of tools.Shell: that product tool is a
// permission boundary, not an OS filesystem/network sandbox.
type sandbox struct {
	files                    *files
	python, scratch, profile string
	mu                       sync.Mutex
}

func newSandbox(f *files) (*sandbox, error) {
	pythonName := os.Getenv("MEADOW_EVOLVE_PYTHON")
	if pythonName == "" {
		pythonName = "python3"
	}
	return newPythonSandbox(f, pythonName)
}

func newPythonSandbox(f *files, pythonName string) (*sandbox, error) {
	if runtime.GOOS != "darwin" {
		return nil, errors.New("preview script execution requires the tested macOS sandbox")
	}
	if _, e := os.Stat("/usr/bin/sandbox-exec"); e != nil {
		return nil, errors.New("macOS sandbox-exec is unavailable")
	}
	python, e := exec.LookPath(pythonName)
	if e != nil {
		return nil, errors.New("python3 is unavailable")
	}
	python, e = filepath.EvalSymlinks(python)
	if e != nil {
		return nil, e
	}
	// Resolve platform launchers (notably /usr/bin/python3) before constructing
	// the allowlist. Only the configured interpreter's actual installation is
	// admitted, never the surrounding home, Applications, or package store.
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer probeCancel()
	probe := exec.CommandContext(probeCtx, python, "-I", "-S", "-B", "-c", "import json,os,sys;print(json.dumps({'executable':os.path.realpath(sys.executable),'prefix':os.path.realpath(sys.base_prefix)}))")
	probe.Env = []string{"PATH=/usr/bin:/bin", "LANG=en_US.UTF-8", "TZ=UTC0", runtimeTextEncodingEnv()}
	metadata, e := probe.Output()
	var installation struct{ Executable, Prefix string }
	if e != nil || len(metadata) > 8192 || json.Unmarshal(metadata, &installation) != nil || installation.Executable == "" || installation.Prefix == "" {
		return nil, errors.New("cannot identify isolated Python installation")
	}
	python = installation.Executable
	return newRuntimeSandbox(f, python, installation.Prefix, []string{python, "-I", "-S", "-B", "-c", "print('sandbox-ready')"})
}

// Runtime paths are trusted native composition capabilities, never supplied by
// a candidate. Node does not depend on a host Python installation.
func newNodeSandbox(f *files, node string) (*sandbox, error) {
	if runtime.GOOS != "darwin" {
		return nil, errors.New("native Evo program execution requires macOS sandbox-exec")
	}
	if !filepath.IsAbs(node) {
		return nil, errors.New("Node runtime must be an explicit absolute path")
	}
	node, err := filepath.EvalSymlinks(node)
	if err != nil {
		return nil, err
	}
	return newRuntimeSandbox(f, node, filepath.Dir(node), []string{node, "-e", "console.log('sandbox-ready')"})
}

func newRuntimeSandbox(f *files, python, prefix string, preflight []string) (*sandbox, error) {
	// Runtime HOME/TMP files are owned execution resources, not project evidence.
	// Keeping them outside the workspace prevents tools and candidate inventories
	// from presenting interpreter scratch as user source or deliverables.
	scratch, e := os.MkdirTemp("", "meadow-evolve-scratch-")
	if e != nil {
		return nil, e
	}
	canonicalScratch, e := filepath.EvalSymlinks(scratch)
	if e != nil {
		_ = os.RemoveAll(scratch)
		return nil, e
	}
	scratch = canonicalScratch
	s := &sandbox{files: f, python: python, scratch: scratch}
	reads := []string{f.workspacePath, scratch, "/System", "/usr/lib", "/usr/share", "/bin", "/sbin", "/usr/bin", "/dev/null", "/dev/urandom", "/dev/random"}
	if f.harnessPath != "" {
		reads = append(reads, f.harnessPath)
	}
	// The selected interpreter and its own installation are the only optional
	// runtime mount. Never mount /Users, $HOME, or all Homebrew packages.
	pythonRoot := prefix
	if strings.HasPrefix(python, "/opt/homebrew/Cellar/") {
		parts := strings.Split(strings.TrimPrefix(python, "/opt/homebrew/Cellar/"), "/")
		if len(parts) >= 2 {
			pythonRoot = filepath.Join("/opt/homebrew/Cellar", parts[0], parts[1])
		}
	}
	reads = append(reads, pythonRoot)
	writes := []string{scratch}
	if f.mode == "executor" {
		writes = append(writes, f.workspacePath)
	} else {
		writes = append(writes, filepath.Join(f.workspacePath, "output"))
		if f.harvest != "" {
			writes = append(writes, filepath.Join(f.workspacePath, filepath.FromSlash(f.harvest)))
		}
	}
	var b strings.Builder
	// Modern dyld/libignition opens the root directory as an openat anchor.
	// A literal root grant permits that directory only, not any descendants.
	// Scripts may replace their own image but cannot fork detached processes
	// that would survive the shared deadline/process-group cleanup.
	b.WriteString("(version 1)\n(deny default)\n(allow process-exec)\n(deny process-fork)\n(allow sysctl-read)\n(allow file-read-metadata)\n(allow file-read* (literal \"/\"))\n(allow file-read*\n")
	for _, p := range reads {
		fmt.Fprintf(&b, " (subpath %s)\n", strconv.Quote(p))
	}
	b.WriteString(")\n(allow file-write*\n")
	for _, p := range writes {
		fmt.Fprintf(&b, " (subpath %s)\n", strconv.Quote(p))
	}
	b.WriteString(" (literal \"/dev/null\"))\n")
	// Explicit deny wins even if an embedding path accidentally overlaps.
	if f.harnessPath != "" {
		fmt.Fprintf(&b, "(deny file-write* (subpath %s))\n", strconv.Quote(f.harnessPath))
	}
	b.WriteString("(deny network*)\n")
	s.profile = b.String()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, code, e := s.run(ctx, preflight, nil)
	if e != nil || code != 0 {
		s.close()
		if ctx.Err() != nil {
			return nil, fmt.Errorf("sandbox runtime startup: %w", ctx.Err())
		}
		return nil, fmt.Errorf("sandbox unavailable: runtime startup was denied (exit %d): %s", code, output)
	}
	return s, nil
}

func (s *sandbox) close() {
	if s != nil && s.scratch != "" {
		_ = os.RemoveAll(s.scratch)
	}
}

func (s *sandbox) readOnly() *sandbox {
	if s == nil {
		return nil
	}
	return &sandbox{files: s.files, python: s.python, scratch: s.scratch, profile: s.profile + fmt.Sprintf("(deny file-write* (subpath %s) (subpath %s))\n", strconv.Quote(s.files.workspacePath), strconv.Quote(s.scratch))}
}

type boundedBuffer struct {
	bytes.Buffer
	remaining, omitted int
}

// CoreFoundation otherwise asks the user-directory service for this setting
// during dyld initialization. That IPC is unavailable in the narrow sandbox;
// an explicit noninteractive default avoids a startup stall without granting
// additional Mach, network or filesystem access.
func runtimeTextEncodingEnv() string {
	return fmt.Sprintf("__CF_USER_TEXT_ENCODING=0x%X:0x0:0x0", os.Getuid())
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) > b.remaining {
		p = p[:max(0, b.remaining)]
	}
	_, _ = b.Buffer.Write(p)
	b.remaining -= len(p)
	b.omitted += n - len(p)
	return n, nil
}
func (b *boundedBuffer) String() string {
	if b.omitted > 0 {
		return b.Buffer.String() + fmt.Sprintf("\n[Output truncated: omitted %d bytes after the 65536-byte capture limit.]", b.omitted)
	}
	return b.Buffer.String()
}

func (s *sandbox) run(ctx context.Context, argv []string, stdin []byte) (string, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	args := append([]string{"-p", s.profile}, argv...)
	cmd := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", args...)
	cmd.Dir = s.files.workspacePath
	cmd.Env = []string{"PATH=" + filepath.Dir(s.python) + ":/usr/bin:/bin", "HOME=" + s.scratch, "TMPDIR=" + s.scratch, "LANG=en_US.UTF-8", "TZ=UTC0", runtimeTextEncodingEnv(), "PYTHONDONTWRITEBYTECODE=1", "MEADOW_WORKSPACE_ROOT=" + s.files.workspacePath, "MEADOW_HARNESS_ROOT=" + s.files.harnessPath}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin = bytes.NewReader(stdin)
	out := &boundedBuffer{remaining: 64 * 1024}
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	code := 0
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	if err != nil && code == 0 {
		code = -1
	}
	if ctx.Err() != nil {
		return out.String(), code, errors.New("sandbox process deadline reached")
	}
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			return out.String(), code, errors.New("sandbox process could not execute")
		}
	}
	return out.String(), code, nil
}

func (s *sandbox) pythonTool(harnessOnly bool) tools.Tool {
	name := "run_python"
	desc := "Execute an EXISTING workspace .py file in an OS sandbox; file_path must name an existing file. Use a path established by a directory listing, read or successful write. This tool does not create a missing script or accept inline Python/shell commands. Optional args are passed to that existing script. Network, fork and host files are denied. Python cwd is the real workspace directory. Use relative paths or MEADOW_WORKSPACE_ROOT / MEADOW_HARNESS_ROOT in Python, never the virtual file-tool path as an OS path. " + s.files.instruction()
	if harnessOnly {
		name = "run_harness_script"
		desc = "Run a frozen harness Python helper under the same filesystem/network sandbox; the harness remains immutable."
	}
	return &nativeTool{name: name, description: desc, kind: tools.KindExecute, schema: schema(map[string]any{"file_path": str("Virtual .py path"), "args": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 32}, "purpose": map[string]any{"type": "string", "enum": []string{"validation"}}, "validation_paths": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "maxItems": 8, "description": "Optional explicit validation targets; requires purpose=validation. Only unchanged before/after hashes are observed, never proof of correctness."}}, "file_path"), call: func(ctx context.Context, a map[string]any) tools.Result {
		return s.files.trackMutations(s.scratchName(), func() tools.Result {
			name := text(a, "file_path")
			if harnessOnly && !strings.HasPrefix(name, "/harness/") {
				return failure(errors.New("helper must be in /harness"))
			}
			if !strings.HasSuffix(name, ".py") {
				return failure(errors.New("only Python scripts are supported"))
			}
			_, rel, e := s.files.resolve(name, false)
			if e != nil {
				return failure(e)
			}
			root := s.files.workspacePath
			if strings.HasPrefix(name, "/harness/") {
				root = s.files.harnessPath
			}
			// Isolated Python omits script/current directories from sys.path. Add
			// only the declared sandbox mounts so ordinary local imports work.
			bootstrap := "import os,runpy,sys; p=sys.argv.pop(1); sys.path[:0]=[os.path.dirname(p),os.environ['MEADOW_WORKSPACE_ROOT']]; sys.argv[0]=p; runpy.run_path(p,run_name='__main__')"
			argv := []string{s.python, "-I", "-S", "-B", "-c", bootstrap, filepath.Join(root, filepath.FromSlash(rel))}
			if raw, ok := a["args"].([]any); ok {
				if len(raw) > 32 {
					return failure(errors.New("too many script arguments"))
				}
				for _, item := range raw {
					v, ok := item.(string)
					if !ok || len(v) > 4096 {
						return failure(errors.New("invalid script argument"))
					}
					argv = append(argv, v)
				}
			}
			observed, e := s.files.validationTargets(a)
			if e != nil {
				return failure(e)
			}
			output, code, e := s.run(ctx, argv, nil)
			body, _ := json.Marshal(map[string]any{"exit_code": code, "output": output, "sandbox": "macos-seatbelt", "network": "denied"})
			result := tools.Result{LLMContent: string(body), ProcessExited: e == nil && code >= 0, Err: e}
			if e == nil && code != 0 {
				result.Err = fmt.Errorf("Python exited %d", code)
			}
			s.files.attachValidation(&result, observed)
			return result
		})
	}}
}
