// Stream Service for WiFi-DensePose UI

import { API_CONFIG } from '../config/api.config.js';
import { apiService } from './api.service.js';

export class StreamService {
  // Get streaming status
  async getStatus() {
    return apiService.get(API_CONFIG.ENDPOINTS.STREAM.STATUS);
  }

  // Get streaming metrics
  async getMetrics() {
    return apiService.get(API_CONFIG.ENDPOINTS.STREAM.METRICS);
  }
}

// Create singleton instance
export const streamService = new StreamService();
