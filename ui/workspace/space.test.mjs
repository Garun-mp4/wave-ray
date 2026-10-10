import test from 'node:test';
import assert from 'node:assert/strict';
import { project, validDevices, renderSpace } from './space.js';

const device={id:'rx1',kind:'esp32',node_id:1,label:'Sensor',position:[-1,-2,-3]};
test('layout accepts negative coordinates, routers and the exact origin',()=>{
  assert.equal(validDevices([device,{id:'ap',kind:'router',label:'Router',position:[0,0,0]}]).length,2);
});
test('untrusted scene geometry cannot create people or invalid positions',()=>{
  for(const changed of [{position:[0,0]},{position:[NaN,0,0]},{position:[Infinity,0,0]},{position:[10001,0,0]},{kind:'person'},{node_id:-1},{node_id:256}]) {
    assert.deepEqual(validDevices([{...device,...changed}]),[]);
  }
  assert.equal(validDevices([device,device]).length,1);
  assert.equal(validDevices([device,{...device,id:'rx2'}]).length,1);
});
test('perspective projects above and below origin and responds to camera rotation',()=>{
  const camera={center:[0,0,0],distance:10,yaw:0,pitch:0,pan:[0,0]};
  assert.equal(project([0,0,0],camera,800,600).y,300);
  assert.ok(project([0,-2,0],camera,800,600).y>300);
  assert.ok(project([0,2,0],camera,800,600).y<300);
  assert.equal(project([0,0,-11],camera,800,600),null);
  assert.notDeepEqual(project([2,0,1],camera,800,600),project([2,0,1],{...camera,yaw:1},800,600));
});
test('space has accessible controls, no floor or simulated humans and escapes API errors',()=>{
  const html=renderSpace({error:'<script>bad</script>'});
  assert.match(html,/tabindex="0"/);
  assert.match(html,/No device positions yet/);
  assert.match(html,/Person locations, pose, and animal detection are not available/);
  assert.doesNotMatch(html,/<script>|GridHelper|FloorPlane|Math.random/);
  assert.match(html,/&lt;script&gt;/);
});
