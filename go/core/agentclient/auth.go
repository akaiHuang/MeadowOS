package agentclient

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/buildinfo"
	"github.com/hey-meadow/meadow-code/go/core/daemontransport"
	"github.com/hey-meadow/meadow-code/go/core/productauth"
)

type authDoFunc func(context.Context, string, string, any, any) error
type authPrepareFunc func(context.Context) error
type authOpenFunc func(string) error
type authSleepFunc func(context.Context, time.Duration) error

func RunAuthCLI(args []string, stdout, stderr io.Writer) int {
	command := NewAuthCLICommand()
	defer command.Close()
	return command.Run(args, stdout, stderr)
}

// AuthCLICommand keeps a cold helper alive across a complete account command,
// including a status preflight followed by a device or license API request.
type AuthCLICommand struct {
	ownership daemonOwnership
	ctx       context.Context
	stop      context.CancelFunc
}

func NewAuthCLICommand() *AuthCLICommand {
	ctx, stop := portableCommandContext()
	return &AuthCLICommand{ctx: ctx, stop: stop}
}

func (command *AuthCLICommand) Context() context.Context {
	if command.ctx == nil {
		return context.Background()
	}
	return command.ctx
}

func (command *AuthCLICommand) Close() error {
	if command.stop != nil {
		defer command.stop()
	}
	return command.ownership.close()
}

func (command *AuthCLICommand) Run(args []string, stdout, stderr io.Writer) int {
	if !productAuthEditionAvailable() {
		fmt.Fprintln(stderr, "Meadow product authentication is unavailable in this public development package; use a protected build configured by the release pipeline.")
		return 1
	}
	prepare := func(ctx context.Context) error {
		workspace, err := os.Getwd()
		if err != nil {
			return err
		}
		return ensureAuthDaemon(command.ownership.context(ctx), workspace)
	}
	return runAuthCLIWithContext(command.Context(), args, stdout, stderr, prepare, daemonAuthDo, openBrowser, sleepContext)
}

func runAuthCLI(args []string, stdout, stderr io.Writer, prepare authPrepareFunc, do authDoFunc, open authOpenFunc, sleep authSleepFunc) int {
	return runAuthCLIWithContext(context.Background(), args, stdout, stderr, prepare, do, open, sleep)
}

func runAuthCLIWithContext(parent context.Context, args []string, stdout, stderr io.Writer, prepare authPrepareFunc, do authDoFunc, open authOpenFunc, sleep authSleepFunc) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: meadow auth <login|status|refresh|redeem|logout>")
		return 2
	}
	ctx, cancel := context.WithTimeout(parent, 20*time.Minute)
	defer cancel()
	if err := prepare(ctx); err != nil {
		fmt.Fprintln(stderr, "Meadow authentication service is unavailable")
		return 1
	}
	switch args[0] {
	case "status":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: meadow auth status")
			return 2
		}
		var status productauth.Status
		if err := do(ctx, http.MethodGet, "/auth/status", nil, &status); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if err := validateAuthStatus(status); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		printAuthStatus(stdout, status)
		return 0
	case "refresh":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: meadow auth refresh")
			return 2
		}
		var status productauth.Status
		if err := do(ctx, http.MethodPost, "/auth/refresh", productauth.RefreshRequest{Schema: productauth.SchemaV1}, &status); err != nil {
			fmt.Fprintf(stderr, "auth refresh failed: %v\n", err)
			return 1
		}
		if err := status.Validate(); err != nil {
			fmt.Fprintln(stderr, "auth refresh failed: daemon returned an invalid authorization status")
			return 1
		}
		printAuthStatus(stdout, status)
		return 0
	case "redeem":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "usage: meadow auth redeem <serial>")
			return 2
		}
		requestID, err := newAuthRequestID()
		if err != nil {
			fmt.Fprintln(stderr, "auth redeem failed: secure request identifier is unavailable")
			return 1
		}
		idempotencyKey, err := newAuthRequestID()
		if err != nil {
			fmt.Fprintln(stderr, "auth redeem failed: secure idempotency identifier is unavailable")
			return 1
		}
		request := productauth.RedeemRequest{
			Schema: productauth.SchemaV1, Serial: args[1], RequestID: requestID, IdempotencyKey: idempotencyKey,
		}
		var result productauth.RedeemResult
		if err := do(ctx, http.MethodPost, "/license/redeem", request, &result); err != nil {
			fmt.Fprintf(stderr, "auth redeem failed: %v\n", err)
			return 1
		}
		if result.Schema != productauth.SchemaV1 || result.Receipt.RequestID != requestID ||
			strings.TrimSpace(result.Receipt.ReceiptID) == "" || result.Receipt.Status != "accepted" ||
			result.Availability.Validate() != nil {
			fmt.Fprintln(stderr, "auth redeem failed: daemon returned an invalid redemption receipt")
			return 1
		}
		printAuthStatus(stdout, result.Availability)
		return 0
	case "logout":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: meadow auth logout")
			return 2
		}
		if err := do(ctx, http.MethodPost, "/auth/logout", productauth.LogoutRequest{Schema: productauth.SchemaV1}, nil); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, "logged out")
		return 0
	case "login":
		flags := flag.NewFlagSet("meadow auth login", flag.ContinueOnError)
		flags.SetOutput(stderr)
		deviceCode := flags.Bool("device-code", false, "do not open a local browser")
		if err := flags.Parse(args[1:]); err != nil || len(flags.Args()) != 0 {
			return 2
		}
		mode := productauth.LoginModeBrowser
		if *deviceCode {
			mode = productauth.LoginModeDeviceCode
		}
		var started productauth.StartResult
		if err := do(ctx, http.MethodPost, "/auth/start", productauth.StartRequest{Schema: productauth.SchemaV1, Mode: mode}, &started); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if err := validateAuthStart(started); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		verificationURL := started.VerificationURIComplete
		if verificationURL == "" {
			verificationURL = started.VerificationURI
		}
		opened := false
		if !*deviceCode && safeBrowserURL(verificationURL) {
			opened = open(verificationURL) == nil
		}
		if opened {
			fmt.Fprintln(stdout, "browser login started")
		} else {
			fmt.Fprintf(stdout, "Open %s and enter code %s\n", started.VerificationURI, started.UserCode)
		}
		for {
			delay := time.Duration(started.Interval) * time.Second
			if delay < time.Second {
				delay = time.Second
			}
			if err := sleep(ctx, delay); err != nil {
				fmt.Fprintln(stderr, "login cancelled or expired")
				return 1
			}
			var polled productauth.PollResult
			if err := do(ctx, http.MethodPost, "/auth/poll", productauth.PollRequest{Schema: productauth.SchemaV1, TransactionID: started.TransactionID}, &polled); err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			if err := validateAuthPoll(polled); err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			switch polled.State {
			case productauth.StateLoginPending:
				if polled.Interval > 0 {
					started.Interval = polled.Interval
				}
				continue
			case productauth.StateAuthorizedGeneral, productauth.StateAuthorizedPro:
				if polled.Status == nil || validateAuthStatus(*polled.Status) != nil {
					fmt.Fprintln(stderr, "authentication response is invalid")
					return 1
				}
				printAuthStatus(stdout, *polled.Status)
				return 0
			case productauth.StateLoginDenied:
				fmt.Fprintln(stderr, "login denied")
				return 1
			default:
				fmt.Fprintln(stderr, "login expired")
				return 1
			}
		}
	default:
		fmt.Fprintf(stderr, "unknown auth command %q\n", args[0])
		return 2
	}
}

func requireProtectedCapability(ctx context.Context, transport *daemontransport.Client, requirement string) error {
	if buildinfo.VMLab {
		if requirement == productauth.AdmissionGeneral {
			return ctx.Err()
		}
		return errors.New("VM lab has no paid account capabilities")
	}
	var status productauth.Status
	if err := daemonAuthDoWithClient(ctx, transport, http.MethodGet, "/entitlement/status", nil, &status); err != nil {
		return err
	}
	if err := validateAuthStatus(status); err != nil {
		return err
	}
	if requirement == productauth.AdmissionGeneral &&
		(status.State == productauth.StateAuthorizedGeneral || status.State == productauth.StateAuthorizedPro) {
		return nil
	}
	if requirement == productauth.AdmissionPro && status.State == productauth.StateAuthorizedPro {
		return nil
	}
	if requirement == productauth.AdmissionPro {
		return errors.New("upgrade_required: this mode requires Meadow Pro")
	}
	if requirement != productauth.AdmissionGeneral {
		return errors.New("unknown_capability: protected authorization requirement is unsupported")
	}
	return errors.New("login_required: run `meadow auth login`")
}

func daemonAuthDo(ctx context.Context, method, path string, input, output any) error {
	client, err := daemontransport.NewClient()
	if err != nil {
		return errors.New("Meadow authentication service is unavailable")
	}
	return daemonAuthDoWithClient(ctx, client, method, path, input, output)
}

func daemonAuthDoWithClient(ctx context.Context, client *daemontransport.Client, method, path string, input, output any) error {
	request, err := client.NewRequest(ctx, method, path, input)
	if err != nil {
		return errors.New("Meadow authentication request failed")
	}
	response, err := client.HTTP.Do(request)
	if err != nil {
		return errors.New("Meadow authentication service is unavailable")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 256<<10))
	if err != nil {
		return errors.New("Meadow authentication response failed")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var envelope struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &envelope)
		if envelope.Error.Code != "" {
			return fmt.Errorf("%s: %s", envelope.Error.Code, envelope.Error.Message)
		}
		return errors.New("Meadow authentication request was rejected")
	}
	if output != nil && len(body) != 0 {
		decoder := json.NewDecoder(strings.NewReader(string(body)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(output); err != nil {
			return errors.New("Meadow authentication response is invalid")
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			return errors.New("Meadow authentication response is invalid")
		}
	}
	return nil
}

func printAuthStatus(output io.Writer, status productauth.Status) {
	fmt.Fprintf(output, "state: %s\n", status.State)
	if capabilities := status.Capabilities.EnabledNames(); len(capabilities) != 0 {
		fmt.Fprintf(output, "capabilities: %s\n", strings.Join(capabilities, ","))
	}
}

func safeBrowserURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Fragment == ""
}

func validateAuthStart(started productauth.StartResult) error {
	if started.Schema != productauth.SchemaV1 || started.State != productauth.StateLoginPending {
		return errors.New("authentication response is invalid")
	}
	if started.TransactionID == "" || started.UserCode == "" || started.VerificationURI == "" ||
		started.ExpiresIn <= 0 || started.Interval <= 0 || !safeBrowserURL(started.VerificationURI) {
		return errors.New("authentication response is invalid")
	}
	if started.VerificationURIComplete != "" && !safeBrowserURL(started.VerificationURIComplete) {
		return errors.New("authentication response is invalid")
	}
	return nil
}

func validateAuthPoll(polled productauth.PollResult) error {
	if polled.Schema != productauth.SchemaV1 {
		return errors.New("authentication response is invalid")
	}
	switch polled.State {
	case productauth.StateLoginPending:
		if polled.ErrorCode != "authorization_pending" && polled.ErrorCode != "slow_down" {
			return errors.New("authentication response is invalid")
		}
		if polled.Interval <= 0 {
			return errors.New("authentication response is invalid")
		}
	case productauth.StateAuthorizedGeneral, productauth.StateAuthorizedPro:
		if polled.ErrorCode != "" || polled.Status == nil || validateAuthStatus(*polled.Status) != nil || polled.Status.State != polled.State {
			return errors.New("authentication response is invalid")
		}
	case productauth.StateLoginDenied:
		if polled.ErrorCode != "access_denied" {
			return errors.New("authentication response is invalid")
		}
	case productauth.StateLoginExpired:
		if polled.ErrorCode != "expired_token" {
			return errors.New("authentication response is invalid")
		}
	default:
		return errors.New("authentication response is invalid")
	}
	return nil
}

func validateAuthStatus(status productauth.Status) error {
	if err := status.Validate(); err != nil {
		return errors.New("authentication response is invalid")
	}
	return nil
}

func openBrowser(value string) error {
	if !safeBrowserURL(value) || runtime.GOOS != "darwin" {
		return errors.New("browser launch unavailable")
	}
	return exec.Command("open", value).Start()
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
