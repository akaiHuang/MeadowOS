package agentclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hey-meadow/meadow-code/go/core/daemontransport"
)

const maxModelVaultResponseBytes = 256 << 10

var ErrModelImportUnsupported = errors.New("model import requires an opaque file capability")

type ModelCatalogStatus struct {
	State            string `json:"state"`
	Sequence         uint64 `json:"sequence,omitempty"`
	Digest           string `json:"digest,omitempty"`
	InstallAvailable bool   `json:"installAvailable"`
}

type OfficialModel struct {
	ModelID      string `json:"modelId"`
	Version      string `json:"version"`
	ProfileID    string `json:"profileId"`
	LicenseID    string `json:"licenseId"`
	Installed    bool   `json:"installed"`
	RuntimeReady bool   `json:"runtimeReady"`
}

type RemovedOfficialModel struct {
	ModelID string `json:"modelId"`
	Version string `json:"version"`
	State   string `json:"state"`
}

type ModelVaultError struct {
	StatusCode  int
	Code        string
	Unavailable bool
}

func (err *ModelVaultError) Error() string {
	if err == nil {
		return "official model operation failed"
	}
	switch err.Code {
	case "MODEL_IMPORT_UNSUPPORTED":
		return ErrModelImportUnsupported.Error()
	case "MODEL_REMOVAL_DENIED":
		return "model removal authorization was denied"
	case "MODEL_NOT_FOUND":
		return "official model was not found"
	case "MODEL_REJECTED":
		return "official model operation was rejected"
	}
	if err.Unavailable {
		return "Meadow Agent model service is unavailable"
	}
	return "official model operation failed"
}

func (err *ModelVaultError) Is(target error) bool {
	return target == ErrModelImportUnsupported && err != nil && err.Code == "MODEL_IMPORT_UNSUPPORTED"
}

type modelVaultDoFunc func(context.Context, string, string, any) (*http.Response, error)

// ModelVaultClient exposes only server-authoritative model catalog and vault
// operations. No method accepts an origin URL, local path, digest, archive, or
// verification override.
type ModelVaultClient struct {
	do modelVaultDoFunc
}

func NewModelVaultClient() (*ModelVaultClient, error) {
	transport, err := daemontransport.NewClient()
	if err != nil {
		return nil, &ModelVaultError{Unavailable: true}
	}
	return &ModelVaultClient{do: func(ctx context.Context, method, path string, body any) (*http.Response, error) {
		request, err := transport.NewRequest(ctx, method, path, body)
		if err != nil {
			return nil, err
		}
		return transport.HTTP.Do(request)
	}}, nil
}

func (client *ModelVaultClient) CatalogStatus(ctx context.Context) (ModelCatalogStatus, error) {
	var result ModelCatalogStatus
	err := client.request(ctx, http.MethodGet, "/agent/models/catalog/status", nil, &result)
	if err == nil {
		err = validateCatalogStatus(result)
	}
	return result, err
}

func (client *ModelVaultClient) Catalog(ctx context.Context) ([]OfficialModel, error) {
	var result struct {
		Models []OfficialModel `json:"models"`
	}
	err := client.request(ctx, http.MethodGet, "/agent/models/catalog", nil, &result)
	if err == nil {
		err = validateOfficialModels(result.Models)
	}
	return result.Models, err
}

func (client *ModelVaultClient) List(ctx context.Context) ([]OfficialModel, error) {
	var result struct {
		Models []OfficialModel `json:"models"`
	}
	err := client.request(ctx, http.MethodGet, "/agent/models", nil, &result)
	if err == nil {
		err = validateOfficialModels(result.Models)
	}
	return result.Models, err
}

func (client *ModelVaultClient) Status(ctx context.Context, modelID, version string) (OfficialModel, error) {
	var result OfficialModel
	if !validOfficialModelID(modelID) || !validOfficialModelID(version) {
		return result, fmt.Errorf("invalid official model identity")
	}
	err := client.request(ctx, http.MethodPost, "/agent/models/status", struct {
		ModelID string `json:"modelId"`
		Version string `json:"version"`
	}{ModelID: modelID, Version: version}, &result)
	if err == nil {
		err = validateOfficialModel(result)
	}
	return result, err
}

func (client *ModelVaultClient) Install(ctx context.Context, modelID, version, licenseSubject string, licenseAccepted bool) (OfficialModel, error) {
	var result OfficialModel
	if !validOfficialModelID(modelID) || !validOfficialModelID(version) || !licenseAccepted || !validLicenseSubject(licenseSubject) {
		return result, fmt.Errorf("model identity, license subject, and explicit acceptance are required")
	}
	err := client.request(ctx, http.MethodPost, "/agent/models/install", struct {
		ModelID         string `json:"modelId"`
		Version         string `json:"version"`
		LicenseAccepted bool   `json:"licenseAccepted"`
		LicenseSubject  string `json:"licenseSubject"`
	}{ModelID: modelID, Version: version, LicenseAccepted: true, LicenseSubject: licenseSubject}, &result)
	if err == nil {
		err = validateOfficialModel(result)
		if err == nil && (!result.Installed || !result.RuntimeReady) {
			err = fmt.Errorf("official model install did not become runtime ready")
		}
	}
	return result, err
}

func (client *ModelVaultClient) Remove(ctx context.Context, modelID, version string, confirmed bool) (RemovedOfficialModel, error) {
	var result RemovedOfficialModel
	if !validOfficialModelID(modelID) || !validOfficialModelID(version) || !confirmed {
		return result, fmt.Errorf("model identity and explicit removal confirmation are required")
	}
	err := client.request(ctx, http.MethodPost, "/agent/models/remove", struct {
		ModelID   string `json:"modelId"`
		Version   string `json:"version"`
		Confirmed bool   `json:"confirmed"`
	}{ModelID: modelID, Version: version, Confirmed: true}, &result)
	if err == nil && (!validOfficialModelID(result.ModelID) || !validOfficialModelID(result.Version) || result.State != "removed") {
		err = fmt.Errorf("invalid official model response")
	}
	return result, err
}

func (client *ModelVaultClient) Import(ctx context.Context) error {
	var ignored struct{}
	err := client.request(ctx, http.MethodPost, "/agent/models/import", nil, &ignored)
	if err == nil {
		return fmt.Errorf("model import unexpectedly succeeded without an opaque file capability")
	}
	return err
}

func (client *ModelVaultClient) request(ctx context.Context, method, path string, body, destination any) error {
	if client == nil || client.do == nil {
		return &ModelVaultError{Unavailable: true}
	}
	response, err := client.do(ctx, method, path, body)
	if err != nil || response == nil || response.Body == nil {
		return &ModelVaultError{Unavailable: true}
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, maxModelVaultResponseBytes+1)
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		var failure struct {
			Code string `json:"code"`
		}
		_ = json.NewDecoder(limited).Decode(&failure)
		if !validModelErrorCode(failure.Code) {
			failure.Code = ""
		}
		return &ModelVaultError{StatusCode: response.StatusCode, Code: failure.Code}
	}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("invalid official model response")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("invalid official model response")
	}
	return nil
}

func validateCatalogStatus(status ModelCatalogStatus) error {
	if !validSafeModelField(status.State, 64) || status.Sequence == 0 || !validDigest(status.Digest) {
		return fmt.Errorf("invalid official model response")
	}
	return nil
}

func validateOfficialModels(models []OfficialModel) error {
	if len(models) > 128 {
		return fmt.Errorf("invalid official model response")
	}
	for _, model := range models {
		if err := validateOfficialModel(model); err != nil {
			return err
		}
	}
	return nil
}

func validateOfficialModel(model OfficialModel) error {
	if !validOfficialModelID(model.ModelID) || !validOfficialModelID(model.Version) {
		return fmt.Errorf("invalid official model response")
	}
	for _, value := range []string{model.ProfileID, model.LicenseID} {
		if value != "" && !validSafeModelField(value, 256) {
			return fmt.Errorf("invalid official model response")
		}
	}
	return nil
}

func validOfficialModelID(value string) bool {
	if value == "" || len(value) > 192 || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

func validLicenseSubject(value string) bool {
	if value == "" || len(value) > 256 || strings.TrimSpace(value) != value || strings.ContainsAny(value, "/\\:") || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func validSafeModelField(value string, limit int) bool {
	if value == "" || len(value) > limit || strings.TrimSpace(value) != value || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'f') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

func validModelErrorCode(value string) bool {
	return value != "" && len(value) <= 80 && validUpdateToken(value, 80)
}

type modelVaultPrepare func(context.Context, string) error

func RunModelVaultCLI(args []string, stdin io.Reader, stdout, stderr io.Writer, workspace string) int {
	ctx, stop := portableCommandContext()
	defer stop()
	owner := &daemonOwnership{}
	defer owner.close()
	return runModelVaultCLIWithContext(ctx, args, stdin, stdout, stderr, workspace, func(ctx context.Context, workspace string) error {
		return ensureUpdateDaemon(owner.context(ctx), workspace)
	})
}

func isModelVaultCommand(command string) bool {
	switch command {
	case "catalog", "list", "status", "install", "remove", "import":
		return true
	default:
		return false
	}
}

func runModelVaultCLI(args []string, stdin io.Reader, stdout, stderr io.Writer, workspace string, prepare modelVaultPrepare) int {
	return runModelVaultCLIWithContext(context.Background(), args, stdin, stdout, stderr, workspace, prepare)
}

func runModelVaultCLIWithContext(parent context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, workspace string, prepare modelVaultPrepare) int {
	if len(args) == 0 || !isModelVaultCommand(args[0]) {
		return 2
	}
	action := args[0]
	flags := flag.NewFlagSet("meadow model "+action, flag.ContinueOnError)
	flags.SetOutput(stderr)
	workspaceFlag := flags.String("C", "", "workspace root")
	asJSON := flags.Bool("json", false, "print JSON")
	licenseSubject := flags.String("license-subject", "", "license subject")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	positionals := flags.Args()
	if (*licenseSubject != "" && action != "install") || (*asJSON && (action == "install" || action == "remove" || action == "import")) {
		fmt.Fprintln(stderr, "Meadow model: unsupported option for this command")
		return 2
	}
	wantIdentities := 0
	switch action {
	case "status":
		if len(positionals) != 0 && len(positionals) != 2 {
			fmt.Fprintln(stderr, "Usage: meadow model status [MODEL_ID VERSION]")
			return 2
		}
		wantIdentities = len(positionals)
	case "install", "remove":
		wantIdentities = 2
		if len(positionals) != 2 {
			fmt.Fprintf(stderr, "Usage: meadow model %s MODEL_ID VERSION\n", action)
			return 2
		}
	default:
		if len(positionals) != 0 {
			fmt.Fprintf(stderr, "Meadow model %s does not accept arguments\n", action)
			return 2
		}
	}
	if wantIdentities == 2 && (!validOfficialModelID(positionals[0]) || !validOfficialModelID(positionals[1])) {
		fmt.Fprintln(stderr, "Meadow model: invalid official model identity")
		return 2
	}
	if *licenseSubject != "" && !validLicenseSubject(*licenseSubject) {
		fmt.Fprintln(stderr, "Meadow model: invalid license subject")
		return 2
	}
	if workspace == "" {
		workspace = *workspaceFlag
	}
	if workspace == "" {
		var err error
		workspace, err = os.Getwd()
		if err != nil {
			fmt.Fprintln(stderr, "Meadow Agent model service is unavailable")
			return 1
		}
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Minute)
	defer cancel()
	if err := prepare(ctx, workspace); err != nil {
		fmt.Fprintln(stderr, "Meadow Agent model service is unavailable")
		return 1
	}
	client, err := NewModelVaultClient()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	encode := func(value any) int {
		encoder := json.NewEncoder(stdout)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(value); err != nil {
			fmt.Fprintln(stderr, "Meadow model: write output failed")
			return 1
		}
		return 0
	}
	switch action {
	case "catalog":
		status, err := client.CatalogStatus(ctx)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		models, err := client.Catalog(ctx)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if *asJSON {
			return encode(struct {
				State            string          `json:"state"`
				Sequence         uint64          `json:"sequence"`
				InstallAvailable bool            `json:"installAvailable"`
				Models           []OfficialModel `json:"models"`
			}{status.State, status.Sequence, status.InstallAvailable, models})
		}
		fmt.Fprintf(stdout, "catalog %s, sequence %d, install available: %t\n", status.State, status.Sequence, status.InstallAvailable)
		printOfficialModels(stdout, models)
	case "list":
		models, err := client.List(ctx)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if *asJSON {
			return encode(struct {
				Models []OfficialModel `json:"models"`
			}{models})
		}
		if len(models) == 0 {
			fmt.Fprintln(stdout, "healthy: no model installed")
			return 0
		}
		printOfficialModels(stdout, models)
	case "status":
		if len(positionals) == 0 {
			models, err := client.List(ctx)
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			if *asJSON {
				return encode(struct {
					Healthy bool            `json:"healthy"`
					Models  []OfficialModel `json:"models"`
				}{true, models})
			}
			if len(models) == 0 {
				fmt.Fprintln(stdout, "healthy: no model installed")
			} else {
				printOfficialModels(stdout, models)
			}
			return 0
		}
		model, err := client.Status(ctx, positionals[0], positionals[1])
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if *asJSON {
			return encode(model)
		}
		printOfficialModels(stdout, []OfficialModel{model})
	case "install":
		catalog, err := client.Catalog(ctx)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		model, found := findOfficialModel(catalog, positionals[0], positionals[1])
		if !found {
			fmt.Fprintln(stderr, "official model was not found")
			return 1
		}
		reader := bufio.NewReader(stdin)
		subject := strings.TrimSpace(*licenseSubject)
		if subject == "" {
			fmt.Fprint(stderr, "License subject: ")
			subject, _ = reader.ReadString('\n')
			subject = strings.TrimSpace(subject)
		}
		if !validLicenseSubject(subject) {
			fmt.Fprintln(stderr, "Meadow model: invalid license subject")
			return 2
		}
		fmt.Fprintf(stderr, "License %s applies to %s@%s. Type ACCEPT to continue: ", model.LicenseID, model.ModelID, model.Version)
		confirmation, _ := reader.ReadString('\n')
		if strings.TrimSpace(confirmation) != "ACCEPT" {
			fmt.Fprintln(stderr, "model install cancelled")
			return 2
		}
		installed, err := client.Install(ctx, model.ModelID, model.Version, subject, true)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "installed and runtime ready: %s@%s\n", installed.ModelID, installed.Version)
	case "remove":
		fmt.Fprintf(stderr, "Type REMOVE to authorize removal of %s@%s: ", positionals[0], positionals[1])
		confirmation, _ := bufio.NewReader(stdin).ReadString('\n')
		if strings.TrimSpace(confirmation) != "REMOVE" {
			fmt.Fprintln(stderr, "model removal cancelled")
			return 2
		}
		removed, err := client.Remove(ctx, positionals[0], positionals[1], true)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "removed: %s@%s\n", removed.ModelID, removed.Version)
	case "import":
		err := client.Import(ctx)
		if errors.Is(err, ErrModelImportUnsupported) {
			fmt.Fprintln(stderr, ErrModelImportUnsupported)
			return 1
		}
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	return 0
}

func findOfficialModel(models []OfficialModel, modelID, version string) (OfficialModel, bool) {
	for _, model := range models {
		if model.ModelID == modelID && model.Version == version {
			return model, true
		}
	}
	return OfficialModel{}, false
}

func printOfficialModels(output io.Writer, models []OfficialModel) {
	for _, model := range models {
		state := "available"
		if model.RuntimeReady {
			state = "runtime-ready"
		} else if model.Installed {
			state = "installed"
		}
		fmt.Fprintf(output, "%s@%s\t%s\tlicense %s\n", model.ModelID, model.Version, state, model.LicenseID)
	}
}
