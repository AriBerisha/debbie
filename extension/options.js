// The options page: the connection, the sites the agent may open,
// screenshots, the browser's name, and what the agent does.
import { DEFAULT_SITES, originsFor, siteOf } from "./rules.js";
import { PILLS, command, connection, el, nameField, pill, wordmark } from "./shared.js";

// ALL_SITES is what Chrome requires before an extension may capture a tab.
// The manifest asks for it, so screenshots work from the start; the user
// can still restrict the extension's site access in Chrome.
const ALL_SITES = ["<all_urls>"];

document.getElementById("mark").append(wordmark(22));
nameField(document.getElementById("name"), document.getElementById("saved"));
const manifest = chrome.runtime.getManifest();
document.getElementById("build").textContent = `debbie browser ${manifest.version_name || manifest.version}`;

async function sites() {
  return (await chrome.storage.local.get({ sites: DEFAULT_SITES })).sites;
}

// statusParts says how the connection stands, and what to do about it.
function statusParts(c) {
  if (c.state === "offline") {
    const p = el("p", "", "Chrome can't reach debbie" + (c.hostError ? ` (${c.hostError})` : "") + ". Set it up from a debbie session with ");
    p.append(command("/browser install"), document.createTextNode(", then reload this extension."));
    return [p];
  }
  if (c.state === "ready") {
    const p = el("p", "", "Connected to debbie, but no session has web access on. Turn it on in a session with ");
    p.append(command("/web safe"), document.createTextNode("."));
    return [p];
  }
  // The session: where it works, its process, and what the agent does.
  const details = el("dl", "details");
  const add = (term, ...value) => {
    const dd = el("dd");
    dd.append(...value);
    details.append(el("dt", "", term), dd);
  };
  add("Workspace", el("code", "path", c.session.workspace));
  add("Process", String(c.session.pid));
  const agent = el("span");
  pill(agent, c.working ? "ok busy" : "off", c.working ? "Working in its tab" : c.agentTab ? "Idle, its tab is open" : "No page open yet");
  add("Agent", agent);
  return [details];
}

// show draws the page. It learns everything first and then changes the
// page in one go, so calls that overlap, as a click and the storage change
// it makes do, can't mix their lists.
async function show() {
  const list = await sites();
  const c = await connection();
  const shots = await chrome.permissions.contains({ origins: ALL_SITES });

  const items = list.map((site) => {
    const item = el("li");
    const text = el("span", "text");
    text.append(el("span", "name", site), el("span", "sub", "and its subdomains"));
    const remove = el("button", "remove", "Remove");
    remove.type = "button";
    remove.setAttribute("aria-label", `Remove ${site}`);
    remove.onclick = async () => {
      await chrome.storage.local.set({ sites: (await sites()).filter((s) => s !== site) });
      // Access to every site stays granted (Chrome refuses to take back
      // what the manifest asks for), but the list decides.
      await chrome.permissions.remove({ origins: originsFor(site) }).catch(() => {});
      show();
    };
    item.append(el("span", "avatar", site[0]), text, remove);
    return item;
  });
  if (items.length === 0) {
    const none = el("li", "none", "None: the agent can't open any page.");
    items.push(none);
  }
  document.getElementById("sites").replaceChildren(...items);
  const count = list.length === 1 ? "1 site" : `${list.length} sites`;
  document.getElementById("count").textContent = count;
  document.getElementById("remove-all-count").textContent = count;
  document.getElementById("remove-all").hidden = list.length === 0;
  if (list.length === 0) confirmRemoveAll.hidden = true;

  pill(document.getElementById("pill"), ...PILLS[c.state]);
  document.getElementById("status").replaceChildren(...statusParts(c));

  pill(document.getElementById("shots-pill"), shots ? "ok" : "warn", shots ? "Allowed" : "Blocked");
  document.getElementById("shots").textContent = shots
    ? "debbie takes them only when its model can see images, as you chose when you set the model up; other models read pages' text."
    : "The extension's site access is restricted in Chrome. To allow them, open chrome://extensions, " +
      "click Details on this extension, and set Site access to “On all sites”.";
}

const input = document.getElementById("site");
const error = document.getElementById("site-error");
function problem(text) {
  error.textContent = text;
  error.hidden = !text;
  input.setAttribute("aria-invalid", text ? "true" : "false");
}
input.addEventListener("input", () => problem(""));

document.getElementById("add").onsubmit = async (event) => {
  event.preventDefault();
  const site = siteOf(input.value);
  if (!site) {
    problem("Give a site's name, such as example.com.");
    input.focus();
    return;
  }
  // With access to every site this is granted at once; if the user
  // restricted the extension's site access, Chrome asks them. This must
  // run in the click, before any other await.
  if (!(await chrome.permissions.request({ origins: originsFor(site) }))) {
    problem(`Chrome didn't allow ${site}.`);
    return;
  }
  const list = await sites();
  if (!list.includes(site)) await chrome.storage.local.set({ sites: [...list, site] });
  input.value = "";
  problem("");
  show();
};

// Remove all empties the list, after a confirmation in the page. Access to
// every site stays granted, as above; the empty list decides.
const confirmRemoveAll = document.getElementById("remove-all-confirm");
document.getElementById("remove-all").onclick = () => {
  confirmRemoveAll.hidden = false;
  document.getElementById("remove-all-no").focus();
};
document.getElementById("remove-all-no").onclick = () => {
  confirmRemoveAll.hidden = true;
};
document.getElementById("remove-all-yes").onclick = async () => {
  const list = await sites();
  await chrome.storage.local.set({ sites: [] });
  for (const site of list) await chrome.permissions.remove({ origins: originsFor(site) }).catch(() => {});
  confirmRemoveAll.hidden = true;
  show();
};

chrome.storage.onChanged.addListener(show);
show();
