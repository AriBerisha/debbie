// The sites the agent may open, and what it may never open whatever the
// sites (SPEC §20.6). Pure functions, tested by tests/run.html.

// DEFAULT_SITES are allowed until the user changes the list in the
// options; the manifest grants their host permission.
export const DEFAULT_SITES = ["youtube.com"];

// isLocal reports whether a host name is an address or a name for this
// machine or its network, which the agent never opens.
function isLocal(host) {
  return /^\d+\.\d+\.\d+\.\d+$/.test(host) || host.includes(":") || host.startsWith("[") ||
    host === "localhost" || host.endsWith(".localhost") || host.endsWith(".local") || !host.includes(".");
}

// siteOf turns what the user typed, a name or a URL, into a site as the
// list keeps it: lowercase, without "www.", a port or a path; "" when it
// isn't a site the agent could open.
export function siteOf(text) {
  let host = String(text).trim().toLowerCase();
  if (host.includes("://")) {
    try {
      host = new URL(host).hostname;
    } catch {
      return "";
    }
  }
  host = host.replace(/[/?#].*$/, "").replace(/:\d+$/, "").replace(/\.$/, "");
  if (host.startsWith("www.")) host = host.slice(4);
  if (!/^[a-z0-9-]+(\.[a-z0-9-]+)+$/.test(host) || isLocal(host)) return "";
  return host;
}

// allowedSite returns the site of sites that host is, or is under; "" for
// none.
export function allowedSite(host, sites) {
  host = String(host).toLowerCase().replace(/\.$/, "");
  return sites.find((s) => host === s || host.endsWith("." + s)) || "";
}

// baseSite returns the site that covers all of a host's domain:
// example.com for docs.example.com, bbc.co.uk for news.bbc.co.uk. Without
// the public suffix list it is a guess: the last two labels, or three when
// the second is a registry's name under a country code.
export function baseSite(host) {
  const labels = String(host).toLowerCase().split(".");
  if (labels.length <= 2) return labels.join(".");
  const [sld, tld] = labels.slice(-2);
  const n = tld.length === 2 && /^(ac|co|com|edu|gov|go|gob|ltd|me|mil|ne|net|nom|or|org|plc)$/.test(sld) ? 3 : 2;
  return labels.slice(-n).join(".");
}

// unlistedSite returns the site that url needs on the list to open, which
// the extension asks the user to add (SPEC §20.0): "" when the URL opens
// already, or is refused for a reason no list changes.
export function unlistedSite(url, sites) {
  let u;
  try {
    u = new URL(url);
  } catch {
    return "";
  }
  const site = siteOf(u.hostname);
  if (!site || allowedSite(u.hostname, sites)) return "";
  return checkURL(url, [...sites, site]) ? "" : site;
}

// checkURL says why the agent may not open url; "" when it may.
export function checkURL(url, sites) {
  let u;
  try {
    u = new URL(url);
  } catch {
    return "it isn't a URL";
  }
  if (u.protocol !== "https:") return `only https pages open, not ${u.protocol} ones`;
  if (u.username || u.password) return "it carries a user name or password";
  const host = u.hostname.toLowerCase();
  if (isLocal(host)) return "it is on this machine or a private network";
  if (host === "chromewebstore.google.com" || host === "chrome.google.com" && u.pathname.startsWith("/webstore")) {
    return "it is the Chrome Web Store";
  }
  if (!allowedSite(host, sites)) {
    return `${host} isn't one of the sites the extension allows (${sites.join(", ") || "none"}); the extension's options add sites`;
  }
  return "";
}

// originsFor returns the host permissions that cover a site and its
// subdomains.
export function originsFor(site) {
  return [`https://${site}/*`, `https://*.${site}/*`];
}

// CONFIRM matches what an element says when clicking it would do something
// the user always confirms, even in yolo (SPEC §20.6): buying, paying,
// subscribing, joining, donating, sending, deleting, posting, signing in,
// up or out, accepting terms, granting access, liking, reporting. The CLI
// has the same list (internal/browser/rules.go).
const CONFIRM = /\b(buy|purchase|pay|payment|order|checkout|check out|subscribe|unsubscribe|join|donate|transfer|send|delete|remove|publish|post|comment|sign up|signup|sign in|signin|sign out|signout|log in|login|log out|logout|register|accept|agree|allow|authori[sz]e|grant|like|dislike|report)\b/i;

// confirmReason returns the word that makes a click always ask first, ""
// when none does. A link is judged by its address, since its text is
// whatever the page says, such as a video's title; anything else by its
// label and its form's action.
export function confirmReason(el) {
  let text = `${String(el.name || "").slice(0, 100)} ${el.formAction || ""}`;
  if (el.role === "link" && !el.formAction) {
    try {
      const u = new URL(el.href);
      text = decodeURIComponent(u.pathname + u.search).replace(/[/?&=_.-]+/g, " ");
    } catch {
      text = "";
    }
  }
  const m = CONFIRM.exec(text);
  return m ? m[1].toLowerCase() : "";
}
