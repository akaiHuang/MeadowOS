(() => {
  if (window.__meadowWebMCPBridge) return;

  const MAX_TOOLS = 64;
  const MAX_NAME = 128;
  const MAX_TITLE = 256;
  const MAX_DESCRIPTION = 2048;
  const MAX_SCHEMA_JSON = 16384;
  const MAX_EXECUTION_MS = 10000;
  const registry = new Map();
  const registeredHandles = new WeakMap();
  let revision = 0;
  let executionInProgress = false;
  let activeExecutionController = null;
  const documentID = crypto.randomUUID();
  let implementation = 'compatibility';

  const copyJSON = (value, fallback) => {
    try {
      if (value === undefined) return fallback;
      return JSON.parse(JSON.stringify(value));
    } catch (_) {
      throw new TypeError('WebMCP metadata must be JSON-serializable');
    }
  };

  const boundedString = (value, max, field, required = false) => {
    const text = typeof value === 'string' ? value.trim() : '';
    if (required && !text) throw new TypeError(`WebMCP ${field} is required`);
    if (text.length > max) throw new RangeError(`WebMCP ${field} exceeds ${max} characters`);
    return text;
  };

  const normalizeTool = (tool) => {
    if (!tool || typeof tool !== 'object') throw new TypeError('WebMCP tool must be an object');
    const name = boundedString(tool.name, MAX_NAME, 'name', true);
    if (!/^[A-Za-z0-9._:-]+$/.test(name)) {
      throw new TypeError('WebMCP tool name contains unsupported characters');
    }
    if (registry.has(name)) throw new TypeError(`WebMCP tool already registered: ${name}`);
    if (registry.size >= MAX_TOOLS) throw new RangeError(`WebMCP tool limit is ${MAX_TOOLS}`);
    if (typeof tool.execute !== 'function') throw new TypeError('WebMCP tool execute must be a function');

    const inputSchema = copyJSON(tool.inputSchema, {type: 'object'});
    if (!inputSchema || typeof inputSchema !== 'object' || Array.isArray(inputSchema)) {
      throw new TypeError('WebMCP inputSchema must be an object');
    }
    const schemaJSON = JSON.stringify(inputSchema);
    if (schemaJSON.length > MAX_SCHEMA_JSON) {
      throw new RangeError(`WebMCP inputSchema exceeds ${MAX_SCHEMA_JSON} characters`);
    }
    const rawAnnotations = copyJSON(tool.annotations, {});
    const annotations = {
      readOnlyHint: rawAnnotations.readOnlyHint === true,
      destructiveHint: rawAnnotations.destructiveHint === true,
      idempotentHint: rawAnnotations.idempotentHint === true,
      openWorldHint: rawAnnotations.openWorldHint === true,
      consequentialHint: rawAnnotations.consequentialHint === true,
      untrustedContentHint: rawAnnotations.untrustedContentHint === true,
      debugging: rawAnnotations.debugging === true,
    };
    return {
      descriptor: {
        name,
        title: boundedString(tool.title, MAX_TITLE, 'title'),
        description: boundedString(tool.description, MAX_DESCRIPTION, 'description', true),
        inputSchema,
        annotations,
        state_change_declared: rawAnnotations.readOnlyHint === false &&
          rawAnnotations.consequentialHint === false &&
          rawAnnotations.destructiveHint !== true && rawAnnotations.openWorldHint !== true,
      },
      execute: tool.execute,
    };
  };

  const unregisterLocal = (name, entry) => {
    const key = String(name || '').trim();
    if (entry && registry.get(key) !== entry) return;
    if (registry.delete(key)) revision += 1;
  };

  const registerLocal = (entry, options = {}) => {
    const signal = options.signal;
    if (signal?.aborted) return;
    const name = entry.descriptor.name;
    if (registry.has(name)) throw new Error(`WebMCP tool already registered: ${name}`);
    if (registry.size >= MAX_TOOLS) throw new Error('WebMCP tool limit reached');
    registry.set(name, entry);
    revision += 1;
    signal?.addEventListener('abort', () => unregisterLocal(name, entry), {once: true});
  };

  const unsafeKeys = value => {
    if (!value || typeof value !== 'object') return false;
    return Object.keys(value).some(key =>
      key === '__proto__' || key === 'prototype' || key === 'constructor' || unsafeKeys(value[key]));
  };
  const sameJSON = (a, b) => {
    if (a === b) return true;
    if (!a || !b || typeof a !== 'object' || typeof b !== 'object') return false;
    if (Array.isArray(a) !== Array.isArray(b)) return false;
    const keys = Object.keys(a);
    return keys.length === Object.keys(b).length &&
      keys.every(key => Object.hasOwn(b, key) && sameJSON(a[key], b[key]));
  };
  // A bounded subset of JSON Schema. State-changing tools fail closed on
  // unsupported constraints instead of pretending that a schema was checked.
  const supportedKeywords = new Set(['type', 'properties', 'required', 'additionalProperties', 'items',
    'enum', 'const', 'minimum', 'maximum', 'exclusiveMinimum', 'exclusiveMaximum',
    'minLength', 'maxLength', 'minItems', 'maxItems', 'minProperties', 'maxProperties',
    'title', 'description', 'default', 'examples', '$schema', '$id', '$comment']);
  const schemaTypes = new Set(['object', 'array', 'integer', 'number', 'null', 'string', 'boolean']);
  const validateSchema = (schema, strict, depth = 0) => {
    if (depth > 24) throw new Error('WebMCP input schema exceeds validation depth');
    if (typeof schema === 'boolean') return;
    if (!schema || typeof schema !== 'object' || Array.isArray(schema)) throw new Error('WebMCP input schema must be an object or boolean');
    if (strict) for (const key of Object.keys(schema)) {
      if (!supportedKeywords.has(key)) throw new Error(`WebMCP input schema keyword is unsupported: ${key}`);
    }
    if (schema.type !== undefined) {
      const types = Array.isArray(schema.type) ? schema.type : [schema.type];
      if (!types.length || !types.every(type => schemaTypes.has(type))) throw new Error('WebMCP input schema has an invalid type');
    }
    if (schema.enum !== undefined && (!Array.isArray(schema.enum) || !schema.enum.length)) throw new Error('WebMCP schema enum must be a nonempty array');
    if (schema.required !== undefined && (!Array.isArray(schema.required) || !schema.required.every(key => typeof key === 'string'))) throw new Error('WebMCP schema required must be an array of names');
    for (const key of ['minimum', 'maximum', 'exclusiveMinimum', 'exclusiveMaximum']) {
      if (schema[key] !== undefined && (typeof schema[key] !== 'number' || !Number.isFinite(schema[key]))) throw new Error(`WebMCP schema ${key} must be a finite number`);
    }
    for (const key of ['minLength', 'maxLength', 'minItems', 'maxItems', 'minProperties', 'maxProperties']) {
      if (schema[key] !== undefined && (!Number.isInteger(schema[key]) || schema[key] < 0)) throw new Error(`WebMCP schema ${key} must be a nonnegative integer`);
    }
    if (schema.properties !== undefined) {
      if (!schema.properties || typeof schema.properties !== 'object' || Array.isArray(schema.properties)) throw new Error('WebMCP schema properties must be an object');
      for (const child of Object.values(schema.properties)) validateSchema(child, strict, depth + 1);
    }
    if (schema.items !== undefined) validateSchema(schema.items, strict, depth + 1);
    if (schema.additionalProperties !== undefined) validateSchema(schema.additionalProperties, strict, depth + 1);
  };
  const validateInput = (value, schema, strict = false, path = '$', depth = 0) => {
    if (depth > 24) throw new Error('WebMCP input schema exceeds validation depth');
    if (schema === true) return;
    if (schema === false) throw new Error(`WebMCP input ${path} is forbidden by its schema`);
    if (!schema || typeof schema !== 'object' || Array.isArray(schema))
      throw new Error(`WebMCP input schema at ${path} must be an object or boolean`);
    if (depth === 0) validateSchema(schema, strict);
    const fail = detail => { throw new Error(`WebMCP input ${path} ${detail}`); };
    const types = Array.isArray(schema.type) ? schema.type : schema.type ? [schema.type] : [];
    const hasType = type => {
      switch (type) {
        case 'object': return value !== null && typeof value === 'object' && !Array.isArray(value);
        case 'array': return Array.isArray(value);
        case 'integer': return typeof value === 'number' && Number.isInteger(value);
        case 'number': return typeof value === 'number' && Number.isFinite(value);
        case 'null': return value === null;
        case 'string': return typeof value === 'string';
        case 'boolean': return typeof value === 'boolean';
        default: throw new Error(`WebMCP schema has unsupported type: ${type}`);
      }
    };
    if (types.length && !types.some(hasType)) fail(`must have type ${types.join(' or ')}`);
    if (schema.enum && (!Array.isArray(schema.enum) || !schema.enum.some(candidate => sameJSON(value, candidate)))) fail('is not an allowed enum value');
    if (Object.hasOwn(schema, 'const') && !sameJSON(value, schema.const)) fail('does not match const');
    if (typeof value === 'number') {
      if (schema.minimum !== undefined && value < schema.minimum) fail(`must be >= ${schema.minimum}`);
      if (schema.maximum !== undefined && value > schema.maximum) fail(`must be <= ${schema.maximum}`);
      if (schema.exclusiveMinimum !== undefined && value <= schema.exclusiveMinimum) fail(`must be > ${schema.exclusiveMinimum}`);
      if (schema.exclusiveMaximum !== undefined && value >= schema.exclusiveMaximum) fail(`must be < ${schema.exclusiveMaximum}`);
    }
    if (typeof value === 'string') {
      const length = Array.from(value).length;
      if (schema.minLength !== undefined && length < schema.minLength) fail('is too short');
      if (schema.maxLength !== undefined && length > schema.maxLength) fail('is too long');
    }
    if (Array.isArray(value)) {
      if (schema.minItems !== undefined && value.length < schema.minItems) fail('has too few items');
      if (schema.maxItems !== undefined && value.length > schema.maxItems) fail('has too many items');
      if (schema.items !== undefined) value.forEach((child, index) => validateInput(child, schema.items, strict, `${path}[${index}]`, depth + 1));
    } else if (value !== null && typeof value === 'object') {
      const keys = Object.keys(value);
      if (schema.minProperties !== undefined && keys.length < schema.minProperties) fail('has too few properties');
      if (schema.maxProperties !== undefined && keys.length > schema.maxProperties) fail('has too many properties');
      if (schema.required !== undefined && !Array.isArray(schema.required)) throw new Error('WebMCP schema required must be an array');
      for (const key of schema.required || []) if (!Object.hasOwn(value, key)) fail(`requires property ${key}`);
      const properties = schema.properties || {};
      for (const key of keys) {
        if (Object.hasOwn(properties, key)) validateInput(value[key], properties[key], strict, `${path}.${key}`, depth + 1);
        else if (schema.additionalProperties === false) fail(`has unexpected property ${key}`);
        else if (schema.additionalProperties && typeof schema.additionalProperties === 'object')
          validateInput(value[key], schema.additionalProperties, strict, `${path}.${key}`, depth + 1);
      }
    }
  };
  const invoke = async (entry, input, strict, signal) => {
    if (signal?.aborted) throw new Error('WebMCP execution was cancelled');
    if (unsafeKeys(input)) throw new Error('WebMCP input contains a reserved prototype key');
    validateInput(input, entry.descriptor.inputSchema, strict);
    return await entry.execute(copyJSON(input, {}), {signal});
  };

  const compatibilityContext = Object.freeze({
    async registerTool(tool, options = {}) {
      registerLocal(normalizeTool(tool), options);
    },
    // Legacy Meadow convenience; current draft removal uses AbortSignal.
    async unregisterTool(name) { unregisterLocal(name); },
    async getTools(options = {}) {
      if (options.fromOrigins?.length) throw new Error('Cross-document WebMCP discovery is not supported by this compatibility bridge');
      return Array.from(registry.values(), entry => {
        const handle = copyJSON({...entry.descriptor, origin: location.origin}, {});
        handle.window = window;
        registeredHandles.set(handle, entry);
        return handle;
      });
    },
    async executeTool(tool, input = {}, options = {}) {
      const name = typeof tool === 'string' ? tool : tool?.name;
      const entry = registry.get(String(name || '').trim());
      if (typeof tool !== 'string' && registeredHandles.get(tool) !== entry) throw new Error('WebMCP RegisteredTool handle is stale or not from this document');
      if (!entry) throw new Error(`Unknown WebMCP tool: ${name}`);
      if (options.signal?.aborted) throw new Error('WebMCP execution aborted');
      const controller = new AbortController();
      options.signal?.addEventListener('abort', () => controller.abort(), {once: true});
      return JSON.stringify(await invoke(entry, input, false, controller.signal));
    },
  });

  const nativeContext = document.modelContext;
  if (!nativeContext) {
    try {
      Object.defineProperty(document, 'modelContext', {
        value: compatibilityContext,
        configurable: false,
        enumerable: false,
        writable: false,
      });
    } catch (_) {
      document.modelContext = compatibilityContext;
    }
    // Older Meadow pages use navigator.modelContext; expose it only as a
    // compatibility alias, without replacing an existing browser API.
    if (!navigator.modelContext) Object.defineProperty(navigator, 'modelContext', {value: compatibilityContext});
  } else if (typeof nativeContext.registerTool === 'function') {
    implementation = 'native-mirror';
    try {
      const nativeRegister = nativeContext.registerTool.bind(nativeContext);
      const mirroredRegister = async function(tool, options = {}) {
        const entry = normalizeTool(tool);
        await nativeRegister(tool, options);
        // Do not admit registrations rejected asynchronously by the browser.
        // A concurrent accepted registration still has to pass local bounds.
        if (registry.has(entry.descriptor.name)) throw new Error('WebMCP tool already mirrored');
        registerLocal(entry, options);
      };
      nativeContext.registerTool = mirroredRegister;
      if (nativeContext.registerTool !== mirroredRegister) throw new Error('Native WebMCP registration cannot be mirrored');
      if (typeof nativeContext.unregisterTool === 'function') {
        const nativeUnregister = nativeContext.unregisterTool.bind(nativeContext);
        nativeContext.unregisterTool = async function(name, options) {
          const result = await nativeUnregister(name, options);
          unregisterLocal(name);
          return result;
        };
      }
    } catch (_) {
      implementation = 'native-unmirrored';
    }
  }

  const bridge = Object.freeze({
    cancelExecution() {
      if (!activeExecutionController) return false;
      activeExecutionController.abort();
      return true;
    },
    describe() {
      return JSON.stringify({
        protocol_version: 'meadow.webmcp.v1',
        implementation,
        revision,
        document_id: documentID,
        url: location.href,
        title: document.title || '',
        origin: location.origin === 'null' ? 'file://' : location.origin,
        tools: Array.from(registry.values(), entry => copyJSON(entry.descriptor, {})),
      });
    },
    async execute(name, inputJSON, options = {}) {
      if (executionInProgress) throw new Error('WebMCP page execution is already in progress');
      const key = String(name || '').trim();
      const entry = registry.get(key);
      if (!entry) throw new Error(`Unknown WebMCP tool: ${key}`);
      const annotations = entry.descriptor.annotations;
      const changesState = !annotations.readOnlyHint;
      const pinnedURL = location.href;
      const pinnedRevision = revision;
      const origin = location.origin === 'null' ? 'file://' : location.origin;
      const checkPins = () => {
        if (options.expected_revision !== undefined && options.expected_revision !== revision ||
            options.document_id !== undefined && options.document_id !== documentID ||
            options.expected_url !== undefined && options.expected_url !== location.href ||
            options.expected_origin !== undefined && options.expected_origin !== origin ||
            location.href !== pinnedURL || revision !== pinnedRevision)
          throw new Error('WebMCP page or catalog is stale');
      };
      checkPins();
      if (changesState) {
        if (options.allow_state_change !== true || location.protocol !== 'file:' ||
            options.expected_revision === undefined || !options.document_id ||
            !options.expected_url || !options.expected_origin || !entry.descriptor.state_change_declared || annotations.destructiveHint ||
            annotations.openWorldHint || annotations.consequentialHint)
          throw new Error('confirmation_required: WebMCP state change is not authorized for this local page');
      }
      let input = {};
      if (inputJSON && String(inputJSON).trim()) input = JSON.parse(String(inputJSON));
      if (!input || typeof input !== 'object') throw new Error('WebMCP input must be a JSON object or array');
      if (unsafeKeys(input)) throw new Error('WebMCP input contains a reserved prototype key');
      validateInput(input, entry.descriptor.inputSchema, changesState);
      const stateName = String(options.state_tool_name || '').trim();
      let stateEntry;
      if (stateName) {
        stateEntry = registry.get(stateName);
        const hints = stateEntry?.descriptor.annotations;
        if (!hints?.readOnlyHint || hints.destructiveHint || hints.openWorldHint || hints.consequentialHint)
          throw new Error('WebMCP state_tool_name must advertise a read-only closed-world tool');
        validateInput({}, stateEntry.descriptor.inputSchema, true);
      }
      const stringify = (value, max, label) => {
        const json = JSON.stringify(value === undefined ? null : value);
        if (new TextEncoder().encode(json).length > max) throw new Error(`WebMCP ${label} exceeds ${max} bytes`);
        return json;
      };
      const controller = new AbortController();
      activeExecutionController = controller;
      executionInProgress = true;
      let timer;
      const work = (async () => {
        const before = stateEntry ? stringify(await invoke(stateEntry, {}, true, controller.signal), 16384, 'state snapshot') : undefined;
        checkPins();
        if (controller.signal.aborted) throw new Error('WebMCP execution was cancelled');
        const result = await invoke(entry, input, changesState, controller.signal);
        checkPins();
        const after = stateEntry ? stringify(await invoke(stateEntry, {}, true, controller.signal), 16384, 'state snapshot') : undefined;
        checkPins();
        return JSON.stringify({
          ok: true,
          revision,
          document_id: documentID,
          tool_name: key,
          result_json: stringify(result, 65536, 'result'),
          state_tool_name: stateName || undefined,
          before_state_json: before,
          after_state_json: after,
        });
      })();
      // Timed-out handlers may ignore AbortSignal and settle later. Keep
      // this document busy until they do; never overlap/replay an action.
      const settled = () => {
        executionInProgress = false;
        if (activeExecutionController === controller) activeExecutionController = null;
      };
      work.then(settled, settled);
      try {
        return await Promise.race([work, new Promise((_, reject) => {
          timer = setTimeout(() => {
            controller.abort();
            reject(new Error('WebMCP execution timed out after 10000 ms; state may have changed, do not blindly retry'));
          }, MAX_EXECUTION_MS);
        })]);
      } finally {
        clearTimeout(timer);
        controller.abort();
      }
    },
  });

  Object.defineProperty(window, '__meadowWebMCPBridge', {
    value: bridge,
    configurable: false,
    enumerable: false,
    writable: false,
  });
})();