// Training Service for WiFi-DensePose UI
// Manages training lifecycle, progress streaming, and CSI recordings.

import { buildWsUrl } from '../config/api.config.js';
import { apiService } from './api.service.js';
import { probeWsAuthRequired, resolveWsUrl } from './ws-ticket.js';

export class TrainingService {
  constructor() {
    this.progressSocket = null;
    this.progressReconnectTimer = null;
    this.progressKeepaliveTimer = null;
    this.progressConnectPromise = null;
    this.progressConnectionEpoch = 0;
    this.progressReconnectAttempt = 0;
    this.progressShouldReconnect = false;
    this.listeners = {};
    this.logger = this.createLogger();
  }

  createLogger() {
    return {
      debug: (...args) => console.debug('[TRAIN-DEBUG]', new Date().toISOString(), ...args),
      info: (...args) => console.info('[TRAIN-INFO]', new Date().toISOString(), ...args),
      warn: (...args) => console.warn('[TRAIN-WARN]', new Date().toISOString(), ...args),
      error: (...args) => console.error('[TRAIN-ERROR]', new Date().toISOString(), ...args)
    };
  }

  // --- Event emitter helpers ---

  on(event, callback) {
    if (!this.listeners[event]) {
      this.listeners[event] = [];
    }
    this.listeners[event].push(callback);
    return () => this.off(event, callback);
  }

  off(event, callback) {
    if (!this.listeners[event]) return;
    this.listeners[event] = this.listeners[event].filter(cb => cb !== callback);
  }

  emit(event, data) {
    if (!this.listeners[event]) return;
    this.listeners[event].forEach(cb => {
      try { cb(data); } catch (err) { this.logger.error('Listener error', { event, err }); }
    });
  }

  // --- Training API methods ---

  async startTraining(config) {
    try {
      this.logger.info('Starting training', { config });
      const data = await apiService.post('/api/v1/train/start', config);
      this.emit('training-started', data);
      return data;
    } catch (error) {
      this.logger.error('Failed to start training', { error: error.message });
      throw error;
    }
  }

  async stopTraining() {
    try {
      this.logger.info('Stopping training');
      const data = await apiService.post('/api/v1/train/stop', {});
      this.emit('training-stopped', data);
      return data;
    } catch (error) {
      this.logger.error('Failed to stop training', { error: error.message });
      throw error;
    }
  }

  async getTrainingStatus() {
    try {
      const data = await apiService.get('/api/v1/train/status');
      return data;
    } catch (error) {
      this.logger.error('Failed to get training status', { error: error.message });
      throw error;
    }
  }

  async startPretraining(config) {
    try {
      this.logger.info('Starting pretraining', { config });
      const data = await apiService.post('/api/v1/train/pretrain', config);
      this.emit('training-started', data);
      return data;
    } catch (error) {
      this.logger.error('Failed to start pretraining', { error: error.message });
      throw error;
    }
  }

  async startLoraTraining(config) {
    try {
      this.logger.info('Starting LoRA training', { config });
      const data = await apiService.post('/api/v1/train/lora', config);
      this.emit('training-started', data);
      return data;
    } catch (error) {
      this.logger.error('Failed to start LoRA training', { error: error.message });
      throw error;
    }
  }

  // --- Recording API methods ---

  async listRecordings() {
    try {
      const data = await apiService.get('/api/v1/recording/list');
      return data?.recordings ?? [];
    } catch (error) {
      this.logger.error('Failed to list recordings', { error: error.message });
      throw error;
    }
  }

  async startRecording(config) {
    try {
      this.logger.info('Starting recording', { config });
      const data = await apiService.post('/api/v1/recording/start', config);
      this.emit('recording-started', data);
      return data;
    } catch (error) {
      this.logger.error('Failed to start recording', { error: error.message });
      throw error;
    }
  }

  async stopRecording() {
    try {
      this.logger.info('Stopping recording');
      const data = await apiService.post('/api/v1/recording/stop', {});
      this.emit('recording-stopped', data);
      return data;
    } catch (error) {
      this.logger.error('Failed to stop recording', { error: error.message });
      throw error;
    }
  }

  async deleteRecording(id) {
    try {
      this.logger.info('Deleting recording', { id });
      const data = await apiService.delete(
        `/api/v1/recording/${encodeURIComponent(id)}`
      );
      return data;
    } catch (error) {
      this.logger.error('Failed to delete recording', { id, error: error.message });
      throw error;
    }
  }

  // --- WebSocket progress stream ---

  connectProgressStream() {
    if (this.progressSocket && this.progressSocket.readyState <= WebSocket.OPEN) {
      this.logger.warn('Progress stream already connected');
      return this.progressSocket;
    }
    if (this.progressConnectPromise) return this.progressConnectPromise;

    this.progressShouldReconnect = true;
    this.progressReconnectAttempt = 0;
    const epoch = ++this.progressConnectionEpoch;
    return this._beginProgressConnection(epoch);
  }

  _beginProgressConnection(epoch) {
    const attempt = this._openProgressSocket(epoch);
    let trackedAttempt;
    trackedAttempt = attempt.finally(() => {
      if (this.progressConnectPromise === trackedAttempt) this.progressConnectPromise = null;
    });
    this.progressConnectPromise = trackedAttempt;
    return trackedAttempt;
  }

  async _openProgressSocket(epoch) {
    const baseUrl = buildWsUrl('/ws/train/progress');
    let url = baseUrl;
    let authRequired = null;
    try {
      ({ url, authRequired } = await resolveWsUrl(baseUrl));
    } catch (error) {
      this.logger.warn('Ticket request failed; trying the socket without a ticket', { error: error.message });
    }
    if (!this.progressShouldReconnect || epoch !== this.progressConnectionEpoch) return null;
    if (authRequired) {
      this.progressShouldReconnect = false;
      this.emit('progress-auth-required', { reason: authRequired });
      return null;
    }

    const ticketed = url !== baseUrl;
    let opened = false;
    let ws;
    try {
      ws = new WebSocket(url);
    } catch (error) {
      this.emit('progress-error', { error });
      this._scheduleProgressReconnect(epoch);
      return null;
    }
    this.progressSocket = ws;

    const isCurrent = () => epoch === this.progressConnectionEpoch && this.progressSocket === ws;
    ws.onopen = () => {
      if (!isCurrent()) {
        ws.close(1000, 'superseded');
        return;
      }
      opened = true;
      this.progressReconnectAttempt = 0;
      this.logger.info('Progress stream connected');
      this.emit('progress-connected', {});
      if (this.progressKeepaliveTimer) clearInterval(this.progressKeepaliveTimer);
      this.progressKeepaliveTimer = setInterval(() => {
        if (isCurrent() && ws.readyState === WebSocket.OPEN) {
          ws.send(JSON.stringify({ type: 'ping' }));
        }
      }, 30000);
    };

    ws.onmessage = (event) => {
      if (!isCurrent()) return;
      try {
        const message = JSON.parse(event.data);
        if (message?.type === 'pong') return;
        this.emit('progress-message', message);
        // Rust and Go both envelope stream messages as {type, data}. Keep the
        // existing UI event payload as the status/progress data object.
        this.emit('progress', message?.data && typeof message.data === 'object' ? message.data : message);
      } catch (error) {
        this.logger.warn('Failed to parse progress message', { error: error.message });
      }
    };

    ws.onerror = (error) => {
      if (!isCurrent()) return;
      this.logger.error('Progress stream error', { error });
      this.emit('progress-error', { error });
    };

    ws.onclose = (event) => {
      if (!isCurrent()) return;
      this.logger.info('Progress stream disconnected', { code: event?.code });
      this.progressSocket = null;
      if (this.progressKeepaliveTimer) {
        clearInterval(this.progressKeepaliveTimer);
        this.progressKeepaliveTimer = null;
      }
      this.emit('progress-disconnected', { code: event?.code });
      if (!this.progressShouldReconnect) return;
      if (!opened && !ticketed) {
        void probeWsAuthRequired().then((reason) => {
          if (!this.progressShouldReconnect || epoch !== this.progressConnectionEpoch) return;
          if (reason) {
            this.progressShouldReconnect = false;
            this.emit('progress-auth-required', { reason });
            return;
          }
          this._scheduleProgressReconnect(epoch);
        });
      } else {
        this._scheduleProgressReconnect(epoch);
      }
    };
    return ws;
  }

  _scheduleProgressReconnect(epoch) {
    if (!this.progressShouldReconnect || epoch !== this.progressConnectionEpoch || this.progressReconnectTimer) return;
    const delays = [500, 1000, 2000, 5000, 10000, 30000];
    const delay = delays[Math.min(this.progressReconnectAttempt, delays.length - 1)];
    this.progressReconnectAttempt = Math.min(this.progressReconnectAttempt + 1, delays.length - 1);
    this.emit('progress-reconnecting', { attempt: this.progressReconnectAttempt, delay });
    this.progressReconnectTimer = setTimeout(() => {
      this.progressReconnectTimer = null;
      if (!this.progressShouldReconnect || epoch !== this.progressConnectionEpoch) return;
      this._beginProgressConnection(epoch);
    }, delay);
  }

  disconnectProgressStream() {
    this.progressShouldReconnect = false;
    this.progressConnectionEpoch++;
    this.progressConnectPromise = null;
    if (this.progressReconnectTimer) {
      clearTimeout(this.progressReconnectTimer);
      this.progressReconnectTimer = null;
    }
    if (this.progressKeepaliveTimer) {
      clearInterval(this.progressKeepaliveTimer);
      this.progressKeepaliveTimer = null;
    }
    if (this.progressSocket) {
      const socket = this.progressSocket;
      this.progressSocket = null;
      socket.close(1000, 'client disconnected');
    }
    this.emit('progress-disconnected', { intentional: true });
  }

  dispose() {
    this.disconnectProgressStream();
    this.listeners = {};
    this.logger.info('TrainingService disposed');
  }
}

// Create singleton instance
export const trainingService = new TrainingService();
