// Human-readable public search metadata only. Never render source, guidance,
// retrieval internals or HTML from a model/tool payload.
function bounded(value, limit) {
  if (typeof value !== 'string') return '';
  const chars = Array.from(value.replace(/\s+/g, ' ').trim());
  return chars.length > limit ? chars.slice(0, limit).join('') + '…' : chars.join('');
}

export function artifactSearchPresentation(output) {
  let value = output;
  if (typeof value === 'string') {
    if (value.length > 32768) return { summary: '搜尋結果過大', text: '結果超過顯示上限，請縮小搜尋範圍。' };
    try { value = JSON.parse(value); } catch { return null; }
  }
  if (!value || typeof value !== 'object' || !Array.isArray(value.results) ||
      (value.kind !== 'artifact_search_result' && value.source_code_included !== false)) return null;
  const hits = value.results.slice(0, 3).filter((hit) => hit && typeof hit === 'object');
  const count = Number.isSafeInteger(value.result_count) && value.result_count >= hits.length
    ? value.result_count : value.results.length;
  if (count === 0) return { summary: '未找到相符素材', text: '未找到相符素材。可以調整搜尋條件後再試。' };
  const lines = [`找到 ${count} 項素材`];
  for (const [index, hit] of hits.entries()) {
    const title = bounded(hit.title, 64) || '未命名素材';
    const id = bounded(hit.asset_id, 128);
    const description = bounded(hit.description, 120);
    lines.push(`${index + 1}. ${title}${id ? `\nID：${id}` : ''}${description ? `\n${description}` : ''}`);
  }
  if (count > hits.length) lines.push(`顯示 ${hits.length} 項，其餘結果已省略。`);
  return { summary: `找到 ${count} 項素材`, text: lines.join('\n\n') };
}
