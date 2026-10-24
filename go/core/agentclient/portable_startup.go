//go:build !meadow_protected

package agentclient

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/buildinfo"
	"github.com/hey-meadow/meadow-code/go/core/daemontransport"
	"github.com/hey-meadow/meadow-code/go/core/modelconnection"
	"github.com/hey-meadow/meadow-code/go/core/session"
)

// Control requests attach to the same authenticated helper even when Desktop
// has changed its model selection. A cold start still explicitly selects the
// profile's API catalog, including a fresh profile with no catalog yet.
func portableControlLaunchConfig(config daemontransport.LaunchConfig) (daemontransport.LaunchConfig, error) {
	if buildinfo.DesktopPackageMode != "portable" {
		return config, nil
	}
	config, err := portableProfileLaunchConfig(config)
	if err != nil {
		return daemontransport.LaunchConfig{}, err
	}
	config.AttachOnly = true
	config.CoreBaseURL, config.ModelID = "", ""
	config.ConnectionID, config.ConnectionFingerprint = "", ""
	config.HarnessMode, config.ApprovalMode = "", ""
	return config, nil
}

func portableProfileLaunchConfig(config daemontransport.LaunchConfig) (daemontransport.LaunchConfig, error) {
	root, err := session.PortableStateRoot()
	if err != nil {
		return daemontransport.LaunchConfig{}, err
	}
	config.StateRoot = root
	config.ConnectionFile = filepath.Join(root, "model-connections-v1.json")
	return config, nil
}

func portableRuntimeLaunchConfig(config daemontransport.LaunchConfig) (daemontransport.LaunchConfig, error) {
	if buildinfo.DesktopPackageMode != "portable" {
		return config, nil
	}
	if strings.TrimSpace(config.CoreBaseURL) != "" {
		return daemontransport.LaunchConfig{}, fmt.Errorf("portable Meadow uses the Desktop model connection; Core URL overrides are unavailable")
	}
	config, err := portableProfileLaunchConfig(config)
	if err != nil {
		return daemontransport.LaunchConfig{}, err
	}
	config.AttachOnly = false
	catalog, err := modelconnection.Load(config.ConnectionFile)
	if os.IsNotExist(err) {
		catalog, err = &modelconnection.Catalog{Version: modelconnection.Version}, nil
	}
	if err != nil {
		return daemontransport.LaunchConfig{}, fmt.Errorf("read Desktop model connections: %w", err)
	}
	config.ConnectionID = ""
	config.ConnectionFingerprint = ""
	requestedModel := strings.TrimSpace(config.ModelID)
	if len(catalog.Connections) > 0 {
		config.ConnectionFingerprint = catalog.Fingerprint()
	}
	if catalog.SelectedConnectionID == "" {
		if requestedModel != "" && requestedModel != "unconfigured" {
			return daemontransport.LaunchConfig{}, fmt.Errorf("configure a model connection in Meadow Desktop before selecting a model")
		}
		config.ModelID = "unconfigured"
		return config, nil
	}
	connection, selectedModel, err := catalog.Resolve()
	if err != nil {
		return daemontransport.LaunchConfig{}, err
	}
	config.ConnectionID = connection.ID
	config.ModelID = selectedModel.ID
	if requestedModel != "" {
		if _, ok := connection.LookupModel(requestedModel); !ok {
			return daemontransport.LaunchConfig{}, fmt.Errorf("requested model is unavailable on the selected Desktop connection")
		}
		config.ModelID = requestedModel
	}
	return config, nil
}
