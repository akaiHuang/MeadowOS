package productauth

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/licensing"
)

const (
	deviceProofSeedKey = "product-auth-device-proof-seed-v1"
	storedLeaseKey     = "product-auth-signed-lease-v1"
	maximumLeaseLife   = 24 * time.Hour
)

type verifiedLease struct {
	KeyID              string
	Subject            string
	LeaseID            string
	DeviceID           string
	Plan               Plan
	Capabilities       Capabilities
	ExpiresAt          time.Time
	EffectiveExpiresAt time.Time
	OfflineGrace       bool
	RevocationEpoch    uint64
}

type leaseClaims struct {
	Schema          string       `json:"schema"`
	LeaseVersion    uint64       `json:"lease_version"`
	Product         string       `json:"product"`
	Issuer          string       `json:"iss"`
	Subject         string       `json:"sub"`
	Audience        string       `json:"aud"`
	LeaseID         string       `json:"jti"`
	IssuedAt        int64        `json:"iat"`
	ExpiresAt       int64        `json:"exp"`
	Status          string       `json:"status"`
	Plan            Plan         `json:"plan"`
	Capabilities    Capabilities `json:"capabilities"`
	Device          leaseDevice  `json:"device"`
	RevocationEpoch uint64       `json:"revocation_epoch"`
	KeyID           string       `json:"kid"`
}

type leaseDevice struct {
	ID                 string `json:"id"`
	InstallationDigest string `json:"installation_digest"`
	Platform           string `json:"platform"`
	ClientName         string `json:"client_name"`
	KeyThumbprint      string `json:"key_thumbprint"`
}

func (service *Service) verifySignedLease(ctx context.Context, compact string, binding licensing.DeviceBinding, devicePublicKey string) (verifiedLease, error) {
	parts := strings.Split(compact, ".")
	if len(parts) != 3 || len(compact) > 32<<10 {
		return verifiedLease{}, authError("lease_rejected", "signed lease is invalid", http.StatusForbidden, false, nil)
	}
	headerBytes, err := decodeCanonicalBase64URL(parts[0], 1, 1024)
	if err != nil {
		return verifiedLease{}, authError("lease_rejected", "signed lease header is invalid", http.StatusForbidden, false, err)
	}
	var header struct {
		Algorithm string `json:"alg"`
		KeyID     string `json:"kid"`
		Type      string `json:"typ"`
	}
	if strictJSON(headerBytes, &header) != nil || header.Algorithm != "EdDSA" || header.Type != LeaseTypeV1 || !validOpaque(header.KeyID, 128) {
		return verifiedLease{}, authError("lease_rejected", "signed lease header is unsupported", http.StatusForbidden, false, nil)
	}
	key := service.leaseKeys[header.KeyID]
	if len(key) != ed25519.PublicKeySize {
		return verifiedLease{}, authError("unknown_lease_key", "signed lease key is not trusted", http.StatusForbidden, false, nil)
	}
	signature, err := decodeCanonicalBase64URL(parts[2], ed25519.SignatureSize, ed25519.SignatureSize)
	if err != nil || !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), signature) {
		return verifiedLease{}, authError("lease_rejected", "signed lease signature is invalid", http.StatusForbidden, false, err)
	}
	payload, err := decodeCanonicalBase64URL(parts[1], 1, 24<<10)
	if err != nil {
		return verifiedLease{}, authError("lease_rejected", "signed lease claims are invalid", http.StatusForbidden, false, err)
	}
	var claims leaseClaims
	if strictJSON(payload, &claims) != nil {
		return verifiedLease{}, authError("lease_rejected", "signed lease claims are invalid", http.StatusForbidden, false, nil)
	}
	if claims.Schema != SchemaV1 || claims.LeaseVersion != service.leaseVersion || claims.Product != service.product ||
		claims.Issuer != service.client.Issuer() || claims.Audience != service.client.Audience() ||
		claims.Status != LeaseStatusOK || claims.KeyID != header.KeyID || claims.Subject == "" ||
		claims.LeaseID == "" || claims.LeaseID != claims.Subject || claims.IssuedAt <= 0 || claims.ExpiresAt <= claims.IssuedAt {
		return verifiedLease{}, authError("lease_rejected", "signed lease claims are outside policy", http.StatusForbidden, false, nil)
	}
	if claims.Plan != PlanGeneral && claims.Plan != PlanPro {
		return verifiedLease{}, authError("unknown_plan", "signed lease plan is unsupported", http.StatusForbidden, false, nil)
	}
	if err := claims.Capabilities.Validate(claims.Plan); err != nil {
		return verifiedLease{}, authError("unknown_capability", "signed lease capabilities are unsupported", http.StatusForbidden, false, err)
	}
	if binding.Algorithm != licensing.DeviceBindingSHA256 || binding.Digest != service.installationID {
		return verifiedLease{}, authError("device_mismatch", "signed lease device binding is invalid", http.StatusForbidden, false, nil)
	}
	expectedInstallation := domainDigestHex("meadow.installation.public.v1", service.installationID)
	expectedThumbprint := domainDigestHex("meadow.device-auth-key.v1", devicePublicKey)
	if claims.Device.ID == "" || claims.Device.InstallationDigest != expectedInstallation ||
		claims.Device.KeyThumbprint != expectedThumbprint || claims.Device.Platform != service.platform ||
		claims.Device.ClientName != service.clientName {
		return verifiedLease{}, authError("device_mismatch", "signed lease device binding does not match this installation", http.StatusForbidden, false, nil)
	}
	now := service.now().UTC()
	issuedAt := time.Unix(claims.IssuedAt, 0).UTC()
	expiresAt := time.Unix(claims.ExpiresAt, 0).UTC()
	effectiveExpiresAt := expiresAt.Add(service.maxOfflineGrace)
	if issuedAt.After(now.Add(service.futureClockSkew)) || !effectiveExpiresAt.After(now) || expiresAt.Sub(issuedAt) > maximumLeaseLife {
		return verifiedLease{}, authError("lease_expired", "signed lease is expired or outside timing policy", http.StatusUnauthorized, false, nil)
	}
	if err := service.advanceAntiRollback(ctx, now.Unix(), claims.RevocationEpoch); err != nil {
		return verifiedLease{}, err
	}
	return verifiedLease{
		KeyID: header.KeyID, Subject: claims.Subject, LeaseID: claims.LeaseID, DeviceID: claims.Device.ID,
		Plan: claims.Plan, Capabilities: claims.Capabilities, ExpiresAt: expiresAt,
		EffectiveExpiresAt: effectiveExpiresAt, OfflineGrace: !expiresAt.After(now),
		RevocationEpoch: claims.RevocationEpoch,
	}, nil
}

func (service *Service) advanceAntiRollback(ctx context.Context, observed int64, epoch uint64) error {
	for attempt := 0; attempt < 4; attempt++ {
		current, err := service.store.LoadAntiRollback(ctx, service.product)
		var previous *licensing.AntiRollbackState
		if errors.Is(err, licensing.ErrAntiRollbackStateNotFound) {
			current = licensing.AntiRollbackState{}
		} else if err != nil {
			return authError("anti_rollback_rejected", "authorization anti-rollback state is unavailable", http.StatusForbidden, false, err)
		} else {
			copy := current
			previous = &copy
		}
		if epoch < current.RevocationEpoch || observed+int64(service.clockRollbackTolerance/time.Second) < current.ObservedUnix {
			return authError("anti_rollback_rejected", "signed lease rollback was rejected", http.StatusForbidden, false, nil)
		}
		next := current
		if observed > next.ObservedUnix {
			next.ObservedUnix = observed
		}
		if epoch > next.RevocationEpoch {
			next.RevocationEpoch = epoch
		}
		if previous != nil && next == current {
			return nil
		}
		if err := service.store.CompareAndSwapAntiRollback(ctx, service.product, previous, next); errors.Is(err, licensing.ErrAntiRollbackConflict) {
			continue
		} else if err != nil {
			return authError("anti_rollback_rejected", "authorization anti-rollback state could not be stored", http.StatusForbidden, false, err)
		}
		return nil
	}
	return authError("anti_rollback_rejected", "authorization anti-rollback state changed concurrently", http.StatusForbidden, false, nil)
}

func (service *Service) deviceProofKey(ctx context.Context) (ed25519.PrivateKey, string, error) {
	seed, err := service.store.LoadSecureValue(ctx, deviceProofSeedKey)
	if errors.Is(err, licensing.ErrSecureStoreNotFound) {
		seed = make([]byte, ed25519.SeedSize)
		if err := service.random(seed); err != nil {
			return nil, "", authError("device_binding_unavailable", "device proof key could not be created", http.StatusServiceUnavailable, false, err)
		}
		if err := service.store.StoreSecureValues(ctx, map[string][]byte{deviceProofSeedKey: seed}); err != nil {
			clear(seed)
			return nil, "", authError("keychain_unavailable", "device proof key could not be stored", http.StatusServiceUnavailable, false, err)
		}
	} else if err != nil {
		return nil, "", authError("keychain_unavailable", "device proof key is unavailable", http.StatusServiceUnavailable, false, err)
	}
	defer clear(seed)
	if len(seed) != ed25519.SeedSize {
		return nil, "", authError("device_binding_unavailable", "device proof key is invalid", http.StatusServiceUnavailable, false, nil)
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	return privateKey, base64.RawURLEncoding.EncodeToString(publicKey), nil
}

func signDeviceProof(privateKey ed25519.PrivateKey, action, credential, installationID, requestID, idempotencyKey string) string {
	payload := deviceProofPayload(action, credential, installationID, requestID, idempotencyKey, "")
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, []byte(payload)))
}

func signBoundDeviceProof(privateKey ed25519.PrivateKey, action, credential, installationID, requestID, idempotencyKey, binding string) string {
	payload := deviceProofPayload(action, credential, installationID, requestID, idempotencyKey, binding)
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, []byte(payload)))
}

// Keep action framing identical to the canonical membership deviceProofPayload.
// Binding-only actions have no empty idempotency field in their signed bytes.
func deviceProofPayload(action, credential, installationID, requestID, idempotencyKey, binding string) string {
	payload := "meadow-device-auth-" + action + "-v1\x00" + credential + "\x00" + installationID + "\x00" + requestID
	switch action {
	case "serial":
		return payload + "\x00" + idempotencyKey
	case "device_revoke":
		return payload + "\x00" + idempotencyKey + "\x00" + binding
	case "conversation_authorize", "prompt_authorize", "prompt_consume", "referral_accept",
		"model_access_status", "model_access_key", "model_access_usage", "telemetry_ingest",
		"prompt_grant_settle", "prompt_grant_close":
		return payload + "\x00" + binding
	default:
		return payload
	}
}

func domainDigestHex(domain, value string) string {
	digest := sha256.Sum256([]byte(domain + "\x00" + value))
	return hex.EncodeToString(digest[:])
}

func decodeCanonicalBase64URL(value string, minimum, maximum int) ([]byte, error) {
	if value == "" || strings.Contains(value, "=") {
		return nil, errors.New("base64url value is not canonical")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) < minimum || len(decoded) > maximum || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return nil, errors.New("base64url value is invalid")
	}
	return decoded, nil
}

func strictJSON(payload []byte, output any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON data")
	}
	return nil
}
