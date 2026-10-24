//go:build !meadow_protected

package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"

	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/agentdaemon"
	"github.com/hey-meadow/meadow-code/go/core/buildinfo"
	"github.com/hey-meadow/meadow-code/go/core/engine"
	"github.com/hey-meadow/meadow-code/go/core/modelconnection"
	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/runtimecontrol"
	"github.com/hey-meadow/meadow-code/go/core/session"
)

func loadStartupConfig(defaultWorkspace string) (startupConfig, error) {
	browserFD, browserSecretFD := desktopBrowserDescriptorFlags()
	workspace := flag.String("workspace", envOr("MEADOW_AGENT_WORKSPACE", defaultWorkspace), "workspace root")
	connectionFile := flag.String("connection-file", strings.TrimSpace(os.Getenv("MEADOW_MODEL_CONNECTIONS_FILE")), "private API connection catalog")
	connectionID := flag.String("connection-id", "", "selected API connection ID")
	coreBaseURL := flag.String("core-base-url", strings.TrimSpace(os.Getenv("MEADOW_CORE_BASE_URL")), "local Meadow Core base URL")
	modelID := flag.String("model", strings.TrimSpace(os.Getenv("MEADOW_MODEL_ID")), "resident local model ID")
	modeValue := flag.String("approval-mode", envOr("MEADOW_APPROVAL_MODE", string(engine.ModeDefault)), "plan|default|auto-edit|yolo")
	useRouter := flag.Bool("use-router", strings.TrimSpace(os.Getenv("MEADOW_USE_ROUTER")) == "1", "enable legacy semantic router")
	harnessMode := flag.String("harness-mode", strings.TrimSpace(os.Getenv("MEADOW_HARNESS_MODE")), "kernel|codex-style (empty selects by model family)")
	conversationProfile := flag.String("conversation-profile", strings.TrimSpace(os.Getenv("MEADOW_CONVERSATION_PROFILE")), "meadow-os (empty keeps the default product profile)")
	flag.Parse()
	mode, err := engine.ParseApprovalMode(strings.TrimSpace(*modeValue))
	if err != nil {
		return startupConfig{}, err
	}
	stateRoot := strings.TrimSpace(os.Getenv("MEADOW_HOME"))
	if buildinfo.DesktopPackageMode == "portable" {
		stateRoot, err = session.PortableStateRoot()
		if err != nil {
			return startupConfig{}, err
		}
		if strings.TrimSpace(*connectionFile) == "" {
			*connectionFile = filepath.Join(stateRoot, "model-connections-v1.json")
		}
	} else if stateRoot == "" {
		stateRoot = session.PrimaryDir()
	}
	config := startupConfig{
		agent: agentdaemon.Config{
			WorkspaceRoot: *workspace, ModelID: *modelID, ApprovalMode: mode,
			UseRouter: *useRouter, HarnessMode: *harnessMode,
			StateRoot: stateRoot, ConversationProfile: *conversationProfile,
		},
		coreBaseURL:    *coreBaseURL,
		connectionFile: *connectionFile, connectionID: *connectionID,
	}
	if config.connectionFile != "" {
		catalog, loadErr := modelconnection.Load(config.connectionFile)
		if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
			return startupConfig{}, loadErr
		}
		if catalog != nil && len(catalog.Connections) > 0 {
			connection, model, resolveErr := catalog.Resolve()
			if resolveErr != nil {
				return startupConfig{}, resolveErr
			}
			if config.connectionID != "" && config.connectionID != connection.ID {
				return startupConfig{}, errors.New("selected API connection changed")
			}
			config.connectionID, config.agent.ConnectionID = connection.ID, connection.ID
			config.connectionFingerprint = catalog.Fingerprint()
			if config.agent.ModelID == "" || config.agent.ModelID == "unconfigured" {
				config.agent.ModelID = model.ID
			}
		} else {
			config.agent.ModelID = "unconfigured"
		}
	}
	if err := configureDesktopBrowserDescriptors(&config, *browserFD, *browserSecretFD); err != nil {
		return startupConfig{}, err
	}
	return config, nil
}

func composeStartup(config startupConfig) (startupComposition, error) {
	if buildinfo.VMLab && (buildinfo.DesktopPackageMode != "portable" || config.connectionFile == "") {
		return startupComposition{}, errors.New("VM lab requires the isolated portable API composition")
	}
	if buildinfo.DesktopPackageMode == "portable" && config.connectionFile == "" {
		return startupComposition{}, errors.New("portable Meadow Agent requires an API connection catalog path")
	}
	if config.connectionFile != "" {
		return composeConnectionStartup(config)
	}
	coreBaseURL := runtimecontrol.NormalizeLegacyCoreBaseURL(config.coreBaseURL)
	owner := runtimecontrol.NewLegacyCoreAdapter(runtimecontrol.LegacyCoreOptions{
		BaseURL: coreBaseURL, InitialModel: config.agent.ModelID,
	})
	auth, closeAuth, err := composePublicProductAuth(context.Background())
	if err != nil {
		_ = owner.Close()
		return startupComposition{}, err
	}
	source := agentdaemon.ModelSourceFunc(func(modelID string) (agentcore.KernelModel, error) {
		return provider.NewCoreClient(coreBaseURL, "", modelID), nil
	})
	return startupComposition{
		runtimeControl: owner,
		modelSource:    source,
		productAuth:    auth,
		optionalAuth:   true,
		requireAccount: true,
		closeFn: func() error {
			return errors.Join(owner.Close(), closeAuth())
		},
	}, nil
}

// API connections are independently managed services. No Core process or
// lifecycle capability is composed for this path, including paired Core keys.
func composeConnectionStartup(config startupConfig) (startupComposition, error) {
	owner, err := runtimecontrol.NewExternalAdapter(config.connectionFile, config.connectionID, config.agent.ModelID)
	if err != nil {
		return startupComposition{}, err
	}
	var auth agentdaemon.ProductAuthService
	closeAuth := func() error { return nil }
	if !buildinfo.VMLab {
		auth, closeAuth, err = composePublicProductAuth(context.Background())
	}
	if err != nil {
		return startupComposition{}, err
	}
	source := agentdaemon.ModelSourceFunc(func(modelID string) (agentcore.KernelModel, error) {
		connection, err := owner.Connection()
		if err != nil {
			return nil, err
		}
		capability, ok := connection.LookupModel(modelID)
		if !ok {
			return nil, errors.New("API model is not configured")
		}
		key := ""
		if connection.Kind != modelconnection.KindManagedAPI {
			key, err = modelconnection.Credential(config.connectionFile, connection.ID)
			if err != nil {
				return nil, err
			}
		}
		client, err := configuredConnectionClient(connection, capability, key)
		if err != nil {
			return nil, err
		}
		if connection.Kind == modelconnection.KindManagedAPI {
			client.CredentialResolver = managedConnectionCredentialResolver(auth, connection)
		}
		client.SetOutputTokenLimit(capability.MaxOutputTokens)
		return client, nil
	})
	return startupComposition{runtimeControl: owner, modelSource: source, productAuth: auth, optionalAuth: true, requireAccount: !buildinfo.VMLab, closeFn: func() error { return errors.Join(owner.Close(), closeAuth()) }}, nil
}

func configuredConnectionClient(connection modelconnection.Connection, model modelconnection.Model, key string) (*provider.Client, error) {
	if err := modelconnection.ValidateProtocol(connection.Kind, connection.Protocol); err != nil {
		return nil, err
	}
	if connection.Kind == modelconnection.KindMeadowCore {
		return provider.NewConfiguredCoreClient(connection.BaseURL, key, model.ID, model.SupportsImages), nil
	}
	client, err := provider.NewExternalProtocolClient(connection.BaseURL, key, model.ID, model.SupportsImages, string(connection.Protocol))
	if err == nil {
		switch model.ReasoningControl {
		case "openai-effort":
			client.SetExternalOpenAIReasoningEffort(true)
		case "inception-effort":
			client.SetExternalInceptionReasoningEffort(true)
		}
	}
	return client, err
}
