/**
 * Shared tunnel core for all hooks.
 *
 * Usage (from a platform hook):
 *   var tunnel = window.__tunnelCore({
 *     getDC:      function() { return activeDC; },
 *     wsUrl:      'ws://127.0.0.1:9000/ws',
 *     log:        function() { console.log('[HOOK]', ...arguments); },
 *     onWsOpen:   function() {},   // optional
 *     onWsClose:  function() {},   // optional
 *   });
 *
 * Returned object:
 *   tunnel.handleChunk(arrayBuffer)   -- feed incoming DC binary messages
 *   tunnel.connectWS()                -- establish WS to relay
 *   tunnel.sendRaw(data)              -- send string or ArrayBuffer over DC
 *   tunnel.closeWS()                  -- tear down WS
 *   tunnel.isWsOpen()                 -- check WS state
 *   tunnel.destroy()                  -- cleanup all intervals
 *   tunnel.runBandwidthTest(mb)       -- bandwidth measurement utility
 */
(function() {
  'use strict';

  var CHUNK = 994;
  var MAX_DC_QUEUE = 2000;
  var RECV_TTL_MS = 10000;
  var RECV_CLEANUP_MS = 5000;
  var BACKOFF_INIT = 2000;
  var BACKOFF_MAX = 30000;

  window.__tunnelCore = function(opts) {
    var getDC     = opts.getDC;
    var wsUrl     = opts.wsUrl || 'ws://127.0.0.1:9000/ws';
    var log       = opts.log || function() {};
    var onWsOpen  = opts.onWsOpen || function() {};
    var onWsClose = opts.onWsClose || function() {};

    var activeWS = null;
    var wsOpen = false;
    var wsBackoff = BACKOFF_INIT;
    var dcQueue = [];
    var dcDraining = false;
    var sendMsgId = 0;
    var recvBufs = {};
    var destroyed = false;

    // --- recvBufs TTL cleanup ---
    var cleanupInterval = setInterval(function() {
      var now = Date.now();
      for (var k in recvBufs) {
        if (now - recvBufs[k].ts > RECV_TTL_MS) {
          delete recvBufs[k];
        }
      }
    }, RECV_CLEANUP_MS);

    // --- chunked send ---
    function sendRaw(data) {
      var dc = getDC();
      if (!dc || dc.readyState !== 'open') return;

      if (typeof data === 'string') {
        dcQueue.push(data);
        drainDC();
        return;
      }

      var u8 = new Uint8Array(data instanceof ArrayBuffer ? data : data.buffer || data);
      var total = Math.ceil(u8.length / CHUNK) || 1;
      var id = (sendMsgId++) & 0xFFFF;

      for (var i = 0; i < total; i++) {
        var p = u8.subarray(i * CHUNK, Math.min((i + 1) * CHUNK, u8.length));
        var f = new Uint8Array(6 + p.length);
        f[0] = id >> 8; f[1] = id & 0xFF;
        f[2] = i >> 8;  f[3] = i & 0xFF;
        f[4] = total >> 8; f[5] = total & 0xFF;
        f.set(p, 6);
        dcQueue.push(f.buffer);
      }

      drainDC();
    }

    // --- chunked receive ---
    function handleChunk(data) {
      var u8 = new Uint8Array(data);
      if (u8.length < 6) return;

      var id    = (u8[0] << 8) | u8[1];
      var idx   = (u8[2] << 8) | u8[3];
      var total = (u8[4] << 8) | u8[5];
      var payload = u8.subarray(6);

      if (total === 1) {
        if (activeWS && wsOpen) {
          activeWS.send(payload.buffer.slice(payload.byteOffset, payload.byteOffset + payload.byteLength));
        }
        return;
      }

      var r = recvBufs[id];
      if (!r) {
        r = { c: [], n: 0, s: 0, ts: Date.now() };
        recvBufs[id] = r;
      }
      if (!r.c[idx]) {
        r.c[idx] = payload;
        r.n++;
        r.s += payload.length;
      }
      if (r.n === total) {
        var out = new Uint8Array(r.s);
        for (var i = 0, o = 0; i < total; i++) {
          out.set(r.c[i], o);
          o += r.c[i].length;
        }
        delete recvBufs[id];
        if (activeWS && wsOpen) {
          activeWS.send(out.buffer);
        }
      }
    }

    // --- DC queue drain with backpressure ---
    function drainDC() {
      var dc = getDC();
      if (dcDraining || !dc || dc.readyState !== 'open') return;
      dcDraining = true;

      while (dcQueue.length > 0) {
        if (dc.bufferedAmount > 64 * 1024) {
          dc.bufferedAmountLowThreshold = 16 * 1024;
          dc.onbufferedamountlow = function() {
            dc.onbufferedamountlow = null;
            dcDraining = false;
            drainDC();
          };
          return;
        }
        dc.send(dcQueue.shift());
      }
      dcDraining = false;
    }

    // --- WebSocket to relay ---
    function connectWS() {
      if (destroyed) return;
      if (activeWS && activeWS.readyState === WebSocket.OPEN) {
        activeWS.close();
      }

      var ws = new WebSocket(wsUrl);
      ws.binaryType = 'arraybuffer';
      activeWS = ws;

      ws.onopen = function() {
        if (ws !== activeWS) return;
        log('WebSocket connected to Go relay');
        wsOpen = true;
        wsBackoff = BACKOFF_INIT;
        onWsOpen(ws);
      };

      ws.onclose = function() {
        if (ws !== activeWS) return;
        wsOpen = false;
        onWsClose();
        var dc = getDC();
        if (dc && dc.readyState === 'open' && !destroyed) {
          log('WebSocket disconnected, reconnecting in ' + (wsBackoff / 1000) + 's...');
          var delay = wsBackoff;
          wsBackoff = Math.min(wsBackoff * 2, BACKOFF_MAX);
          setTimeout(function() {
            var dc2 = getDC();
            if (dc2 && dc2.readyState === 'open' && ws === activeWS) {
              connectWS();
            }
          }, delay);
        }
      };

      ws.onerror = function() {
        if (ws !== activeWS) return;
        log('WebSocket error');
      };

      ws.onmessage = function(e) {
        if (dcQueue.length > MAX_DC_QUEUE) return;
        sendRaw(e.data);
      };
    }

    function closeWS() {
      if (activeWS) {
        var ws = activeWS;
        activeWS = null;
        wsOpen = false;
        ws.close();
      }
    }

    function isWsOpen() {
      return wsOpen;
    }

    // --- bandwidth test ---
    function runBandwidthTest(totalMB) {
      totalMB = totalMB || 1;
      var dc = getDC();
      if (!dc || dc.readyState !== 'open') { log('DC not open'); return; }
      var chunkSize = 4096;
      var totalBytes = totalMB * 1024 * 1024;
      var sent = 0;
      var start = performance.now();
      sendRaw('bw:start');
      log('Starting bandwidth test: ' + totalMB + ' MB...');
      var sendBatch = function() {
        while (sent < totalBytes) {
          if (dc.bufferedAmount > 512 * 1024) {
            setTimeout(sendBatch, 5);
            return;
          }
          sendRaw(new ArrayBuffer(chunkSize));
          sent += chunkSize;
        }
        sendRaw('bw:done');
        var elapsed = (performance.now() - start) / 1000;
        var kbps = (totalBytes * 8 / 1024 / elapsed).toFixed(1);
        log('=== SEND COMPLETE: ' + (totalBytes / 1024).toFixed(1) + ' KB in ' + elapsed.toFixed(2) + 's = ' + kbps + ' kbps ===');
      };
      sendBatch();
    }

    function resetQueue() {
      dcQueue = [];
      dcDraining = false;
      recvBufs = {};
    }

    function destroy() {
      destroyed = true;
      clearInterval(cleanupInterval);
      closeWS();
      resetQueue();
    }

    return {
      sendRaw: sendRaw,
      handleChunk: handleChunk,
      drainDC: drainDC,
      connectWS: connectWS,
      closeWS: closeWS,
      isWsOpen: isWsOpen,
      resetQueue: resetQueue,
      destroy: destroy,
      runBandwidthTest: runBandwidthTest
    };
  };
})();
