(() => {
  'use strict';

  const log = (...args) => console.log('[HOOK]', ...args);
  const peers = [];
  let activeDC = null;
  let dcOpen = false;

  const tunnel = window.__tunnelCore({
    getDC:     () => activeDC,
    log:       log,
    onWsOpen:  () => {
      if (typeof AndroidBridge !== 'undefined' && AndroidBridge.onTunnelReady) {
        AndroidBridge.onTunnelReady();
      }
    },
  });

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
      tunnel.connectWS();
    };

    dc.onclose = () => {
      if (dc !== activeDC) return;
      log('DataChannel closed');
      dcOpen = false;
      tunnel.resetQueue();
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
          log('=== RECV COMPLETE: ' + (bwRecvBytes / 1024).toFixed(1) + ' KB in ' + elapsed.toFixed(2) + 's = ' + kbps + ' kbps ===');
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
        tunnel.handleChunk(e.data);
      }
    };
  }

  window.__hook = { peers: peers, log: log };
  window.__hook.runBandwidthTest = tunnel.runBandwidthTest;

  log('Hook installed');
})();
