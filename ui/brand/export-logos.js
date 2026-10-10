// Run through the existing Playwright CLI from the project root.
// Rasterizes the approved SVG and bundled font without inventing a new mark.
async page => {
  await page.goto('http://localhost:3000/ui/');
  const source = await page.evaluate(async () => {
    const response = await fetch('/ui/brand/logo.svg');
    if (!response.ok) throw Error('Cannot read approved SVG');
    return response.text();
  });
  await page.setContent('<style>@font-face{font-family:Inter;src:url(http://localhost:3000/ui/workspace/fonts/InterVariable.woff2);font-weight:100 900}html,body{margin:0;background:transparent}canvas{display:block}</style><canvas id="export"></canvas>');
  await page.evaluate(async () => {
    await document.fonts.load('590 112px Inter');
    await document.fonts.ready;
    if (!document.fonts.check('590 112px Inter')) throw Error('Wordmark font unavailable');
  });
  const variants = [
    ['mark-dark-1024',1024,1024,'#08090a',null,false],
    ['mark-light-1024',1024,1024,'#f7f8f8',null,false],
    ['mark-accent-1024',1024,1024,'#5e6ad2',null,false],
    ['wordmark-dark-1200',1200,256,'#08090a',null,true],
    ['wordmark-light-1200',1200,256,'#f7f8f8',null,true],
    ['wordmark-on-light-1200',1200,256,'#08090a','#f7f8f8',true],
    ['wordmark-on-dark-1200',1200,256,'#f7f8f8','#0f1011',true],
    ['favicon-32',32,32,'#f7f8f8','#0f1011',false],
    ['apple-touch-icon-180',180,180,'#f7f8f8','#0f1011',false],
    ['app-icon-192',192,192,'#f7f8f8','#0f1011',false],
    ['app-icon-512',512,512,'#f7f8f8','#0f1011',false],
    ['app-icon-maskable-512',512,512,'#f7f8f8','#0f1011',false]
  ];
  const exported = [];
  for (const [name,width,height,color,background,wordmark] of variants) {
    await page.setViewportSize({width:Math.max(width,390),height:Math.max(height,300)});
    const metadata = await page.evaluate(async ({source,name,width,height,color,background,wordmark}) => {
      const canvas=document.querySelector('canvas'); canvas.width=width; canvas.height=height;
      const context=canvas.getContext('2d');
      if(background){context.fillStyle=background;context.fillRect(0,0,width,height);}
      const image=new Image();
      image.src='data:image/svg+xml;charset=utf-8,'+encodeURIComponent(source.replaceAll('currentColor',color));
      await image.decode();
      if(wordmark){
        const size=144,gap=36;
        context.font='590 112px Inter';context.letterSpacing='-3px';
        const textWidth=context.measureText('Wave Ray').width;
        const x=(width-size-gap-textWidth)/2;
        context.drawImage(image,x,(height-size)/2,size,size);
        const metrics=context.measureText('Wave Ray');
        const baseline=(height+metrics.actualBoundingBoxAscent-metrics.actualBoundingBoxDescent)/2;
        context.fillStyle=color;context.fillText('Wave Ray',x+size+gap,baseline);
      }else{
        const size=width*(name.includes('maskable')?0.60:0.80);
        context.drawImage(image,(width-size)/2,(height-size)/2,size,size);
      }
      const alpha=context.getImageData(0,0,1,1).data[3];
      if(alpha !== (background?255:0))throw Error('Incorrect transparency: '+name);
      return {name,width,height,transparent:!background};
    },{source,name,width,height,color,background,wordmark});
    await page.locator('canvas').screenshot({path:'ui/brand/png/'+name+'.png',omitBackground:true});
    exported.push(metadata);
  }
  return exported;
}
