(() => {
  'use strict';

  const WS_URL = 'ws://127.0.0.1:9000/ws';
  const CHUNK = 994;
  const log = (...args) => console.log('[HOOK]', ...args);
  const peers = [];
  let activeWS = null;
  let activeDC = null;
  let dcOpen = false;
  let wsOpen = false;
  let dcQueue = [];
  let dcDraining = false;
  let sendMsgId = 0;
  let recvBufs = {};

  const OrigPC = window.RTCPeerConnection;
  window.RTCPeerConnection = function (config) {
    log('New PeerConnection created');
    const pc = new OrigPC(config);
    peers.push(pc);

    pc.addEventListener('connectionstatechange', () => {
      log('Connection state:', pc.connectionState);
      if (pc.connectionState === 'connected') {
        log('=== CALL CONNECTED ===');
        setupDC(pc);
      }
    });

    return pc;
  };

  Object.keys(OrigPC).forEach((key) => {
    window.RTCPeerConnection[key] = OrigPC[key];
  });
  window.RTCPeerConnection.prototype = OrigPC.prototype;

  function setupDC(pc) {
    const dc = pc.createDataChannel('tunnel', { negotiated: true, id: 2 });
    dc.binaryType = 'arraybuffer';
    bindDC(dc);
  }

  function bindDC(dc) {
    if (activeDC && activeDC !== dc && activeDC.readyState === 'open') {
      activeDC.close();
    }
    activeDC = dc;
    dcOpen = false;

    dc.onopen = () => {
      if (dc !== activeDC) return;
      log('DataChannel open');
      dcOpen = true;
      connectWS();
    };

    dc.onclose = () => {
      if (dc !== activeDC) return;
      log('DataChannel closed');
      dcOpen = false;
      dcQueue = [];
      recvBufs = {};
    };

    let bwRecvBytes = 0;
    let bwRecvStart = 0;
    let bwMode = false;

    dc.onmessage = (e) => {
      if (dc !== activeDC) return;
      if (bwMode) {
        if (e.data instanceof ArrayBuffer) {
          if (bwRecvStart === 0) bwRecvStart = performance.now();
          bwRecvBytes += e.data.byteLength;
          return;
        }
        if (typeof e.data === 'string' && e.data === 'bw:done') {
          var elapsed = (performance.now() - bwRecvStart) / 1000;
          var kbps = (bwRecvBytes * 8 / 1024 / elapsed).toFixed(1);
          log('=== RECV COMPLETE: ' + (bwRecvBytes/1024).toFixed(1) + ' KB in ' + elapsed.toFixed(2) + 's = ' + kbps + ' kbps ===');
          bwRecvBytes = 0;
          bwRecvStart = 0;
          bwMode = false;
          return;
        }
      }
      if (typeof e.data === 'string' && e.data === 'bw:start') {
        bwMode = true;
        bwRecvBytes = 0;
        bwRecvStart = 0;
        return;
      }
      if (e.data instanceof ArrayBuffer) {
        handleChunk(e.data);
      }
    };
  }

  function sendRaw(data) {
    if (!activeDC || activeDC.readyState !== 'open') return;
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
      f[2] = i >> 8; f[3] = i & 0xFF;
      f[4] = total >> 8; f[5] = total & 0xFF;
      f.set(p, 6);
      dcQueue.push(f.buffer);
    }

    drainDC();
  }

  function handleChunk(data) {
    var u8 = new Uint8Array(data);
    if (u8.length < 6) return;

    var id = (u8[0] << 8) | u8[1];
    var idx = (u8[2] << 8) | u8[3];
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
      r = { c: [], n: 0, s: 0 };
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

  function drainDC() {
    if (dcDraining || !activeDC || activeDC.readyState !== 'open') return;
    dcDraining = true;
    while (dcQueue.length > 0) {
      if (activeDC.bufferedAmount > 64 * 1024) {
        activeDC.bufferedAmountLowThreshold = 16 * 1024;
        activeDC.onbufferedamountlow = function() {
          activeDC.onbufferedamountlow = null;
          dcDraining = false;
          drainDC();
        };
        return;
      }
      activeDC.send(dcQueue.shift());
    }
    dcDraining = false;
  }


  function connectWS() {
    if (activeWS && activeWS.readyState === WebSocket.OPEN) {
      activeWS.close();
    }
    var ws = new WebSocket(WS_URL);
    ws.binaryType = 'arraybuffer';
    activeWS = ws;

    ws.onopen = () => {
      if (ws !== activeWS) return;
      log('WebSocket connected to Go relay');
      wsOpen = true;
      if (typeof AndroidBridge !== 'undefined' && AndroidBridge.onTunnelReady) {
        AndroidBridge.onTunnelReady();
      }
    };

    ws.onclose = () => {
      if (ws !== activeWS) return;
      wsOpen = false;
      if (dcOpen) {
        log('WebSocket disconnected, reconnecting in 2s...');
        setTimeout(() => {
          if (dcOpen && ws === activeWS) connectWS();
        }, 2000);
      }
    };

    ws.onerror = () => {
      if (ws !== activeWS) return;
      log('WebSocket error');
    };

    ws.onmessage = (e) => {
      sendRaw(e.data);
    };
  }

  window.__hook = { peers: peers, log: log };
  window.__hook.runBandwidthTest = function(totalMB) {
    totalMB = totalMB || 1;
    if (!dcOpen || !activeDC) { log('DC not open'); return; }
    var chunkSize = 4096;
    var totalBytes = totalMB * 1024 * 1024;
    var sent = 0;
    var start = performance.now();
    sendRaw('bw:start');
    log('Starting bandwidth test: ' + totalMB + ' MB...');
    var sendBatch = function() {
      while (sent < totalBytes) {
        if (activeDC.bufferedAmount > 512 * 1024) {
          setTimeout(sendBatch, 5);
          return;
        }
        sendRaw(new ArrayBuffer(chunkSize));
        sent += chunkSize;
      }
      sendRaw('bw:done');
      var elapsed = (performance.now() - start) / 1000;
      var kbps = (totalBytes * 8 / 1024 / elapsed).toFixed(1);
      log('=== SEND COMPLETE: ' + (totalBytes/1024).toFixed(1) + ' KB in ' + elapsed.toFixed(2) + 's = ' + kbps + ' kbps ===');
    };
    sendBatch();
  };

  log('Hook installed');
})();
