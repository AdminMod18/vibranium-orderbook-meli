// Convierte un SVG a PNG (2x) con Playwright:  node docs/img/src/render.js docs/img/arquitectura-kubernetes.svg
const { chromium } = require('playwright');
const fs = require('fs'), path = require('path');
(async () => {
  const svg = path.resolve(process.argv[2]);
  const m = fs.readFileSync(svg, 'utf8').match(/viewBox="0 0 (\d+) (\d+)"/);
  const w = +m[1], h = +m[2];
  const b = await chromium.launch();
  const p = await b.newPage({ viewport: { width: w, height: h }, deviceScaleFactor: 1.5 });
  await p.goto('file://' + svg);
  await p.screenshot({ path: svg.replace(/\.svg$/, '.png') });
  await b.close();
  console.log('PNG:', svg.replace(/\.svg$/, '.png'));
})();
