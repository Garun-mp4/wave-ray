async page => {
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.goto('http://localhost:3000/ui/');
  await page.waitForFunction(() => document.querySelector('.monitor-columns'));
  const rows = [];
  const routes = ['overview', 'space', 'devices', 'calibration', 'recordings', 'models', 'settings', 'integrations', 'help'];
  for (const theme of ['light', 'dark']) {
    if (await page.evaluate(() => document.documentElement.dataset.theme) !== theme) await page.locator('#theme-toggle').click();
    await page.reload();
    if (await page.evaluate(() => document.documentElement.dataset.theme) !== theme) throw Error('Theme did not survive reload');
    for (const width of [390, 768, 1024, 1440]) {
      await page.setViewportSize({width, height: width === 390 ? 844 : 1000});
      for (const route of routes) {
        await page.evaluate(route => { location.hash = route; }, route);
        await page.waitForFunction(route => document.title.toLowerCase().includes(route === 'overview' ? 'monitor' : route === 'help' ? 'help' : route), route);
        await page.locator('.loading-view').waitFor({state:'hidden'});
        const result = await page.evaluate(() => ({
          route: location.hash, width: innerWidth, scroll: document.documentElement.scrollWidth,
          background: getComputedStyle(document.body).backgroundColor,
          font: getComputedStyle(document.body).fontFamily,
          headings: document.querySelectorAll('main h1').length,
          errors: document.querySelector('main .inline-error')?.textContent || null
        }));
        if (result.scroll > width) throw Error('Overflow: ' + JSON.stringify(result));
        if (result.headings !== 1) throw Error('Missing page heading: ' + route);
        rows.push({...result, theme});
        if ((width === 390 && ['overview','space'].includes(route)) || (width === 1440 && ['overview','space','calibration','models','settings'].includes(route))) {
          await page.screenshot({path:'.playwright-cli/final-'+theme+'-'+width+'-'+route+'.png', fullPage:true});
        }
      }
    }
  }
  await page.setViewportSize({width:390,height:844});
  await page.locator('#menu-toggle').click();
  await page.locator('[data-route="devices"]').click();
  if (await page.locator('#menu-toggle').getAttribute('aria-expanded') !== 'false') throw Error('Menu did not close');
  await page.locator('#menu-toggle').click();
  await page.keyboard.press('Escape');
  if (await page.locator('#menu-toggle').getAttribute('aria-expanded') !== 'false') throw Error('Escape failed');
  await page.setViewportSize({width:1440,height:1000});
  await page.locator('[data-route="overview"]').click();
  await page.locator('.diagnostics summary').click();
  await page.locator('#refresh-button').click();
  if (!await page.locator('.diagnostics').getAttribute('open').then(value => value !== null)) throw Error('Refresh collapsed diagnostics');
  await page.locator('[data-route="settings"]').click();
  await page.locator('.skip-link').focus();
  await page.keyboard.press('Enter');
  if (await page.evaluate(() => location.hash) !== '#settings') throw Error('Skip link changed route');
  await page.goto('http://localhost:3000/ui/observatory.html');
  await page.waitForURL('**/ui/#overview');
  await page.goto('http://localhost:3000/ui/pose-fusion.html');
  await page.waitForURL('**/ui/#overview');
  if (errors.length) throw Error('Runtime errors: '+JSON.stringify(errors));
  return { checked: rows.length, errors, themes: ["light","dark"], widths: [390,768,1024,1440], interactions: "passed" };
}
