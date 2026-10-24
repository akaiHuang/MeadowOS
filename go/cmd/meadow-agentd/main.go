// meadow-agentd is the single local orchestration authority used by thin CLI
// and Desktop clients. It never opens a TCP listener. Model traffic uses the
// configured provider; background observations use the fixed account receiver.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/agentdaemon"
	"github.com/hey-meadow/meadow-code/go/core/buildinfo"
	"github.com/hey-meadow/meadow-code/go/core/daemontransport"
	"github.com/hey-meadow/meadow-code/go/core/desktopbrowserrpc"
	"github.com/hey-meadow/meadow-code/go/core/productsecurity"
	"github.com/hey-meadow/meadow-code/go/core/protectedbootstrap"
	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/runtimecontrol"
	"github.com/hey-meadow/meadow-code/go/core/telemetry"
)

type startupConfig struct {
	agent                 agentdaemon.Config
	coreBaseURL           string
	connectionFile        string
	connectionID          string
	connectionFingerprint string
	browserFD             uintptr
	browserSecretFD       uintptr
	browserClientFactory  desktopbrowserrpc.ClientFactory
}

type startupComposition struct {
	telemetry      *telemetry.Queue
	runtimeControl runtimecontrol.RuntimeControl
	modelSource    agentdaemon.ModelSource
	browser        agentdaemon.BrowserOwner
	productUpdate  agentdaemon.ProductUpdateService
	officialModels agentdaemon.OfficialModelService
	productAuth    agentdaemon.ProductAuthService
	optionalAuth   bool
	requireAccount bool
	verifyIdentity bool
	protectedState *protectedbootstrap.Prepared
	modelID        string
	handler        http.Handler
	shutdown       <-chan struct{}
	closeFn        func() error
}

func (composition *startupComposition) Close() error {
	if composition == nil {
		return nil
	}
	closeTelemetry(composition.telemetry)
	if composition.closeFn != nil {
		return composition.closeFn()
	}
	var failures []error
	if composition.browser != nil {
		failures = append(failures, composition.browser.Close())
	}
	if composition.runtimeControl != nil {
		failures = append(failures, composition.runtimeControl.Close())
	}
	if composition.protectedState != nil {
		failures = append(failures, composition.protectedState.Close())
	}
	return errors.Join(failures...)
}

func handlerForComposition(config startupConfig, composition startupComposition) (http.Handler, error) {
	if composition.handler != nil {
		return composition.handler, nil
	}
	if composition.modelID != "" {
		config.agent.ModelID = composition.modelID
	}
	commercialMembership, _ := composition.productAuth.(agentdaemon.CommercialMembershipService)
	if composition.optionalAuth && !composition.requireAccount && buildinfo.DesktopPackageMode != "portable" {
		// Development-only compositions can omit account usage. Every portable
		// Desktop submission needs a server grant, independently of model origin.
		commercialMembership = nil
	}
	modelSource := composition.modelSource
	if composition.telemetry != nil {
		original := modelSource
		modelSource = agentdaemon.ModelSourceFunc(func(modelID string) (agentcore.KernelModel, error) {
			model, err := original.Model(modelID)
			if client, ok := model.(*provider.Client); ok {
				client.Telemetry = composition.telemetry
			}
			return model, err
		})
	}
	return agentdaemon.NewHandler(config.agent, agentdaemon.Dependencies{
		RuntimeControl:               composition.runtimeControl,
		ModelSource:                  modelSource,
		Browser:                      composition.browser,
		ProductUpdate:                composition.productUpdate,
		OfficialModels:               composition.officialModels,
		ProductAuth:                  composition.productAuth,
		TraceObserver:                composition.telemetry,
		ProductAuthOptional:          composition.optionalAuth,
		RequireAccountAuthentication: composition.requireAccount,
		CommercialMembership:         commercialMembership,
		RequireVerifiedIdentity:      composition.verifyIdentity,
		BrowserClientFactory:         config.browserClientFactory,
	})
}

func protectedHandlerForStartup(config startupConfig, handler http.Handler) (http.Handler, error) {
	var appliedIdentity func() (string, string, string)
	if configured, ok := handler.(interface {
		AppliedConnectionIdentity() (string, string, string)
	}); ok {
		appliedIdentity = configured.AppliedConnectionIdentity
	}
	return daemontransport.ProtectedHandlerWithConfig(handler, daemontransport.HealthConfig{
		StateRoot:                 config.agent.StateRoot,
		ModelID:                   config.agent.ModelID,
		HarnessMode:               config.agent.HarnessMode,
		ConnectionID:              config.connectionID,
		ConnectionFingerprint:     config.connectionFingerprint,
		ConnectionMode:            config.connectionFile != "",
		ApprovalMode:              string(config.agent.ApprovalMode),
		AppliedConnectionIdentity: appliedIdentity,
	})
}

func main() {
	// This explicit maintenance command must never initialize the daemon, Core,
	// workspace or ordinary account store. Its handler validates its own fixed
	// signed-preview boundary and emits only a sanitized migration receipt.
	migrationContext, cancelMigration := context.WithTimeout(context.Background(), 30*time.Second)
	migrationHandled, migrationErr := handlePublicProductAuthMigration(migrationContext, os.Args[1:], os.Stdout)
	cancelMigration()
	if migrationHandled {
		if migrationErr != nil {
			_, _ = os.Stderr.WriteString("account storage migration did not complete; inspect sanitized receipt and prerequisites\n")
			os.Exit(1)
		}
		return
	}
	log.SetFlags(log.Ldate | log.Ltime | log.LUTC)
	if err := productsecurity.VerifyCurrentBundle(); err != nil {
		log.Fatalf("verify Meadow product bundle: %v", err)
	}
	defaultWorkspace, err := os.Getwd()
	if err != nil {
		log.Fatalf("resolve current workspace: %v", err)
	}
	config, err := loadStartupConfig(defaultWorkspace)
	if err != nil {
		log.Fatal(err)
	}
	if config.browserFD != 0 {
		connection, secret, err := desktopbrowserrpc.ReadInherited(config.browserFD, config.browserSecretFD)
		if err != nil {
			log.Fatalf("adopt Desktop Browser channel: %v", err)
		}
		if err := daemontransport.VerifyDesktopBrowserPeer(connection); err != nil {
			clear(secret)
			_ = connection.Close()
			log.Fatalf("verify Desktop Browser parent: %v", err)
		}
		client, err := desktopbrowserrpc.NewClient(context.Background(), connection, secret)
		clear(secret)
		if err != nil {
			log.Fatalf("authenticate Desktop Browser channel: %v", err)
		}
		defer client.Close()
		config.browserClientFactory = client.Factory
	}
	composition, err := composeStartup(config)
	if err != nil {
		log.Fatalf("compose Meadow Agent runtime ownership: %v", err)
	}
	configureTelemetry(config, &composition)
	handler, err := handlerForComposition(config, composition)
	if err != nil {
		_ = composition.Close()
		log.Fatalf("compose Meadow Agent: %v", err)
	}
	defer func() {
		if err := composition.Close(); err != nil {
			log.Printf("close Meadow Agent composition: %v", err)
		}
	}()
	protected, err := protectedHandlerForStartup(config, handler)
	if err != nil {
		log.Fatalf("protect Meadow Agent transport: %v", err)
	}
	listener, err := daemontransport.Listen()
	if err != nil {
		log.Fatal(err)
	}
	socketPath, _ := daemontransport.DefaultSocketPath()
	defer func() {
		_ = listener.Close()
		if socketPath != "" {
			_ = os.Remove(socketPath)
		}
	}()

	server := newAgentHTTPServer(protected, handler)
	serveErrors := make(chan error, 1)
	go func() {
		serveErrors <- server.Serve(listener)
	}()
	log.Printf("Meadow Agent ready on private local socket %s", socketPath)
	if owner, ok := handler.(interface{ StartBackgroundServices(context.Context) error }); ok {
		if err := owner.StartBackgroundServices(context.Background()); err != nil {
			log.Printf("restore Meadow scheduled work: %v", err)
		}
	}
	// Keep the authenticated control plane available independently of model
	// startup. Opening Desktop must not start Core or allocate model weights;
	// runtimeOwnedModel.ChatStream lazily activates the selected model on the
	// first prompt. Development launchers may opt into the old warm-up when they
	// explicitly need it for latency experiments.
	if composition.handler == nil && composition.runtimeControl != nil && warmRuntimeOnStartup() {
		go func() {
			if err := composition.runtimeControl.Ensure(context.Background()); err != nil {
				log.Printf("background Meadow Runtime startup: %v", err)
			}
		}()
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	var serveErr error
	select {
	case signalValue := <-signals:
		log.Printf("stopping Meadow Agent after %s", signalValue)
	case <-composition.shutdown:
		log.Printf("stopping auth-only Meadow Agent after terminal authorization request")
	case serveErr = <-serveErrors:
	}

	// Leave headroom inside the supervisor's shortest ordinary two-second
	// stop context. Cancel request-owned subprocesses before draining handlers.
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("Meadow Agent shutdown: %v", err)
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		log.Fatalf("Meadow Agent stopped: %v", serveErr)
	}
}

func envOr(name string, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func warmRuntimeOnStartup() bool {
	return strings.TrimSpace(os.Getenv("MEADOW_WARM_RUNTIME_ON_STARTUP")) == "1"
}
