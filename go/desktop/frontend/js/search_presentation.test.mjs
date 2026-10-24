import assert from 'node:assert/strict';
import test from 'node:test';
import {
  normalizeResearchSources,
  groupResearchSourcesByCall,
  safeSourceURL,
  safeThumbnailData,
  sourceEvidenceLabel,
} from './search_presentation.js';

test('only valid credential-free web source links become cards', () => {
  const sources = normalizeResearchSources([
    null, {}, { url: 'javascript:alert(1)' },
    { url: 'data:text/html,test' }, { url: '/relative-source' },
    { url: 'https://user:password@example.com/private' },
    { url: 'https://example.com/article', title: 'Original source' },
    { url: 'https://example.com/article', title: 'Duplicate result' },
  ]);
  assert.equal(sources.length, 1);
  assert.equal(sources[0].title, 'Original source');
  assert.equal(sources[0].url, 'https://example.com/article');
  assert.equal(safeSourceURL('https://example.com/' + 'x'.repeat(4096)), null);
});

test('presentation preserves source-controlled strings as text and bounded metadata', () => {
  const [source] = normalizeResearchSources([{
    url: 'https://www.example.com/source',
    title: '<img src=x onerror=alert(1)>',
    snippet: 'A <script>raw evidence</script> & a quote ".',
    contentKind: 'snippet',
    thumbnailURL: 'https://cdn.example.com/preview.jpg',
    fetchedAt: '2026-09-28T00:00:00Z',
    evidenceTruncated: true,
  }]);
  assert.equal(source.title, '<img src=x onerror=alert(1)>');
  assert.equal(source.excerpt, 'A <script>raw evidence</script> & a quote ".');
  assert.equal(source.host, 'example.com');
  assert.equal(source.thumbnailURL, 'https://cdn.example.com/preview.jpg');
  assert.equal(source.evidenceTruncated, true);
  assert.equal(sourceEvidenceLabel(source), '搜尋摘要');
  const [clipped] = normalizeResearchSources([{ url: source.url, snippet: 'x'.repeat(9000) }]);
  assert.equal(clipped.excerpt.length, 4000);
  assert.equal(clipped.snippetTruncated, true);
  assert.equal(clipped.evidenceTruncated, false);
  const [serverClipped] = normalizeResearchSources([{
    url: source.url, snippet: 'Server display excerpt', snippetTruncated: true,
  }]);
  assert.equal(serverClipped.snippetTruncated, true);
  assert.equal(serverClipped.evidenceTruncated, false);
});

test('the same URL may carry separate observations in different tool calls', () => {
  const groups = groupResearchSourcesByCall([
    { callId: 'call-1', url: 'https://example.com/a', snippet: 'First observation' },
    { callId: 'call-1', url: 'https://example.com/a', snippet: 'Duplicate' },
    { callId: 'call-2', url: 'https://example.com/a', snippet: 'Later observation' },
    { url: 'https://example.com/legacy', title: 'Earlier session source' },
    { callId: 'unsafe', url: 'javascript:alert(1)' },
  ]);
  assert.deepEqual(groups.map(group => [group.callId, group.sources.length]), [
    ['call-1', 1], ['call-2', 1], ['history', 1],
  ]);
  assert.equal(groups[1].sources[0].excerpt, 'Later observation');
});

test('reused call IDs from distinct runs preserve the run used by preview authorization', () => {
  const groups = groupResearchSourcesByCall([
    { runId: 'run-a', callId: 'call_1', url: 'https://example.com/a', snippet: 'Earlier' },
    { runId: 'run-b', callId: 'call_1', url: 'https://example.com/a', snippet: 'Later' },
  ]);
  assert.equal(groups.length, 2);
  assert.equal(groups[0].runId, 'run-a');
  assert.equal(groups[0].sources[0].runId, 'run-a');
  assert.equal(groups[1].sources[0].excerpt, 'Later');
});

test('preview authorization retains the exact validated receipt URL across normalization', () => {
  for (const receiptURL of [
    'https://example.com',
    'https://example.com:443/path',
    'https://例子.測試/資料',
  ]) {
    const [source] = normalizeResearchSources([{ url: receiptURL, runId: '' }]);
    assert.equal(source.url, new URL(receiptURL).href);
    assert.equal(source.receiptURL, receiptURL);
    assert.equal(source.runId, '');
    assert.equal(normalizeResearchSources([source])[0].receiptURL, receiptURL);
  }
  const [source] = normalizeResearchSources([{
    url: 'https://example.com', receiptURL: 'https://unrelated.example/private',
  }]);
  assert.equal(source.receiptURL, 'https://example.com');
});

test('unknown or failed retrievals never acquire a page-read claim', () => {
  assert.equal(sourceEvidenceLabel({ contentKind: 'unknown', excerpt: '' }), '來源連結');
  assert.equal(sourceEvidenceLabel({ contentKind: 'unknown', excerpt: 'Received text' }), '工具回傳內容');
  assert.equal(sourceEvidenceLabel({ contentKind: 'snippet', excerpt: 'Discovery result' }), '搜尋摘要');
  assert.equal(sourceEvidenceLabel({ contentKind: 'page', fetchError: 'timeout' }), '頁面擷取未成功');
  assert.equal(sourceEvidenceLabel({ contentKind: 'page' }), '頁面擷取內容');
  assert.equal(sourceEvidenceLabel({ contentKind: 'body' }), '頁面擷取內容');
});

test('preview images are bounded JPEG data only, never external URLs or SVG', () => {
  assert.equal(safeThumbnailData('https://example.com/image.jpg'), null);
  assert.equal(safeThumbnailData('data:image/svg+xml;base64,PHN2Zz4='), null);
  assert.equal(safeThumbnailData('data:image/jpeg;base64,abcd" onerror="x'), null);
  assert.equal(safeThumbnailData('data:image/jpeg;base64,' + 'A'.repeat(33000)), null);
  assert.equal(safeThumbnailData('data:image/jpeg;base64,/9j/4AAQ=='), 'data:image/jpeg;base64,/9j/4AAQ==');
});
