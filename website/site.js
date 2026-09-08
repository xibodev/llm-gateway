document.documentElement.classList.add("js");
const menuButton = document.querySelector("[data-menu-button]");
const menu = document.querySelector("[data-menu]");

if (menuButton && menu) {
  menuButton.addEventListener("click", () => {
    const open = menuButton.getAttribute("aria-expanded") !== "true";
    menuButton.setAttribute("aria-expanded", String(open));
    menu.toggleAttribute("data-open", open);
  });
  menu.addEventListener("click", (event) => {
    if (event.target.closest("a")) {
      menuButton.setAttribute("aria-expanded", "false");
      menu.removeAttribute("data-open");
    }
  });
  menu.addEventListener("keydown", (event) => {
    if (event.key === "Escape") {
      menuButton.setAttribute("aria-expanded", "false");
      menu.removeAttribute("data-open");
      menuButton.focus();
    }
  });
}

const liveRegion = document.querySelector("[data-live-region]");
for (const button of document.querySelectorAll("[data-copy]")) {
  button.addEventListener("click", async () => {
    const selector = button.getAttribute("data-copy");
    const target = selector ? document.querySelector(selector) : null;
    if (!target) return;
    try {
      await navigator.clipboard.writeText(target.textContent.trim());
      button.textContent = "Copied";
      if (liveRegion) liveRegion.textContent = "Code copied to clipboard.";
      window.setTimeout(() => { button.textContent = "Copy"; }, 1600);
    } catch {
      const range = document.createRange();
      range.selectNodeContents(target);
      const selection = window.getSelection();
      selection.removeAllRanges();
      selection.addRange(range);
      target.focus();
      button.textContent = "Code selected";
      if (liveRegion) liveRegion.textContent = "Clipboard unavailable. Code selected; press Ctrl+C or Command+C to copy.";
      window.setTimeout(() => { button.textContent = "Copy"; }, 2400);
    }
  });
}

const dialog = document.querySelector("#site-search");
const searchInput = dialog?.querySelector("input");
const results = dialog?.querySelector("[data-search-results]");
const count = dialog?.querySelector("[data-search-count]");
let searchIndex;
let loading;
const siteRoot = new URL(".", document.currentScript.src);

async function openSearch() {
  if (!dialog) return;
  if (!dialog.open) dialog.showModal();
  searchInput.focus();
  if (searchIndex) return renderResults(searchInput.value);
  if (loading) return;
  count.textContent = "Loading documentation…";
  results.setAttribute("aria-busy", "true");
  loading = true;
  try {
    const response = await fetch(new URL("search.json", siteRoot));
    if (!response.ok) throw new Error("Search unavailable");
    const index = await response.json();
    if (!Array.isArray(index) || !index.every(item => typeof item.title === "string" && typeof item.url === "string")) throw new Error("Invalid search index");
    searchIndex = index;
    renderResults(searchInput.value);
  } catch {
    count.textContent = "Search could not load. Browse the documentation or close and reopen to retry.";
    const fallback = document.createElement("a");
    fallback.href = new URL("docs.html", siteRoot);
    fallback.textContent = "Browse documentation";
    results.replaceChildren(fallback);
  } finally {
    results.removeAttribute("aria-busy");
    loading = false;
  }
}

function renderResults(query) {
  if (!results || !searchIndex) return;
  const terms = query.toLowerCase().trim().split(/\s+/).filter(Boolean);
  const matches = searchIndex.filter((item) => {
    const haystack = `${item.title} ${item.description} ${item.keywords}`.toLowerCase();
    return terms.every((term) => haystack.includes(term));
  });
  results.replaceChildren(...matches.map((item) => {
    const link = document.createElement("a");
    link.href = new URL(item.url, siteRoot);
    const title = document.createElement("strong");
    title.textContent = item.title;
    const detail = document.createElement("span");
    detail.textContent = item.description;
    link.append(title, detail);
    return link;
  }));
  if (count) count.textContent = matches.length ? `${matches.length} result${matches.length === 1 ? "" : "s"}. Use Tab or arrow keys to navigate.` : "No results. Try a model, client, or task such as backup.";
}

for (const button of document.querySelectorAll("[data-search-open]")) {
  button.addEventListener("click", () => void openSearch());
}
dialog?.querySelector("[data-search-close]")?.addEventListener("click", () => dialog.close());
searchInput?.addEventListener("input", () => renderResults(searchInput.value));
dialog?.addEventListener("keydown", (event) => {
  if (event.key === "Tab") {
    const focusable = [...dialog.querySelectorAll("input, button, a[href]")];
    const first = focusable[0];
    const last = focusable.at(-1);
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
    return;
  }
  if (event.key === "Escape") {
    event.preventDefault();
    dialog.close();
    return;
  }
  if (!["ArrowDown", "ArrowUp"].includes(event.key)) return;
  const links = [...results.querySelectorAll("a")];
  if (!links.length) return;
  event.preventDefault();
  const current = links.indexOf(document.activeElement);
  if (event.key === "ArrowUp" && current === 0) searchInput.focus();
  else links[(current + (event.key === "ArrowDown" ? 1 : -1) + links.length) % links.length].focus();
});
for (const block of document.querySelectorAll("pre, .table-wrap")) {
  block.tabIndex = 0;
  block.setAttribute("role", "region");
  block.setAttribute("aria-label", block.matches("pre") ? "Code example" : "Scrollable reference table");
}
window.addEventListener("keydown", (event) => {
  if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "k") {
    event.preventDefault();
    void openSearch();
  }
});
