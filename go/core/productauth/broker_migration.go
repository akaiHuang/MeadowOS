package productauth

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/hey-meadow/meadow-code/go/core/keybroker"
)

// BrokerAuthorizationResource is the single product-auth record eligible for
// the explicit, operator-controlled update migration. Other namespaces and
// resources are never inspected or copied.
const BrokerAuthorizationResource = "product-auth-meadow"

type BrokerMigrationReceipt struct {
	Outcome            string            `json:"outcome"` // not_copied, copied, or unknown after an ambiguous Keychain write
	ErrorCode          string            `json:"error_code,omitempty"`
	Copied             bool              `json:"copied"`
	LeaseCurrent       bool              `json:"lease_current"`
	SourceVersion      keybroker.Version `json:"source_version"`
	DestinationVersion keybroker.Version `json:"destination_version"`
}

type brokerMigrationState interface {
	Current(context.Context, string) (*keybroker.Lease, error)
	Replace(context.Context, string, keybroker.Version, []byte) (keybroker.Version, error)
}

type brokerMigrationFailure string

func (failure brokerMigrationFailure) Error() string {
	return "productauth migration: " + string(failure)
}

func migrationError(code string) error { return brokerMigrationFailure(code) }

func migrationReadError(stage string, err error) error {
	switch {
	case errors.Is(err, keybroker.ErrNotFound):
		return migrationError(stage + "_missing")
	case errors.Is(err, keybroker.ErrInteractionRequired):
		return migrationError(stage + "_interaction_required")
	default:
		return migrationError(stage + "_unavailable")
	}
}

// The audited pre-update observer uses Python json.dumps(sort_keys=True,
// separators=(',', ':')) with default ensure_ascii=True. Match those exact
// bytes, including Unicode surrogate pairs, without constraining opaque IDs to
// a guessed shape or using Go's default HTML escaping for '<', '>' and '&'.
func migrationIdentityJSON(subject, deviceID string) []byte {
	quote := func(value string) string {
		var jsonValue bytes.Buffer
		encoder := json.NewEncoder(&jsonValue)
		encoder.SetEscapeHTML(false)
		_ = encoder.Encode(value) // Encoding a string cannot fail for this buffer.
		var ascii strings.Builder
		for _, char := range strings.TrimSuffix(jsonValue.String(), "\n") {
			switch {
			case char < 0x7f:
				ascii.WriteRune(char)
			case char <= 0xffff:
				fmt.Fprintf(&ascii, `\u%04x`, char)
			default:
				high, low := utf16.EncodeRune(char)
				fmt.Fprintf(&ascii, `\u%04x\u%04x`, high, low)
			}
		}
		return ascii.String()
	}
	return []byte(`{"device_id":` + quote(deviceID) + `,"subject":` + quote(subject) + `}`)
}

// MigrateBrokerAuthorization copies one exact opaque snapshot into an absent
// destination using CAS version zero. The caller must first attest the exact
// source executable/namespace and destination policy. This does not log in,
// refresh, delete the old record, or treat an expired lease as authorization.
// Verification and anti-rollback checks operate only on an in-memory snapshot.
// The caller must keep the source writer stopped throughout this operation;
// distinct Keychain namespaces do not support one cross-namespace transaction.
func MigrateBrokerAuthorization(ctx context.Context, source, destination *keybroker.LicenseDeviceState, config BuildConfig, deps Dependencies, expectedIdentitySHA256 string) (BrokerMigrationReceipt, error) {
	if ctx == nil || ctx.Err() != nil || source == nil || destination == nil || source == destination {
		return BrokerMigrationReceipt{Outcome: "not_copied", ErrorCode: "invalid_request"}, migrationError("invalid_request")
	}
	receipt, err := migrateBrokerAuthorization(ctx, source, destination, config, deps, expectedIdentitySHA256)
	var failure brokerMigrationFailure
	if errors.As(err, &failure) {
		receipt.ErrorCode = string(failure)
	}
	return receipt, err
}

func migrateBrokerAuthorization(ctx context.Context, source, destination brokerMigrationState, config BuildConfig, deps Dependencies, expectedIdentitySHA256 string) (BrokerMigrationReceipt, error) {
	receipt := BrokerMigrationReceipt{Outcome: "not_copied"}
	expected, err := hex.DecodeString(expectedIdentitySHA256)
	if err != nil || len(expected) != sha256.Size || hex.EncodeToString(expected) != expectedIdentitySHA256 {
		return receipt, migrationError("invalid_expected_identity")
	}
	if existing, err := destination.Current(ctx, BrokerAuthorizationResource); !errors.Is(err, keybroker.ErrNotFound) {
		if existing != nil {
			existing.Destroy()
		}
		if err != nil {
			return receipt, migrationReadError("destination", err)
		}
		return receipt, migrationError("destination_not_empty")
	}
	lease, err := source.Current(ctx, BrokerAuthorizationResource)
	if err != nil {
		return receipt, migrationReadError("source", err)
	}
	defer lease.Destroy()
	receipt.SourceVersion = lease.Version()
	err = lease.WithBytes(func(payload []byte) error {
		if len(payload) == 0 || len(payload) > 1<<20 {
			return migrationError("source_invalid")
		}
		var doc brokerStateDocument
		if strictJSON(payload, &doc) != nil || doc.Schema != brokerStateSchema || doc.Refresh == "" || len(doc.Refresh) > 8192 || doc.Lease == "" || len(doc.Lease) > 32<<10 || doc.Denial != "" || doc.RegisteredGeneral {
			return migrationError("source_invalid")
		}
		snapshot := NewMemoryStore()
		defer func() {
			for _, value := range snapshot.values {
				clear(value)
			}
			snapshot.refresh, snapshot.lease, snapshot.denial = "", "", ""
			clear(snapshot.values)
			clear(snapshot.rollback)
		}()
		snapshot.refresh, snapshot.lease = doc.Refresh, doc.Lease
		for key, encoded := range doc.Values {
			value, decodeErr := base64.RawStdEncoding.DecodeString(encoded)
			if decodeErr != nil || base64.RawStdEncoding.EncodeToString(value) != encoded {
				clear(value)
				return migrationError("source_invalid")
			}
			snapshot.values[key] = value
		}
		if len(snapshot.values[deviceProofSeedKey]) != ed25519.SeedSize {
			return migrationError("device_proof_missing")
		}
		for product, rollback := range doc.Rollback {
			snapshot.rollback[product] = rollback
		}
		deps.Store = snapshot
		service, serviceErr := NewService(config, deps)
		if serviceErr != nil {
			return migrationError("configuration_invalid")
		}
		status, statusErr := service.Status(ctx)
		if statusErr != nil {
			return migrationError("source_verification_failed")
		}
		var subject, deviceID string
		if status.Authenticated && status.Entitlement != nil {
			subject, deviceID = status.Subject, status.Entitlement.DeviceID
			receipt.LeaseCurrent = true
		} else if status.Reason == "lease_expired" {
			// verifySignedLease checks signature, policy and device binding before
			// returning lease_expired. Its timing code also covers future issuance
			// and oversized lifetimes: those are not eligible historical snapshots.
			parts := strings.Split(doc.Lease, ".")
			if len(parts) != 3 {
				return migrationError("source_verification_failed")
			}
			claimsJSON, decodeErr := decodeCanonicalBase64URL(parts[1], 1, 24<<10)
			var claims leaseClaims
			if decodeErr != nil || strictJSON(claimsJSON, &claims) != nil {
				return migrationError("source_verification_failed")
			}
			now := service.now().UTC()
			issued, expires := time.Unix(claims.IssuedAt, 0), time.Unix(claims.ExpiresAt, 0)
			if issued.After(now.Add(service.futureClockSkew)) || expires.Sub(issued) > maximumLeaseLife || expires.Add(service.maxOfflineGrace).After(now) || service.advanceAntiRollback(ctx, now.Unix(), claims.RevocationEpoch) != nil {
				return migrationError("source_verification_failed")
			}
			subject, deviceID = claims.Subject, claims.Device.ID
		} else {
			return migrationError("source_verification_failed")
		}
		identity := migrationIdentityJSON(subject, deviceID)
		actual := sha256.Sum256(identity)
		clear(identity)
		if subtle.ConstantTimeCompare(actual[:], expected) != 1 {
			return migrationError("identity_mismatch")
		}
		if ctx.Err() != nil {
			return migrationError("cancelled")
		}
		current, currentErr := source.Current(ctx, BrokerAuthorizationResource)
		if currentErr != nil {
			return migrationError("source_changed_or_unavailable")
		}
		unchanged := current.Version() == receipt.SourceVersion
		current.Destroy()
		if !unchanged {
			return migrationError("source_changed_or_unavailable")
		}
		version, writeErr := destination.Replace(ctx, BrokerAuthorizationResource, 0, payload)
		if writeErr != nil {
			if errors.Is(writeErr, keybroker.ErrVersionConflict) {
				return migrationError("destination_cas_failed")
			}
			// Security.framework may commit a Keychain add just before cancellation
			// or a response failure is observed. Never report definitive no-write,
			// retry blindly, overwrite, or remove a possibly committed destination.
			receipt.Outcome = "unknown"
			return migrationError("destination_write_outcome_unknown")
		}
		receipt.Outcome, receipt.Copied, receipt.DestinationVersion = "copied", true, version
		return nil
	})
	return receipt, err
}
