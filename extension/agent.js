// The agent's hands in a page. The extension injects this into the agent's
// tab, in the extension's isolated world, where the page's scripts can't
// reach it. It lists the page's interactive elements under short refs,
// moves a visible cursor, clicks, hovers and scrolls, and draws the glow
// that shows the agent is at work. It writes no attributes into the page:
// its only element is its overlay, in a closed shadow root.
(() => {
  if (globalThis.__debbie) return;

  const INTERACTIVE = "a[href], button, input:not([type=hidden]), select, textarea, summary, " +
    "[role=button], [role=link], [role=tab], [role=menuitem], [role=checkbox], [role=switch], " +
    "[role=option], [role=combobox], [contenteditable=''], [contenteditable=true]";
  const MAX_ELEMENTS = 300;

  const refs = new Map(); // "e12" → element, from the last interactive read
  let cursor = { x: -40, y: -40 };
  let overlay = null;

  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

  // ui returns the overlay, adding it again if the page removed it.
  function ui() {
    if (overlay && overlay.host.isConnected) return overlay;
    const host = document.createElement("debbie-agent");
    host.style.cssText = "all: initial; position: fixed; inset: 0; pointer-events: none; z-index: 2147483647;";
    const root = host.attachShadow({ mode: "closed" });
    root.innerHTML = `<style>
      .glow { position: fixed; inset: 0; pointer-events: none; opacity: 0; transition: opacity .5s ease;
        box-shadow: inset 0 0 16px 4px rgba(53, 174, 148, .8), inset 0 0 70px 22px rgba(33, 97, 94, .45);
        animation: pulse 2.4s ease-in-out infinite; }
      .glow.on { opacity: 1; }
      @keyframes pulse { 50% { box-shadow: inset 0 0 26px 7px rgba(159, 245, 221, .75), inset 0 0 90px 30px rgba(53, 174, 148, .35); } }
      .cursor { position: fixed; left: 0; top: 0; width: 24px; height: 24px; opacity: 0;
        transition: transform .45s cubic-bezier(.2, .8, .2, 1), opacity .3s; filter: drop-shadow(0 1px 3px rgba(0, 0, 0, .5)); }
      .cursor.on { opacity: 1; }
      .ripple { position: fixed; left: -16px; top: -16px; width: 32px; height: 32px; border-radius: 50%;
        border: 2px solid #9ff5dd; opacity: 0; }
      .ripple.go { animation: ripple .55s ease-out; }
      @keyframes ripple { from { opacity: .95; scale: .3; } to { opacity: 0; scale: 1.7; } }
      .badge { position: fixed; left: 50%; bottom: 18px; translate: -50% 12px; opacity: 0;
        transition: opacity .35s ease, translate .35s ease;
        display: flex; align-items: center; gap: 8px; padding: 7px 14px 7px 11px; border-radius: 999px;
        background: rgba(13, 43, 39, .9); color: #dffcf3; font: 600 12.5px/1 system-ui, "Segoe UI", sans-serif;
        letter-spacing: .01em; white-space: nowrap;
        box-shadow: 0 6px 24px rgba(0, 0, 0, .28), inset 0 0 0 1px rgba(159, 245, 221, .28); }
      .badge.on { opacity: 1; translate: -50% 0; }
      .badge .dot { width: 8px; height: 8px; border-radius: 50%; background: #9ff5dd;
        box-shadow: 0 0 0 0 rgba(159, 245, 221, .6); animation: beat 1.6s ease-out infinite; }
      @keyframes beat { 0% { box-shadow: 0 0 0 0 rgba(159, 245, 221, .55); } 100% { box-shadow: 0 0 0 9px rgba(159, 245, 221, 0); } }
    </style>
    <div class="glow"></div>
    <div class="badge" role="status"><span class="dot"></span>debbie is working in this tab</div>
    <svg class="cursor" viewBox="0 0 24 24"><path d="M3 2 L3 19 L7.8 14.6 L11 21.6 L14.1 20.2 L11 13.2 L17.4 13.2 Z"
      fill="#35ae94" stroke="#0d2b27" stroke-width="1.4" stroke-linejoin="round"/></svg>
    <div class="ripple"></div>`;
    document.documentElement.append(host);
    overlay = { host, glow: root.querySelector(".glow"), cursor: root.querySelector(".cursor"), ripple: root.querySelector(".ripple"),
      badge: root.querySelector(".badge") };
    place(cursor.x, cursor.y, true);
    return overlay;
  }

  function place(x, y, instant) {
    const c = ui().cursor;
    c.style.transition = instant ? "none" : "";
    c.style.transform = `translate(${x - 3}px, ${y - 2}px)`;
  }

  // moveTo glides the cursor to a point and waits until it is there.
  async function moveTo(x, y) {
    ui().cursor.classList.add("on");
    const far = Math.hypot(x - cursor.x, y - cursor.y) > 4;
    place(x, y, false);
    cursor = { x, y };
    await sleep(far ? 480 : 60);
  }

  function ripple(x, y) {
    const r = ui().ripple;
    r.style.translate = `${x}px ${y}px`;
    r.classList.remove("go");
    void r.offsetWidth; // restarts the animation
    r.classList.add("go");
  }

  function visible(el) {
    const r = el.getBoundingClientRect();
    if (r.width < 1 || r.height < 1) return false;
    const s = getComputedStyle(el);
    return s.visibility !== "hidden" && s.display !== "none" && s.opacity !== "0";
  }

  function onScreen(el) {
    const r = el.getBoundingClientRect();
    return r.bottom > 0 && r.right > 0 && r.top < innerHeight && r.left < innerWidth;
  }

  function roleOf(el) {
    const role = el.getAttribute("role");
    if (role) return role;
    const tag = el.tagName.toLowerCase();
    if (tag === "a") return "link";
    if (tag === "select") return "combobox";
    if (tag === "textarea") return "textbox";
    if (tag === "summary") return "button";
    if (tag === "input") {
      const kinds = { checkbox: "checkbox", radio: "radio", submit: "button", button: "button", reset: "button", image: "button", range: "slider", search: "searchbox" };
      return kinds[(el.type || "text").toLowerCase()] || "textbox";
    }
    return tag;
  }

  // sensitive: a field whose value the agent never sees or touches.
  function sensitive(el) {
    if (el.tagName !== "INPUT" && el.tagName !== "TEXTAREA") return false;
    const type = (el.type || "").toLowerCase();
    const ac = (el.getAttribute("autocomplete") || "").toLowerCase();
    return type === "password" || /\bcc-|one-time-code|current-password|new-password/.test(ac);
  }

  function nameOf(el) {
    const img = el.querySelector && el.querySelector("img[alt]");
    const alt = img && !/^(true|false)$/i.test(img.alt) ? img.alt : ""; // some pages write alt="true"
    const label = el.getAttribute("aria-label") || (el.labels && el.labels[0] && el.labels[0].innerText) ||
      el.innerText || el.getAttribute("title") || el.getAttribute("alt") || alt ||
      el.getAttribute("placeholder") || (["submit", "button", "reset"].includes(el.type) ? el.value : "") || "";
    return label.replace(/\s+/g, " ").trim().slice(0, 100);
  }

  function captcha(el) {
    for (let e = el; e; e = e.parentElement) {
      const marks = `${e.id} ${typeof e.className === "string" ? e.className : ""}`.toLowerCase();
      if (/captcha|turnstile/.test(marks)) return true;
      if (e.tagName === "IFRAME" && /captcha|turnstile|challenges/.test(e.src || "")) return true;
    }
    return false;
  }

  function describe(el) {
    const a = el.closest("a[href]");
    const form = el.closest("form");
    const submits = el.tagName === "BUTTON" || (el.tagName === "INPUT" && ["submit", "image"].includes(el.type));
    return {
      tag: el.tagName.toLowerCase(),
      role: roleOf(el),
      name: sensitive(el) ? "(a sensitive field)" : nameOf(el),
      href: a ? a.href : "",
      target: a ? a.target : "",
      formAction: submits && form ? form.action : "",
      captcha: captcha(el),
      sensitive: sensitive(el),
      onScreen: onScreen(el),
      disabled: !!el.disabled,
    };
  }

  // find returns the element a target names: a ref from the last
  // interactive read, or a point in CSS pixels, taken as the interactive
  // element there.
  function find(target) {
    if (target.ref) {
      const el = refs.get(target.ref);
      if (!el || !el.isConnected) {
        throw new Error(`${target.ref} is gone: the page changed since it was read; read it again with mode=interactive`);
      }
      return el;
    }
    const { x, y } = target;
    if (!(x >= 0 && y >= 0 && x <= innerWidth && y <= innerHeight)) throw new Error("the point is outside what the window shows");
    let el = document.elementFromPoint(x, y);
    while (el && el.shadowRoot) {
      const inner = el.shadowRoot.elementFromPoint(x, y);
      if (!inner || inner === el) break;
      el = inner;
    }
    if (!el) throw new Error("nothing is at that point");
    return el.closest(INTERACTIVE) || el;
  }

  // reach scrolls an element into view, if need be, and returns the point
  // to act on.
  async function reach(el, target) {
    if (!target.ref) return { x: target.x, y: target.y };
    if (!onScreen(el)) {
      el.scrollIntoView({ block: "center", inline: "center" });
      await sleep(350);
    }
    const r = el.getBoundingClientRect();
    return { x: r.left + r.width / 2, y: r.top + r.height / 2 };
  }

  function fire(el, type, at) {
    const init = { bubbles: !type.endsWith("enter"), cancelable: true, composed: true, view: window, clientX: at.x, clientY: at.y, button: 0 };
    el.dispatchEvent(type.startsWith("pointer") ? new PointerEvent(type, { ...init, pointerType: "mouse", isPrimary: true }) : new MouseEvent(type, init));
  }

  function valueOf(el) {
    if (!("value" in el) || el.tagName === "BUTTON" || el.tagName === "LI") return undefined;
    return sensitive(el) ? "(hidden)" : String(el.value).slice(0, 100);
  }

  globalThis.__debbie = {
    glow(on) {
      ui().glow.classList.toggle("on", on);
      ui().badge.classList.toggle("on", on);
      if (!on) ui().cursor.classList.remove("on");
    },
    // hidden keeps the overlay out of a screenshot. A capture takes the
    // last frame drawn, so it waits until one without the overlay is (two
    // animation frames), or a moment where the tab draws none.
    hidden(hide) {
      ui().host.style.display = hide ? "none" : "";
      return new Promise((resolve) => {
        setTimeout(resolve, 250);
        requestAnimationFrame(() => requestAnimationFrame(resolve));
      });
    },
    viewport() {
      return { width: innerWidth, height: innerHeight };
    },
    // settled waits until the page stops changing, as pages that fill
    // themselves in after loading do: no change for quiet milliseconds,
    // or at most max. A tab the user isn't looking at is slower: Chrome
    // runs its timers about once a second, so it waits longer.
    settled(quiet = 600, max = 6000) {
      if (document.hidden) {
        quiet = Math.max(quiet, 1500);
        max = Math.max(max, 15000);
      }
      return new Promise((resolve) => {
        const done = () => {
          observer.disconnect();
          clearTimeout(timer);
          clearTimeout(limit);
          resolve();
        };
        let timer = setTimeout(done, quiet);
        const limit = setTimeout(done, max);
        const observer = new MutationObserver(() => {
          clearTimeout(timer);
          timer = setTimeout(done, quiet);
        });
        observer.observe(document.documentElement, { childList: true, subtree: true });
      });
    },
    interactive() {
      refs.clear();
      // Elements the page hides from screen readers are left out: they
      // repeat others, as a video's thumbnail link repeats its title.
      const all = [...document.querySelectorAll(INTERACTIVE)].filter((el) => visible(el) && !el.closest("[aria-hidden=true]"));
      all.sort((a, b) => onScreen(b) - onScreen(a)); // those on screen first; the sort is stable
      const elements = all.slice(0, MAX_ELEMENTS).map((el, i) => {
        const ref = "e" + (i + 1);
        refs.set(ref, el);
        const d = describe(el);
        return { ref, role: d.role, name: d.name, href: d.href, value: valueOf(el), onScreen: d.onScreen, disabled: d.disabled };
      });
      return { elements, total: all.length };
    },
    describe(target) {
      return describe(find(target));
    },
    async click(target) {
      const el = find(target);
      const at = await reach(el, target);
      await moveTo(at.x, at.y);
      ripple(at.x, at.y);
      for (const type of ["pointerover", "pointerenter", "mouseover", "mouseenter", "pointermove", "mousemove", "pointerdown", "mousedown"]) {
        fire(el, type, at);
      }
      if (typeof el.focus === "function") el.focus({ preventScroll: true });
      fire(el, "pointerup", at);
      fire(el, "mouseup", at);
      el.click();
      return describe(el);
    },
    async hover(target) {
      const el = find(target);
      const at = await reach(el, target);
      await moveTo(at.x, at.y);
      for (const type of ["pointerover", "pointerenter", "mouseover", "mouseenter", "pointermove", "mousemove"]) fire(el, type, at);
      return describe(el);
    },
    async scroll(direction) {
      // Instant: a smooth scroll stands still in a tab the user isn't
      // looking at, where nothing is drawn.
      const page = document.documentElement;
      const go = () => {
        if (direction === "top") scrollTo({ top: 0, behavior: "instant" });
        else if (direction === "bottom") scrollTo({ top: page.scrollHeight, behavior: "instant" });
        else scrollBy({ top: (direction === "up" ? -0.8 : 0.8) * innerHeight, behavior: "instant" });
      };
      const before = scrollY;
      go();
      await sleep(400); // for pages that load more as they scroll
      // A page still settling may have put itself back: once more.
      const bottom = page.scrollHeight - innerHeight;
      const stuck = scrollY === before && (direction === "up" || direction === "top" ? before > 0 : before < bottom);
      if (stuck) {
        go();
        await sleep(400);
      }
      const room = Math.max(1, page.scrollHeight - innerHeight);
      return { percent: Math.min(100, Math.round((100 * scrollY) / room)), height: page.scrollHeight };
    },
  };
})();
