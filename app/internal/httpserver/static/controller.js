(() => {
  const detail = document.querySelector('#detail');
  const actions = document.querySelector('#actions');
  const configuration = document.querySelector('#configuration');
  const configurationFeedback = document.querySelector('#configuration-feedback');
  const newRun = document.querySelector('#new-run');
  const submitButtons = document.querySelectorAll('button[type="submit"]');
  const detailURL = id => '/ui/stacks?id=' + encodeURIComponent(id);
  const json = value => ({headers: {'Content-Type': 'application/json'}, body: JSON.stringify(value)});
  let busy = false;
  let configurationEditor;

  function updateButtons() {
    for (const button of submitButtons) {
      const unavailable = button.form === configuration && !configurationEditor?.valid(button.value);
      button.disabled = busy || unavailable;
    }
    configurationEditor?.sync(busy);
  }
  async function request(url, options, expected) {
    const response = await fetch(url, {credentials: 'same-origin', cache: 'no-store', ...options});
    if (response.status !== expected) throw new Error((await response.text()).trim() || `Request failed (${response.status})`);
    return response;
  }
  function showMessage(element, message, error = false) {
    element.textContent = message;
    element.toggleAttribute('data-error', error);
  }
  async function run(form, operation) {
    if (busy) return;
    busy = true;
    updateButtons();
    const feedback = form === configuration ? configurationFeedback : form.querySelector('.feedback');
    showMessage(feedback, '');
    try {
      const destination = await operation(feedback);
      if (destination) {
        location.assign(destination);
        return;
      }
    } catch (error) {
      showMessage(feedback, error.message, true);
    }
    busy = false;
    updateButtons();
  }
  window.addEventListener('pageshow', event => {
    if (event.persisted && busy) location.reload();
  });

  if (newRun) {
    newRun.addEventListener('submit', event => {
      event.preventDefault();
      run(newRun, async () => {
        const template = newRun.elements.template.value;
        const response = await request(newRun.action, {method: 'POST', ...json({template})}, 201);
        return detailURL((await response.json()).id);
      });
    });
  }

  function composeText(bytes) {
    const first = bytes[0];
    const second = bytes[1];
    const encoding = first === 0xff && second === 0xfe ? 'utf-16le' :
      first === 0xfe && second === 0xff ? 'utf-16be' : 'utf-8';
    return new TextDecoder(encoding, {fatal: true}).decode(bytes);
  }

  function mountServiceRow(container, service, saved) {
    const row = document.createElement('li');
    row.className = 'service-row';
    const title = document.createElement('div');
    const heading = document.createElement('h3');
    heading.textContent = service.name;
    title.append(heading);
    if (service.repository) {
      const source = document.createElement('small');
      source.textContent = service.repository;
      title.append(source);
    }

    const fields = document.createElement('div');
    fields.className = 'service-reference-fields';
    const mode = document.createElement('select');
    const input = document.createElement('input');
    const git = service.kind === 'git';
    mode.setAttribute('aria-label', (git ? 'Git ref' : 'Image reference') + ' for ' + service.name);
    mode.add(new Option(service.value || 'No manifest default', 'default'));
    mode.add(new Option(git ? 'Override Git ref…' : 'Override image…', 'override'));
    input.type = 'text';
    input.spellcheck = false;
    input.placeholder = git ? 'Branch, tag, or commit SHA' : 'registry.example.com/image:tag';
    input.setAttribute('aria-label', 'Custom reference for ' + service.name);
    if (saved?.kind === service.kind) {
      input.value = saved.value;
      mode.value = 'override';
    }
    function syncMode() {
      input.hidden = mode.value === 'default';
      input.required = !input.hidden;
    }
    mode.addEventListener('change', () => {
      syncMode();
      if (!input.hidden) input.focus();
    });
    syncMode();
    fields.append(mode, input);
    row.append(title, fields);
    container.append(row);
    return {service, mode, input};
  }

  function appendPorts(service, label, ports) {
    const row = document.createElement('p');
    row.textContent = label + ' ' + ports.join(', ');
    service.append(row);
  }

  function renderServiceSummary(container, services) {
    if (!container) return;
    if (!services.length) {
      container.textContent = 'No services to summarize.';
      return;
    }
    const list = document.createElement('ul');
    for (const service of services) {
      const item = document.createElement('li');
      const name = document.createElement('h3');
      name.textContent = service.name;
      item.append(name);
      if (service.http_port) {
        const route = document.createElement('span');
        route.className = 'service-route';
        route.textContent = `${service.https ? 'HTTPS' : 'HTTP'} route · ${service.http_port}/tcp`;
        item.append(route);
      }
      if (service.ports?.length) {
        appendPorts(item, 'Published', service.ports.map(port => port + '/tcp'));
      }
      if (service.exposed?.length) {
        appendPorts(item, 'Internal', service.exposed.map(port => port.includes('/') ? port : port + '/tcp'));
      }
      if (!service.ports?.length && !service.exposed?.length) {
        const empty = document.createElement('p');
        empty.textContent = 'No exposed ports';
        item.append(empty);
      }
      list.append(item);
    }
    container.replaceChildren(list);
  }

  function mountConfiguration(form, data, {mountEnvironmentEditor, mountManifestEditor}) {
    const {id, launchDisabled, name: originalName} = form.dataset;
    const manifest = form.querySelector('#manifest-yaml');
    const inspect = form.querySelector('#inspect-manifest');
    const manifestFeedback = form.querySelector('#manifest-feedback');
    const file = form.querySelector('#manifest-file');
    const name = form.querySelector('#template-name');
    const discard = form.querySelector('#remove-draft');
    const serviceList = form.querySelector('#service-references');
    const summary = form.querySelector('#service-summary');
    const editor = mountEnvironmentEditor(form, (data.services ?? []).map(service => service.name), data.environment ?? {}, updateButtons);
    let serviceRows = [];
    let acceptedYAML;
    const originalYAML = composeText(Uint8Array.from(atob(data.content), character => character.charCodeAt(0)));
    const yaml = mountManifestEditor(manifest, originalYAML, () => {
      showMessage(manifestFeedback, yaml.text() === acceptedYAML ? '' : 'Update services to apply the edited manifest.');
      updateButtons();
    });
    const initialYAML = yaml.text();
    let savedYAML = initialYAML;
    if (data.problem) showMessage(manifestFeedback, data.problem, true);
    else acceptedYAML = initialYAML;

    function references() {
      return Object.fromEntries(serviceRows.filter(row => row.mode.value === 'override').map(({service, input}) =>
        [service.name, {kind: service.kind, value: input.value}]));
    }
    function renderServiceRows(services, saved) {
      if (!serviceList) return;
      serviceList.replaceChildren();
      serviceRows = services.map(service => mountServiceRow(serviceList, service,
        Object.hasOwn(saved, service.name) ? saved[service.name] : undefined));
    }
    renderServiceRows(data.services ?? [], data.references ?? {});
    renderServiceSummary(summary, data.services ?? []);
    form.addEventListener('keydown', event => {
      if (id && event.key === 'Enter' && !event.isComposing && event.target.matches('input[type="text"]')) {
        event.preventDefault();
      }
    });
    name?.addEventListener('input', updateButtons);
    if (file) file.addEventListener('change', async () => {
      const [selected] = file.files;
      if (!selected || busy) return;
      try {
        const text = composeText(new Uint8Array(await selected.arrayBuffer()));
        if (busy) return;
        if (name && !name.value.trim()) name.value = selected.name.replace(/\.ya?ml$/i, '');
        yaml.replace(text);
      } catch (error) {
        showMessage(manifestFeedback, 'Cannot read this YAML file. ' + error.message, true);
      } finally {
        file.value = '';
      }
    });
    inspect.addEventListener('click', () => {
      run(form, async () => {
        try {
          const response = await request('/ui/manifest-services', {method: 'POST', ...json({inline_yaml: yaml.text()})}, 200);
          const services = await response.json();
          renderServiceRows(services, references());
          renderServiceSummary(summary, services);
          editor.setServices(services.map(service => service.name));
          acceptedYAML = yaml.text();
          showMessage(manifestFeedback, 'Services updated.');
        } catch (error) {
          showMessage(manifestFeedback, error.message, true);
        }
      });
    });
    if (discard) discard.addEventListener('click', () => {
      if (!confirm('Discard this draft run?')) return;
      run(form, async () => {
        await request('/stacks/' + encodeURIComponent(id), {method: 'DELETE'}, 204);
        return '/ui';
      });
    });
    return {
      values() {
        const values = {environment: editor.values(), references: references()};
        const text = yaml.text();
        if ((!id && !originalName) || savedYAML !== text) values.inline_yaml = text === initialYAML ? originalYAML : text;
        if (name) values.name = name.value === originalName ? originalName : name.value.trim();
        return values;
      },
      markSaved: () => { savedYAML = yaml.text(); },
      valid: action => yaml.text() === acceptedYAML && editor.valid() && (!name || name.value.trim()) &&
        (action !== 'launch' || !launchDisabled),
      sync(locked) {
        inspect.disabled = locked || !yaml.text().trim() || yaml.text() === acceptedYAML;
        yaml.setReadOnly(locked);
        if (file) file.disabled = locked;
        if (name) name.readOnly = locked;
        if (discard) discard.disabled = locked;
        for (const {mode, input} of serviceRows) {
          mode.disabled = locked;
          input.readOnly = locked;
        }
        editor.setReadOnly(locked);
      },
    };
  }

  if (configuration) {
    const {id, name} = configuration.dataset;
    import('/ui/environment-editor.js').then(editors => {
      const data = JSON.parse(document.querySelector('#configuration-data').textContent);
      configurationEditor = mountConfiguration(configuration, data, editors);
      showMessage(configurationFeedback, '');
      updateButtons();
    }).catch(error => showMessage(configurationFeedback, error.message + ' Refresh the page to try again.', true));
    configuration.addEventListener('submit', event => {
      event.preventDefault();
      const action = event.submitter?.value ?? 'save';
      if (!configurationEditor?.valid(action)) return;
      run(configuration, async feedback => {
        const values = configurationEditor.values();
        if (id) {
          const url = '/ui/runs/' + encodeURIComponent(id);
          if (action === 'launch') {
            await request(url + '/launch', {method: 'POST', ...json(values)}, 202);
            return detailURL(id);
          }
          await request(url, {method: 'PUT', ...json(values)}, 204);
          configurationEditor.markSaved();
          showMessage(feedback, 'Draft saved.');
        } else if (name) {
          await request('/ui/templates/' + encodeURIComponent(name), {method: 'PUT', ...json(values)}, 204);
          if (values.name !== name) return '/ui/templates/edit?name=' + encodeURIComponent(values.name);
          configurationEditor.markSaved();
          showMessage(feedback, 'Template saved.');
        } else {
          await request('/ui/templates', {method: 'POST', ...json(values)}, 201);
          return '/ui/templates';
        }
      });
    });
  }

  function mountOriginalManifest(query) {
    const panel = document.querySelector('#original-manifest');
    const source = document.querySelector('#manifest-source');
    const feedback = document.querySelector('#manifest-status');
    let loading = false;
    async function showManifest() {
      if (!panel.open || !source.hidden || loading) return;
      loading = true;
      showMessage(feedback, 'Loading original manifest…');
      try {
        const response = await fetch('/ui/stacks/manifest' + query, {cache: 'no-store'});
        if (response.status === 404) {
          showMessage(feedback, 'No original manifest is saved for this run yet.');
        } else {
          if (!response.ok) throw new Error();
          source.textContent = composeText(new Uint8Array(await response.arrayBuffer()));
          source.hidden = false;
          showMessage(feedback, '');
        }
      } catch (_) {
        showMessage(feedback, 'Cannot display the original manifest. Close and reopen to retry.', true);
      } finally {
        loading = false;
      }
    }
    panel.addEventListener('toggle', showManifest);
    showManifest();
    return showManifest;
  }

  function trackProgress(query, showManifest) {
    const log = document.querySelector('#progress-log');
    if (!log || detail.dataset.inProgress !== 'true') return;
    const empty = document.querySelector('#progress-empty');
    const feedback = document.querySelector('#poll-status');
    log.scrollTop = log.scrollHeight;
    function append({time, message}) {
      const entry = document.createElement('li');
      const timestamp = document.createElement('time');
      timestamp.dateTime = time;
      timestamp.textContent = time.slice(0, 19).replace('T', ' ') + ' UTC';
      const text = document.createElement('span');
      text.textContent = message;
      entry.append(timestamp, text);
      log.append(entry);
    }
    const poll = async () => {
      try {
        const response = await fetch('/ui/stacks/activity' + query, {cache: 'no-store'});
        if (response.status === 404) {
          showMessage(feedback, 'This run has been removed.');
          return;
        }
        if (!response.ok) throw new Error();
        const status = await response.json();
        if (status.state !== detail.dataset.state) {
          location.reload();
          return;
        }
        const entries = status.activity ?? [];
        if (entries.length > log.children.length) {
          const following = log.scrollHeight - log.clientHeight - log.scrollTop < 32;
          entries.slice(log.children.length).forEach(append);
          log.hidden = false;
          empty.hidden = true;
          if (following) log.scrollTop = log.scrollHeight;
        }
        if (status.has_manifest) showManifest();
        showMessage(feedback, '');
      } catch (_) {
        showMessage(feedback, 'Cannot read run status. Retrying.', true);
      }
      setTimeout(poll, 1000);
    };
    setTimeout(poll, 1000);
  }

  if (detail) {
    const id = detail.dataset.id;
    const endpoint = '/stacks/' + encodeURIComponent(id);
    if (actions) actions.addEventListener('submit', event => {
      event.preventDefault();
      const action = event.submitter?.value;
      if (!action) return;
      if (action === 'remove' && !confirm('Remove this run and its data?')) return;
      run(actions, async () => {
        const remove = action === 'remove';
        const path = remove ? endpoint : endpoint + '/' + action;
        await request(path, {method: remove ? 'DELETE' : 'POST'}, action === 'stop' || remove ? 204 : 202);
        return remove ? '/ui' : detailURL(id);
      });
    });
    const query = '?id=' + encodeURIComponent(id);
    trackProgress(query, mountOriginalManifest(query));
  }
  updateButtons();
})();
