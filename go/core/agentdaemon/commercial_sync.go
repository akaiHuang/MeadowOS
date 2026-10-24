package agentdaemon

import (
	"context"
	"sort"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/productauth"
)

// Accepted bindings are the durable outbox. Restarting the application or a
// failed receipt request never forgets an accepted prompt or charges it twice.
// This worker settles existing authorizations only; it cannot authorize work.
func (bridge *commercialBridge) kickConsumptionSync(principal string) {
	bridge.kickConsumptionWorker(principal, false)
}

// A new durable acceptance must not lose its wake-up when the current worker
// has already taken its final pending snapshot. Passive usage polls do not
// extend the bounded retry budget.
func (bridge *commercialBridge) kickAcceptedConsumptionSync(principal string) {
	bridge.kickConsumptionWorker(principal, true)
}

// Called only with the trusted native auth service's freshly returned status.
// This resumes the durable outbox after restart without relying on the browser
// to acknowledge a response. The worker still verifies identity for each call.
func (bridge *commercialBridge) resumeConsumption(status productauth.Status) {
	if principal, _, err := bridge.principalFromStatus(status, nil); err == nil {
		bridge.kickConsumptionSync(principal)
		if bridge.promptGrants != nil {
			bridge.promptGrants.kickPrepare()
		}
	}
}

func (bridge *commercialBridge) kickConsumptionWorker(principal string, accepted bool) {
	if len(bridge.store.pendingConsumption(principal)) == 0 {
		return
	}
	bridge.syncMu.Lock()
	if bridge.syncing == nil {
		bridge.syncing = make(map[string]bool)
	}
	if bridge.syncing[principal] {
		if accepted {
			if bridge.syncAgain == nil {
				bridge.syncAgain = make(map[string]bool)
			}
			bridge.syncAgain[principal] = true
		}
		bridge.syncMu.Unlock()
		return
	}
	bridge.syncing[principal] = true
	bridge.syncMu.Unlock()
	go func() {
		defer func() {
			bridge.syncMu.Lock()
			delete(bridge.syncing, principal)
			again := bridge.syncAgain[principal]
			delete(bridge.syncAgain, principal)
			bridge.syncMu.Unlock()
			if again {
				bridge.kickConsumptionSync(principal)
			}
		}()
		for attempt := 0; attempt < 3; attempt++ {
			if attempt > 0 {
				time.Sleep(time.Duration(attempt*2) * time.Second)
			}
			pending := bridge.store.pendingConsumption(principal)
			if len(pending) == 0 {
				return
			}
			for _, binding := range pending {
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				if binding.PromptGrantID != "" {
					if bridge.promptGrants == nil {
						cancel()
						return
					}
					err := bridge.promptGrants.settle(ctx, binding)
					cancel()
					if err != nil {
						break
					}
					continue
				}
				current, _, err := bridge.principalAccess(ctx)
				if err != nil || current != principal {
					cancel()
					return
				}
				receipt, err := bridge.service.CommercialConsumePrompt(ctx, productauth.CommercialConsumeRequest{
					RequestID: binding.RequestID, AuthorizationID: binding.AuthorizationID,
				})
				current, _, identityErr := bridge.principalAccess(ctx)
				cancel()
				if identityErr != nil || current != principal {
					return
				}
				if err != nil || !receipt.Consumed || receipt.AuthorizationID != binding.AuthorizationID ||
					receipt.Schema != productauth.CommercialSchemaV2 || receipt.PolicyVersion != 2 || productauth.ValidateCommercialQuota(receipt.Usage) != nil {
					break
				}
				if bridge.store.markConsumed(binding.AuthorizationID) != nil {
					return
				}
			}
		}
	}()
}

func (store *commercialBindingStore) pendingConsumption(principal string) []commercialBinding {
	store.mu.Lock()
	defer store.mu.Unlock()
	rows := []commercialBinding{}
	for _, binding := range store.bindings {
		if binding.PrincipalKey == principal && binding.Scope == "prompt" && binding.Accepted && !binding.Consumed {
			rows = append(rows, *binding)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].RequestID < rows[j].RequestID })
	if len(rows) > 16 {
		rows = rows[:16]
	}
	return rows
}
