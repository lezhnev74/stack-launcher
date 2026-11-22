const identifier = /^[A-Za-z_][A-Za-z0-9_]*$/;

function parseKey(text) {
  text = text.trim();
  if (identifier.test(text)) return text;
  if (text.startsWith('"')) return JSON.parse(text);
}

function parseValue(text) {
  const quoted = text.trim();
  if (quoted.startsWith('"')) return JSON.parse(quoted);
  if (quoted.startsWith("'")) {
    if (quoted.length < 2 || !quoted.endsWith("'")) throw new Error('Missing closing single quote.');
    return quoted.slice(1, -1);
  }
  return text;
}

function parseAssignment(line, values) {
  if (!line.trim() || line.trimStart().startsWith('#')) return '';
  const equals = line.indexOf('=');
  if (equals < 0) return 'Use KEY=value with an equals sign.';
  let key;
  try {
    key = parseKey(line.slice(0, equals));
  } catch (_) {
    return 'Use a valid quoted variable name.';
  }
  if (!key || key.includes('\0')) return 'Use a variable name such as APP_MODE.';
  if (Object.hasOwn(values, key)) return 'This variable is already defined in this editor.';

  let value;
  try {
    value = parseValue(line.slice(equals + 1));
  } catch (_) {
    return 'Check the value’s closing quote and escapes.';
  }
  if (value.includes('\0')) return 'A value cannot contain a NUL character.';
  values[key] = value;
  return '';
}

export function parseEnvironment(text) {
  const values = Object.create(null);
  const diagnostics = [];
  let from = 0;
  const lines = text.split('\n');
  for (const [index, line] of lines.entries()) {
    const message = parseAssignment(line, values);
    const to = from + line.length;
    if (message) diagnostics.push({from, to, severity: 'error', message, line: index + 1});
    from = to + 1;
  }
  return {values, diagnostics};
}

export function formatEnvironment(values) {
  return Object.keys(values).sort().map(key => {
    const value = values[key];
    const name = identifier.test(key) ? key : JSON.stringify(key);
    const escaped = value !== value.trim() || /^["']/.test(value) || /[\u0000-\u001f\u007f]/.test(value);
    return `${name}=${escaped ? JSON.stringify(value) : value}`;
  }).join('\n');
}
