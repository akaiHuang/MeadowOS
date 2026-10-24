import { test } from 'node:test';
import assert from 'node:assert/strict';
import { capabilitiesForPlan, parseStatus } from './commercial_contract.js';

function generalStatus(limit) {
  return {
    schema: 'meadow.product-auth.v1',
    state: 'authorized_general',
    plan: 'general',
    authenticated: true,
    capabilities: { ...capabilitiesForPlan('general'), 'devices.max': limit },
    lease: {
      lease_id: 'signed-lease',
      device_id: 'same-installation',
      effective_expires_at: '2030-01-01T00:00:00Z',
      revocation_epoch: 1,
    },
  };
}

test('new General policy allows three devices and preserves the old signed limit', () => {
  assert.equal(capabilitiesForPlan('general')['devices.max'], 3);
  assert.equal(capabilitiesForPlan('pro')['devices.max'], 3);
  assert.equal(capabilitiesForPlan('guest')['devices.max'], 0);
  assert.equal(parseStatus(generalStatus(3)).capabilities['devices.max'], 3);
  assert.equal(parseStatus(generalStatus(1)).capabilities['devices.max'], 1);
});

test('legacy compatibility accepts no arbitrary limits or paid capabilities', () => {
  for (const limit of [0, 2, 4, 16, '3', 1.5]) {
    assert.throws(() => parseStatus(generalStatus(limit)));
  }
  for (const limit of [1, 3]) {
    for (const field of ['model.35b', 'vision', 'agent.auto', 'commercial_use']) {
      const raw = generalStatus(limit);
      raw.capabilities[field] = true;
      assert.throws(() => parseStatus(raw));
    }
    const raw = generalStatus(limit);
    raw.capabilities.extra = false;
    assert.throws(() => parseStatus(raw));
  }
  assert.throws(() => parseStatus({ ...generalStatus(1), lease: undefined }));
});
