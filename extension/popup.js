// The toolbar popup: how the connection stands and what the agent does,
// the name of this browser, which the session shows, the sites and
// screenshots at a glance, and the way to the options page.
import { DEFAULT_SITES } from "./rules.js";
import { PILLS, command, connection, el, nameField, pill, shortPath, wordmark } from "./shared.js";

document.getElementById("mark").append(wordmark(15));
nameField(document.getElementById("name"), document.getElementById("saved"));
document.getElementById("options").onclick = () => chrome.runtime.openOptionsPage();
const manifest = chrome.runtime.getManifest();
document.getElementById("build").textContent = manifest.version_name || manifest.version;

// stateCard says what the session and the agent are doing, or how to get
// there.
function stateCard(c) {
  const card = document.getElementById("state");
  const parts = [];
  if (c.state === "offline") {
    parts.push(el("div", "title", "Chrome can't reach debbie"));
    const p = el("p", "detail", "Set it up from a debbie session: ");
    p.append(command("/browser install"));
    parts.push(p);
    if (c.hostError) parts.push(el("p", "detail", `Chrome says: ${c.hostError}`));
  } else if (c.state === "ready") {
    parts.push(el("div", "title", "No session has web access on"));
    const p = el("p", "detail", "Turn it on in debbie: ");
    p.append(command("/web safe"));
    parts.push(p);
  } else {
    // The session's folder by name, its path under it, and the agent.
    const path = c.session.workspace;
    const title = el("div", "title", "Session in ");
    title.append(el("span", "folder", path.split(/[\\/]/).filter(Boolean).pop() || path));
    const where = el("div", "where", shortPath(path));
    where.title = path;
    const row = el("div", "row");
    const doing = el("span", "");
    pill(doing, c.working ? "ok busy" : "off", c.working ? "Working in its tab" : c.agentTab ? "Idle" : "No page open yet");
    row.append(doing);
    if (c.agentTab) {
      const show = el("button", "ghost small", "Show its tab");
      show.type = "button";
      show.onclick = async () => {
        await chrome.runtime.sendMessage("showAgentTab").catch(() => {});
        window.close();
      };
      row.append(show);
    }
    parts.push(title, where, row);
  }
  card.replaceChildren(...parts);
}

async function show() {
  const c = await connection();
  pill(document.getElementById("pill"), ...PILLS[c.state]);
  stateCard(c);

  const list = (await chrome.storage.local.get({ sites: DEFAULT_SITES })).sites;
  const chips = list.slice(0, 2).map((s) => el("span", "chip", s));
  if (list.length > 2) chips.push(el("span", "chip", `+${list.length - 2}`));
  if (list.length === 0) chips.push(el("span", "muted", "none"));
  document.getElementById("sites").replaceChildren(...chips);

  const shots = el("span", "");
  if (await chrome.permissions.contains({ origins: ["<all_urls>"] })) {
    pill(shots, "ok", "Allowed");
    shots.title = "debbie takes them only when its model can see images.";
  } else {
    pill(shots, "warn", "Blocked");
    shots.title = "The extension's site access is restricted in Chrome; the settings page says how to undo that.";
  }
  document.getElementById("shots").replaceChildren(shots);
}

chrome.storage.onChanged.addListener(show);
show();
