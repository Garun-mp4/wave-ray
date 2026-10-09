// Keep the UI service surface limited to endpoints implemented by the Rust API.
// Run: node --test ui/services/api-contract.test.mjs

import { test } from 'node:test';
import assert from 'node:assert/strict';

import { API_CONFIG } from '../config/api.config.js';
import { PoseService } from './pose.service.js';
import { StreamService } from './stream.service.js';
import { isTrainingTaskSupported } from '../components/TrainingPanel.js';

test('pose endpoint constants match implemented Rust routes', () => {
  assert.equal(API_CONFIG.ENDPOINTS.POSE.CURRENT, '/api/v1/pose/current');
  assert.equal(API_CONFIG.ENDPOINTS.POSE.ZONES_SUMMARY, '/api/v1/pose/zones/summary');
  assert.equal(API_CONFIG.ENDPOINTS.POSE.ACTIVITIES, '/api/v1/pose/activities');
  assert.equal(API_CONFIG.ENDPOINTS.POSE.CALIBRATE, '/api/v1/pose/calibrate');
  assert.equal(API_CONFIG.ENDPOINTS.POSE.CALIBRATION_STATUS, '/api/v1/pose/calibration/status');
  assert.equal(API_CONFIG.ENDPOINTS.POSE.STATS, '/api/v1/pose/stats');
  for (const unsupported of ['ANALYZE', 'ZONE_OCCUPANCY', 'HISTORICAL']) {
    assert.equal(API_CONFIG.ENDPOINTS.POSE[unsupported], undefined);
  }

  const pose = new PoseService();
  assert.equal(typeof pose.getZonesSummary, 'function');
  assert.equal(pose.getZoneOccupancy, undefined);
  assert.equal(pose.getHistoricalData, undefined);
  assert.equal(pose.analyzePose, undefined);
  assert.equal(pose.updateStreamConfig, undefined);
  assert.equal(pose.requestStreamStatus, undefined);
});

test('stream REST surface uses Rust status and metrics routes only', () => {
  assert.equal(API_CONFIG.ENDPOINTS.STREAM.STATUS, '/api/v1/stream/status');
  assert.equal(API_CONFIG.ENDPOINTS.STREAM.METRICS, '/api/v1/metrics');
  assert.equal(API_CONFIG.ENDPOINTS.STREAM.WS_POSE, '/api/v1/stream/pose');
  for (const unsupported of ['START', 'STOP', 'CLIENTS', 'DISCONNECT_CLIENT', 'BROADCAST', 'WS_EVENTS']) {
    assert.equal(API_CONFIG.ENDPOINTS.STREAM[unsupported], undefined);
  }

  const stream = new StreamService();
  assert.equal(typeof stream.getStatus, 'function');
  assert.equal(typeof stream.getMetrics, 'function');
  assert.equal(stream.start, undefined);
  assert.equal(stream.stop, undefined);
  assert.equal(stream.getClients, undefined);
  assert.equal(stream.broadcast, undefined);
});

test('training controls honor the server advertised task capabilities', () => {
  const capabilities = { enabled: true, supported_tasks: ['activity_classifier'] };
  assert.equal(isTrainingTaskSupported(capabilities, 'activity_classifier'), true);
  assert.equal(isTrainingTaskSupported(capabilities, 'pretrain'), false);
  assert.equal(isTrainingTaskSupported(capabilities, 'lora'), false);
  assert.equal(isTrainingTaskSupported({ enabled: false }, 'activity_classifier'), false);
  assert.equal(isTrainingTaskSupported({ enabled: true }, 'pretrain'), true,
    'Rust deployments without the Go capability extension keep their existing behavior');
});
