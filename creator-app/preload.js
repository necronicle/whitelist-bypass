const { contextBridge, ipcRenderer } = require('electron');

contextBridge.exposeInMainWorld('bridge', {
  onRelayLog: (cb) => ipcRenderer.on('relay-log', (e, msg) => cb(msg)),
  getHookCode: (url) => ipcRenderer.invoke('get-hook-code', url),
  setTransportMode: (mode) => ipcRenderer.send('set-transport-mode', mode)
});
