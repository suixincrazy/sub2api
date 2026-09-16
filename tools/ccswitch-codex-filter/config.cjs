'use strict';

const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');
const {localOrigin, upstreamOrigin, isLoopbackHost} = require('./proxy.cjs');

function atomicWrite(filename, text) {
  fs.mkdirSync(path.dirname(filename), {recursive: true});
  const temp = filename + '.filter-' + process.pid + '-' + Math.random().toString(16).slice(2) + '.tmp';
  try {
    fs.writeFileSync(temp, text, {flag: 'wx', mode: 0o600});
    fs.renameSync(temp, filename);
  } finally { if (fs.existsSync(temp)) fs.unlinkSync(temp); }
}

function unquote(value) {
  return value.startsWith("'") ? value.slice(1, -1) : JSON.parse(value);
}

function sectionProvider(line) {
  const match = line.match(/^\s*\[\s*model_providers\s*\.\s*("(?:[^"\\]|\\.)*"|'[^']*'|[A-Za-z0-9_-]+)\s*\]\s*(?:#.*)?$/);
  return match ? (/^["']/.test(match[1]) ? unquote(match[1]) : match[1]) : null;
}

function providerBase(text, requestedProvider) {
  const lines = text.replace(/^\uFEFF/, '').split(/\r?\n/);
  let provider = requestedProvider;
  if (!provider) {
    for (const line of lines) {
      if (/^\s*\[/.test(line)) break;
      const match = line.match(/^\s*model_provider\s*=\s*("(?:[^"\\]|\\.)*"|'[^']*')\s*(?:#.*)?$/);
      if (match) provider = unquote(match[1]);
    }
  }
  if (!provider) return null;
  let inProvider = false;
  const found = [];
  for (const line of lines) {
    if (/^\s*\[/.test(line)) {
      inProvider = sectionProvider(line) === provider;
    } else if (inProvider) {
      const match = line.match(/^(\s*base_url\s*=\s*)("(?:[^"\\]|\\.)*"|'[^']*')(\s*(?:#.*)?)$/);
      if (match) found.push({provider, line, url: unquote(match[2]), prefix: match[1], suffix: match[3]});
    }
  }
  if (found.length > 1) throw new Error('Duplicate base_url entries; refusing to edit config.');
  return found[0] || null;
}

function replaceBase(text, entry, url, originalLine) {
  const lines = text.split(/(?<=\n)/);
  let inProvider = false;
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i].replace(/\r?\n$/, '').replace(/^\uFEFF/, '');
    if (/^\s*\[/.test(line)) {
      inProvider = sectionProvider(line) === entry.provider;
    } else if (inProvider && line === entry.line) {
      lines[i] = (originalLine ?? entry.prefix + JSON.stringify(url) + entry.suffix)
        + (lines[i].endsWith('\r\n') ? '\r\n' : lines[i].endsWith('\n') ? '\n' : '');
      return lines.join('');
    }
  }
  throw new Error('Provider config changed; refusing to overwrite it.');
}

function effectivePort(url) {
  return Number(url.port || (url.protocol === 'https:' ? 443 : 80));
}

// Compares a provider base_url against an origin. Loopback spellings are interchangeable
// (127.0.0.1 / localhost / [::1]); remote hosts are compared by name, so a non-local
// upstream can be recognized too.
function sameEndpoint(url, origin) {
  try {
    const left = new URL(url), right = new URL(origin);
    if (left.username || left.password) return false;
    if (left.protocol !== right.protocol) return false;
    if (effectivePort(left) !== effectivePort(right)) return false;
    if (isLoopbackHost(left.hostname) && isLoopbackHost(right.hostname)) return true;
    return left.hostname.toLowerCase() === right.hostname.toLowerCase();
  } catch { return false; }
}

function rewriteOrigin(url, origin) {
  const result = new URL(url), target = new URL(origin);
  result.protocol = target.protocol;
  result.host = target.host;
  return result.href;
}

function ccSwitchDatabaseDefault() {
  return path.join(os.homedir(), '.cc-switch', 'cc-switch.db');
}

// Optional integration. Only called when the CC Switch mode is requested explicitly;
// nothing else in this filter reads that database, and there is no hardcoded fallback port.
function discoverCcSwitch(databasePath = ccSwitchDatabaseDefault()) {
  if (!fs.existsSync(databasePath)) {
    throw new Error('CC Switch database not found at ' + databasePath
      + '. Use --upstream to point at any gateway instead, or omit both to adopt the address already in config.toml.');
  }
  const {DatabaseSync} = require('node:sqlite');
  const db = new DatabaseSync(databasePath, {readOnly: true});
  try {
    const row = db.prepare("SELECT listen_address, listen_port FROM proxy_config WHERE app_type = 'codex'").get();
    if (!row) throw new Error('CC Switch Codex proxy configuration not found.');
    const address = row.listen_address;
    const host = ['0.0.0.0', '127.0.0.1', 'localhost'].includes(address) ? '127.0.0.1'
      : ['::', '::1'].includes(address) ? '[::1]' : address;
    return localOrigin('http://' + host + ':' + Number(row.listen_port));
  } finally { db.close(); }
}

// Three ways to decide what the filter forwards to, in precedence order:
//   fixed     --upstream URL      any http(s) gateway, local or remote
//   cc-switch --cc-db / --cc-switch  read CC Switch's Codex proxy port (optional integration)
//   adopt     neither             take over whatever config.toml currently points at
// Returns null for adopt: the address is only known when the config is read, so the
// router resolves it per sync instead of pinning one value up front.
function resolveUpstream({upstream, ccSwitch, databasePath} = {}) {
  if (upstream) return {mode: 'fixed', origin: upstreamOrigin(upstream)};
  if (ccSwitch || databasePath) {
    return {mode: 'cc-switch', origin: discoverCcSwitch(databasePath || ccSwitchDatabaseDefault())};
  }
  return {mode: 'adopt', origin: null};
}

// In adopt mode the upstream is whatever config.toml pointed at before takeover, so it is
// recovered from the saved route rather than from a configured value.
function adoptedOrigin(routes) {
  let origin = null;
  for (const route of routes.values()) {
    try { origin = new URL(route.originalUrl).origin; } catch { /* keep the previous one */ }
  }
  return origin;
}

class ConfigRouter {
  constructor({configPath, backupPath, filterOrigin, getUpstream, log = () => {}}) {
    this.configPath = path.resolve(configPath);
    this.backupPath = backupPath;
    this.filterOrigin = localOrigin(filterOrigin);
    this.getUpstream = getUpstream;
    this.log = log;
    this.state = 'waiting-for-upstream';
    this.routes = new Map();
    this.adopted = null;
    if (fs.existsSync(backupPath)) {
      const backup = JSON.parse(fs.readFileSync(backupPath, 'utf8'));
      if (backup.configPath !== this.configPath || backup.filterOrigin !== this.filterOrigin || backup.version !== 1) {
        throw new Error('Existing address backup belongs to another config/port/version. Restore it before changing options.');
      }
      this.routes = new Map(backup.routes);
      this.adopted = adoptedOrigin(this.routes);
    }
  }
  // Where requests are forwarded: the configured upstream, or in adopt mode the address that
  // was in config.toml before takeover (the config now points at the filter, so it is read
  // back from the address backup).
  upstream() {
    return this.getUpstream() || this.adopted;
  }
  save() {
    atomicWrite(this.backupPath, JSON.stringify({version: 1, configPath: this.configPath,
      filterOrigin: this.filterOrigin, routes: [...this.routes]}, null, 2));
  }
  changeState(state) {
    if (this.state !== state) { this.state = state; this.log('config-state', {state}); }
  }
  sync() {
    if (!fs.existsSync(this.configPath)) { this.changeState('waiting-for-codex-config'); return; }
    const text = fs.readFileSync(this.configPath, 'utf8');
    const entry = providerBase(text);
    if (!entry) { this.changeState('waiting-for-supported-provider'); return; }
    if (sameEndpoint(entry.url, this.filterOrigin)) {
      if (this.routes.get(entry.provider)?.filteredUrl !== entry.url) {
        throw new Error('Filter URL found without its original-address backup; refusing to guess.');
      }
      this.changeState('filtering');
      return;
    }
    // adopt mode returns null: whatever the config points at right now becomes the upstream.
    const upstream = this.getUpstream();
    if (upstream && !sameEndpoint(entry.url, upstream)) { this.changeState('waiting-for-upstream'); return; }
    if (new URL(entry.url).search || new URL(entry.url).hash) {
      throw new Error('Query strings and fragments in provider base_url are not supported.');
    }
    const filteredUrl = rewriteOrigin(entry.url, this.filterOrigin);
    const changed = replaceBase(text, entry, filteredUrl);
    const filteredLine = providerBase(changed).line;
    // Recovery info is saved before redirecting Codex; auth and full config are never copied.
    this.routes.set(entry.provider, {originalUrl: entry.url, originalLine: entry.line, filteredUrl, filteredLine});
    // Remember the adopted address now: once the config points at the filter, this is the
    // only place the pre-takeover address survives in memory.
    if (!upstream) this.adopted = new URL(entry.url).origin;
    this.save();
    if (fs.readFileSync(this.configPath, 'utf8') !== text) return;
    atomicWrite(this.configPath, changed);
    this.changeState('filtering');
    this.log('config-attached', {provider: entry.provider});
  }
  restore() {
    if (!fs.existsSync(this.configPath)) return;
    let text = fs.readFileSync(this.configPath, 'utf8');
    const original = text;
    for (const [provider, route] of this.routes) {
      const entry = providerBase(text, provider);
      if (entry?.url === route.filteredUrl) {
        text = replaceBase(text, entry, route.originalUrl,
          entry.line === route.filteredLine ? route.originalLine : undefined);
      }
    }
    if (text !== original) {
      if (fs.readFileSync(this.configPath, 'utf8') !== original) throw new Error('Config changed while restoring; retry Stop.');
      atomicWrite(this.configPath, text);
    }
    this.changeState('restored');
  }
}

module.exports = {atomicWrite, providerBase, replaceBase, sameEndpoint, ccSwitchDatabaseDefault,
  discoverCcSwitch, resolveUpstream, ConfigRouter};
