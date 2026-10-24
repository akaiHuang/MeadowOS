package buildinfo

import (
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
)

const ProtectedCompositionSchema = "meadow.protected-release-composition.v1"

type ProtectedCompositionInfo struct {
	Schema             string `json:"schema"`
	Protected          bool   `json:"protected"`
	BuildTag           string `json:"build_tag"`
	ProductAuthOrigin  string `json:"product_auth_origin"`
	AuthorizationOwner string `json:"authorization_owner"`
}

func ProtectedComposition() (ProtectedCompositionInfo, error) {
	if protectedBuildTag != "meadow_protected" {
		return ProtectedCompositionInfo{}, errors.New("protected build tag is unavailable")
	}
	origin := strings.TrimSpace(ProtectedProductAuthOrigin)
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" {
		return ProtectedCompositionInfo{}, errors.New("protected product-auth origin is unavailable")
	}
	return ProtectedCompositionInfo{
		Schema: ProtectedCompositionSchema, Protected: true,
		BuildTag: protectedBuildTag, ProductAuthOrigin: origin,
		AuthorizationOwner: "meadow-agentd",
	}, nil
}

func WriteProtectedComposition(output io.Writer) error {
	if output == nil {
		return errors.New("protected composition output is unavailable")
	}
	composition, err := ProtectedComposition()
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(composition)
}
