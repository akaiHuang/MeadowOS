package agentclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hey-meadow/meadow-code/go/core/daemontransport"
)

const maxUpdateResponseBytes = 256 << 10

type UpdateCheck struct {
	FeedID       string `json:"feedId"`
	Sequence     int64  `json:"sequence"`
	ReleaseID    string `json:"releaseId"`
	Version      string `json:"version"`
	BuildNumber  int64  `json:"buildNumber"`
	FeedDigest   string `json:"feedDigest"`
	ArtifactID   string `json:"artifactId"`
	Architecture string `json:"architecture"`
	MinOS        string `json:"minOs"`
	StagedID     string `json:"stagedId,omitempty"`
}

type UpdateStage struct {
	StagedID    string `json:"stagedId"`
	ReleaseID   string `json:"releaseId"`
	Version     string `json:"version"`
	BuildNumber int64  `json:"buildNumber"`
}

type UpdateInstall struct {
	StagedID         string `json:"stagedId"`
	HandoffReference string `json:"handoffReference"`
	Accepted         bool   `json:"accepted"`
}

type UpdateStatus struct {
	State     string         `json:"state"`
	Operation string         `json:"operation,omitempty"`
	Busy      bool           `json:"busy"`
	Check     *UpdateCheck   `json:"check,omitempty"`
	Stage     *UpdateStage   `json:"stage,omitempty"`
	Install   *UpdateInstall `json:"install,omitempty"`
	ErrorCode string         `json:"errorCode,omitempty"`
}

// UpdateError intentionally retains only the server-authoritative safe code
// and HTTP status. Response bodies and transport details never cross this
// client boundary.
type UpdateError struct {
	StatusCode  int
	Code        string
	Unavailable bool
}

func (err *UpdateError) Error() string {
	if err == nil {
		return "product update operation failed"
	}
	if err.Unavailable {
		return "Meadow Agent update service is unavailable"
	}
	if err.Code != "" {
		return "product update operation failed (" + err.Code + ")"
	}
	return "product update operation failed"
}

type updateDoFunc func(context.Context, string, string, any) (*http.Response, error)

// UpdateClient exposes only the server-authoritative product-update routes.
// It has no URL, archive, keyring, filesystem path, or verification override.
type UpdateClient struct {
	do updateDoFunc
}

func NewUpdateClient() (*UpdateClient, error) {
	transport, err := daemontransport.NewClient()
	if err != nil {
		return nil, &UpdateError{Unavailable: true}
	}
	return updateClientFromTransport(transport), nil
}

func updateClientFromTransport(transport *daemontransport.Client) *UpdateClient {
	return &UpdateClient{do: func(ctx context.Context, method, path string, body any) (*http.Response, error) {
		var reader io.Reader
		if body != nil {
			encoded, err := json.Marshal(body)
			if err != nil {
				return nil, err
			}
			reader = bytes.NewReader(encoded)
		}
		request, err := transport.NewRequest(ctx, method, path, reader)
		if err != nil {
			return nil, err
		}
		if body != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		return transport.HTTP.Do(request)
	}}
}

func (client *UpdateClient) Check(ctx context.Context) (UpdateCheck, error) {
	var result UpdateCheck
	err := client.request(ctx, http.MethodPost, "/agent/update/check", nil, &result)
	if err == nil {
		err = validateUpdateCheck(result)
	}
	return result, err
}

func (client *UpdateClient) Status(ctx context.Context) (UpdateStatus, error) {
	var result UpdateStatus
	err := client.request(ctx, http.MethodGet, "/agent/update/status", nil, &result)
	if err == nil {
		err = validateUpdateStatus(result)
	}
	return result, err
}

func (client *UpdateClient) Stage(ctx context.Context) (UpdateStage, error) {
	var result UpdateStage
	err := client.request(ctx, http.MethodPost, "/agent/update/stage", nil, &result)
	if err == nil {
		err = validateUpdateStage(result)
	}
	return result, err
}

func (client *UpdateClient) Install(ctx context.Context, stagedID string) (UpdateInstall, error) {
	var result UpdateInstall
	if !validUpdateToken(stagedID, 160) {
		return result, fmt.Errorf("invalid staged update id")
	}
	err := client.request(ctx, http.MethodPost, "/agent/update/install", struct {
		StagedID string `json:"stagedId"`
	}{StagedID: stagedID}, &result)
	if err == nil {
		err = validateUpdateInstall(result)
	}
	return result, err
}

func (client *UpdateClient) request(ctx context.Context, method, path string, body, destination any) error {
	if client == nil || client.do == nil {
		return &UpdateError{Unavailable: true}
	}
	response, err := client.do(ctx, method, path, body)
	if err != nil {
		return &UpdateError{Unavailable: true}
	}
	if response == nil || response.Body == nil {
		return &UpdateError{Unavailable: true}
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, maxUpdateResponseBytes+1)
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		var failure struct {
			Code string `json:"code"`
		}
		_ = json.NewDecoder(limited).Decode(&failure)
		if !validUpdateToken(failure.Code, 80) {
			failure.Code = ""
		}
		return &UpdateError{StatusCode: response.StatusCode, Code: failure.Code}
	}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("invalid product update response")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("invalid product update response")
	}
	return nil
}

func validateUpdateCheck(value UpdateCheck) error {
	for _, field := range []string{value.FeedID, value.ReleaseID, value.Version, value.FeedDigest, value.ArtifactID, value.Architecture, value.MinOS} {
		if !validServerField(field, 256) {
			return fmt.Errorf("invalid product update response")
		}
	}
	if value.StagedID != "" && !validUpdateToken(value.StagedID, 160) {
		return fmt.Errorf("invalid product update response")
	}
	return nil
}

func validateUpdateStage(value UpdateStage) error {
	if !validUpdateToken(value.StagedID, 160) || !validServerField(value.ReleaseID, 256) || !validServerField(value.Version, 128) {
		return fmt.Errorf("invalid product update response")
	}
	return nil
}

func validateUpdateInstall(value UpdateInstall) error {
	if !validUpdateToken(value.StagedID, 160) || !validServerField(value.HandoffReference, 256) {
		return fmt.Errorf("invalid product update response")
	}
	return nil
}

func validateUpdateStatus(value UpdateStatus) error {
	switch value.State {
	case "idle", "checking", "available", "staging", "staged", "installing", "handoff", "completed", "failed":
	default:
		return fmt.Errorf("invalid product update response")
	}
	if value.Operation != "" && !validUpdateToken(value.Operation, 80) {
		return fmt.Errorf("invalid product update response")
	}
	if value.ErrorCode != "" && !validUpdateToken(value.ErrorCode, 80) {
		return fmt.Errorf("invalid product update response")
	}
	if value.Check != nil {
		if err := validateUpdateCheck(*value.Check); err != nil {
			return err
		}
	}
	if value.Stage != nil {
		if err := validateUpdateStage(*value.Stage); err != nil {
			return err
		}
	}
	if value.Install != nil {
		if err := validateUpdateInstall(*value.Install); err != nil {
			return err
		}
	}
	return nil
}

func validServerField(value string, limit int) bool {
	if value == "" || len(value) > limit || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func validUpdateToken(value string, limit int) bool {
	if value == "" || len(value) > limit {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

type updateDaemonPrepare func(context.Context, string) error

// RunUpdateCLI is shared by the direct and protected CLI entrypoints. Parsing
// completes before daemon startup, so unsupported override flags fail without
// touching the local service.
func RunUpdateCLI(args []string, stdout, stderr io.Writer, workspace string) int {
	ctx, stop := portableCommandContext()
	defer stop()
	owner := &daemonOwnership{}
	defer owner.close()
	return runUpdateCLIWithContext(ctx, args, stdout, stderr, workspace, func(ctx context.Context, workspace string) error {
		return ensureUpdateDaemon(owner.context(ctx), workspace)
	})
}

func ensureUpdateDaemon(ctx context.Context, workspace string) error {
	binaryPath, err := daemontransport.BundledDaemonPath()
	if err != nil {
		return &UpdateError{Unavailable: true}
	}
	config, err := authDaemonLaunchConfig(binaryPath, workspace)
	if err != nil {
		return &UpdateError{Unavailable: true}
	}
	helper, err := daemontransport.EnsureDaemon(ctx, config)
	if err != nil {
		return &UpdateError{Unavailable: true}
	}
	retainOwnedDaemon(ctx, helper)
	return nil
}

func runUpdateCLI(args []string, stdout, stderr io.Writer, workspace string, prepare updateDaemonPrepare) int {
	return runUpdateCLIWithContext(context.Background(), args, stdout, stderr, workspace, prepare)
}

func runUpdateCLIWithContext(parent context.Context, args []string, stdout, stderr io.Writer, workspace string, prepare updateDaemonPrepare) int {
	return runUpdateCLIWithDependencies(parent, args, stdout, stderr, workspace, prepare, NewUpdateClient)
}

func runUpdateCLIWithDependencies(parent context.Context, args []string, stdout, stderr io.Writer, workspace string, prepare updateDaemonPrepare, newClient func() (*UpdateClient, error)) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: meadow update <check|status|stage|install STAGED_ID>")
		return 2
	}
	command := args[0]
	switch command {
	case "check", "status", "stage":
		if len(args) != 1 {
			fmt.Fprintf(stderr, "meadow update %s does not accept arguments\n", command)
			return 2
		}
	case "install":
		if len(args) != 2 || !validUpdateToken(args[1], 160) {
			fmt.Fprintln(stderr, "usage: meadow update install STAGED_ID")
			return 2
		}
	default:
		fmt.Fprintf(stderr, "unknown update command %q\n", command)
		return 2
	}
	if strings.TrimSpace(workspace) == "" {
		var err error
		workspace, err = os.Getwd()
		if err != nil {
			fmt.Fprintln(stderr, "Meadow Agent update service is unavailable")
			return 1
		}
	}
	timeout := 2 * time.Minute
	if command == "stage" {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	if err := prepare(ctx, workspace); err != nil {
		fmt.Fprintln(stderr, "Meadow Agent update service is unavailable")
		return 1
	}
	client, err := newClient()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	switch command {
	case "check":
		result, err := client.Check(ctx)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "available: %s (build %d)\n", result.Version, result.BuildNumber)
	case "status":
		result, err := client.Status(ctx)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, result.State)
	case "stage":
		result, err := client.Stage(ctx)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "staged: %s (build %d), id %s\n", result.Version, result.BuildNumber, result.StagedID)
	case "install":
		result, err := client.Install(ctx, args[1])
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if !result.Accepted {
			fmt.Fprintln(stderr, "product update install was not accepted")
			return 1
		}
		fmt.Fprintf(stdout, "install accepted: %s\n", result.StagedID)
	}
	return 0
}
