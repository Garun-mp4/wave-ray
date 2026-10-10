async page => {
  await page.goto('http://localhost:3000/ui/');
  await page.waitForFunction(() => document.querySelector('.monitor-columns'));
  await page.evaluate(() => {
    window.__verifyOriginalFetch = window.fetch;
    window.fetch = (input, init) => String(input).endsWith('/api/v1/info')
      ? Promise.resolve(new Response(JSON.stringify({error:'Token required'}), {status:401,headers:{'Content-Type':'application/json'}}))
      : window.__verifyOriginalFetch(input, init);
  });
  await page.locator('#refresh-button').click();
  await page.getByRole('link',{name:'Configure access',exact:true}).waitFor({timeout:5000});
  if (await page.locator('#api-status-text').textContent() !== 'Authorization needed') throw Error('Authorization not visible');
  await page.evaluate(() => { window.fetch = window.__verifyOriginalFetch; delete window.__verifyOriginalFetch; });
  await page.locator('#refresh-button').click();
  await page.waitForFunction(() => document.querySelector('#api-status-text').textContent === 'API online');
  await page.evaluate(() => {
    window.__verifyOriginalFetch = window.fetch;
    window.fetch = (input, init) => String(input).endsWith('/api/v1/nodes')
      ? Promise.resolve(new Response(JSON.stringify({error:'Device service temporarily unavailable'}), {status:503,headers:{'Content-Type':'application/json'}}))
      : window.__verifyOriginalFetch(input, init);
  });
  await page.locator('[data-route="devices"]').click();
  await page.getByText('Device service temporarily unavailable',{exact:true}).first().waitFor({timeout:5000});
  await page.evaluate(() => { window.fetch = window.__verifyOriginalFetch; delete window.__verifyOriginalFetch; });
  await page.locator('#refresh-button').click();
  await page.getByText('Device service temporarily unavailable',{exact:true}).first().waitFor({state:'hidden'});
  await page.evaluate(() => {
    window.__verifyOriginalFetch = window.fetch;
    window.fetch = (input, init) => String(input).endsWith('/api/v1/nodes')
      ? Promise.resolve(new Response(JSON.stringify({nodes:[{node_id:7,status:'stale',csi_status:'stale',last_seen_ms:30000}]}), {headers:{'Content-Type':'application/json'}}))
      : window.__verifyOriginalFetch(input, init);
  });
  await page.locator('#refresh-button').click();
  await page.locator('[data-device-row]').first().waitFor();
  await page.locator('.device-filter').fill('no-such-node');
  if (await page.locator('[data-device-row]:visible').count()) throw Error('Device filter failed');
  await page.locator('.device-filter').fill('');
  if (!await page.locator('[data-device-row]:visible').count()) throw Error('Device filter recovery failed');
  await page.evaluate(() => { window.fetch = window.__verifyOriginalFetch; delete window.__verifyOriginalFetch; });
  await page.locator('[data-route="overview"]').click();
  return {authorization:'passed',apiErrorRecovery:'passed',deviceFilter:'passed with isolated stale-node fixture'};
}
