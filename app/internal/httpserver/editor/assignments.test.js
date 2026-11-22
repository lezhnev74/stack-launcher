import assert from 'node:assert/strict';
import test from 'node:test';
import {formatEnvironment, parseEnvironment} from './assignments.js';

test('saved values round-trip as literal environment strings', () => {
  const saved = JSON.parse('{"EMPTY":"","BEARER":" $TOKEN=1 # value ","ODD":"\'literal quote",' +
    '"MULTILINE":"row 1\\nrow 2\\r\\nrow 3","an odd key\\n":"works","__proto__":"literal"}');
  const parsed = parseEnvironment(formatEnvironment(saved));
  assert.deepEqual(parsed.diagnostics, []);
  assert.deepEqual({...parsed.values}, saved);

  const pasted = parseEnvironment('# settings\nEMPTY=\nURL=https://example.test/?a=b#anchor\nPRICE= $dollars\nMODE=\'dev\'');
  assert.deepEqual(pasted.diagnostics, []);
  assert.deepEqual({...pasted.values}, {EMPTY: '', URL: 'https://example.test/?a=b#anchor', PRICE: ' $dollars', MODE: 'dev'});
});

test('reports malformed assignments with their line ranges', () => {
  const lines = ['# comment', 'MISSING_EQUALS', '=empty-name', 'X=valid', 'X=duplicate', 'BAD="unterminated', 'NUL="\\u0000"'];
  const {values, diagnostics} = parseEnvironment(lines.join('\n'));
  assert.deepEqual({...values}, {X: 'valid'});
  assert.deepEqual(diagnostics.map(diagnostic => diagnostic.line), [2, 3, 5, 6, 7]);
  for (const {from, to, line, severity} of diagnostics) {
    assert.equal(severity, 'error');
    assert.equal(lines.join('\n').slice(from, to), lines[line - 1]);
  }
});
