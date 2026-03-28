(() => {
  'use strict';

  const log = (...args) => console.log('[HOOK]', ...args);
  const peers = [];
  let outboundDC = null;
  let dcCreating = false;
  let tunnelReady = false;

  // --- Fake getUserMedia for Telemost (WebView has no real camera) ---
  var origGUM = navigator.mediaDevices.getUserMedia.bind(navigator.mediaDevices);
  navigator.mediaDevices.getUserMedia = function(c) {
    log('Intercepting getUserMedia');
    var canvas = document.createElement('canvas');
    canvas.width = 2; canvas.height = 2;
    var stream = canvas.captureStream(1);
    if (c && c.audio) {
      var actx = new AudioContext();
      var dest = actx.createMediaStreamDestination();
      var t = dest.stream.getAudioTracks()[0];
      t.enabled = false;
      stream.addTrack(t);
    }
    return Promise.resolve(stream);
  };
  navigator.mediaDevices.enumerateDevices = function() {
    return Promise.resolve([
      {deviceId:'fake-cam',kind:'videoinput',label:'Camera',groupId:'g1',toJSON:function(){return this}},
      {deviceId:'fake-mic',kind:'audioinput',label:'Microphone',groupId:'g2',toJSON:function(){return this}},
      {deviceId:'fake-spk',kind:'audiooutput',label:'Speaker',groupId:'g3',toJSON:function(){return this}}
    ]);
  };

  // --- Signaling WS intercept ---
  var signalingWS = null;
  var lastPcSeq = 0;
  var OrigWebSocket = window.WebSocket;
  window.WebSocket = function(url, protocols) {
    var ws = protocols ? new OrigWebSocket(url, protocols) : new OrigWebSocket(url);
    if (url && (url.indexOf('strm.yandex') !== -1 || url.indexOf('jvb.telemost') !== -1)) {
      log('Signaling WS found: ' + url);
      signalingWS = ws;
      var origSend = ws.send.bind(ws);
      ws.send = function(data) {
        try {
          var msg = JSON.parse(data);
          if (msg.type === 'publisherSdpOffer' && msg.payload && msg.payload.pcSeq) {
            lastPcSeq = msg.payload.pcSeq;
          }
        } catch(e) {}
        return origSend(data);
      };
      ws.addEventListener('message', function(e) {
        try {
          var msg = JSON.parse(e.data);
          if (msg.type === 'publisherSdpAnswer' && msg.payload && msg.payload.sdp) {
            var pc0 = peers[0];
            if (pc0 && pc0.signalingState === 'have-local-offer') {
              pc0.setRemoteDescription({ type: 'answer', sdp: msg.payload.sdp }).catch(function(e) {
                log('setRemoteDescription error: ' + e.message);
              });
            }
          }
        } catch(e) {}
      });
    }
    return ws;
  };
  window.WebSocket.prototype = OrigWebSocket.prototype;
  window.WebSocket.CONNECTING = OrigWebSocket.CONNECTING;
  window.WebSocket.OPEN = OrigWebSocket.OPEN;
  window.WebSocket.CLOSING = OrigWebSocket.CLOSING;
  window.WebSocket.CLOSED = OrigWebSocket.CLOSED;

  // --- Tunnel core ---
  const tunnel = window.__tunnelCore({
    getDC:     () => outboundDC,
    log:       log,
    onWsOpen:  () => {
      if (typeof AndroidBridge !== 'undefined' && AndroidBridge.onTunnelReady) {
        AndroidBridge.onTunnelReady();
      }
    },
  });

  // --- PeerConnection intercept ---
  const OrigPC = window.RTCPeerConnection;
  window.RTCPeerConnection = function (config) {
    log('New PeerConnection created');
    const pc = new OrigPC(config);
    peers.push(pc);
    var pcIdx = peers.length - 1;

    pc.addEventListener('connectionstatechange', () => {
      log('Connection state:', pc.connectionState);
      if (pc.connectionState === 'connected') {
        log('=== CALL CONNECTED on PC' + pcIdx + ' ===');
        pc.getSenders().forEach(function(s) {
          if (s.track) s.track.stop();
          s.replaceTrack(null).catch(function(){});
        });
        if (!outboundDC && !dcCreating) {
          createTunnelDC(pc, origCreateDC);
        }
      }
    });

    var origCreateDC = pc.createDataChannel.bind(pc);
    pc.createDataChannel = function(label, opts) {
      return origCreateDC(label, opts);
    };

    pc.addEventListener('datachannel', function(e) {
      var ch = e.channel;
      ch.binaryType = 'arraybuffer';
      log('Incoming DC: label=' + ch.label + ' id=' + ch.id + ' on PC' + pcIdx);

      if (ch.label === 'sharing') {
        log('Inbound tunnel DC found on PC' + pcIdx);
        ch.addEventListener('message', function(m) {
          if (typeof m.data === 'string') {
            if (m.data === 'tunnel:ping') { tunnel.sendRaw('tunnel:pong'); return; }
            if (m.data === 'tunnel:pong') {
              if (!tunnelReady) {
                tunnelReady = true;
                log('DataChannel confirmed on PC' + pcIdx);
                tunnel.connectWS();
              }
              return;
            }
          }
          if (m.data instanceof ArrayBuffer) {
            tunnel.handleChunk(m.data);
          }
        });
      }
    });

    return pc;
  };

  function createTunnelDC(pc, origCreateDC) {
    dcCreating = true;
    setTimeout(function() {
      log('Creating tunnel DC');
      var dc = origCreateDC('sharing', { ordered: true });
      dc.binaryType = 'arraybuffer';
      outboundDC = dc;
      dc.addEventListener('open', function() {
        log('DataChannel open');
        startPinging();
      });
      dc.addEventListener('close', function() {
        log('DataChannel closed');
        outboundDC = null;
        dcCreating = false;
        tunnelReady = false;
        tunnel.resetQueue();
      });
      pc.createOffer().then(function(offer) {
        return pc.setLocalDescription(offer).then(function() {
          if (signalingWS && signalingWS.readyState === 1) {
            signalingWS.send(JSON.stringify({
              type: 'publisherSdpOffer',
              payload: { pcSeq: lastPcSeq, sdp: offer.sdp, tracks: [] }
            }));
            log('Sent renegotiation offer via signaling WS');
          }
        });
      }).catch(function(e) {
        log('Renegotiation error: ' + e.message);
      });
    }, 3000);
  }

  function startPinging() {
    var iv = setInterval(function() {
      if (tunnelReady) { clearInterval(iv); return; }
      tunnel.sendRaw('tunnel:ping');
      log('Sent tunnel:ping');
    }, 5000);
  }

  Object.keys(OrigPC).forEach((key) => {
    window.RTCPeerConnection[key] = OrigPC[key];
  });
  window.RTCPeerConnection.prototype = OrigPC.prototype;

  window.__hook = { peers: peers, log: log };
  window.__hook.runBandwidthTest = tunnel.runBandwidthTest;

  log('Hook installed');
})();
