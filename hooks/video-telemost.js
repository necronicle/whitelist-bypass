// VP8 Video mode hook for Yandex Telemost (creator & joiner).
// Like video-vk.js but also includes fake media devices and
// Telemost-specific signaling WebSocket interception.
(() => {
  'use strict';

  const SIG_URL = 'ws://127.0.0.1:9001/signaling';
  const log = (...args) => console.log('[HOOK]', ...args);

  let sigWS = null;
  let mockPC = null;

  // --- Fake media for Telemost ---
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

  // --- Telemost signaling WS intercept ---
  // We intercept the Telemost signaling WS to capture ICE servers and forward
  // SDP offers/answers through our signaling proxy.
  var telemostSigWS = null;
  var lastPcSeq = 0;
  window.__OrigWebSocket = window.__OrigWebSocket || window.WebSocket;
  var OrigWebSocket = window.__OrigWebSocket;

  window.WebSocket = function(url, protocols) {
    var ws = protocols ? new OrigWebSocket(url, protocols) : new OrigWebSocket(url);
    if (url && (url.indexOf('strm.yandex') !== -1 || url.indexOf('jvb.telemost') !== -1)) {
      log('Telemost signaling WS found: ' + url);
      telemostSigWS = ws;
      var origSend = ws.send.bind(ws);
      ws.send = function(data) {
        try {
          var msg = JSON.parse(data);
          if (msg.type === 'publisherSdpOffer' && msg.payload && msg.payload.pcSeq) {
            lastPcSeq = msg.payload.pcSeq;
            // Forward ICE servers to Go if available.
          }
        } catch(e) {}
        return origSend(data);
      };
      ws.addEventListener('message', function(e) {
        try {
          var msg = JSON.parse(e.data);
          if (msg.type === 'publisherSdpAnswer' && msg.payload && msg.payload.sdp) {
            // Forward SDP answer to Go signaling.
            sendSignaling({ type: 'answer', sdp: msg.payload.sdp });
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

  // --- Go signaling connection ---
  function connectSignaling() {
    if (sigWS && sigWS.readyState <= 1) return;
    log('Connecting to signaling server...');
    sigWS = new OrigWebSocket(SIG_URL);
    sigWS.onopen = () => log('Signaling WS connected');
    sigWS.onclose = () => {
      log('Signaling WS closed, reconnecting in 2s...');
      setTimeout(connectSignaling, 2000);
    };
    sigWS.onerror = () => log('Signaling WS error');
    sigWS.onmessage = (e) => {
      try {
        var msg = JSON.parse(e.data);
        if (mockPC) mockPC._handleSignaling(msg);
      } catch (err) {
        log('Signaling parse error: ' + err.message);
      }
    };
  }

  function sendSignaling(msg) {
    if (sigWS && sigWS.readyState === 1) {
      sigWS.send(JSON.stringify(msg));
    }
  }

  // --- MockPeerConnection ---
  function MockPeerConnection(config) {
    this._config = config;
    this._localDesc = null;
    this._remoteDesc = null;
    this._state = 'new';
    this._iceState = 'new';
    this._signalingState = 'stable';
    this._onicecandidate = null;
    this._ontrack = null;
    this._onconnectionstatechange = null;
    this._onsignalingstatechange = null;
    this._onicegatheringstatechange = null;
    this._oniceconnectionstatechange = null;
    this._ondatachannel = null;
    this._listeners = {};
    this._senders = [];
    this._receivers = [];

    mockPC = this;

    if (config && config.iceServers) {
      sendSignaling({ type: 'config', servers: config.iceServers });
    }

    log('MockPeerConnection created (VP8 Telemost mode)');
  }

  MockPeerConnection.prototype.createOffer = function() {
    return Promise.resolve({ type: 'offer', sdp: '' });
  };

  MockPeerConnection.prototype.createAnswer = function() {
    return Promise.resolve({ type: 'answer', sdp: '' });
  };

  MockPeerConnection.prototype.setLocalDescription = function(desc) {
    this._localDesc = desc;
    if (desc && desc.sdp) {
      sendSignaling({ type: desc.type, sdp: desc.sdp });
    }
    return Promise.resolve();
  };

  MockPeerConnection.prototype.setRemoteDescription = function(desc) {
    this._remoteDesc = desc;
    this._signalingState = desc.type === 'offer' ? 'have-remote-offer' : 'stable';
    if (desc && desc.sdp) {
      sendSignaling({ type: desc.type, sdp: desc.sdp });
    }
    return Promise.resolve();
  };

  MockPeerConnection.prototype.addIceCandidate = function(candidate) {
    if (candidate && candidate.candidate) {
      sendSignaling({ type: 'candidate', candidate: candidate });
    }
    return Promise.resolve();
  };

  MockPeerConnection.prototype.addTrack = function(track, stream) {
    var sender = { track: track, replaceTrack: function() { return Promise.resolve(); } };
    this._senders.push(sender);
    return sender;
  };

  MockPeerConnection.prototype.addTransceiver = function(trackOrKind, init) {
    return {
      sender: { track: null, replaceTrack: function() { return Promise.resolve(); } },
      receiver: { track: null },
      direction: (init && init.direction) || 'sendrecv',
      mid: null,
    };
  };

  MockPeerConnection.prototype.getSenders = function() { return this._senders; };
  MockPeerConnection.prototype.getReceivers = function() { return this._receivers; };
  MockPeerConnection.prototype.getTransceivers = function() { return []; };
  MockPeerConnection.prototype.getStats = function() { return Promise.resolve(new Map()); };

  MockPeerConnection.prototype.createDataChannel = function(label, opts) {
    log('createDataChannel (mock): ' + label);
    return {
      label: label, readyState: 'open', binaryType: 'arraybuffer',
      bufferedAmount: 0, send: function() {}, close: function() {},
      onopen: null, onclose: null, onmessage: null, onerror: null,
      addEventListener: function() {}, removeEventListener: function() {},
    };
  };

  MockPeerConnection.prototype.close = function() {
    this._state = 'closed';
    this._signalingState = 'closed';
  };

  MockPeerConnection.prototype.addEventListener = function(type, cb) {
    if (!this._listeners[type]) this._listeners[type] = [];
    this._listeners[type].push(cb);
  };

  MockPeerConnection.prototype.removeEventListener = function(type, cb) {
    if (!this._listeners[type]) return;
    this._listeners[type] = this._listeners[type].filter(function(f) { return f !== cb; });
  };

  MockPeerConnection.prototype._emit = function(type, event) {
    var prop = 'on' + type;
    if (this[prop]) this[prop](event);
    if (this._listeners[type]) {
      this._listeners[type].forEach(function(cb) { cb(event); });
    }
  };

  MockPeerConnection.prototype._handleSignaling = function(msg) {
    switch (msg.type) {
      case 'answer':
        this._remoteDesc = { type: 'answer', sdp: msg.sdp };
        this._signalingState = 'stable';
        log('Received answer from Go');
        break;
      case 'offer':
        this._remoteDesc = { type: 'offer', sdp: msg.sdp };
        this._signalingState = 'have-remote-offer';
        log('Received offer from Go');
        break;
      case 'candidate':
        break;
      case 'connected':
        this._state = 'connected';
        this._iceState = 'connected';
        log('=== CALL CONNECTED (VP8 Telemost mode) ===');
        this._emit('connectionstatechange', {});
        this._emit('iceconnectionstatechange', {});
        break;
      case 'disconnected':
        this._state = 'disconnected';
        this._emit('connectionstatechange', {});
        break;
    }
  };

  Object.defineProperty(MockPeerConnection.prototype, 'connectionState', {
    get: function() { return this._state; }
  });
  Object.defineProperty(MockPeerConnection.prototype, 'iceConnectionState', {
    get: function() { return this._iceState; }
  });
  Object.defineProperty(MockPeerConnection.prototype, 'signalingState', {
    get: function() { return this._signalingState; }
  });
  Object.defineProperty(MockPeerConnection.prototype, 'localDescription', {
    get: function() { return this._localDesc; }
  });
  Object.defineProperty(MockPeerConnection.prototype, 'remoteDescription', {
    get: function() { return this._remoteDesc; }
  });
  Object.defineProperty(MockPeerConnection.prototype, 'iceGatheringState', {
    get: function() { return 'complete'; }
  });

  var OrigPC = window.RTCPeerConnection;
  window.RTCPeerConnection = MockPeerConnection;
  Object.keys(OrigPC).forEach(function(key) {
    window.RTCPeerConnection[key] = OrigPC[key];
  });
  window.RTCPeerConnection.prototype = MockPeerConnection.prototype;

  connectSignaling();

  window.__hook = { log: log };
  log('VP8 Video hook installed (Telemost)');
})();
