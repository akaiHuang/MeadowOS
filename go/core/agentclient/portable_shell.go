//go:build !meadow_protected

package agentclient

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/hey-meadow/meadow-code/go/core/buildinfo"
	"github.com/hey-meadow/meadow-code/go/core/daemontransport"
	"github.com/hey-meadow/meadow-code/go/core/productauth"
	"github.com/hey-meadow/meadow-code/go/core/productsecurity"
)

// RunPortableCLI is a presentation client for the same profile and helper as
// Desktop. It never initializes an inference server or accepts provider keys.
func RunPortableCLI(args []string, stdout, stderr io.Writer) int {
	if buildinfo.DesktopPackageMode != "portable" {
		fmt.Fprintln(stderr, "Meadow portable CLI requires a portable package")
		return 1
	}
	if err := productsecurity.VerifyCurrentBundle(); err != nil {
		fmt.Fprintln(stderr, "Meadow product integrity check failed:", err)
		return 1
	}
	if len(args) > 0 {
		switch args[0] {
		case "auth":
			return RunAuthCLI(args[1:], stdout, stderr)
		case "update":
			return RunUpdateCLI(args[1:], stdout, stderr, "")
		case "pull", "model", "config", "secrets", "/goodAI", "/badAI":
			fmt.Fprintln(stderr, "Manage model connections in Meadow Desktop. Install Meadow Core separately for local models.")
			return 2
		}
	}
	flags := flag.NewFlagSet("meadow", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var images repeatedFlag
	workspaceShort := flags.String("C", "", "workspace root")
	workspaceLong := flags.String("dir", "", "workspace root")
	taskFlag := flags.String("task", "", "user task to run headless")
	modelFlag := flags.String("model", "", "model on the selected Desktop connection")
	resumeFlag := flags.String("resume", "", "durable chat id")
	approvalFlag := flags.String("approval", "default", "plan|default|auto-edit|yolo")
	jsonFlag := flags.Bool("json", false, "print typed events as JSON")
	researchFlag := flags.Bool("research", false, "use the research run mode")
	twoPassFlag := flags.Bool("two-pass", false, "request two-pass execution")
	flags.Var(&images, "image", "attach a local image (repeatable)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	owner := &daemonOwnership{}
	defer owner.close()
	ctx = owner.context(ctx)
	transport, modelID, err := preparePortableRuntime(ctx, workspace, strings.TrimSpace(*modelFlag), approval)
	if err != nil {
		fmt.Fprintln(stderr, "FATAL:", err)
		return 1
	}
	shell := &protectedShell{transport: transport, stdout: stdout, stderr: stderr, approval: approval, json: *jsonFlag}
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
	task := strings.TrimSpace(strings.Join(append([]string{*taskFlag}, flags.Args()...), " "))
	options := map[string]any{"twoPass": *twoPassFlag, "mode": "auto"}
	if *researchFlag {
		options["mode"] = "research"
	}
	if task != "" || len(images) > 0 {
		prompt, err := protectedPrompt(task, images)
		if err != nil {
			fmt.Fprintln(stderr, "FATAL:", err)
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

func preparePortableRuntime(ctx context.Context, workspace, modelID, approval string) (*daemontransport.Client, string, error) {
	return preparePortableRuntimeWith(ctx, workspace, modelID, approval, portableRuntimeStartupDependencies{
		bundledPath: daemontransport.BundledDaemonPath,
		ensure: func(ctx context.Context, config daemontransport.LaunchConfig) (ownedDaemon, error) {
			return daemontransport.EnsureDaemon(ctx, config)
		},
		newClient: daemontransport.NewClient,
		authorize: requireProtectedCapability,
	})
}

type portableRuntimeStartupDependencies struct {
	bundledPath func() (string, error)
	ensure      func(context.Context, daemontransport.LaunchConfig) (ownedDaemon, error)
	newClient   func() (*daemontransport.Client, error)
	authorize   func(context.Context, *daemontransport.Client, string) error
}

func preparePortableRuntimeWith(ctx context.Context, workspace, modelID, approval string, dependencies portableRuntimeStartupDependencies) (*daemontransport.Client, string, error) {
	if buildinfo.DesktopPackageMode != "portable" {
		return nil, "", errors.New("portable startup requires a portable package")
	}
	config, err := portableRuntimeLaunchConfig(daemontransport.LaunchConfig{
		WorkspaceRoot: workspace, ModelID: modelID,
	})
	if err != nil {
		return nil, "", err
	}
	if config.ConnectionID == "" {
		return nil, "", errors.New("configure a model connection in Meadow Desktop, then run `meadow auth login`")
	}
	config.BinaryPath, err = dependencies.bundledPath()
	if err != nil {
		return nil, "", err
	}
	helper, err := dependencies.ensure(ctx, config)
	if err != nil {
		return nil, "", err
	}
	retainOwnedDaemon(ctx, helper)
	transport, err := dependencies.newClient()
	if err != nil {
		return nil, "", err
	}
	if err := dependencies.authorize(ctx, transport, productauth.AdmissionGeneral); err != nil {
		return nil, "", err
	}
	return transport, config.ModelID, nil
}
