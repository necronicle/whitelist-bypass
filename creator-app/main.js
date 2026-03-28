const { app, BrowserWindow, session, ipcMain } = require('electron');
const { spawn } = require('child_process');
const path = require('path');
const fs = require('fs');

let autoUpdater;
try {
  autoUpdater = require('electron-updater').autoUpdater;
} catch (_) {
  // electron-updater not available in dev mode
}

const hooksDir = app.isPackaged
  ? path.join(process.resourcesPath, 'hooks')
  : path.join(__dirname, '..', 'hooks');
const tunnelCore = fs.readFileSync(path.join(hooksDir, 'tunnel-core.js'), 'utf8');
const hookVk = fs.readFileSync(path.join(hooksDir, 'creator-vk.js'), 'utf8');
const hookTelemost = fs.readFileSync(path.join(hooksDir, 'creator-telemost.js'), 'utf8');
const logCapture = "window.__hookLogs=window.__hookLogs||[];var _ol=console.log;console.log=function(){_ol.apply(console,arguments);var m=Array.prototype.slice.call(arguments).join(' ');if(m.indexOf('[HOOK]')!==-1)window.__hookLogs.push(m)};";

let mainWindow;
let relayProcess;

function hookTargetFromUrl(rawUrl) {
  try {
    const host = new URL(rawUrl).hostname.toLowerCase();
    if (host === 'telemost.yandex.ru' || host.endsWith('.telemost.yandex.ru')) {
      return 'telemost';
    }
    if (host === 'vk.com' || host.endsWith('.vk.com')) {
      return 'vk';
    }
  } catch (_) {}
  return null;
}

function buildHookCode(url) {
  const target = hookTargetFromUrl(url);
  if (!target) {
    return null;
  }

  const hook = target === 'telemost' ? hookTelemost : hookVk;
  return [
    logCapture,
    '(function(){',
    '  if (window.__wbHookInstalled) return;',
    '  window.__wbHookInstalled = true;',
    tunnelCore,
    hook,
    '})();'
  ].join('\n');
}

function emitRelayLog(msg) {
  console.log('[relay]', msg);
  if (mainWindow && !mainWindow.isDestroyed()) {
    mainWindow.webContents.send('relay-log', msg);
  }
}

function resolveRelayBinary() {
  const baseDir = app.isPackaged
    ? process.resourcesPath
    : path.join(__dirname, '..', 'relay');

  const candidatesByPlatform = {
    darwin: ['relay', 'relay-darwin'],
    linux: ['relay', 'relay-linux-x64', 'relay-linux-ia32'],
    win32: ['relay.exe', 'relay-windows-x64.exe', 'relay-windows-ia32.exe']
  };

  const candidates = candidatesByPlatform[process.platform] || ['relay'];
  for (const name of candidates) {
    const candidate = path.join(baseDir, name);
    if (fs.existsSync(candidate)) {
      return candidate;
    }
  }

  return null;
}

function startRelay() {
  if (relayProcess) {
    return;
  }

  const net = require('net');
  const sock = new net.Socket();
  let settled = false;

  const finish = (fn) => () => {
    if (settled) {
      return;
    }
    settled = true;
    sock.destroy();
    fn();
  };

  sock.setTimeout(1000);
  sock.once('connect', finish(() => {
    emitRelayLog('Using existing relay on :9000');
  }));
  sock.once('error', finish(spawnRelay));
  sock.once('timeout', finish(spawnRelay));
  sock.connect(9000, '127.0.0.1');
}

function spawnRelay() {
  const relayPath = resolveRelayBinary();
  if (!relayPath) {
    emitRelayLog('Relay binary not found. Build the relay before starting the creator app.');
    return;
  }

  relayProcess = spawn(relayPath, ['--mode', 'creator'], {
    stdio: ['ignore', 'pipe', 'pipe']
  });

  emitRelayLog(`Starting relay: ${path.basename(relayPath)}`);

  relayProcess.on('error', (err) => {
    emitRelayLog(`Relay failed to start: ${err.message}`);
    relayProcess = null;
  });

  relayProcess.stdout.on('data', (data) => {
    data.toString().trim().split('\n').forEach((msg) => {
      if (!msg) return;
      emitRelayLog(msg);
    });
  });
  relayProcess.stderr.on('data', (data) => {
    data.toString().trim().split('\n').forEach((msg) => {
      if (!msg) return;
      emitRelayLog(msg);
    });
  });
  relayProcess.on('close', (code, signal) => {
    emitRelayLog(signal ? `Relay exited via signal ${signal}` : `Relay exited with code ${code}`);
    relayProcess = null;
  });
}

function stripCSP(ses) {
  if (ses.__wbCspPatched) {
    return;
  }
  ses.__wbCspPatched = true;

  ses.webRequest.onHeadersReceived((details, callback) => {
    if (!hookTargetFromUrl(details.url)) {
      callback({ responseHeaders: details.responseHeaders });
      return;
    }

    const headers = { ...details.responseHeaders };
    delete headers['content-security-policy'];
    delete headers['Content-Security-Policy'];
    delete headers['content-security-policy-report-only'];
    delete headers['Content-Security-Policy-Report-Only'];
    callback({ responseHeaders: headers });
  });
}

function createWindow() {
  const ses = session.fromPartition('persist:creator');
  stripCSP(ses);
  ses.setPermissionRequestHandler((webContents, permission, callback, details) => {
    const sourceUrl = (details && details.requestingUrl) || webContents.getURL();
    callback(Boolean(hookTargetFromUrl(sourceUrl)));
  });
  ses.setPermissionCheckHandler((webContents, permission, requestingOrigin) => {
    return Boolean(hookTargetFromUrl(requestingOrigin || webContents.getURL()));
  });

  mainWindow = new BrowserWindow({
    width: 1200,
    height: 800,
    icon: path.join(__dirname, 'resources', 'icon.png'),
    webPreferences: {
      preload: path.join(__dirname, 'preload.js'),
      nodeIntegration: false,
      contextIsolation: true,
      webviewTag: true
    }
  });

  ses.setUserAgent('Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36');

  app.on('session-created', stripCSP);

  mainWindow.loadFile(path.join(__dirname, 'index.html'));
  mainWindow.on('closed', () => { mainWindow = null; });
}

function killRelay() {
  if (relayProcess) {
    relayProcess.kill();
    relayProcess = null;
  }
}


ipcMain.handle('get-hook-code', (e, url) => {
  return buildHookCode(url);
});

app.whenReady().then(() => {
  startRelay();
  createWindow();

  if (autoUpdater) {
    autoUpdater.logger = { info: emitRelayLog, warn: emitRelayLog, error: emitRelayLog };
    autoUpdater.autoDownload = true;
    autoUpdater.autoInstallOnAppQuit = true;
    autoUpdater.on('update-available', (info) => {
      emitRelayLog(`Update available: v${info.version}`);
    });
    autoUpdater.on('update-downloaded', (info) => {
      emitRelayLog(`Update v${info.version} downloaded, will install on quit`);
    });
    autoUpdater.on('error', (err) => {
      emitRelayLog(`Auto-update error: ${err.message}`);
    });
    autoUpdater.checkForUpdatesAndNotify().catch(() => {});
  }
});

app.on('window-all-closed', () => {
  killRelay();
  app.quit();
});

app.on('before-quit', killRelay);
process.on('exit', killRelay);
process.on('SIGINT', () => { killRelay(); process.exit(); });
process.on('SIGTERM', () => { killRelay(); process.exit(); });
