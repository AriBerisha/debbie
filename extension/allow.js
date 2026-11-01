// The window that asks whether the agent may open a site that isn't on
// the list (SPEC §20.0). The service worker opens it with the question's
// ID in the address and takes the answer. Allowing asks Chrome for the
// site's host permission in the click, as the options page does; with
// access to every site that is granted at once.
import { originsFor } from "./rules.js";
import { wordmark } from "./shared.js";

const get = (name) => document.getElementById(name);
const id = new URLSearchParams(location.search).get("ask");
const ask = id === null ? null : await chrome.runtime.sendMessage({ ask: id }).catch(() => null);

if (!ask) {
  get("mark-gone").append(wordmark(18));
  get("gone").hidden = false;
  get("close").onclick = () => window.close();
} else {
  get("mark").append(wordmark(18));
  get("site").textContent = ask.site;
  get("url").textContent = ask.url;
  get("url").title = ask.url;
  const workspace = ask.session && ask.session.workspace ? ask.session.workspace : "";
  const folder = workspace.split(/[\\/]/).filter(Boolean).pop() || "";
  get("who").textContent = folder ? `The debbie session in ${folder} asks for this page.` : "A debbie session asks for this page.";
  const wider = get("wider");
  const chosen = () => (wider.checked ? ask.wider : ask.site);
  if (ask.wider) {
    get("wider-row").hidden = false;
    get("wider-site").textContent = ask.wider;
    wider.onchange = () => (get("yes-site").textContent = chosen());
  }
  get("yes-site").textContent = ask.site;
  const yes = get("yes");
  const no = get("no");
  const answer = async (msg) => {
    yes.disabled = no.disabled = true;
    await chrome.runtime.sendMessage(msg).catch(() => {});
    window.close();
  };
  yes.onclick = async () => {
    const site = chosen();
    // If the user restricted the extension's site access, Chrome asks
    // them here. It must run in the click, before any other await.
    if (!(await chrome.permissions.request({ origins: originsFor(site) }).catch(() => false))) {
      get("error").textContent = `Chrome didn't allow ${site}.`;
      get("error").hidden = false;
      return;
    }
    await answer({ answer: id, site });
  };
  no.onclick = () => answer({ answer: id, no: true });
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape") no.click();
  });
  get("ask").hidden = false;
}
