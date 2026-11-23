import {basicSetup} from 'codemirror';
import {indentWithTab, temporarilySetTabFocusMode} from '@codemirror/commands';
import {yaml} from '@codemirror/lang-yaml';
import {HighlightStyle, StreamLanguage, syntaxHighlighting} from '@codemirror/language';
import {linter, lintGutter} from '@codemirror/lint';
import {Compartment, EditorState} from '@codemirror/state';
import {EditorView, keymap, lineNumbers, placeholder} from '@codemirror/view';
import {tags} from '@lezer/highlight';
import {formatEnvironment, parseEnvironment} from './assignments.js';

const assignmentLanguage = StreamLanguage.define({
  token(stream) {
    if (stream.sol() && stream.match(/^\s*#/)) {
      stream.skipToEnd();
      return 'comment';
    }
    const equals = stream.string.indexOf('=');
    if (equals < 0 || stream.pos > equals) {
      stream.skipToEnd();
      return equals < 0 ? 'variableName' : 'string';
    }
    if (stream.eat('=')) return 'operator';
    stream.eatWhile(character => character !== '=');
    return 'variableName';
  },
});

const colors = syntaxHighlighting(HighlightStyle.define([
  {tag: [tags.variableName, tags.labelName], color: '#5d3cb1', fontWeight: '600'},
  {tag: tags.propertyName, color: '#1744a7', fontWeight: '600'},
  {tag: tags.string, color: '#146347'},
  {tag: tags.operator, color: '#36465d'},
  {tag: tags.comment, color: '#596a80', fontStyle: 'italic'},
]));

const editorTheme = EditorView.theme({
  '&': {backgroundColor: '#fbfcfe', color: '#182230', fontSize: '0.9rem'},
  '.cm-scroller': {fontFamily: 'ui-monospace, SFMono-Regular, Consolas, monospace', lineHeight: '1.6'},
  '.cm-content': {padding: '0.55rem 0'},
  '.cm-line': {padding: '0 0.7rem'},
  '.cm-gutters': {alignSelf: 'stretch', height: 'auto', backgroundColor: '#f1f4f9', borderRight: '1px solid #e2e7ee', color: '#607088'},
  '.cm-activeLine, .cm-activeLineGutter': {backgroundColor: 'transparent'},
  // CodeMirror draws selections behind the text and the active line.
  '&.cm-focused .cm-activeLine': {backgroundColor: '#dce7fb66'},
  '&.cm-focused .cm-activeLineGutter': {backgroundColor: '#e5edfa'},
  '&.cm-focused > .cm-scroller > .cm-selectionLayer .cm-selectionBackground': {backgroundColor: '#b7cffd'},
  '.cm-placeholder': {color: '#596a80'},
  '&.cm-focused, .cm-content:focus-visible': {outline: 'none'},
});

const codeEditing = [
  basicSetup,
  colors,
  editorTheme,
  keymap.of([indentWithTab]),
  EditorView.domEventObservers({
    keydown(event, view) {
      if (event.key === 'Escape') temporarilySetTabFocusMode(view);
    },
  }),
  EditorView.contentAttributes.of({
    'aria-description': 'Tab indents. To leave the editor, press Escape, then Tab.',
    spellcheck: 'false',
  }),
];

function readOnlyExtensions(readOnly) {
  return [EditorState.readOnly.of(readOnly), EditorView.editable.of(!readOnly)];
}

export function mountReadOnlyEnvironment(parent, service, saved) {
  return new EditorView({
    doc: formatEnvironment(saved),
    parent,
    extensions: [
      editorTheme, colors, assignmentLanguage, lineNumbers(), EditorView.lineWrapping,
      readOnlyExtensions(true), placeholder('No saved overrides.'),
      EditorView.contentAttributes.of({
        'aria-label': `Environment overrides for ${service}`,
        tabindex: '0',
      }),
    ],
  });
}

export function mountManifestEditor(parent, doc, onChange) {
  const lock = new Compartment();
  const view = new EditorView({
    doc,
    parent,
    extensions: [
      codeEditing, yaml(), lock.of(readOnlyExtensions(false)),
      EditorView.contentAttributes.of({
        'aria-label': 'Compose YAML',
        'aria-describedby': 'manifest-feedback',
      }),
      EditorView.updateListener.of(update => {
        if (update.docChanged) onChange();
      }),
    ],
  });
  parent.closest('details')?.addEventListener('toggle', () => view.requestMeasure());
  let locked = false;
  return {
    text: () => view.state.doc.toString(),
    replace(text) {
      view.dispatch({changes: {from: 0, to: view.state.doc.length, insert: text}});
    },
    setReadOnly(readOnly) {
      if (locked === readOnly) return;
      locked = readOnly;
      view.dispatch({effects: lock.reconfigure(readOnlyExtensions(readOnly))});
    },
  };
}

function mountService(container, service, saved, onChange) {
  const panel = document.createElement('section');
  panel.className = 'environment-service code-editor';
  const editor = document.createElement('div');
  const summary = document.createElement('p');
  summary.className = 'environment-diagnostic';
  summary.setAttribute('role', 'status');
  summary.hidden = true;
  panel.append(editor, summary);
  container.append(panel);

  const lock = new Compartment();
  const doc = formatEnvironment(saved);
  let parsed = parseEnvironment(doc);
  const view = new EditorView({
    doc,
    parent: editor,
    extensions: [
      codeEditing, assignmentLanguage, lintGutter(), EditorView.lineWrapping,
      placeholder('KEY=value'), lock.of(readOnlyExtensions(false)),
      EditorView.contentAttributes.of({
        'aria-label': `Environment overrides for ${service}`,
        'aria-describedby': 'environment-help',
      }),
      linter(() => parsed.diagnostics, {delay: 125}),
      EditorView.updateListener.of(update => {
        if (!update.docChanged) return;
        parsed = parseEnvironment(update.state.doc.toString());
        const first = parsed.diagnostics[0];
        summary.hidden = !first;
        summary.textContent = first ? `Line ${first.line} · ${first.message}` : '';
        panel.classList.toggle('has-errors', Boolean(first));
        onChange();
      }),
    ],
  });
  return {current: () => parsed, view, lock, panel};
}

export function mountEnvironmentEditor(form, declared, saved, onChange) {
  const picker = form.querySelector('#environment-picker');
  const select = form.querySelector('#environment-service');
  const container = form.querySelector('#environment-editors');
  const editors = new Map();
  const empty = document.createElement('p');
  empty.className = 'muted';
  empty.textContent = 'No Compose services are available.';
  container.append(empty);
  let locked = false;

  function showService() {
    for (const [name, {panel, view}] of editors) {
      panel.hidden = name !== select.value;
      if (!panel.hidden) view.requestMeasure();
    }
  }
  function setServices(names, initial = {}) {
    const next = [...new Set(names)].sort();
    const selected = select.value;
    for (const [name, {panel, view}] of editors) {
      if (next.includes(name)) continue;
      view.destroy();
      panel.remove();
      editors.delete(name);
    }
    for (const name of next) {
      if (editors.has(name)) continue;
      const savedValues = Object.hasOwn(initial, name) ? initial[name] : {};
      const editor = mountService(container, name, savedValues, onChange);
      if (locked) editor.view.dispatch({effects: editor.lock.reconfigure(readOnlyExtensions(true))});
      editors.set(name, editor);
    }
    select.replaceChildren(...next.map(name => new Option(name, name)));
    if (editors.has(selected)) select.value = selected;
    picker.hidden = next.length === 0;
    empty.hidden = !picker.hidden;
    showService();
  }

  select.addEventListener('change', showService);
  setServices([...declared, ...Object.keys(saved)], saved);
  return {
    setServices,
    valid: () => editors.size > 0 && [...editors.values()].every(editor => editor.current().diagnostics.length === 0),
    values() {
      const environment = Object.create(null);
      for (const [name, editor] of editors) {
        const {values} = editor.current();
        if (Object.keys(values).length) environment[name] = values;
      }
      return environment;
    },
    setReadOnly(readOnly) {
      if (locked === readOnly) return;
      locked = readOnly;
      select.disabled = locked;
      for (const {view, lock} of editors.values()) {
        view.dispatch({effects: lock.reconfigure(readOnlyExtensions(readOnly))});
      }
    },
  };
}
