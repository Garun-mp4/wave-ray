/**
 * CanvasRenderer — Renders skeleton overlay on video, CSI heatmap,
 * embedding space visualization, and fusion confidence bars.
 */

import { SKELETON_CONNECTIONS } from './pose-decoder.js';

export class CanvasRenderer {
  constructor() {
    this.colors = {
      joint:      '#5e6ad2',
      jointGlow:  'rgba(94, 106, 210, 0.32)',
      limb:       '#828fff',
      limbGlow:   'rgba(130, 143, 255, 0.16)',
      csiJoint:   '#d0d6e0',
      csiLimb:    '#8a8f98',
      fused:      '#a8b1ff',
      confidence: 'rgba(255,255,255,0.3)',
      videoEmb:   '#828fff',
      csiEmb:     '#d0d6e0',
      fusedEmb:   '#5e6ad2',
    };
  }

  /**
   * Draw skeleton overlay on the video canvas
   * @param {CanvasRenderingContext2D} ctx
   * @param {Array<{x,y,confidence}>} keypoints - Normalized [0,1] coordinates
   * @param {number} width - Canvas width
   * @param {number} height - Canvas height
   * @param {object} opts
   */
  drawSkeleton(ctx, keypoints, width, height, opts = {}) {
    const minConf = opts.minConfidence || 0.3;
    const color = opts.color || 'video';
    const isCsi = color === 'csi';
    const jointColor = isCsi ? this.colors.csiJoint : this.colors.joint;
    const limbColor = isCsi ? this.colors.csiLimb : this.colors.limb;
    const glowColor = isCsi ? 'rgba(208,214,224,0.18)' : this.colors.jointGlow;

    // Extended keypoint styling
    const fingerColor = '#a8b1ff';
    const fingerGlow = 'rgba(168,177,255,0.35)';
    const fingerLimb = 'rgba(168,177,255,0.42)';
    const toeColor = '#d0d6e0';
    const neckColor = '#ffffff';       // White for neck

    ctx.clearRect(0, 0, width, height);

    if (!keypoints || keypoints.length === 0) return;

    // Draw limbs first (behind joints)
    ctx.lineCap = 'round';

    for (const [i, j] of SKELETON_CONNECTIONS) {
      const kpA = keypoints[i];
      const kpB = keypoints[j];
      if (!kpA || !kpB || kpA.confidence < minConf || kpB.confidence < minConf) continue;

      const ax = kpA.x * width, ay = kpA.y * height;
      const bx = kpB.x * width, by = kpB.y * height;
      const avgConf = (kpA.confidence + kpB.confidence) / 2;

      // Is this a hand/finger connection? (indices 17-22)
      const isFingerLink = i >= 17 && i <= 22 || j >= 17 && j <= 22;
      const isToeLink = i >= 23 && i <= 24 || j >= 23 && j <= 24;

      // Glow
      ctx.strokeStyle = isFingerLink ? fingerLimb : this.colors.limbGlow;
      ctx.lineWidth = isFingerLink ? 4 : 8;
      ctx.globalAlpha = avgConf * (isFingerLink ? 0.3 : 0.4);
      ctx.beginPath();
      ctx.moveTo(ax, ay);
      ctx.lineTo(bx, by);
      ctx.stroke();

      // Main line
      ctx.strokeStyle = isFingerLink ? fingerColor : isToeLink ? toeColor : limbColor;
      ctx.lineWidth = isFingerLink || isToeLink ? 1.5 : 2.5;
      ctx.globalAlpha = avgConf;
      ctx.beginPath();
      ctx.moveTo(ax, ay);
      ctx.lineTo(bx, by);
      ctx.stroke();
    }

    // Draw joints
    ctx.globalAlpha = 1;
    for (let idx = 0; idx < keypoints.length; idx++) {
      const kp = keypoints[idx];
      if (!kp || kp.confidence < minConf) continue;

      const x = kp.x * width;
      const y = kp.y * height;
      const isFinger = idx >= 17 && idx <= 22;
      const isToe = idx >= 23 && idx <= 24;
      const isNeck = idx === 25;
      const r = isFinger ? 2 + kp.confidence * 2 : isToe ? 2 : 3 + kp.confidence * 3;
      const jColor = isFinger ? fingerColor : isToe ? toeColor : isNeck ? neckColor : jointColor;
      const gColor = isFinger ? fingerGlow : glowColor;

      // Glow
      ctx.beginPath();
      ctx.arc(x, y, r + (isFinger ? 3 : 4), 0, Math.PI * 2);
      ctx.fillStyle = gColor;
      ctx.globalAlpha = kp.confidence * (isFinger ? 0.5 : 0.6);
      ctx.fill();

      // Joint dot
      ctx.beginPath();
      ctx.arc(x, y, r, 0, Math.PI * 2);
      ctx.fillStyle = jColor;
      ctx.globalAlpha = kp.confidence;
      ctx.fill();

      // White center (body joints only)
      if (!isFinger && !isToe) {
        ctx.beginPath();
        ctx.arc(x, y, r * 0.4, 0, Math.PI * 2);
        ctx.fillStyle = '#fff';
        ctx.globalAlpha = kp.confidence * 0.8;
        ctx.fill();
      }
    }

    ctx.globalAlpha = 1;

    // Confidence label + keypoint count
    if (opts.label) {
      const visCount = keypoints.filter(kp => kp && kp.confidence >= minConf).length;
      ctx.font = '11px "JetBrains Mono", monospace';
      ctx.fillStyle = jointColor;
      ctx.globalAlpha = 0.8;
      ctx.fillText(`${opts.label} · ${visCount} joints`, 8, height - 8);
      ctx.globalAlpha = 1;
    }
  }

  /**
   * Draw CSI amplitude heatmap
   * @param {CanvasRenderingContext2D} ctx
   * @param {{ data: Float32Array, width: number, height: number }} heatmap
   * @param {number} canvasW
   * @param {number} canvasH
   */
  drawCsiHeatmap(ctx, heatmap, canvasW, canvasH) {
    ctx.clearRect(0, 0, canvasW, canvasH);

    if (!heatmap || !heatmap.data || heatmap.height < 2) {
      ctx.fillStyle = '#0f1011';
      ctx.fillRect(0, 0, canvasW, canvasH);
      ctx.font = '11px "JetBrains Mono", monospace';
      ctx.fillStyle = 'rgba(255,255,255,0.3)';
      ctx.fillText('Waiting for CSI data...', 8, canvasH / 2);
      return;
    }

    const { data, width: dw, height: dh } = heatmap;
    const cellW = canvasW / dw;
    const cellH = canvasH / dh;

    for (let y = 0; y < dh; y++) {
      for (let x = 0; x < dw; x++) {
        const val = Math.min(1, Math.max(0, data[y * dw + x]));
        ctx.fillStyle = this._heatmapColor(val);
        ctx.fillRect(x * cellW, y * cellH, cellW + 0.5, cellH + 0.5);
      }
    }

    // Axis labels
    ctx.font = '9px "JetBrains Mono", monospace';
    ctx.fillStyle = 'rgba(255,255,255,0.4)';
    ctx.fillText('Subcarrier →', 4, canvasH - 4);
    ctx.save();
    ctx.translate(canvasW - 4, canvasH - 4);
    ctx.rotate(-Math.PI / 2);
    ctx.fillText('Time ↑', 0, 0);
    ctx.restore();
  }

  /**
   * Draw embedding space 2D projection
   * @param {CanvasRenderingContext2D} ctx
   * @param {{ video: Array, csi: Array, fused: Array }} points
   * @param {number} w
   * @param {number} h
   */
  drawEmbeddingSpace(ctx, points, w, h) {
    ctx.fillStyle = '#08090a';
    ctx.fillRect(0, 0, w, h);

    // Grid
    ctx.strokeStyle = 'rgba(255,255,255,0.05)';
    ctx.lineWidth = 0.5;
    for (let i = 0; i <= 4; i++) {
      const x = (i / 4) * w;
      ctx.beginPath(); ctx.moveTo(x, 0); ctx.lineTo(x, h); ctx.stroke();
      const y = (i / 4) * h;
      ctx.beginPath(); ctx.moveTo(0, y); ctx.lineTo(w, y); ctx.stroke();
    }

    // Axes
    ctx.strokeStyle = 'rgba(255,255,255,0.1)';
    ctx.lineWidth = 1;
    ctx.beginPath(); ctx.moveTo(w / 2, 0); ctx.lineTo(w / 2, h); ctx.stroke();
    ctx.beginPath(); ctx.moveTo(0, h / 2); ctx.lineTo(w, h / 2); ctx.stroke();

    // Auto-scale: find max extent across all point sets
    let maxExtent = 0.01;
    for (const pts of [points.video, points.csi, points.fused]) {
      if (!pts) continue;
      for (const p of pts) {
        if (!p) continue;
        maxExtent = Math.max(maxExtent, Math.abs(p[0]), Math.abs(p[1]));
      }
    }
    const scale = 0.42 / maxExtent; // Fill ~84% of half-width

    const drawPoints = (pts, color, size) => {
      if (!pts || pts.length === 0) return;
      const len = pts.length;

      // Draw trail line connecting recent points
      if (len >= 2) {
        ctx.beginPath();
        let started = false;
        for (let i = 0; i < len; i++) {
          const p = pts[i];
          if (!p) continue;
          const px = w / 2 + p[0] * scale * w;
          const py = h / 2 + p[1] * scale * h;
          if (px < -10 || px > w + 10 || py < -10 || py > h + 10) continue;
          if (!started) { ctx.moveTo(px, py); started = true; }
          else ctx.lineTo(px, py);
        }
        ctx.strokeStyle = color;
        ctx.globalAlpha = 0.2;
        ctx.lineWidth = 1;
        ctx.stroke();
      }

      // Draw dots with glow on newest
      for (let i = 0; i < len; i++) {
        const p = pts[i];
        if (!p) continue;
        const age = 1 - (i / len) * 0.7;
        const px = w / 2 + p[0] * scale * w;
        const py = h / 2 + p[1] * scale * h;

        if (px < -10 || px > w + 10 || py < -10 || py > h + 10) continue;

        // Glow on newest point
        if (i === len - 1) {
          ctx.beginPath();
          ctx.arc(px, py, size + 4, 0, Math.PI * 2);
          ctx.fillStyle = color;
          ctx.globalAlpha = 0.3;
          ctx.fill();
        }

        ctx.beginPath();
        ctx.arc(px, py, i === len - 1 ? size + 1 : size, 0, Math.PI * 2);
        ctx.fillStyle = color;
        ctx.globalAlpha = age * 0.8;
        ctx.fill();
      }
    };

    drawPoints(points.video, this.colors.videoEmb, 3);
    drawPoints(points.csi, this.colors.csiEmb, 3);
    drawPoints(points.fused, this.colors.fusedEmb, 4);
    ctx.globalAlpha = 1;

    // Legend
    ctx.font = '9px "JetBrains Mono", monospace';
    const legends = [
      { color: this.colors.videoEmb, label: 'Video' },
      { color: this.colors.csiEmb, label: 'CSI' },
      { color: this.colors.fusedEmb, label: 'Fused' },
    ];
    legends.forEach((l, i) => {
      const ly = 12 + i * 14;
      ctx.fillStyle = l.color;
      ctx.beginPath();
      ctx.arc(10, ly - 3, 3, 0, Math.PI * 2);
      ctx.fill();
      ctx.fillStyle = 'rgba(255,255,255,0.5)';
      ctx.fillText(l.label, 18, ly);
    });
  }

  _heatmapColor(val) {
    const amount = Math.min(1, Math.max(0, (val - 0.15) / 0.75));
    const start = [15, 16, 17];
    const end = [78, 86, 165];
    const color = start.map((channel, index) => Math.round(channel + (end[index] - channel) * amount));
    return `rgb(${color[0]} ${color[1]} ${color[2]})`;
  }
}
