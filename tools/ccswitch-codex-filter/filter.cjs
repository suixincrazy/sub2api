'use strict';

const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');
const http = require('node:http');
const net = require('node:net');
const {spawn} = require('node:child_process');
const {randomBytes} = require('node:crypto');
const {SERVICE, ADMIN, makeProxy, upstreamOrigin, isLoopbackHost} = require('./proxy.cjs');
const {ConfigRouter, resolveUpstream} = require('./config.cjs');

const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
function jsonFile(filename) {
  try { return JSON.parse(fs.readFileSync(filename, 'utf8')); }
  catch (error) { if (error.code === 'ENOENT') return null; throw error; }
}
function alive(pid) {
  if (!Number.isInteger(pid) || pid < 1) return false;
  try { process.kill(pid, 0); return true; }
  catch (error) { return error.code !== 'ESRCH'; }
}
function request(origin, route, token, method = 'GET') {
  return new Promise((resolve, reject) => {
    const req = http.request(origin + route, {method, headers: token ? {'x-filter-token': token} : {}}, res => {
      let body = '';
      res.setEncoding('utf8');
      res.on('data', chunk => { body += chunk; if (body.length > 65536) res.destroy(); });
      res.on('error', reject);
      res.on('end', () => {
        try {
          const result = JSON.parse(body);
          if (res.statusCode !== 200) throw new Error(result.error?.message || 'Local service returned HTTP ' + res.statusCode);
          resolve(result);
        } catch (error) { reject(error); }
      });
    });
    req.setTimeout(2000, () => req.destroy(new Error('Local service timed out.')));
    req.on('error', reject);
    req.end();
  });
}
async function health(record) {
  if (!record) return null;
  try {
    const result = await request(record.filterOrigin, ADMIN + '/health');
    return result.service === SERVICE && result.instance === record.instance ? result : null;
  } catch { return null; }
}
function reachable(origin) {
  const url = new URL(origin);
  return new Promise(resolve => {
    const socket = net.connect({host: url.hostname.replace(/^\[|\]$/g, ''),
      port: Number(url.port || (url.protocol === 'https:' ? 443 : 80))});
    const done = ok => { socket.destroy(); resolve(ok); };
    socket.setTimeout(1200, () => done(false));
    socket.once('connect', () => done(true));
    socket.once('error', () => done(false));
  });
}
function parseArgs(argv) {
  const command = (argv.shift() || 'help').toLowerCase();
  if (!['help', 'start', 'serve', 'stop', 'status'].includes(command)) throw new Error('Use start, stop, status or serve.');
  const values = {};
  const names = new Set(['port', 'upstream', 'config', 'runtime', 'cc-db']);
  const flags = new Set(['cc-switch']);
  while (argv.length) {
    const key = argv.shift();
    const name = key.startsWith('--') ? key.slice(2) : '';
    if (flags.has(name)) { values[name] = true; continue; }
    if (!names.has(name) || !argv.length) throw new Error('Invalid option: ' + key);
    values[name] = argv.shift();
  }
  if (values.upstream && (values['cc-switch'] || values['cc-db'])) {
    throw new Error('Use either --upstream or the CC Switch options, not both.');
  }
  const port = Number(values.port || 18181);
  if (!Number.isInteger(port) || port < 1024 || port > 65535) throw new Error('Port must be an integer between 1024 and 65535.');
  const configHome = process.env.CODEX_HOME || path.join(os.homedir(), '.codex');
  return {command, port, filterOrigin: 'http://127.0.0.1:' + port,
    configPath: path.resolve(values.config || path.join(configHome, 'config.toml')),
    runtime: path.resolve(values.runtime || path.join(__dirname, '.runtime')),
    databasePath: values['cc-db'] && path.resolve(values['cc-db']),
    ccSwitch: !!values['cc-switch'],
    upstream: values.upstream && upstreamOrigin(values.upstream)};
}
function files(options) {
  return {state: path.join(options.runtime, 'service.json'), backup: path.join(options.runtime, 'routes.json'),
    log: path.join(options.runtime, 'filter.log')};
}
function routerFor(options, backup, getUpstream, log) {
  return new ConfigRouter({configPath: options.configPath, filterOrigin: options.filterOrigin,
    backupPath: backup, getUpstream, log});
}
function removeOwnState(filename, instance) {
  if (jsonFile(filename)?.instance === instance) fs.unlinkSync(filename);
}
// Resolves what the filter forwards to. Returns null in adopt mode, where the address is
// only known once config.toml is read, so the router supplies it from that point on.
function upstreamFor(options) {
  const {origin} = resolveUpstream(options);
  if (origin) {
    const url = new URL(origin);
    const port = Number(url.port || (url.protocol === 'https:' ? 443 : 80));
    // Only a loopback upstream could collide with the filter's own listener.
    if (isLoopbackHost(url.hostname) && port === options.port) {
      throw new Error('Filter port must differ from the upstream port.');
    }
  }
  return origin;
}
async function serve(options) {
  const locations = files(options);
  fs.mkdirSync(options.runtime, {recursive: true});
  let upstream = upstreamFor(options);
  const instance = randomBytes(12).toString('hex');
  const record = {service: SERVICE, pid: process.pid, instance, token: randomBytes(32).toString('hex'),
    filterOrigin: options.filterOrigin, configPath: options.configPath, startedAt: new Date().toISOString()};
  const previous = jsonFile(locations.state);
  if (previous) {
    if (alive(previous.pid) || await health(previous)) throw new Error('A filter process is already running. Use status or stop.');
    fs.unlinkSync(locations.state);
  }
  // Exclusive creation serializes simultaneous starts; don't overwrite another instance.
  fs.writeFileSync(locations.state, JSON.stringify(record, null, 2), {flag: 'wx', mode: 0o600});
  let router, server, timer, stopping = false, lastError = null;
  const log = (event, data = {}) => console.log(JSON.stringify({time: new Date().toISOString(), event, ...data}));
  const cleanup = () => removeOwnState(locations.state, instance);
  const shutdown = () => {
    if (stopping) return;
    // If restoration fails, keep serving rather than leaving Codex pointing at a dead port.
    router.restore();
    stopping = true;
    clearInterval(timer);
    log('stopping');
    setTimeout(() => {
      const force = setTimeout(() => server.closeAllConnections(), 10000);
      force.unref();
      server.close(() => { clearTimeout(force); cleanup(); log('stopped'); });
      server.closeIdleConnections();
    }, 50);
  };
  try {
    router = routerFor(options, locations.backup, () => upstream, log);
    // The router is the source of truth for the forwarding address: in adopt mode it is the
    // only component that knows what config.toml pointed at before takeover.
    server = makeProxy({getUpstream: () => router.upstream(), token: record.token, onStop: shutdown, log,
      getState: () => ({instance, state: stopping ? 'stopping' : router.state, upstream: router.upstream(),
        mode: options.mode, filterOrigin: options.filterOrigin, configPath: options.configPath, lastError})});
    await new Promise((resolve, reject) => {
      server.once('error', reject);
      server.listen(options.port, '127.0.0.1', resolve);
    });
    const tick = () => {
      if (stopping) return;
      try {
        upstream = upstreamFor(options);
        router.sync();
        lastError = null;
      } catch (error) {
        const message = error.message;
        if (lastError !== message) log('config-error', {message});
        lastError = message;
        router.changeState('config-error');
      }
    };
    tick();
    timer = setInterval(tick, 1000);
    for (const signal of ['SIGINT', 'SIGTERM']) process.on(signal, () => {
      try { shutdown(); }
      catch (error) { log('restore-failed', {message: error.message}); }
    });
    log('listening', {filterOrigin: options.filterOrigin, upstream: router.upstream(),
      mode: options.mode, state: router.state});
  } catch (error) {
    clearInterval(timer);
    if (server?.listening) server.close();
    cleanup();
    throw error;
  }
}
async function status(options) {
  const record = jsonFile(files(options).state);
  const running = await health(record);
  if (running) {
    const {instance, ...publicState} = running;
    return {...publicState, running: true,
      upstreamReachable: running.upstream ? await reachable(running.upstream) : null};
  }
  return {service: SERVICE, running: false, state: record ? 'stale-state' : 'stopped',
    recoveryBackup: fs.existsSync(files(options).backup)};
}
async function start(options) {
  const locations = files(options);
  const running = await health(jsonFile(locations.state));
  if (running) {
    if (running.filterOrigin !== options.filterOrigin || running.configPath !== options.configPath) {
      throw new Error('Running filter has different options. Stop it before changing config/port.');
    }
    return status(options);
  }
  fs.mkdirSync(options.runtime, {recursive: true});
  const args = [__filename, 'serve', '--port', String(options.port), '--config', options.configPath, '--runtime', options.runtime];
  if (options.upstream) args.push('--upstream', options.upstream);
  if (options.ccSwitch) args.push('--cc-switch');
  if (options.databasePath) args.push('--cc-db', options.databasePath);
  if (fs.existsSync(locations.log) && fs.statSync(locations.log).size > 4 * 1024 * 1024) {
    fs.copyFileSync(locations.log, locations.log + '.previous');
    fs.truncateSync(locations.log, 0);
  }
  const fd = fs.openSync(locations.log, 'a');
  let child;
  try {
    child = spawn(process.execPath, args, {cwd: __dirname, detached: true, windowsHide: true, stdio: ['ignore', fd, fd]});
  } finally { fs.closeSync(fd); }
  let failure;
  child.on('error', error => { failure = error; });
  child.unref();
  for (let count = 0; count < 40; count++) {
    if (failure) throw failure;
    const result = await health(jsonFile(locations.state));
    if (result) {
      if (result.state === 'config-error') throw new Error('Filter started but config was not attached: ' + result.lastError + '. Use stop to restore/stop.');
      return status(options);
    }
    if (child.exitCode !== null) break;
    await delay(150);
  }
  throw new Error('Filter did not become ready. See ' + locations.log);
}
async function stop(options) {
  const locations = files(options);
  const record = jsonFile(locations.state);
  const running = await health(record);
  if (running) {
    await request(record.filterOrigin, ADMIN + '/stop', record.token, 'POST');
    for (let count = 0; count < 100; count++) {
      if (jsonFile(locations.state)?.instance !== record.instance) return {running: false, state: 'stopped', restored: true};
      await delay(150);
    }
    throw new Error('Stop is still draining requests; retry status.');
  }
  if (record && alive(record.pid)) {
    throw new Error('Recorded process is alive but not responding. Refusing to kill an unverified PID; retry stop after it exits.');
  }
  const backup = jsonFile(locations.backup);
  if (backup) {
    const restoreOptions = {...options, configPath: backup.configPath, filterOrigin: backup.filterOrigin};
    // Restoring only rewrites addresses from the backup; it never forwards, so no upstream
    // has to be resolved here. Stop must work even when the upstream is gone.
    routerFor(restoreOptions, locations.backup, () => null).restore();
  }
  if (record) removeOwnState(locations.state, record.instance);
  return {running: false, state: 'stopped', restored: !!backup};
}

async function main() {
  if (Number(process.versions.node.split('.')[0]) < 24) throw new Error('Node.js 24 or later is required (built-in SQLite and zstd).');
  const options = parseArgs(process.argv.slice(2));
  if (options.command === 'help') {
    console.log([
      'node filter.cjs start|stop|status|serve [--port 18181] [--config PATH] [--runtime PATH]',
      '',
      'Upstream selection (pick one; the default needs no local helper):',
      '  (default)          adopt whatever config.toml already points at',
      '  --upstream URL     forward to any http(s) gateway, local or remote',
      '  --cc-switch        read the Codex proxy port from CC Switch (optional integration)',
      '  --cc-db PATH       same as --cc-switch with an explicit database path',
    ].join('\n'));
  } else if (options.command === 'serve') await serve(options);
  else console.log(JSON.stringify(await ({start, stop, status}[options.command])(options), null, 2));
}
if (require.main === module) main().catch(error => { console.error('Filter: ' + error.message); process.exitCode = 1; });
module.exports = {parseArgs, start, stop, status, serve, request};
