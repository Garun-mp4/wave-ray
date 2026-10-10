async page => {
  const results = [];
  for (const theme of ['light','dark']) {
    if (await page.evaluate(() => document.documentElement.dataset.theme) !== theme) await page.locator('#theme-toggle').click();
    for (const route of ['overview','space','devices','calibration','recordings','models','settings','integrations','help']) {
      await page.evaluate(route => location.hash = route, route);
      await page.waitForFunction(route => document.title.toLowerCase().includes(route === 'overview' ? 'monitor' : route === 'help' ? 'help' : route), route);
      await page.locator('.loading-view').waitFor({state:'hidden'});
      const result = await page.evaluate(() => {
        const rgb = text => (text.match(/[\d.]+/g) || []).map(Number);
        const luminance = color => color.slice(0,3).map(v=>v/255).map(v=>v<=.04045?v/12.92:((v+.055)/1.055)**2.4).reduce((sum,v,i)=>sum+v*[.2126,.7152,.0722][i],0);
        const background = node => {
          const ancestors=[];
          for(let p=node;p;p=p.parentElement) ancestors.unshift(p);
          return ancestors.reduce((base,p)=>{
            const c=rgb(getComputedStyle(p).backgroundColor), a=c.length>3?c[3]:1;
            return c.slice(0,3).map((v,i)=>v*a+base[i]*(1-a));
          },[255,255,255]);
        };
        const failures=[];let checked=0,min=99;
        for(const el of document.querySelectorAll('.page-description,.form-hint,.source-notice,.button-primary:not(:disabled),.badge,.section-note,.nav-link,.text-link')) {
          if(!el.getClientRects().length || getComputedStyle(el).visibility==='hidden') continue;
          const fg=luminance(rgb(getComputedStyle(el).color)),bg=luminance(background(el));
          const ratio=(Math.max(fg,bg)+.05)/(Math.min(fg,bg)+.05);
          checked++; min=Math.min(min,ratio);
          if(ratio<4.5) failures.push({text:el.textContent.slice(0,60),ratio});
        }
        return {checked,min,failures};
      });
      results.push({theme,route,...result});
    }
  }
  const failed=results.filter(r=>r.failures.length);
  if(failed.length) throw Error(JSON.stringify(failed));
  return {samples:results.reduce((n,r)=>n+r.checked,0),minimumRatio:Math.min(...results.map(r=>r.min)).toFixed(2),failures:0};
}
