import test from 'node:test';
import assert from 'node:assert/strict';
import { createPromptGrantWarmup } from './prompt_grant.js';
const grant = () => ({schema:'meadow.desktop-prompt-grant.v1', ready:true, available:1, expires_at:new Date(Date.now()+60000).toISOString()});

test('warmup coalesces, caches until debit, and never revives a signed-out account', async () => {
  let key = 'a', calls = 0, finish;
  const warmup = createPromptGrantWarmup({identity:()=>key, prepare:()=>{ calls++; return new Promise(r=>finish=r); }});
  const a = warmup.warm(), b = warmup.warm();
  assert.equal(calls, 1); finish(grant());
  assert.equal((await a).available, 1); await b; await warmup.warm(); assert.equal(calls, 1);
  warmup.debit(); const c = warmup.warm(); assert.equal(calls, 2);
  key = ''; warmup.invalidate(); finish(grant()); assert.equal(await c, null);
  key = 'b'; const d = warmup.warm(); assert.equal(calls, 3); finish(grant());
  assert.equal((await d).key, 'b');
});

test('a pending pre-debit projection cannot restore spent readiness', async () => {
  let calls=0, finish;
  const warmup=createPromptGrantWarmup({identity:()=>'a', prepare:()=>{ calls++; return new Promise(r=>finish=r); }});
  const a=warmup.warm(); warmup.debit(); finish(grant()); assert.equal(await a,null);
  const b=warmup.warm(); assert.equal(calls,2); finish(grant()); await b;
});

test('unbounded or expired capacity cannot be advertised', async () => {
  for (const value of [{...grant(),available:100}, {...grant(),expires_at:'2000-01-01'}]) {
    const warmup=createPromptGrantWarmup({identity:()=>'a',prepare:async()=>value});
    await assert.rejects(warmup.warm());
  }
});
