import {readFile} from 'node:fs/promises';
import {build} from 'esbuild';

const {packages} = JSON.parse(await readFile('package-lock.json', 'utf8'));
const licenses = await Promise.all(Object.entries(packages)
  .filter(([path, metadata]) => path && !metadata.dev)
  .map(async ([path]) => {
    const license = await readFile(`${path}/LICENSE`, 'utf8');
    return `${path.replace('node_modules/', '')}\n\n${license.trim()}`;
  }));

await build({
  entryPoints: ['internal/httpserver/editor/environment.js'],
  outfile: 'internal/httpserver/static/environment-editor.js',
  bundle: true,
  minify: true,
  format: 'esm',
  banner: {js: '// Built by npm run build from internal/httpserver/editor.'},
  footer: {js: `/*\n${licenses.join('\n\n')}\n*/`},
});
