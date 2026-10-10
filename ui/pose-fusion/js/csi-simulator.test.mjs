import test from 'node:test';
import assert from 'node:assert/strict';
import { CsiSimulator } from './csi-simulator.js';

test('synthetic Go frames never promote CSI to live measurements', () => {
  const simulator = new CsiSimulator();
  let verifiedFrames = 0;
  simulator.onVerifiedFrame = () => { verifiedFrames += 1; };

  simulator._handleJsonFrame({
    source: 'simulate',
    source_state: 'synthetic',
    nodes: [{ amplitude: [1, 2, 3, 4] }],
  });

  assert.equal(simulator.isLive, false);
  assert.equal(simulator.mode, 'demo');
  assert.equal(verifiedFrames, 0);
});

test('JSON CSI frames require explicit live provenance before promotion', () => {
  const simulator = new CsiSimulator();

  simulator._handleJsonFrame({ nodes: [{ amplitude: [1, 2, 3, 4] }] });
  assert.equal(simulator.isLive, false);

  simulator._handleJsonFrame({
    source: 'esp32:live',
    nodes: [{ amplitude: [1, 2, 3, 4] }],
  });
  assert.equal(simulator.isLive, true);
});
