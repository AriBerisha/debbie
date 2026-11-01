// The extension's service worker. It keeps a native messaging port open to
// debbie's host, which relays the requests of the session that has
// web access on, and does what they ask in the agent's tab, only on the
// sites the user allows (rules.js): open a page, read it, take a
// screenshot, click, hover, scroll. A page on a site that isn't on the
// list opens a small window that asks the user whether to allow the site,
// and a yes adds it. The agent's tab sits in the user's window, in a tab
// group whose title shows whether the agent is at work, and while it is,
// a glow runs around the page. The extension makes no network requests of
// its own: pages load because the agent opened them, and downloads they
// start are cancelled.
import { DEFAULT_SITES, baseSite, checkURL, confirmReason, unlistedSite } from "./rules.js";

const HOST = "debbie";
const MAX_TEXT = 100000; // characters of a page's text the agent gets
const MAX_LINKS = 300;
const LOAD_TIMEOUT = 60000;
const SETTLE = 1500; // after a page loads, for its scripts to fill it in
const SHOT_WIDTH = 1280; // the widest a screenshot is sent
const IDLE_AFTER = 4000; // the agent counts as at work this long after it acted
const ASK_TIMEOUT = 120000; // for the user to say whether a site may be added
const ASK_WINDOW = { width: 470, height: 440 }; // the window that asks

// The agent's tab group: its title says whether the agent is at work.
const GROUP_BUSY = { title: "🟢 debbie", color: "cyan" };
const GROUP_IDLE = { title: "💤 debbie", color: "grey" };

let port = null;
let agentTab = null; // the agent's tab, by ID
let agentGroup = null; // its tab group, by ID
let agentURL = ""; // the page it shows, when the extension may see it
let pending = null; // the navigation under way: { url, fail }
let lastShot = null; // the latest screenshot's page and scale, for clicks by position
let idleTimer = null;
let working = false; // the agent is at work, as the group's title says
let connected = false; // a session is connected
let session = null; // the session connected: { workspace, pid }
const asks = new Map(); // the user's pending answers about sites, by ask ID
let nextAsk = 1;
const declined = new Set(); // sites the user didn't allow this session: not asked about again

// The toolbar icon: teal while a session is connected, gray when not; a
// dot on it while the agent is at work.
const ICON_ON = { 16: "icons/icon-16.png", 32: "icons/icon-32.png" };
const ICON_OFF = { 16: "icons/icon-off-16.png", 32: "icons/icon-off-32.png" };

function showToolbar() {
  chrome.action.setIcon({ path: connected ? ICON_ON : ICON_OFF }).catch(() => {});
  chrome.action.setBadgeText({ text: working ? " " : "" });
  chrome.action.setBadgeBackgroundColor({ color: "#35ae94" });
  chrome.action.setTitle({ title: !connected ? "debbie: no session" : working ? "debbie: working in its tab" : "debbie: connected" });
}

// setWorking records whether the agent is at work, for the toolbar, the
// popup and the options page.
function setWorking(on) {
  if (working === on) return;
  working = on;
  chrome.storage.session.set({ working });
  showToolbar();
}

async function sites() {
  return (await chrome.storage.local.get({ sites: DEFAULT_SITES })).sites;
}

async function browserName() {
  return (await chrome.storage.local.get({ name: "" })).name;
}

// connect opens the port, which starts the host. While it is open, Chrome
// keeps this worker running (Chrome 105 and later); when the host ends,
// try again in a while.
function connect() {
  port = chrome.runtime.connectNative(HOST);
  port.onMessage.addListener(onMessage);
  port.onDisconnect.addListener(() => {
    const error = chrome.runtime.lastError ? chrome.runtime.lastError.message : "the host ended";
    port = null;
    showSession(null, error);
    setTimeout(connect, 10000);
  });
}

// showSession records which session is connected, for the popup and the
// options page, and shows it on the toolbar. When the session goes, the
// agent's tab leaves its group, which then disappears, the questions it
// asked close, and the sites it was refused may be asked about again.
function showSession(connectedSession, hostError = "") {
  session = connectedSession;
  chrome.storage.session.set({ session, hostError });
  connected = Boolean(session);
  showToolbar();
  if (!session) {
    setWorking(false);
    clearTimeout(idleTimer);
    if (agentTab !== null) {
      inPage(agentTab, "glow", false).catch(() => {});
      chrome.tabs.ungroup(agentTab).catch(() => {});
    }
    agentGroup = null;
    for (const ask of asks.values()) ask.settle("cancel");
    declined.clear();
  }
}

async function onMessage(msg) {
  if (msg.event === "session") {
    showSession(msg.connected ? { workspace: msg.workspace, pid: msg.pid } : null);
    return;
  }
  if (msg.event === "cancel") {
    // The session gave up on a call, as when the user pressed Ctrl+C
    // while Chrome asked them about a site.
    for (const ask of asks.values()) if (ask.call === msg.call) ask.settle("cancel");
    return;
  }
  if (msg.id === undefined) return;
  await busy();
  try {
    const result = await handle(msg.method, msg.params || {}, msg.id);
    port?.postMessage({ id: msg.id, result });
  } catch (e) {
    port?.postMessage({ id: msg.id, error: String((e && e.message) || e) });
  } finally {
    clearTimeout(idleTimer);
    idleTimer = setTimeout(idle, IDLE_AFTER);
  }
}

async function handle(method, params, call) {
  switch (method) {
    case "hello": {
      // The build is the manifest's version_name, which the install sets:
      // Chrome reports the copy it runs, which may be older than the files.
      const manifest = chrome.runtime.getManifest();
      return { version: manifest.version, build: manifest.version_name || "", sites: await sites(), name: await browserName() };
    }
    case "navigate":
      return navigate(String(params.url || ""), call);
    case "read":
      return read(["links", "interactive"].includes(params.mode) ? params.mode : "text");
    case "screenshot":
      return screenshot();
    case "describe":
      return describeTarget(params);
    case "click":
      return click(params, call);
    case "hover":
      return hover(params);
    case "scroll":
      return scroll(String(params.direction || "down"));
  }
  throw new Error(`unknown method ${method}`);
}

// allowed returns the sites the agent may open url on, asking the user
// first when its site isn't on the list (SPEC §20.0): a small window asks
// whether to allow the site, and yes adds it, with its subdomains, to the
// list the options page shows. No refuses, and the site isn't asked
// about again while this session lasts; a window closed or left
// unanswered refuses this once. Anything the list can't change (plain
// http, a local address) is refused outright. call is the session's call
// this is for, which it may cancel.
async function allowed(url, call) {
  const list = await sites();
  const problem = checkURL(url, list);
  if (!problem) return list;
  const site = unlistedSite(url, list);
  if (!site) throw new Error("refused: " + problem);
  if (declined.has(site)) {
    throw new Error(`refused: you didn't allow ${site} when Chrome asked, so the agent doesn't open it this session; the extension's options add sites`);
  }
  const answer = await askUser({ site, url, call });
  switch (answer) {
    case "no":
      declined.add(site);
      throw new Error(`refused: you didn't allow ${site} when Chrome asked, so the agent doesn't open it this session`);
    case "closed":
      throw new Error(`refused: the window asking whether the agent may open ${site} was closed without an answer`);
    case "timeout":
      throw new Error(`refused: nobody answered, within two minutes, whether the agent may open ${site}`);
    case "cancel":
      throw new Error("the call was cancelled while Chrome asked about " + site);
  }
  // The user allowed answer: the site itself, or all of its domain.
  const now = await sites();
  if (!now.includes(answer)) await chrome.storage.local.set({ sites: [...now, answer] });
  return sites();
}

// askUser opens the window that asks about a site, and resolves to the
// site the user allowed, or to why they didn't: "no", "closed", "timeout"
// or "cancel". The session hears that the question is being asked, so it
// waits longer than for a page, and shows it.
function askUser(ask) {
  return new Promise((resolve) => {
    const id = String(nextAsk++);
    const entry = { ...ask, id, window: null, timer: null };
    entry.settle = (answer) => {
      if (!asks.delete(id)) return;
      clearTimeout(entry.timer);
      if (entry.window !== null) chrome.windows.remove(entry.window).catch(() => {});
      resolve(answer);
    };
    asks.set(id, entry);
    entry.timer = setTimeout(() => entry.settle("timeout"), ASK_TIMEOUT);
    port?.postMessage({ event: "asking", call: ask.call, site: ask.site, url: ask.url });
    chrome.windows.create({ url: chrome.runtime.getURL("allow.html?ask=" + id), type: "popup", focused: true, ...ASK_WINDOW }).then((w) => {
      entry.window = w.id;
      if (!asks.has(id)) chrome.windows.remove(w.id).catch(() => {}); // answered before it opened
    }, () => entry.settle("closed"));
  });
}

// askAnswer takes the allow page's messages: what to ask about, and the
// user's answer. The site allowed can only be the one asked about or all
// of its domain.
function askAnswer(msg) {
  if (msg.ask !== undefined) {
    const ask = asks.get(String(msg.ask));
    if (!ask) return null;
    const wider = baseSite(ask.site);
    return { site: ask.site, url: ask.url, wider: wider !== ask.site ? wider : "", session };
  }
  const ask = asks.get(String(msg.answer));
  if (!ask) return false;
  if (msg.site && [ask.site, baseSite(ask.site)].includes(msg.site)) {
    ask.settle(msg.site);
  } else {
    ask.settle("no");
  }
  return true;
}

// busy shows that the agent is at work: the group's title, and the glow
// in its tab.
async function busy() {
  clearTimeout(idleTimer);
  setWorking(true);
  await setGroup(GROUP_BUSY);
  if (agentTab !== null) inPage(agentTab, "glow", true).catch(() => {});
}

async function idle() {
  setWorking(false);
  await setGroup(GROUP_IDLE);
  if (agentTab !== null) inPage(agentTab, "glow", false).catch(() => {});
}

// setGroup puts the agent's tab in its group, making one if need be, and
// sets the group's title.
async function setGroup(look) {
  if (agentTab === null) return;
  try {
    const tab = await chrome.tabs.get(agentTab);
    if (agentGroup === null || tab.groupId !== agentGroup) {
      agentGroup = await chrome.tabs.group({ tabIds: [agentTab], createProperties: { windowId: tab.windowId } });
    }
    await chrome.tabGroups.update(agentGroup, look);
  } catch {
    // The user moved or closed the tab: the next action finds it again.
  }
}

// inPage runs one of agent.js's functions in a tab, injecting it first if
// the page hasn't got it.
async function inPage(tabId, method, ...args) {
  await chrome.scripting.executeScript({ target: { tabId }, files: ["agent.js"] });
  const [{ result }] = await chrome.scripting.executeScript({
    target: { tabId },
    args: [method, args],
    func: (m, a) => Promise.resolve().then(() => globalThis.__debbie[m](...a)).then(
      (ok) => ({ ok }),
      (e) => ({ error: String((e && e.message) || e) }),
    ),
  });
  if (!result) throw new Error("the page didn't answer");
  if (result.error) throw new Error(result.error);
  return result.ok;
}

// agent returns the agent's tab, if it is still open.
async function agent() {
  if (agentTab === null) return null;
  return chrome.tabs.get(agentTab).catch(() => null);
}

// agentOn returns the agent's tab if it shows an allowed page, else says
// why not.
async function agentOn() {
  const tab = await agent();
  if (!tab) throw new Error("there is no agent tab: open a page first");
  if (!tab.url) throw new Error("refused: the agent's tab is on a site the extension doesn't allow");
  const problem = checkURL(tab.url, await sites());
  if (problem) throw new Error("refused: the agent's tab is at " + tab.url + ": " + problem);
  return tab;
}

async function navigate(url, call) {
  const list = await allowed(url, call);
  if (pending) throw new Error("another page is still loading");
  let tab = await agent();
  const wait = loaded(tab ? tab.id : null);
  pending = { url, fail: wait.fail };
  try {
    if (tab) {
      await chrome.tabs.update(tab.id, { url });
    } else {
      // A tab of its own in the user's window, behind the one they look
      // at, in the agent's group.
      tab = await chrome.tabs.create({ url, active: false });
      agentTab = tab.id;
      wait.tab = tab.id;
      await setGroup(GROUP_BUSY);
    }
    tab = await wait.promise;
  } finally {
    pending = null;
  }
  await settled(tab.id);
  tab = (await agent()) || tab;
  // Without host permission for where it ended up, Chrome hides the URL.
  if (!tab.url || checkURL(tab.url, list)) return { url: null, title: null };
  agentURL = tab.url;
  inPage(tab.id, "glow", true).catch(() => {});
  return { url: tab.url, title: tab.title };
}

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// settled waits for a page to stop changing (agent.js's settled), or a
// moment where the extension can't look into it.
async function settled(tabId, max = 6000) {
  await inPage(tabId, "settled", 600, max).catch(() => sleep(SETTLE));
}

// loaded waits for the agent's tab to finish loading a page. Its tab can
// be set after the call, for a tab about to be created; fail ends the wait
// early.
function loaded(tabId, timeout = LOAD_TIMEOUT) {
  const w = { tab: tabId };
  w.promise = new Promise((resolve, reject) => {
    const finish = () => {
      clearTimeout(timer);
      chrome.tabs.onUpdated.removeListener(listener);
    };
    const timer = setTimeout(() => {
      finish();
      reject(new Error("the page took over a minute to load"));
    }, timeout);
    const listener = (id, info, tab) => {
      if (id === w.tab && info.status === "complete") {
        finish();
        resolve(tab);
      }
    };
    chrome.tabs.onUpdated.addListener(listener);
    w.fail = (message) => {
      finish();
      reject(new Error(message));
    };
  });
  return w;
}

// settle waits after a click for a page it may load: a load that starts
// within a moment, until it ends. A page that changes without loading,
// as YouTube's do, has changed by then.
async function settle(tabId) {
  let started = false;
  const listener = (id, info) => {
    if (id === tabId && info.status === "loading") started = true;
  };
  chrome.tabs.onUpdated.addListener(listener);
  await sleep(1200);
  chrome.tabs.onUpdated.removeListener(listener);
  const tab = await chrome.tabs.get(tabId).catch(() => null);
  if (started && tab && tab.status !== "complete") {
    await loaded(tabId, 30000).promise.catch(() => {});
    await sleep(SETTLE);
  }
}

async function read(mode) {
  const tab = await agentOn();
  const page = { url: tab.url, title: tab.title };
  if (mode === "interactive") return { ...page, ...(await inPage(tab.id, "interactive")) };
  const [{ result }] = await chrome.scripting.executeScript({
    target: { tabId: tab.id },
    args: [mode, MAX_LINKS],
    func: (mode, maxLinks) => {
      if (mode !== "links") return { text: document.body ? document.body.innerText : "" };
      const seen = new Set();
      const links = [];
      for (const a of document.querySelectorAll("a[href]")) {
        const url = a.href;
        if (!/^https?:/.test(url) || seen.has(url)) continue;
        seen.add(url);
        const text = (a.innerText || a.getAttribute("aria-label") || a.title || "").replace(/\s+/g, " ").trim();
        links.push({ text: text.slice(0, 100), url });
        if (links.length >= maxLinks) break;
      }
      return { links };
    },
  });
  if (mode === "links") return { ...page, links: result.links };
  const text = String(result.text || "");
  return { ...page, text: text.slice(0, MAX_TEXT), truncated: text.length > MAX_TEXT };
}

// pointAt turns a target's position, in the pixels of the latest
// screenshot, into the page's own (CSS) pixels.
function pointAt(tab, params) {
  if (params.ref) return { ref: String(params.ref) };
  if (!lastShot || lastShot.tab !== tab.id || lastShot.url !== tab.url) {
    throw new Error("take a screenshot of this page first: x and y are pixels of the latest screenshot");
  }
  return { x: Number(params.x) * lastShot.scale, y: Number(params.y) * lastShot.scale };
}

async function describeTarget(params) {
  const tab = await agentOn();
  return { ...(await inPage(tab.id, "describe", pointAt(tab, params))), page: tab.url };
}

// click clicks an element, after the checks the extension makes whatever
// the CLI asked: never a CAPTCHA, never a link off the allowed sites
// (unless the user allows the site when asked), and an action the user
// always confirms only once they did. A click that leads off the allowed
// sites is undone.
async function click(params, call) {
  const tab = await agentOn();
  let list = await sites();
  const target = pointAt(tab, params);
  const el = await inPage(tab.id, "describe", target);
  if (el.captcha) throw new Error("refused: it is part of a CAPTCHA, which the agent never touches; the user can solve it");
  if (el.disabled) throw new Error("it is disabled");
  const reason = confirmReason(el);
  if (reason && !params.confirmed) throw new Error(`refused: clicking it would ${reason}, which needs the user's confirmation`);
  if (el.href && el.role === "link") {
    try {
      list = await allowed(el.href, call);
    } catch (e) {
      throw new Error("refused: the link goes to " + el.href + ": " + String((e && e.message) || e).replace(/^refused: /, ""));
    }
    if (el.target && el.target !== "_self") {
      // It would open a tab of the user's: the agent's tab goes instead.
      const page = await navigate(el.href, call);
      return { ...page, navigated: true, element: el };
    }
  }
  const before = tab.url;
  await inPage(tab.id, "click", target);
  await settle(tab.id);
  const after = await agent();
  if (!after || !after.url || checkURL(after.url, list)) {
    await chrome.tabs.goBack(tab.id).catch(() => {});
    throw new Error("refused: the click led off the allowed sites, so the agent's tab went back");
  }
  agentURL = after.url;
  await settled(after.id, after.url !== before ? 6000 : 2000); // the page's scripts answer the click
  inPage(after.id, "glow", true).catch(() => {});
  const now = (await agent()) || after;
  return { url: now.url, title: now.title, navigated: now.url !== before, element: el };
}

async function hover(params) {
  const tab = await agentOn();
  const el = await inPage(tab.id, "hover", pointAt(tab, params));
  return { url: tab.url, title: tab.title, element: el };
}

async function scroll(direction) {
  if (!["up", "down", "top", "bottom"].includes(direction)) throw new Error("direction is up, down, top or bottom");
  const tab = await agentOn();
  return { url: tab.url, title: tab.title, ...(await inPage(tab.id, "scroll", direction)) };
}

// screenshot captures the agent's tab, for a model that sees images: the
// CLI offers it only to such a model. Chrome captures only a window's
// active tab, so the agent's is brought to the front for a moment.
async function screenshot() {
  const tab = await agentOn();
  // Chrome captures a tab only for an extension that may read every site,
  // which the manifest asks for; the user can restrict it in Chrome.
  if (!(await chrome.permissions.contains({ origins: ["<all_urls>"] }))) {
    throw new Error("Chrome doesn't let the extension capture tabs, because its site access is restricted: " +
      "in chrome://extensions, click Details on the extension and set Site access to “On all sites”");
  }
  const [front] = await chrome.tabs.query({ active: true, windowId: tab.windowId });
  const away = front && front.id !== tab.id;
  if (away) {
    await chrome.tabs.update(tab.id, { active: true });
    await sleep(250);
  }
  await inPage(tab.id, "hidden", true).catch(() => {});
  let shot;
  try {
    shot = await chrome.tabs.captureVisibleTab(tab.windowId, { format: "jpeg", quality: 80 });
  } catch (e) {
    throw new Error("Chrome couldn't capture the agent's tab (is its window minimized?): " + ((e && e.message) || e));
  } finally {
    await inPage(tab.id, "hidden", false).catch(() => {});
    if (away) await chrome.tabs.update(front.id, { active: true }).catch(() => {});
  }
  const image = await shrink(shot, SHOT_WIDTH);
  const view = await inPage(tab.id, "viewport");
  lastShot = { tab: tab.id, url: tab.url, scale: view.width / image.width };
  return { url: tab.url, title: tab.title, ...image };
}

// shrink scales a captured JPEG data URL down to at most width pixels
// wide, and returns it in base64 with its size.
async function shrink(dataURL, width) {
  const data = dataURL.slice(dataURL.indexOf(",") + 1);
  const bytes = Uint8Array.from(atob(data), (c) => c.charCodeAt(0));
  const bitmap = await createImageBitmap(new Blob([bytes], { type: "image/jpeg" }));
  if (bitmap.width <= width) {
    return { mediaType: "image/jpeg", data, width: bitmap.width, height: bitmap.height };
  }
  const height = Math.round((bitmap.height * width) / bitmap.width);
  const canvas = new OffscreenCanvas(width, height);
  canvas.getContext("2d").drawImage(bitmap, 0, 0, width, height);
  const out = new Uint8Array(await (await canvas.convertToBlob({ type: "image/jpeg", quality: 0.8 })).arrayBuffer());
  let binary = "";
  for (let i = 0; i < out.length; i += 0x8000) binary += String.fromCharCode(...out.subarray(i, i + 0x8000));
  return { mediaType: "image/jpeg", data: btoa(binary), width, height };
}

// Downloads the agent's pages start are cancelled (SPEC §20.6): the one an
// address it opens starts, and those its page starts. A download item
// doesn't say which tab started it; its URL and referrer do.
chrome.downloads.onCreated.addListener(async (item) => {
  const byNavigation = pending && item.url === pending.url;
  const byPage = agentURL !== "" && item.referrer === agentURL;
  if (!byNavigation && !byPage) return;
  await chrome.downloads.cancel(item.id).catch(() => {});
  await chrome.downloads.erase({ id: item.id }).catch(() => {});
  if (byNavigation) pending.fail("refused: the address started a download, which the extension cancelled");
});

chrome.tabs.onUpdated.addListener((id, info) => {
  if (id === agentTab && info.url !== undefined) agentURL = info.url;
});
chrome.tabs.onRemoved.addListener((id) => {
  if (id === agentTab) {
    agentTab = agentGroup = null;
    agentURL = "";
  }
});
// The browser's name, from the popup, and the sites, from the options page
// or an answer to the window that asks, go to the session at once.
chrome.storage.onChanged.addListener((changes, area) => {
  if (area !== "local") return;
  if (changes.name) port?.postMessage({ event: "name", name: changes.name.newValue || "" });
  if (changes.sites) port?.postMessage({ event: "sites", sites: changes.sites.newValue || [] });
});
// The popup and the options page ask how things stand, the popup can
// bring the agent's tab to the front, and the window that asks about a
// site learns what to ask and brings the answer.
chrome.runtime.onMessage.addListener((msg, _sender, reply) => {
  if (msg === "status") reply({ connected: port !== null, working, agentTab: agentTab !== null });
  if (msg === "showAgentTab" && agentTab !== null) {
    chrome.tabs.get(agentTab).then((tab) => {
      chrome.tabs.update(tab.id, { active: true });
      chrome.windows.update(tab.windowId, { focused: true });
    }).catch(() => {});
    reply(true);
  }
  if (msg && typeof msg === "object" && (msg.ask !== undefined || msg.answer !== undefined)) reply(askAnswer(msg));
});
// A window that asked about a site and was closed answers nothing.
chrome.windows.onRemoved.addListener((windowId) => {
  for (const ask of asks.values()) {
    if (ask.window === windowId) {
      ask.window = null;
      ask.settle("closed");
    }
  }
});
// These start the worker when Chrome starts and when the extension is
// installed; the code below then connects.
chrome.runtime.onStartup.addListener(() => {});
chrome.runtime.onInstalled.addListener(() => {});
chrome.storage.session.set({ working: false });
showToolbar();
connect();
