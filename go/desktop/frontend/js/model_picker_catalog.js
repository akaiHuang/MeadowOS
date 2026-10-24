// Native storage contains only models previously verified for each connection.
// A model ID alone is not unique across providers; keep the connection identity.
export function configuredPickerRows(liveRows, catalog) {
  const live = Array.isArray(liveRows) ? liveRows : [];
  if (catalog?.mode !== 'connections' || !Array.isArray(catalog.connections)) return live;
  const rows = [];
  for (const connection of catalog.connections) {
    if (typeof connection?.id !== 'string' || !Array.isArray(connection.models)) continue;
    for (const model of connection.models) {
      if (typeof model?.id !== 'string' || !model.id || model.enabled === false) continue;
      const selected = connection.id === catalog.selectedConnectionId;
      const current = selected ? live.find((row) => row.modelId === model.id) : null;
      rows.push({
        ...(current || {}), modelId: model.id, name: model.name || model.id,
        connectionId: connection.id, provider: connection.name,
        delivery: 'api', multimodal: current?.multimodal ?? model.supportsImages === true,
        active: selected && (current?.active === true || model.id === catalog.selectedModelId),
        live: selected && current?.live === true,
        description: connection.name || '模型 API',
      });
    }
  }
  // An explicitly empty/unchecked catalog must not resurrect cached live rows.
  return rows;
}
