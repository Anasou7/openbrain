// OpenBrain dashboard enhancements. Every page works without this file; it
// adds the mobile navigation sheet, rail collapse, the context-rail toggle and
// copy buttons.
(() => {
  const root = document.documentElement;
  const base = root.dataset.base || "";

  document.addEventListener("click", (e) => {
    const el = e.target.closest("[data-rail-toggle], [data-ctx-toggle], [data-sheet-open], [data-sheet-close], [data-copy]");
    if (!el) return;

    if (el.matches("[data-rail-toggle]")) {
      const collapse = root.dataset.rail !== "collapsed";
      if (collapse) root.dataset.rail = "collapsed";
      else delete root.dataset.rail;
      // A plain cookie, so the server renders the next page in the same state.
      document.cookie = `ob_rail=${collapse ? "collapsed" : ""}; path=${base || "/"}; max-age=31536000; samesite=lax`;
      el.setAttribute("aria-label", collapse ? "Expand navigation" : "Collapse navigation");
      el.title = el.getAttribute("aria-label");
    } else if (el.matches("[data-ctx-toggle]")) {
      root.classList.toggle("ctx-open");
    } else if (el.matches("[data-sheet-open]")) {
      document.getElementById("nav-sheet")?.showModal();
    } else if (el.matches("[data-sheet-close]")) {
      el.closest("dialog")?.close();
    } else if (el.matches("[data-copy]")) {
      navigator.clipboard?.writeText(el.dataset.copy).then(() => {
        const label = el.textContent;
        el.textContent = "Copied";
        setTimeout(() => { el.textContent = label; }, 1500);
      });
    }
  });

  // Clicking the scrim (the dialog element itself, outside its content) closes the sheet.
  const sheet = document.getElementById("nav-sheet");
  sheet?.addEventListener("click", (e) => { if (e.target === sheet) sheet.close(); });

  if (root.dataset.rail === "collapsed") {
    const t = document.querySelector("[data-rail-toggle]");
    t?.setAttribute("aria-label", "Expand navigation");
    if (t) t.title = "Expand navigation";
  }
})();
