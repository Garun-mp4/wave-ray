async page => {
  const errors=[];
  page.on('pageerror', error => errors.push(error.message));
  await page.goto('http://localhost:3000/ui/#space');
  await page.locator('#installation-space').waitFor();
  const liveState=await page.evaluate(async()=>{
    const health=await fetch('/health/ready').then(r=>r.json());
    const layout=await fetch('/api/v1/space/layout').then(r=>r.json());
    return {source:health.source,frames:health.frames_received,devices:layout.layout.devices.length};
  });
  if(liveState.source.includes('simulat')||liveState.frames!==0)throw Error('Expected hardware waiting state: '+JSON.stringify(liveState));
  // Isolated browser fixtures exercise layout editing without writing made-up
  // device positions or CSI measurements to the running server.
  await page.evaluate(()=>{
    window.spaceRealFetch=window.fetch;
    window.qaLayout={revision:0,devices:[
      {id:'qa-rx1',kind:'esp32',node_id:1,label:'QA / Receiver 1',position:[-2,-1,-2]},
      {id:'qa-rx2',kind:'esp32',node_id:2,label:'QA / Receiver 2',position:[2,1,1]},
      {id:'qa-rx3',kind:'esp32',node_id:3,label:'QA / Receiver 3',position:[-1,2,2]},
      {id:'qa-ap',kind:'router',label:'QA / Access point',position:[0,0,0]}
    ]};
    window.fetch=async(input,init={})=>{
      const path=new URL(typeof input==='string'?input:input.url||String(input),location.origin).pathname;
      if(path==='/api/v1/space/layout'){
        if(init.method==='POST'){
          const next=JSON.parse(init.body);
          if(next.revision!==window.qaLayout.revision)return new Response(JSON.stringify({error:'Refresh required'}),{status:409,headers:{'Content-Type':'application/json'}});
          window.qaLayout={...next,revision:next.revision+1};
        }
        return new Response(JSON.stringify({layout:window.qaLayout}),{headers:{'Content-Type':'application/json'}});
      }
      return window.spaceRealFetch(input,init);
    };
  });
  await page.locator('#refresh-button').click();
  await page.waitForFunction(()=>document.querySelectorAll('.space-device').length===4);
  const canvas=page.locator('#installation-space');
  const before=await canvas.evaluate(c=>c.toDataURL());
  await canvas.focus();
  await page.keyboard.press('ArrowRight');
  await page.waitForFunction(before=>document.querySelector('#installation-space').toDataURL()!==before,before);
  await page.keyboard.press('Shift+ArrowDown');
  await page.keyboard.press('+');
  await page.keyboard.press('Home');
  const rect=await canvas.boundingBox();
  await page.mouse.move(rect.x+rect.width/2,rect.y+rect.height/2);
  await page.mouse.down();await page.mouse.move(rect.x+rect.width/2+60,rect.y+rect.height/2+30);await page.mouse.up();
  await page.mouse.wheel(0,80);
  await page.locator('[data-space-view="fit"]').click();
  for(const theme of ['dark','light']){
    if(await page.evaluate(()=>document.documentElement.dataset.theme)!==theme)await page.locator('#theme-toggle').click();
    for(const width of [1440,390]){
      await page.setViewportSize({width,height:width===390?844:1000});
      await page.waitForFunction(() => { const c=document.querySelector('#installation-space');return c.width===Math.round(c.clientWidth*Math.min(devicePixelRatio||1,2))&&c.height===Math.round(c.clientHeight*Math.min(devicePixelRatio||1,2)); });
      await page.screenshot({path:'.playwright-cli/space-qa-'+theme+'-'+width+'.png',fullPage:true});
      const overflow=await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth);
      if(overflow)throw Error('Space overflow '+width);
    }
  }
  await page.setViewportSize({width:1440,height:1000});
  await page.locator('.space-editor summary').click();
  await page.locator('#space-label').fill('QA / Below origin');
  await page.locator('#space-node').fill('4');
  for(const [key,value]of Object.entries({x:'1',y:'-4',z:'-2'}))await page.locator('[data-space-form] [name="'+key+'"]').fill(value);
  await page.locator('[data-space-form] button[type="submit"]').click();
  await page.waitForFunction(()=>document.querySelectorAll('.space-device').length===5);
  if(!await page.locator('[data-space-feedback]').textContent().then(t=>t.includes('Saved')))throw Error('Save feedback missing');
  await page.getByRole('button',{name:'QA / Below origin Awaiting CSI 1 / -4 / -2 m',exact:true}).click();
  if(await page.locator('[data-space-form] [name="y"]').inputValue()!=='-4')throw Error('Signed vertical coordinate lost');
  await page.locator('[data-space-remove]').last().click();
  await page.waitForFunction(()=>document.querySelectorAll('.space-device').length===4);
  await page.locator('[data-space-clear]').click();
  await page.locator('#space-kind').selectOption('router');
  if(!await page.locator('#space-node').isDisabled())throw Error('Router requires sensor node ID');
  await page.evaluate(()=>{window.fetch=window.spaceRealFetch;delete window.qaLayout;});
  await page.reload();
  await page.locator('#installation-space').waitFor();
  if(await page.locator('.space-device').count()!==liveState.devices)throw Error('QA fixture leaked to server');
  if(errors.length)throw Error(JSON.stringify(errors));
  return {liveState,controls:'orbit/pan/zoom/fit/keyboard',layout:'add/edit/remove/router passed with isolated fixtures',themes:2,widths:[390,1440],errors};
}
