// What the popup and the options page share: debbie's wordmark, how the
// connection stands, the browser's name field, and small helpers for
// building parts of a page without innerHTML.

// LOGO is internal/ui/logo.txt, the terminal banner's "DEBBIE" and
// debbie.sh's wordmark (Chintzy CPU by Brian Kent): "#" is ink.
const LOGO = [
  "#######...#######.######...######...#..#######",
  "#......#.#........#....##..#....##..#.#.......",
  "#......#.#........#....##..#....##..#.#.......",
  "##.....#.########.########.########.#.########",
  "##.....#.##.......###....#.###....#.#.##......",
  "##.....#.##.......###....#.###....#.#.##......",
  "##.....#.##.......###....#.###....#.#.##......",
  "#######...#######.#######..#######..#..#######",
];

// wordmark draws the logo as SVG, height pixels high, in the gradient the
// page's --logo-from and --logo-to colors give it.
export function wordmark(height) {
  const ns = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(ns, "svg");
  const w = LOGO[0].length;
  svg.setAttribute("viewBox", `0 0 ${w} ${LOGO.length}`);
  svg.setAttribute("height", String(height));
  svg.setAttribute("width", String((height * w) / LOGO.length));
  svg.setAttribute("role", "img");
  svg.setAttribute("aria-label", "debbie");
  svg.setAttribute("shape-rendering", "crispEdges");
  const gradient = document.createElementNS(ns, "linearGradient");
  gradient.setAttribute("id", "logo-gradient");
  for (const [offset, color] of [["0", "--logo-from"], ["1", "--logo-to"]]) {
    const stop = document.createElementNS(ns, "stop");
    stop.setAttribute("offset", offset);
    stop.setAttribute("style", `stop-color: var(${color})`);
    gradient.append(stop);
  }
  const defs = document.createElementNS(ns, "defs");
  defs.append(gradient);
  let d = "";
  LOGO.forEach((row, y) => {
    for (const run of row.matchAll(/#+/g)) d += `M${run.index} ${y}h${run[0].length}v1h-${run[0].length}z`;
  });
  const path = document.createElementNS(ns, "path");
  path.setAttribute("d", d);
  path.setAttribute("fill", "url(#logo-gradient)");
  svg.append(defs, path);
  return svg;
}

// connection says how things stand: "offline" (no host: Chrome couldn't
// start debbie), "ready" (the host runs, but no session has web access
// on) or "session"; with the session, the host's error, and whether the
// agent is at work and has a tab of its own.
export async function connection() {
  const port = await chrome.runtime.sendMessage("status").catch(() => null);
  const { session, hostError } = await chrome.storage.session.get({ session: null, hostError: "" });
  const state = !port || !port.connected ? "offline" : session ? "session" : "ready";
  return { state, session, hostError, working: Boolean(port && port.working), agentTab: Boolean(port && port.agentTab) };
}

// The pill each state shows.
export const PILLS = {
  offline: ["off", "Not connected"],
  ready: ["warn", "Ready"],
  session: ["ok", "Connected"],
};

// pill shows a state as a colored pill: ok, warn or off.
export function pill(element, kind, text) {
  element.className = `pill ${kind}`;
  element.textContent = text;
}

// nameField fills in and saves the browser's name, a moment after typing
// stops; the extension then tells the session. saved shows that it was.
export async function nameField(input, saved) {
  input.value = (await chrome.storage.local.get({ name: "" })).name;
  let timer = null;
  input.addEventListener("input", () => {
    clearTimeout(timer);
    saved.classList.remove("shown");
    timer = setTimeout(async () => {
      await chrome.storage.local.set({ name: input.value.trim().slice(0, 40) });
      saved.classList.add("shown");
    }, 400);
  });
}

// shortPath shows a path in at most room characters: a long one by its
// last parts, which say the most.
export function shortPath(path, room = 40) {
  if (path.length <= room) return path;
  const sep = path.includes("\\") ? "\\" : "/";
  const parts = path.split(/[\\/]/).filter(Boolean);
  let out = parts.pop() || path;
  while (parts.length && out.length + parts[parts.length - 1].length + 3 <= room) out = parts.pop() + sep + out;
  return "…" + sep + out;
}

// el makes an element with a class and text.
export function el(tag, className, text) {
  const e = document.createElement(tag);
  if (className) e.className = className;
  if (text !== undefined) e.textContent = text;
  return e;
}

// command shows a command to type in debbie, with a button that copies
// it.
export function command(text) {
  const box = el("span", "command");
  const code = el("code", "", text);
  const copy = el("button", "copy", "Copy");
  copy.type = "button";
  copy.title = "Copy " + text;
  copy.onclick = async () => {
    try {
      await navigator.clipboard.writeText(text);
      copy.textContent = "Copied";
    } catch {
      copy.textContent = "Select and copy it";
    }
    setTimeout(() => (copy.textContent = "Copy"), 1500);
  };
  box.append(code, copy);
  return box;
}
