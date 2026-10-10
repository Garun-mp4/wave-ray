import { escapeHtml as safe } from './view-model.js';

export function validDevices(devices) {
  if (!Array.isArray(devices) || devices.length > 64) return [];
  const ids = new Set(), nodes = new Set();
  return devices.filter(d => {
    if (!d || !/^[a-zA-Z0-9_-]{1,48}$/.test(d.id) || ids.has(d.id) ||
        !['esp32', 'router'].includes(d.kind) || typeof d.label !== 'string' || !d.label.trim() || d.label.length > 80 ||
        !Array.isArray(d.position) || d.position.length !== 3 || !d.position.every(v => Number.isFinite(v) && Math.abs(v) <= 10000) ||
        (d.kind === 'esp32' && (!Number.isInteger(d.node_id) || d.node_id < 0 || d.node_id > 255 || nodes.has(d.node_id))) ||
        (d.kind === 'router' && d.node_id != null)) return false;
    ids.add(d.id); if (d.kind === 'esp32') nodes.add(d.node_id);
    return true;
  });
}

// Perspective projection with a fully rotatable camera; y is vertical and has
// no lower bound. This is an instrument diagram, not a simulated RF field.
export function project(point, camera, width, height) {
  const [x, y, z] = point.map((v, i) => v - camera.center[i]);
  const a = Math.cos(camera.yaw), b = Math.sin(camera.yaw);
  const c = Math.cos(camera.pitch), d = Math.sin(camera.pitch);
  const rx = a * x - b * z, rz = b * x + a * z;
  const ry = c * y - d * rz, depth = d * y + c * rz;
  const distance = camera.distance + depth;
  if (distance <= camera.distance * 0.05) return null;
  const scale = Math.min(width, height) * 0.9 / distance;
  return { x: width / 2 + rx * scale + camera.pan[0], y: height / 2 - ry * scale + camera.pan[1], depth };
}

export function renderSpace(data) {
  return '<section class="space-workbench" aria-label="Installation space">' +
    '<div class="space-toolbar"><div><strong>Installation coordinates</strong><span>Metres · Y vertical · positions entered by you</span></div>' +
    '<div class="space-controls"><button class="button" type="button" data-space-view="fit">Fit devices</button><button class="icon-button" type="button" data-space-view="in" aria-label="Zoom in">+</button><button class="icon-button" type="button" data-space-view="out" aria-label="Zoom out">−</button></div></div>' +
    '<div class="space-body"><div class="space-viewport"><canvas id="installation-space" tabindex="0" aria-label="3D device layout. Drag to orbit, Shift-drag to pan, scroll to zoom. Arrow keys orbit; Shift and arrow keys pan; plus and minus zoom; Home fits devices." aria-describedby="space-instructions"></canvas>' +
    '<div class="space-empty" data-space-empty><strong>No device positions yet</strong><span>Add your ESP32 and router coordinates to place them here.</span></div>' +
    '<span class="space-origin-note">Origin: 0 / 0 / 0 m</span></div>' +
    '<aside class="space-inspector"><h2>Devices</h2><p class="section-note">Configured positions, not automatically measured locations.</p><div data-space-list></div>' +
    '<details class="space-editor"><summary>Add or edit device</summary><form data-space-form><input name="id" type="hidden">' +
    '<div class="form-field"><label class="form-label" for="space-kind">Device type</label><select class="text-input" id="space-kind" name="kind"><option value="esp32">ESP32</option><option value="router">Router / access point</option></select></div>' +
    '<div class="form-field"><label class="form-label" for="space-label">Name</label><input class="text-input" id="space-label" name="label" maxlength="80" required placeholder="e.g. ESP32 by the window"></div>' +
    '<div class="form-field" data-space-node-field><label class="form-label" for="space-node">Firmware node ID</label><input class="text-input" id="space-node" name="node_id" type="number" min="0" max="255" step="1" required><span class="form-hint">Match the ID reported by your firmware.</span></div>' +
    '<fieldset class="space-coordinates"><legend>Position in metres</legend>' + ['X', 'Y', 'Z'].map(axis => '<label>' + axis + '<input class="text-input" name="' + axis.toLowerCase() + '" type="number" min="-10000" max="10000" step="any" required></label>').join('') + '</fieldset>' +
    '<p class="form-hint">Choose an origin at your transmitter, or another fixed reference. Negative coordinates are supported.</p>' +
    '<div class="form-actions"><button class="button button-primary" type="submit">Save device</button><button class="button" type="button" data-space-clear>New device</button></div>' +
    '<p class="form-hint" data-space-feedback role="status"></p></form></details></aside></div>' +
    '<footer class="space-caption"><span id="space-instructions">Drag to orbit · Shift-drag to pan · scroll to zoom</span><span data-space-count></span></footer></section>' +
    '<p class="measurement-limit">Person locations, pose, and animal detection are not available in this installation.</p>' +
    (data.error ? '<div class="info-banner error-banner" role="alert">' + safe(data.error) + '</div>' : '');
}

export function mountSpace(root, initial, save) {
  const canvas = root.querySelector('#installation-space'), ctx = canvas?.getContext('2d');
  if (!ctx) return { update() {}, measurement() {}, dispose() {} };
  const scope = new AbortController(), options = { signal: scope.signal };
  let data = initial, devices = [], frame = null, dragging = null, saving = false, disposed = false, selected = '';
  let receivedAt = Date.now(), draftRevision = null;
  const liveFrames = new Map();
  const camera = { yaw: -0.65, pitch: 0.35, distance: 9, center: [0, 0, 0], pan: [0, 0] };
  const form = root.querySelector('[data-space-form]'), feedback = root.querySelector('[data-space-feedback]');
  const css = token => getComputedStyle(document.documentElement).getPropertyValue(token).trim();
  function isReceiving(id) {
    if (Date.now() - (liveFrames.get(id) || 0) <= 10000) return true;
    const node = (data.nodes || []).find(n => n.node_id === id);
    return node?.status === 'active' && node?.csi_status === 'active' &&
      Number.isFinite(node.csi_last_seen_ms) && node.csi_last_seen_ms + Date.now() - receivedAt <= 10000;
  }
  function invalidate() { if (frame == null && !disposed) frame = requestAnimationFrame(draw); }
  function fit() {
    const min = [0,0,0], max = [0,0,0];
    devices.forEach(d => d.position.forEach((v,i) => { min[i] = Math.min(min[i],v); max[i] = Math.max(max[i],v); }));
    camera.center = min.map((v,i) => (v + max[i])/2);
    camera.distance = Math.max(5, Math.hypot(...min.map((v,i) => max[i]-v)) * 1.7);
    camera.pan = [0,0]; camera.yaw = -0.65; camera.pitch = 0.35; invalidate();
  }
  function draw() {
    frame = null;
    const w = canvas.clientWidth, h = canvas.clientHeight, ratio = Math.min(window.devicePixelRatio || 1, 2);
    if (!w || !h) return;
    canvas.width = Math.round(w * ratio); canvas.height = Math.round(h * ratio); ctx.scale(ratio, ratio);
    const text = css('--color-text-tertiary'), accent = css('--color-accent-primary'), primary = css('--color-text-primary');
    ctx.fillStyle = css('--color-bg-panel'); ctx.fillRect(0,0,w,h);
    ctx.lineWidth = 1; ctx.font = '12px "Inter Variable", sans-serif';
    const at = p => project(p, camera, w, h);
    function line(a,b,color,dashed=false) {
      const u=at(a),v=at(b); if(!u||!v) return;
      ctx.strokeStyle=color; ctx.setLineDash(dashed ? [3,5] : []); ctx.beginPath(); ctx.moveTo(u.x,u.y); ctx.lineTo(v.x,v.y); ctx.stroke(); ctx.setLineDash([]);
    }
    const extent = Math.max(2, camera.distance / 3);
    ['X','Y','Z'].forEach((axis,i) => {
      const negative=[0,0,0], positive=[0,0,0]; negative[i]=-extent;positive[i]=extent;
      line(negative,[0,0,0],css('--color-border-standard'),true); line([0,0,0],positive,css('--color-border-standard'));
      [negative,positive].forEach((p,j) => { const s=at(p);if(s){ctx.fillStyle=text;ctx.fillText((j ? '+' : '−')+axis,s.x+5,s.y-5);} });
    });
    const origin=at([0,0,0]);if(origin){ctx.fillStyle=text;ctx.beginPath();ctx.arc(origin.x,origin.y,2,0,Math.PI*2);ctx.fill();ctx.fillText('0',origin.x+7,origin.y+14);}
    const labels = [];
    const markers = devices.map(d => at(d.position)).filter(Boolean);
    devices.map(d=>({d,p:at(d.position)})).filter(v=>v.p).sort((a,b)=>b.p.depth-a.p.depth).forEach(({d,p})=>{
      const live=d.kind==='esp32' && isReceiving(d.node_id);
      const color=selected===d.id||live ? accent : text;
      ctx.strokeStyle=color;ctx.fillStyle=css('--color-bg-panel');ctx.lineWidth=selected===d.id?2:1.5;
      if(d.kind==='router'){
        ctx.beginPath();ctx.roundRect(p.x-12,p.y-5,24,12,3);ctx.fill();ctx.stroke();
        ctx.beginPath();ctx.moveTo(p.x-8,p.y-5);ctx.lineTo(p.x-11,p.y-15);ctx.moveTo(p.x+8,p.y-5);ctx.lineTo(p.x+11,p.y-15);ctx.stroke();
      }else{
        const size=0.055*camera.distance, center=d.position;
        const corners=[[-1,-1,-1],[1,-1,-1],[1,1,-1],[-1,1,-1],[-1,-1,1],[1,-1,1],[1,1,1],[-1,1,1]].map(v=>v.map((n,i)=>center[i]+n*size/2));
        [[0,1],[1,2],[2,3],[3,0],[4,5],[5,6],[6,7],[7,4],[0,4],[1,5],[2,6],[3,7]].forEach(([a,b])=>line(corners[a],corners[b],color));
      }
      const label = selected === d.id ? d.label.slice(0, 24) : d.kind === 'esp32' ? 'ESP32 ' + d.node_id : d.label.slice(0, 18);
      const tw = ctx.measureText(label).width;
      const candidates = [[p.x+18,p.y-8],[p.x+18,p.y+20],[p.x-tw-18,p.y-8],[p.x-tw-18,p.y+20],[p.x-tw/2,p.y-26]];
      const spot = candidates.find(([x,y]) => x >= 8 && x+tw < w-8 && y >= 16 && y < h-24 &&
        !markers.some(marker => x < marker.x+18 && x+tw > marker.x-18 && y-14 < marker.y+18 && y+4 > marker.y-18) &&
        !labels.some(box => x < box.x+box.w+8 && x+tw+8 > box.x && y-14 < box.y+6 && y+6 > box.y-14));
      if (spot) {
        const [x,y] = spot; labels.push({x,y,w:tw});
        ctx.fillStyle=css('--color-bg-panel');ctx.fillRect(x-3,y-13,tw+6,18);
        ctx.fillStyle=selected===d.id ? accent : primary;ctx.fillText(label,x,y);
      }
    });
    ctx.lineWidth=1;
  }
  function refreshList() {
    const focused = document.activeElement;
    const action = focused?.dataset.spaceSelect ? "spaceSelect" : focused?.dataset.spaceRemove ? "spaceRemove" : null;
    const focusID = action && focused.dataset[action];
    root.querySelector('[data-space-list]').innerHTML=devices.length ? devices.map(d=>{
      const status=d.kind==='router'?'Configured AP · not probed':isReceiving(d.node_id)?'Receiving CSI':'Awaiting CSI';
      return '<div class="space-device"><button type="button" data-space-select="'+safe(d.id)+'" aria-pressed="'+(selected===d.id)+'"><strong>'+safe(d.label)+'</strong><span>'+safe(status)+'</span><small>'+d.position.map(v=>safe(v)).join(' / ')+' m</small></button><button class="icon-button" type="button" data-space-remove="'+safe(d.id)+'" aria-label="Remove '+safe(d.label)+'"><svg aria-hidden="true"><use href="workspace/icons.svg#icon-x"></use></svg></button></div>';
    }).join('') : '<p class="space-list-empty">Your installation is not configured.</p>';
    if (focusID) [...root.querySelectorAll(action === 'spaceSelect' ? '[data-space-select]' : '[data-space-remove]')].find(button => button.dataset[action] === focusID)?.focus({preventScroll:true});
    root.querySelector('[data-space-empty]').hidden=devices.length>0;
    root.querySelector('[data-space-count]').textContent=devices.length+' configured devices';
  }
  function update(next) { if (next.nodes !== data.nodes) receivedAt=Date.now();data=next;devices=validDevices(data.layout?.devices);refreshList();invalidate(); }
  function measurement(message) {
    if (message?.source !== 'esp32' || message._simulated || message.source_state === 'synthetic') return;
    for (const node of message.nodes || []) {
      if (Number.isInteger(node.node_id) && node.node_id >= 0 && node.node_id <= 255 && node.subcarrier_count > 0) liveFrames.set(node.node_id, Date.now());
    }
  }
  function kindChanged() { const router=form.elements.kind.value==='router';form.elements.node_id.disabled=router;root.querySelector('[data-space-node-field]').hidden=router; }
  form.elements.kind.addEventListener('change',kindChanged,options);
  async function commit(next) {
    if(saving) return; saving=true;feedback.textContent='Saving…';
    form.querySelector('button[type="submit"]').disabled=true;
    try { const result=await save({revision:data.layout?.revision||0,devices:next});if(disposed)return;
      selected='';draftRevision=null;update({...data,layout:result.layout});form.reset();kindChanged();feedback.textContent='Saved on the server. Recalibrate after moving a sensor.';fit();
    }catch(error){if(!disposed) feedback.textContent=error.message||'Could not save. Refresh and retry.';}
    finally{saving=false;if(!disposed)form.querySelector('button[type="submit"]').disabled=false;}
  }
  form.addEventListener('submit',event=>{
    event.preventDefault();if(!form.reportValidity())return;
    if (draftRevision != null && draftRevision !== data.layout?.revision) { feedback.textContent = 'This layout changed elsewhere. Choose New device or select the device again before saving.'; return; }
    const values=new FormData(form),id=values.get('id')||'device-'+crypto.randomUUID().slice(0,16);
    const device={id,kind:values.get('kind'),label:String(values.get('label')).trim(),position:['x','y','z'].map(k=>Number(values.get(k)))};
    if(device.kind==='esp32')device.node_id=Number(values.get('node_id'));
    const next=[...devices.filter(d=>d.id!==id),device];if(validDevices(next).length!==next.length){feedback.textContent='Check coordinates and use a unique firmware node ID.';return;}
    void commit(next);
  },options);
  root.addEventListener('click',event=>{
    const target=event.target.closest('button');if(!target)return;
    if(target.dataset.spaceView){const action=target.dataset.spaceView;if(action==='fit')fit();else{camera.distance=Math.max(0.5,Math.min(100000,camera.distance*(action==='in'?0.8:1.25)));invalidate();}}
    if(target.hasAttribute('data-space-clear')&&!saving){form.reset();selected='';draftRevision=null;kindChanged();refreshList();invalidate();}
    if(target.dataset.spaceSelect&&!saving){const d=devices.find(d=>d.id===target.dataset.spaceSelect);if(!d)return;selected=d.id;draftRevision=data.layout?.revision;
      ['id','kind','label'].forEach(k=>{form.elements[k].value=d[k];});form.elements.node_id.value=d.node_id??'';
      ['x','y','z'].forEach((k,i)=>{form.elements[k].value=d.position[i];});kindChanged();root.querySelector('.space-editor').open=true;refreshList();invalidate();}
    if(target.dataset.spaceRemove&&!saving){void commit(devices.filter(d=>d.id!==target.dataset.spaceRemove));}
  },options);
  canvas.addEventListener('pointerdown',event=>{if(event.button!==0)return;dragging={id:event.pointerId,x:event.clientX,y:event.clientY};canvas.setPointerCapture(event.pointerId);},options);
  canvas.addEventListener('pointermove',event=>{if(!dragging||dragging.id!==event.pointerId)return;const dx=event.clientX-dragging.x,dy=event.clientY-dragging.y;
    if(event.shiftKey){camera.pan[0]+=dx;camera.pan[1]+=dy;}else{camera.yaw+=dx*0.008;camera.pitch+=dy*0.008;}
    dragging.x=event.clientX;dragging.y=event.clientY;invalidate();
  },options);
  ['pointerup','pointercancel','lostpointercapture'].forEach(type=>canvas.addEventListener(type,()=>{dragging=null;},options));
  canvas.addEventListener('wheel',event=>{event.preventDefault();camera.distance=Math.max(0.5,Math.min(100000,camera.distance*Math.exp(Math.max(-100,Math.min(100,event.deltaY))*0.003)));invalidate();},{...options,passive:false});
  canvas.addEventListener('keydown',event=>{
    if(['ArrowLeft','ArrowRight','ArrowUp','ArrowDown','+','=','-','Home'].includes(event.key)){event.preventDefault();
      if(event.key==='Home')fit();
      else if(['+','=','-'].includes(event.key))camera.distance=Math.max(0.5,Math.min(100000,camera.distance*(event.key==='-'?1.15:0.87)));
      else{const horizontal=event.key==='ArrowLeft'||event.key==='ArrowRight',direction=event.key==='ArrowLeft'||event.key==='ArrowUp'?-1:1;
        if(event.shiftKey)camera.pan[horizontal?0:1]+=direction*16;else camera[horizontal?'yaw':'pitch']+=direction*0.12;}
      invalidate();
    }
  },options);
  const resize=new ResizeObserver(invalidate);resize.observe(canvas);
  const theme=new MutationObserver(invalidate);theme.observe(document.documentElement,{attributes:true,attributeFilter:['data-theme']});
  update(initial);fit();
  let lastLive = '';
  const freshness = setInterval(() => {
    const live = devices.map(d => isReceiving(d.node_id)).join(',');
    if (live !== lastLive) { lastLive = live; refreshList(); invalidate(); }
  }, 1000);
  return {update,measurement,dispose(){disposed=true;clearInterval(freshness);scope.abort();resize.disconnect();theme.disconnect();if(frame!=null)cancelAnimationFrame(frame);}};
}
