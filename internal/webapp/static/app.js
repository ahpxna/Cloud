"use strict";

// Family Photo Cloud web app, modelled on the iPhone Photos app: a timeline
// grouped by day, multi-select, albums in folders, albums shared with the
// family (likes, comments, contributions), Recently Deleted, places, Live
// Photos, lossless video cutting, uploads with cancel and preview, and the
// Shortcut key and two-step sign-in settings.
// No third-party code; the page is served with a script-src 'self' CSP and all
// server-provided text is rendered with textContent, never as HTML.

const CREDENTIAL_KEY = "fpc.credential.v1";
const PREFS_KEY = "fpc.prefs.v1";
const ACTIVITY_SEEN_KEY = "fpc.activity-seen.v1";
const PAGE_SIZE = 60;
const TICKET_MAX_AGE_MS = 8 * 60 * 1000;
const SUBTLE_HASH_LIMIT = 128 * 1024 * 1024;
// Files from older uploads have no Live Photo identifier: a still and a video
// with the same base name uploaded close together are one Live Photo.
const LIVE_PAIR_WINDOW_MS = 10 * 60 * 1000;
// Previews for formats the server cannot decode (HEIC, video) are rendered
// once by the owner's browser, uploaded, and reused by every device.
const THUMBNAIL_MAX_SIDE = 400;
const THUMBNAIL_CONCURRENCY = 2;
const MIN_CLIP_SECONDS = 0.5;
const SHARE_LIMIT_BYTES = 700 * 1024 * 1024;

const $ = (id) => document.getElementById(id);

function el(tag, props = {}, ...children) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(props)) {
    if (value === undefined || value === null || value === false) continue;
    if (key === "class") node.className = value;
    else if (key === "text") node.textContent = value;
    else if (key === "dataset") Object.assign(node.dataset, value);
    else if (key.startsWith("on") && typeof value === "function") node.addEventListener(key.slice(2), value);
    else if (typeof value === "boolean") node.setAttribute(key, "");
    else node.setAttribute(key, value);
  }
  for (const child of children.flat()) {
    if (child !== null && child !== undefined && child !== false) node.append(child);
  }
  return node;
}

// ---------------------------------------------------------------- formatting

const dayFormat = new Intl.DateTimeFormat("vi-VN", { weekday: "long", day: "numeric", month: "long" });
const dayYearFormat = new Intl.DateTimeFormat("vi-VN", { day: "numeric", month: "long", year: "numeric" });
const monthFormat = new Intl.DateTimeFormat("vi-VN", { month: "long", year: "numeric" });
const timeFormat = new Intl.DateTimeFormat("vi-VN", { hour: "2-digit", minute: "2-digit" });
// "30 tháng 9, 2026 lúc 06:00" reads better than Intl's "Lúc 06:00 30 tháng 9".
const fullFormat = { format: (date) => dayYearFormat.format(date) + " lúc " + timeFormat.format(date) };

function formatBytes(bytes) {
  if (!bytes) return "0 KB";
  if (bytes < 1024 * 1024) return Math.max(1, Math.round(bytes / 1024)) + " KB";
  if (bytes < 1024 * 1024 * 1024) return (bytes / 1024 / 1024).toFixed(1).replace(".", ",") + " MB";
  return (bytes / 1024 / 1024 / 1024).toFixed(2).replace(".", ",") + " GB";
}

function formatDuration(ms) {
  const total = Math.max(0, Math.round(ms / 1000));
  const hours = Math.floor(total / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const seconds = String(total % 60).padStart(2, "0");
  return hours ? hours + ":" + String(minutes).padStart(2, "0") + ":" + seconds : minutes + ":" + seconds;
}

function formatClock(seconds) {
  const whole = Math.max(0, seconds);
  const minutes = Math.floor(whole / 60);
  const rest = (whole - minutes * 60).toFixed(1).padStart(4, "0").replace(".", ",");
  return minutes + ":" + rest;
}

function startOfDay(date) {
  return new Date(date.getFullYear(), date.getMonth(), date.getDate());
}

function dayLabel(date) {
  const today = startOfDay(new Date());
  const day = startOfDay(date);
  const diff = Math.round((today - day) / 86400000);
  if (diff === 0) return "Hôm nay";
  if (diff === 1) return "Hôm qua";
  if (day.getFullYear() === today.getFullYear()) return capitalize(dayFormat.format(date));
  return dayYearFormat.format(date);
}

function capitalize(text) {
  return text.charAt(0).toUpperCase() + text.slice(1);
}

// takenDate is when a photo was taken as the camera's clock showed it.
function takenDate(item) {
  return new Date(item.taken_at);
}

function describeTaken(item) {
  if (!item.captured_at) return "Tải lên " + fullFormat.format(new Date(item.created_at));
  return capitalize(fullFormat.format(new Date(item.captured_at)));
}

function placeText(item) {
  if (item.place_name) return item.place_name;
  if (item.latitude !== undefined && item.latitude !== null) return coordinateText(item.latitude, item.longitude);
  return "";
}

function coordinateText(latitude, longitude) {
  return Math.abs(latitude).toFixed(3).replace(".", ",") + (latitude >= 0 ? "° B, " : "° N, ") +
    Math.abs(longitude).toFixed(3).replace(".", ",") + (longitude >= 0 ? "° Đ" : "° T");
}

function mapsLink(latitude, longitude, label) {
  const query = new URLSearchParams({ ll: latitude + "," + longitude, z: "14" });
  if (label) query.set("q", label);
  return "https://maps.apple.com/?" + query;
}

function initials(name) {
  return (name || "?").trim().charAt(0).toUpperCase();
}

function isVideo(item) {
  return item.media_type.startsWith("video/");
}

function plural(count, noun) {
  return count.toLocaleString("vi-VN") + " " + noun;
}

// ---------------------------------------------------------------- preferences

const prefs = Object.assign({ sort: "taken_desc", media: "", cols: 0 }, readJSON(PREFS_KEY) || {});

function readJSON(key) {
  try {
    return JSON.parse(localStorage.getItem(key) || "null");
  } catch {
    return null;
  }
}

function writeJSON(key, value) {
  try {
    localStorage.setItem(key, JSON.stringify(value));
  } catch {
    // Private mode or storage blocked: the preference lasts until reload.
  }
}

function savePrefs() {
  writeJSON(PREFS_KEY, prefs);
}

function gridColumns() {
  if (prefs.cols) return prefs.cols;
  const width = window.innerWidth;
  return width >= 1000 ? 8 : width >= 700 ? 6 : width >= 430 ? 5 : 4;
}

function applyGridColumns() {
  document.documentElement.style.setProperty("--cols", String(gridColumns()));
}

// ---------------------------------------------------------------- credentials

const credentials = {
  load() {
    return readJSON(CREDENTIAL_KEY);
  },
  save(value) {
    writeJSON(CREDENTIAL_KEY, value);
    memoryCredential = value;
  },
  clear() {
    try {
      localStorage.removeItem(CREDENTIAL_KEY);
    } catch {
      // ignore
    }
    memoryCredential = null;
  },
};

let memoryCredential = null;
let refreshInFlight = null;

function currentCredential() {
  return memoryCredential || credentials.load();
}

function storeTokenResponse(body) {
  const now = Date.now();
  credentials.save({
    accessToken: body.access_token,
    accessExpiresAt: now + body.expires_in * 1000,
    refreshToken: body.refresh_token,
    refreshExpiresAt: now + body.refresh_expires_in * 1000,
    pendingRotationID: null,
  });
}

class SignedOut extends Error {}

async function accessToken() {
  let credential = currentCredential();
  if (!credential) throw new SignedOut();
  if (credential.accessExpiresAt > Date.now() + 60000) return credential.accessToken;
  if (credential.refreshExpiresAt <= Date.now()) {
    credentials.clear();
    throw new SignedOut();
  }
  if (!refreshInFlight) {
    refreshInFlight = (async () => {
      // Persist the rotation ID first so a lost response is retried with the
      // same ID instead of looking like refresh-token theft.
      const rotationID = credential.pendingRotationID || crypto.randomUUID();
      credentials.save({ ...credential, pendingRotationID: rotationID });
      const response = await fetch("/v1/auth/refresh", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ refresh_token: credential.refreshToken, rotation_request_id: rotationID }),
      });
      if (response.status === 401) {
        credentials.clear();
        throw new SignedOut();
      }
      if (!response.ok) throw new Error(await problemText(response));
      storeTokenResponse(await response.json());
    })().finally(() => {
      refreshInFlight = null;
    });
  }
  await refreshInFlight;
  credential = currentCredential();
  return credential.accessToken;
}

async function api(path, options = {}) {
  const token = await accessToken();
  const headers = { ...(options.headers || {}), Authorization: "Bearer " + token };
  const response = await fetch(path, { ...options, headers });
  if (response.status === 401) {
    credentials.clear();
    throw new SignedOut();
  }
  return response;
}

// apiJSON sends an optional JSON body and returns the decoded reply (or null
// for 204), throwing a readable error for anything else.
async function apiJSON(path, method = "GET", body) {
  const options = { method };
  if (body !== undefined) {
    options.headers = { "Content-Type": "application/json" };
    options.body = JSON.stringify(body);
  }
  const response = await api(path, options);
  if (!response.ok) throw new Error(await problemText(response));
  if (response.status === 204) return null;
  return response.json();
}

const problemMessages = {
  not_found: "Không tìm thấy (có thể đã bị xoá hoặc không còn được chia sẻ).",
  forbidden: "Chỉ chủ album mới làm được việc này.",
  invalid_request: "Yêu cầu không hợp lệ.",
  upload_already_received: "Đã nhận đủ dữ liệu, đang kiểm tra — không cần dừng nữa.",
  upload_busy: "Chưa dừng kịp, thử lại sau vài giây.",
  upload_not_cancellable: "Lượt tải này không dừng được.",
  clip_unsupported: "Không cắt được định dạng video này. Hãy tải cả video.",
  too_many_downloads: "Đang chuẩn bị quá nhiều lượt tải, thử lại sau một phút.",
};

async function problemText(response) {
  try {
    const body = await response.json();
    return problemMessages[body.code] || body.detail || body.code || "Lỗi " + response.status;
  } catch {
    return "Lỗi " + response.status;
  }
}

function handleError(error, target) {
  if (error instanceof SignedOut) {
    showLogin();
    return;
  }
  const message = navigator.onLine ? error.message || String(error) : "Không có mạng. Thử lại sau.";
  if (target) target.textContent = message;
  else toast(message);
}

// ---------------------------------------------------------------- toast & sheet

let toastTimer = null;

function toast(message) {
  const box = $("toast");
  box.textContent = message;
  box.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => {
    box.hidden = true;
  }, 2600);
}

let sheetOnClose = null;

function openSheet(title, ...content) {
  closeSheet();
  $("sheet-title").textContent = title;
  $("sheet-body").replaceChildren(...content.flat().filter(Boolean));
  $("sheet-backdrop").hidden = false;
  document.body.classList.add("no-scroll");
  return $("sheet-body");
}

function closeSheet() {
  if ($("sheet-backdrop").hidden) return;
  $("sheet-backdrop").hidden = true;
  $("sheet-body").replaceChildren();
  if ($("viewer").hidden) document.body.classList.remove("no-scroll");
  const callback = sheetOnClose;
  sheetOnClose = null;
  if (callback) callback();
}

// menu builds an action list for a sheet: [{label, icon, action, destructive, checked}]
function menu(entries) {
  return el(
    "ul",
    { class: "menu" },
    entries.filter(Boolean).map((entry) =>
      el(
        "li",
        {},
        el(
          "button",
          {
            class: (entry.destructive ? "destructive " : "") + (entry.checked ? "checked" : ""),
            onclick: () => {
              if (!entry.keepOpen) closeSheet();
              entry.action();
            },
          },
          el("span", { text: entry.label }),
          entry.icon && !entry.checked ? el("span", { class: "menu-icon", text: entry.icon }) : null,
        ),
      ),
    ),
  );
}

function askText(title, label, value = "", placeholder = "") {
  return new Promise((resolve) => {
    const input = el("input", { value, placeholder, maxlength: "100" });
    input.value = value;
    let answered = false;
    const finish = (result) => {
      answered = true;
      closeSheet();
      resolve(result);
    };
    const form = el(
      "form",
      { onsubmit: (event) => {
        event.preventDefault();
        if (input.value.trim()) finish(input.value.trim());
      } },
      el("label", { text: label }, input),
      el("button", { type: "submit", class: "primary" }, "Xong"),
    );
    openSheet(title, form);
    sheetOnClose = () => {
      if (!answered) resolve(null);
    };
    setTimeout(() => input.focus(), 50);
  });
}

// ---------------------------------------------------------------- navigation

const tabViews = { library: "library", albums: "albums", shared: "shared", uploads: "uploads", settings: "settings" };
let currentRoute = { name: "library", arg: "" };
let routeTab = "library";
let uploadsTimer = null;
let lastAutoRefresh = 0;
let lastTab = "albums";

function parseHash() {
  const hash = decodeURIComponent(location.hash.slice(1));
  const slash = hash.indexOf("/");
  const name = slash < 0 ? hash : hash.slice(0, slash);
  const arg = slash < 0 ? "" : hash.slice(slash + 1);
  return { name: name || "library", arg };
}

function go(hash) {
  if (location.hash === "#" + hash) route();
  else location.hash = hash;
}

function showView(name) {
  for (const view of document.querySelectorAll(".view")) view.hidden = view.id !== "view-" + name;
  $("tabs").hidden = name === "login";
}

function setTab(tab) {
  routeTab = tab;
  for (const button of document.querySelectorAll(".tab")) {
    if (button.dataset.tab === tab) button.setAttribute("aria-current", "page");
    else button.removeAttribute("aria-current");
  }
}

function showLogin() {
  clearInterval(uploadsTimer);
  endSelection();
  closeViewer();
  closeSheet();
  $("login-form").hidden = false;
  $("mfa-form").hidden = true;
  showView("login");
}

function route() {
  if (!currentCredential()) {
    showLogin();
    return;
  }
  const next = parseHash();
  closeViewer();
  closeSheet();
  if (picking && !(next.name === "library")) cancelPicking();
  endSelection();
  clearInterval(uploadsTimer);
  currentRoute = next;
  lastAutoRefresh = Date.now();
  window.scrollTo(0, 0);
  switch (next.name) {
    case "library":
      setTab("library");
      showView("library");
      library.open();
      break;
    case "albums":
      setTab("albums");
      lastTab = "albums";
      showView("albums");
      loadAlbumsTab();
      break;
    case "shared":
      setTab("shared");
      lastTab = "shared";
      showView("shared");
      loadSharedTab();
      break;
    case "uploads":
      setTab("uploads");
      showView("uploads");
      refreshServerUploads();
      uploadsTimer = setInterval(() => {
        if (!document.hidden) refreshServerUploads();
      }, 5000);
      break;
    case "settings":
      setTab("settings");
      showView("settings");
      loadSettings();
      break;
    case "album":
    case "folder":
    case "smart":
    case "place":
    case "places":
      setTab(lastTab);
      showView("collection");
      openCollectionRoute(next);
      break;
    default:
      go("library");
  }
  checkActivity();
}

// Coming back to the page (from the Shortcut, Photos or another app) refreshes
// what is on screen, without losing the place in a long photo grid.
function refreshActiveTab() {
  if (!currentCredential() || $("view-login").hidden === false) return;
  if (Date.now() - lastAutoRefresh < 1500 || !$("viewer").hidden || !$("sheet-backdrop").hidden || selection.active) return;
  lastAutoRefresh = Date.now();
  const name = currentRoute.name;
  if (name === "uploads") refreshServerUploads();
  else if (name === "settings") loadSettings({ keepOpenWork: true });
  else if (name === "albums") loadAlbumsTab();
  else if (name === "shared") loadSharedTab();
  else if (window.scrollY < 300) route();
  checkActivity();
}

// ---------------------------------------------------------------- collections

function baseName(name) {
  const dot = name.lastIndexOf(".");
  return (dot > 0 ? name.slice(0, dot) : name).toLowerCase();
}

// A Collection is one scrolling grid of items (the library, an album, a smart
// album or a place) with day headers, paging and selection.
class Collection {
  constructor(prefix) {
    this.prefix = prefix;
    this.grid = $(prefix + "-grid");
    this.empty = $(prefix + "-empty");
    this.error = $(prefix + "-error");
    this.more = $(prefix + "-more");
    this.params = {};
    this.options = {};
    this.generation = 0;
    this.reset();
  }

  configure(params, options = {}) {
    this.params = params;
    this.options = options;
    this.reset();
    return this.loadMore();
  }

  reset() {
    this.generation += 1;
    this.items = [];
    this.byID = new Map();
    this.tiles = new Map();
    this.cursor = null;
    this.done = false;
    this.loading = false;
    this.loadedAt = 0;
    this.lastKey = null;
    this.section = null;
    this.grid.replaceChildren();
    this.empty.hidden = true;
    this.error.textContent = "";
    this.more.hidden = true;
  }

  get grouping() {
    const sort = this.params.sort || "";
    if (this.options.grouping) return this.options.grouping;
    return sort === "" || sort === "taken_desc" || sort === "taken_asc" || sort === "added_desc" ? "day" : "none";
  }

  async loadMore() {
    if (this.loading || this.done) return;
    const generation = this.generation;
    this.loading = true;
    this.more.hidden = this.items.length === 0;
    try {
      const query = new URLSearchParams({ ...this.params, limit: String(PAGE_SIZE), tickets: "1" });
      for (const [key, value] of [...query]) if (value === "") query.delete(key);
      if (this.cursor) query.set("cursor", this.cursor);
      const page = await apiJSON("/v1/assets?" + query);
      if (generation !== this.generation) return;
      if (!this.loadedAt) this.loadedAt = Date.now();
      this.cursor = page.next_cursor || null;
      this.done = !this.cursor;
      this.append(page.assets);
      this.empty.hidden = this.items.length > 0;
      if (!this.items.length && this.options.emptyText) this.empty.textContent = this.options.emptyText;
    } catch (error) {
      if (generation === this.generation) handleError(error, this.error);
    } finally {
      if (generation === this.generation) {
        this.loading = false;
        this.more.hidden = true;
        // Keep filling until the page can scroll.
        if (!this.done && document.documentElement.scrollHeight <= window.innerHeight + 400 && this.isVisible()) {
          setTimeout(() => this.loadMore(), 0);
        }
      }
    }
  }

  isVisible() {
    return !this.grid.closest(".view").hidden;
  }

  // Load every remaining page (for "select all"), up to a sane limit.
  async loadAll(limit = 3000) {
    while (!this.done && this.items.length < limit) {
      const before = this.items.length;
      await this.loadMore();
      if (this.items.length === before && !this.loading) break;
    }
  }

  append(newItems) {
    const now = Date.now();
    const stills = new Map();
    for (const item of this.items.concat(newItems)) {
      if (!isVideo(item) && !item.live_photo_id && !item.live_video) {
        const key = baseName(item.original_filename);
        if (!stills.has(key)) stills.set(key, item);
      }
    }
    for (const item of newItems) {
      item.ticketsAt = now;
      // Older uploads without a Live Photo identifier pair by name.
      if (isVideo(item) && !item.live_photo_id && this.grouping !== "none") {
        const still = stills.get(baseName(item.original_filename));
        if (still && !still.live_video && item.mine && still.mine &&
          Math.abs(new Date(still.created_at) - new Date(item.created_at)) <= LIVE_PAIR_WINDOW_MS) {
          still.live_video = { ...item, ticketsAt: now };
          still.pairedByName = true;
          const tile = this.tiles.get(still.id);
          if (tile) tile.replaceWith(this.tileFor(still));
          continue;
        }
      }
      this.items.push(item);
      this.byID.set(item.id, item);
      this.place(item);
    }
    scheduleThumbnailScan();
  }

  dayKeyFor(item) {
    const date = this.params.sort === "added_desc" ? new Date(item.created_at) : takenDate(item);
    return date.getFullYear() + "-" + date.getMonth() + "-" + date.getDate();
  }

  place(item) {
    if (this.grouping === "day") {
      const key = this.dayKeyFor(item);
      if (key !== this.lastKey) {
        this.lastKey = key;
        const date = this.params.sort === "added_desc" ? new Date(item.created_at) : takenDate(item);
        const header = el("div", { class: "day-header", dataset: { key } },
          el("span", {}, el("span", { text: dayLabel(date) }), " ", el("span", { class: "muted", text: placeText(item) })),
          el("button", { class: "select-day", hidden: !selection.active, onclick: () => selectDay(this, key) }, "Chọn"));
        this.section = el("div", { class: "grid", dataset: { key } });
        this.grid.append(header, this.section);
      } else if (!this.section.previousElementSibling.querySelector(".muted").textContent && placeText(item)) {
        this.section.previousElementSibling.querySelector(".muted").textContent = placeText(item);
      }
    } else if (!this.section) {
      this.section = el("div", { class: "grid" });
      this.grid.append(this.section);
    }
    const tile = this.tileFor(item);
    this.section.append(tile);
  }

  tileFor(item) {
    const tile = el("button", { class: "tile", type: "button", "aria-label": item.original_filename, dataset: { id: item.id } });
    const placeholder = el("span", { class: "placeholder", text: isVideo(item) ? "🎬" : "🖼" });
    const image = el("img", { alt: "", decoding: "async", hidden: true });
    image.addEventListener("load", () => {
      image.hidden = false;
      placeholder.hidden = true;
    });
    image.addEventListener("error", () => {
      image.hidden = true;
    });
    tile.append(placeholder, image);
    if (item.thumbnail_url) image.src = item.thumbnail_url;
    else if (item.mine && item.view_url) {
      tile.assetRecord = item;
      pendingThumbnailTiles.add(tile);
    }
    if (item.live_video) tile.append(el("span", { class: "badge-live", text: "◎" }));
    if (isVideo(item)) tile.append(el("span", { class: "duration", text: item.duration_ms ? formatDuration(item.duration_ms) : "▶" }));
    if (item.favorite && item.mine) tile.append(el("span", { class: "heart", text: "♥" }));
    if (!item.mine) tile.append(el("span", { class: "owner", text: initials(item.owner_name) }));
    if (item.purge_at) {
      const days = Math.max(0, Math.ceil((new Date(item.purge_at) - Date.now()) / 86400000));
      tile.append(el("span", { class: "purge-days", text: days === 0 ? "Hôm nay" : "Còn " + days + " ngày" }));
    }
    tile.append(el("span", { class: "check" }));
    if (selection.active && selection.ids.has(item.id)) tile.classList.add("selected");
    tile.addEventListener("click", (event) => onTileClick(this, item, tile, event));
    this.tiles.set(item.id, tile);
    return tile;
  }

  refresh(item) {
    const tile = this.tiles.get(item.id);
    if (tile) tile.replaceWith(this.tileFor(item));
  }

  remove(ids) {
    const gone = new Set(ids);
    this.items = this.items.filter((item) => {
      if (gone.has(item.id) || (item.live_video && gone.has(item.live_video.id))) {
        const tile = this.tiles.get(item.id);
        if (tile) tile.remove();
        this.tiles.delete(item.id);
        this.byID.delete(item.id);
        return false;
      }
      return true;
    });
    for (const section of this.grid.querySelectorAll(".grid")) {
      if (!section.children.length) {
        if (section.previousElementSibling && section.previousElementSibling.classList.contains("day-header")) {
          section.previousElementSibling.remove();
        }
        if (section === this.section) {
          this.section = null;
          this.lastKey = null;
        }
        section.remove();
      }
    }
    this.empty.hidden = this.items.length > 0;
  }
}

window.addEventListener("scroll", () => {
  scheduleThumbnailScan();
  const collection = activeCollection();
  if (collection && collection.isVisible() && !collection.done &&
    window.innerHeight + window.scrollY > document.documentElement.scrollHeight - 1500) {
    collection.loadMore();
  }
}, { passive: true });
window.addEventListener("resize", () => {
  applyGridColumns();
  scheduleThumbnailScan();
});

// ---------------------------------------------------------------- thumbnails

const thumbnailQueue = [];
const pendingThumbnailTiles = new Set();
let thumbnailsActive = 0;
let thumbnailScanScheduled = false;

// Queue tiles near the viewport. A plain geometry check on scroll/resize works
// everywhere, including views where IntersectionObserver is throttled.
function scheduleThumbnailScan() {
  if (thumbnailScanScheduled) return;
  thumbnailScanScheduled = true;
  setTimeout(() => {
    thumbnailScanScheduled = false;
    const bottom = window.innerHeight + 600;
    for (const tile of pendingThumbnailTiles) {
      if (!tile.isConnected) {
        pendingThumbnailTiles.delete(tile);
        continue;
      }
      const rect = tile.getBoundingClientRect();
      if (rect.bottom >= -300 && rect.top <= bottom && rect.width > 0) {
        pendingThumbnailTiles.delete(tile);
        thumbnailQueue.push(tile);
      }
    }
    pumpThumbnails();
  }, 100);
}

function pumpThumbnails() {
  while (thumbnailsActive < THUMBNAIL_CONCURRENCY && thumbnailQueue.length) {
    const tile = thumbnailQueue.shift();
    thumbnailsActive += 1;
    makeThumbnail(tile).finally(() => {
      thumbnailsActive -= 1;
      pumpThumbnails();
    });
  }
}

async function makeThumbnail(tile) {
  const asset = tile.assetRecord;
  if (!asset || !tile.isConnected) return;
  let source = null;
  try {
    source = isVideo(asset) ? await videoFrame(asset.view_url, 0.5) : await decodedImage(asset.view_url);
    const blob = await canvasBlob(source, THUMBNAIL_MAX_SIDE, 0.8);
    const image = tile.querySelector("img");
    image.src = URL.createObjectURL(blob);
    const token = await accessToken();
    await fetch("/v1/assets/" + encodeURIComponent(asset.id) + "/thumbnail", {
      method: "PUT",
      headers: { Authorization: "Bearer " + token, "Content-Type": "image/jpeg" },
      body: blob,
    });
  } catch {
    // This browser cannot decode the format (for example HEIC outside Safari);
    // keep the placeholder and let another device create the preview.
  } finally {
    releaseMedia(source);
  }
}

function releaseMedia(source) {
  if (source instanceof HTMLVideoElement) {
    source.removeAttribute("src");
    source.load();
  } else if (source) {
    source.src = "";
  }
}

function decodedImage(url) {
  const image = new Image();
  image.decoding = "async";
  image.src = url;
  return image.decode().then(() => image);
}

function videoFrame(url, at, existing) {
  return new Promise((resolve, reject) => {
    const video = existing || document.createElement("video");
    const timer = setTimeout(() => reject(new Error("video frame timeout")), 20000);
    const seek = () => {
      video.currentTime = Math.min(at, Math.max(0, (video.duration || 1) - 0.05));
    };
    video.muted = true;
    video.playsInline = true;
    video.preload = "auto";
    video.addEventListener("error", () => {
      clearTimeout(timer);
      reject(new Error("video decode failed"));
    }, { once: true });
    video.addEventListener("seeked", () => {
      clearTimeout(timer);
      resolve(video);
    }, { once: true });
    if (existing && video.readyState >= 1) seek();
    else {
      video.addEventListener("loadeddata", seek, { once: true });
      video.src = url;
    }
  });
}

function drawScaled(source, maxSide) {
  const width = source.naturalWidth || source.videoWidth;
  const height = source.naturalHeight || source.videoHeight;
  if (!width || !height) throw new Error("no dimensions");
  const scale = Math.min(1, maxSide / Math.max(width, height));
  const canvas = document.createElement("canvas");
  canvas.width = Math.max(1, Math.round(width * scale));
  canvas.height = Math.max(1, Math.round(height * scale));
  canvas.getContext("2d").drawImage(source, 0, 0, canvas.width, canvas.height);
  return canvas;
}

function canvasBlob(source, maxSide, quality) {
  const canvas = drawScaled(source, maxSide);
  return new Promise((resolve, reject) =>
    canvas.toBlob((blob) => (blob ? resolve(blob) : reject(new Error("encode failed"))), "image/jpeg", quality),
  );
}

// ---------------------------------------------------------------- selection

const selection = {
  active: false,
  collection: null,
  ids: new Set(),
  anchor: null,
};

function activeCollection() {
  if (!$("view-library").hidden) return library.collection;
  if (!$("view-collection").hidden) return collectionView.collection;
  return null;
}

function startSelection(collection) {
  selection.active = true;
  selection.collection = collection;
  selection.ids.clear();
  selection.anchor = null;
  collection.grid.classList.add("selecting");
  for (const button of collection.grid.querySelectorAll(".select-day")) button.hidden = false;
  $("tabs").hidden = true;
  $("selectbar").hidden = false;
  $("library-select").textContent = "Huỷ";
  $("collection-select").textContent = "Huỷ";
  updateSelectionBar();
}

function endSelection() {
  if (!selection.active) return;
  const collection = selection.collection;
  selection.active = false;
  selection.ids.clear();
  if (collection) {
    collection.grid.classList.remove("selecting");
    for (const tile of collection.grid.querySelectorAll(".tile.selected")) tile.classList.remove("selected");
    for (const button of collection.grid.querySelectorAll(".select-day")) button.hidden = true;
  }
  selection.collection = null;
  $("selectbar").hidden = true;
  $("tabs").hidden = $("view-login").hidden === false;
  $("library-select").textContent = "Chọn";
  $("collection-select").textContent = "Chọn";
}

function setSelected(id, value) {
  const tile = selection.collection && selection.collection.tiles.get(id);
  if (value) selection.ids.add(id);
  else selection.ids.delete(id);
  if (tile) tile.classList.toggle("selected", value);
}

function onTileClick(collection, item, tile, event) {
  if (tile.suppressClick) {
    tile.suppressClick = false;
    return;
  }
  if (!selection.active) {
    openViewer(collection, collection.items.indexOf(item));
    return;
  }
  const index = collection.items.indexOf(item);
  if (event.shiftKey && selection.anchor !== null) {
    const [from, to] = [Math.min(selection.anchor, index), Math.max(selection.anchor, index)];
    for (let i = from; i <= to; i++) setSelected(collection.items[i].id, true);
  } else {
    setSelected(item.id, !selection.ids.has(item.id));
    selection.anchor = index;
  }
  updateSelectionBar();
}

function selectDay(collection, key) {
  const ids = collection.items.filter((item) => collection.dayKeyFor(item) === key).map((item) => item.id);
  const all = ids.every((id) => selection.ids.has(id));
  for (const id of ids) setSelected(id, !all);
  updateSelectionBar();
}

// Dragging a finger sideways across tiles selects a run of them, like Photos.
function enableDragSelect(collection) {
  let drag = null;
  let scrollTimer = null;
  const tileIndexAt = (x, y) => {
    const target = document.elementFromPoint(x, y);
    const tile = target && target.closest(".tile");
    if (!tile || !collection.grid.contains(tile)) return -1;
    const item = collection.byID.get(tile.dataset.id);
    return item ? collection.items.indexOf(item) : -1;
  };
  const apply = (index) => {
    if (index < 0 || !drag) return;
    const [from, to] = [Math.min(drag.start, index), Math.max(drag.start, index)];
    collection.items.forEach((item, i) => {
      const inRange = i >= from && i <= to;
      setSelected(item.id, inRange ? drag.value : drag.base.has(item.id));
    });
    updateSelectionBar();
  };
  collection.grid.addEventListener("pointerdown", (event) => {
    if (!selection.active || selection.collection !== collection || event.button > 0) return;
    const index = tileIndexAt(event.clientX, event.clientY);
    if (index < 0) return;
    const id = collection.items[index].id;
    drag = { start: index, x: event.clientX, y: event.clientY, moved: false, value: !selection.ids.has(id), base: new Set(selection.ids), pointer: event.pointerId, lastY: event.clientY };
  });
  collection.grid.addEventListener("pointermove", (event) => {
    if (!drag || event.pointerId !== drag.pointer) return;
    const dx = event.clientX - drag.x;
    const dy = event.clientY - drag.y;
    if (!drag.moved) {
      if (Math.abs(dx) > 10 && Math.abs(dx) > Math.abs(dy)) {
        drag.moved = true;
        try {
          collection.grid.setPointerCapture(event.pointerId);
        } catch {
          // ignore
        }
      } else if (Math.abs(dy) > 12) {
        drag = null;
        return;
      } else return;
    }
    event.preventDefault();
    drag.lastY = event.clientY;
    drag.lastX = event.clientX;
    apply(tileIndexAt(event.clientX, event.clientY));
    clearInterval(scrollTimer);
    const edge = 70;
    if (event.clientY < edge || event.clientY > window.innerHeight - edge - 90) {
      const step = event.clientY < edge ? -14 : 14;
      scrollTimer = setInterval(() => {
        if (!drag) return clearInterval(scrollTimer);
        window.scrollBy(0, step);
        apply(tileIndexAt(drag.lastX, drag.lastY));
      }, 30);
    }
  });
  const finish = (event) => {
    clearInterval(scrollTimer);
    if (drag && drag.moved && event) {
      const tile = event.target && event.target.closest && event.target.closest(".tile");
      if (tile) tile.suppressClick = true;
      for (const t of collection.grid.querySelectorAll(".tile")) t.suppressClick = true;
      setTimeout(() => {
        for (const t of collection.grid.querySelectorAll(".tile")) t.suppressClick = false;
      }, 50);
    }
    drag = null;
  };
  collection.grid.addEventListener("pointerup", finish);
  collection.grid.addEventListener("pointercancel", () => {
    clearInterval(scrollTimer);
    drag = null;
  });
}

function selectedItems() {
  const collection = selection.collection;
  if (!collection) return [];
  return [...selection.ids].map((id) => collection.byID.get(id)).filter(Boolean);
}

// The other half of each selected Live Photo travels with it.
function withLiveVideos(items) {
  const ids = [];
  for (const item of items) {
    ids.push(item.id);
    if (item.live_video) ids.push(item.live_video.id);
  }
  return ids;
}

function updateSelectionBar() {
  const count = selection.ids.size;
  $("select-count").textContent = picking
    ? count ? "Thêm " + count + " mục" : "Chọn ảnh để thêm"
    : count ? "Đã chọn " + count + " mục" : "Chọn mục";
  const collection = selection.collection;
  const all = collection && collection.items.length > 0 && collection.done && count >= collection.items.length;
  $("select-all").textContent = all ? "Bỏ chọn" : "Chọn tất cả";
  $("select-actions").replaceChildren(...selectionActions(count));
}

function actionButton(icon, label, handler, options = {}) {
  return el("button", { class: "action" + (options.danger ? " danger-action" : "") + (options.on ? " on" : ""), disabled: options.disabled, onclick: handler, "aria-label": label },
    el("span", { class: "action-icon", text: icon }), el("span", { text: label }));
}

function selectionActions(count) {
  const none = count === 0;
  if (picking) {
    return [actionButton("＋", "Thêm vào album", () => finishPicking(), { disabled: none })];
  }
  const context = selection.collection ? selection.collection.options.context || {} : {};
  const items = selectedItems();
  const mine = items.filter((item) => item.mine);
  if (context.view === "trash") {
    return [
      actionButton("↺", "Khôi phục", () => batchAction("restore", items), { disabled: none }),
      actionButton("🗑", "Xoá vĩnh viễn", () => purgeItems(items), { disabled: none, danger: true }),
    ];
  }
  const allFavorite = mine.length > 0 && mine.every((item) => item.favorite);
  const actions = [
    actionButton("⤴︎", "Chia sẻ", () => shareItems(items), { disabled: none }),
    actionButton(allFavorite ? "♡" : "♥", allFavorite ? "Bỏ thích" : "Yêu thích", () => batchAction(allFavorite ? "unfavorite" : "favorite", mine), { disabled: !mine.length }),
    actionButton("＋", "Album", () => openAlbumPicker(mine), { disabled: !mine.length }),
  ];
  if (context.view === "hidden") {
    actions.push(actionButton("👁", "Bỏ ẩn", () => batchAction("unhide", mine), { disabled: !mine.length }));
  } else if (!context.album) {
    actions.push(actionButton("⊘", "Ẩn", () => batchAction("hide", mine), { disabled: !mine.length }));
  }
  if (context.album) {
    const removable = context.album.is_owner ? items : mine;
    actions.push(actionButton("⊖", "Bỏ khỏi album", () => removeFromAlbum(context.album, removable), { disabled: !removable.length }));
  }
  actions.push(actionButton("🗑", "Xoá", () => trashItems(mine), { disabled: !mine.length, danger: true }));
  return actions;
}

async function batchAction(action, items) {
  if (!items.length) return;
  try {
    await apiJSON("/v1/assets/batch", "POST", { ids: withLiveVideos(items), action });
    const collection = selection.collection;
    const view = collection.options.context && collection.options.context.view;
    const leaves = { favorite: false, unfavorite: view === "favorites", hide: true, unhide: true, restore: true }[action];
    for (const item of items) {
      if (action === "favorite") item.favorite = true;
      if (action === "unfavorite") item.favorite = false;
      if (!leaves) collection.refresh(item);
    }
    if (leaves) collection.remove(items.map((item) => item.id));
    toast({ favorite: "Đã thêm vào Yêu thích", unfavorite: "Đã bỏ khỏi Yêu thích", hide: "Đã ẩn " + items.length + " mục", unhide: "Đã bỏ ẩn", restore: "Đã khôi phục " + items.length + " mục" }[action]);
    endSelection();
  } catch (error) {
    handleError(error);
  }
}

async function trashItems(items) {
  if (!items.length) return;
  if (!confirm("Xoá " + plural(items.length, "mục") + "? Các mục sẽ nằm trong \"Đã xoá gần đây\" 30 ngày rồi mới bị xoá vĩnh viễn.")) return;
  try {
    await apiJSON("/v1/assets/batch", "POST", { ids: withLiveVideos(items), action: "trash" });
    selection.collection.remove(items.map((item) => item.id));
    toast("Đã chuyển " + plural(items.length, "mục") + " vào Đã xoá gần đây");
    endSelection();
  } catch (error) {
    handleError(error);
  }
}

async function purgeItems(items) {
  if (!items.length) return;
  if (!confirm("Xoá vĩnh viễn " + plural(items.length, "mục") + "? Không thể hoàn tác.")) return;
  try {
    await apiJSON("/v1/assets/batch", "POST", { ids: withLiveVideos(items), action: "purge" });
    selection.collection.remove(items.map((item) => item.id));
    toast("Đã xoá vĩnh viễn");
    endSelection();
  } catch (error) {
    handleError(error);
  }
}

async function removeFromAlbum(album, items) {
  if (!items.length) return;
  try {
    await apiJSON("/v1/albums/" + album.id + "/assets/remove", "POST", { ids: withLiveVideos(items) });
    selection.collection.remove(items.map((item) => item.id));
    toast("Đã bỏ " + plural(items.length, "mục") + " khỏi album (ảnh vẫn còn trong Thư viện)");
    endSelection();
  } catch (error) {
    handleError(error);
  }
}

// ---------------------------------------------------------------- library

const sortLabels = {
  taken_desc: "Ngày chụp — mới nhất",
  taken_asc: "Ngày chụp — cũ nhất",
  added_desc: "Ngày tải lên — mới nhất",
  size_desc: "Dung lượng — lớn nhất",
  name_asc: "Tên file (A–Z)",
  favorites_first: "Yêu thích lên đầu",
};

const library = {
  collection: new Collection("library"),
  search: "",

  open() {
    applyGridColumns();
    const params = { sort: prefs.sort, media: prefs.media, q: this.search };
    this.collection.configure(params, { context: { view: "library" }, emptyText: this.search ? "Không tìm thấy ảnh nào." : $("library-empty").textContent });
    this.updateNote();
    this.updateCount();
    if (picking) startPickingSelection();
  },

  updateNote() {
    const parts = [];
    if (prefs.sort !== "taken_desc") parts.push(sortLabels[prefs.sort]);
    if (prefs.media) parts.push(prefs.media === "photo" ? "Chỉ ảnh" : "Chỉ video");
    if (this.search) parts.push("Tìm: \"" + this.search + "\"");
    const note = $("library-filter-note");
    note.hidden = parts.length === 0;
    note.replaceChildren(el("span", { text: parts.join(" · ") }), el("button", { class: "text-button", onclick: () => {
      prefs.sort = "taken_desc";
      prefs.media = "";
      savePrefs();
      this.search = "";
      $("library-search").value = "";
      $("library-search-bar").hidden = true;
      this.open();
    } }, "Bỏ lọc"));
  },

  async updateCount() {
    try {
      const summary = await apiJSON("/v1/library/summary");
      const usage = summary.usage;
      $("library-count").textContent = plural(usage.photo_count, "ảnh") + ", " + plural(usage.video_count, "video");
      librarySummary = summary;
    } catch {
      $("library-count").textContent = "";
    }
  },

  openSortSheet() {
    const sorts = Object.keys(sortLabels).map((sort) => ({
      label: sortLabels[sort], checked: prefs.sort === sort, action: () => {
        prefs.sort = sort;
        savePrefs();
        this.open();
      },
    }));
    const filters = [["", "Tất cả"], ["photo", "Chỉ ảnh"], ["video", "Chỉ video"]].map(([value, label]) => ({
      label, checked: prefs.media === value, action: () => {
        prefs.media = value;
        savePrefs();
        this.open();
      },
    }));
    const sizes = [[0, "Tự động"], [3, "To"], [5, "Vừa"], [7, "Nhỏ"]].map(([value, label]) => ({
      label, checked: prefs.cols === value, action: () => {
        prefs.cols = value;
        savePrefs();
        applyGridColumns();
        scheduleThumbnailScan();
      },
    }));
    openSheet("Sắp xếp & lọc",
      el("h3", { class: "section-title", text: "Sắp xếp theo" }), menu(sorts),
      el("h3", { class: "section-title", text: "Hiển thị" }), menu(filters),
      el("h3", { class: "section-title", text: "Cỡ ô ảnh" }), menu(sizes),
      el("p", { class: "muted small", text: "Muốn xem theo vị trí? Mở tab Album → Địa điểm." }));
  },
};

let librarySummary = null;
let searchTimer = null;

// ---------------------------------------------------------------- picking

// Adding photos to an album borrows the library grid in selection mode.
let picking = null;

function startPicking(album) {
  picking = { album };
  go("library");
}

function startPickingSelection() {
  startSelection(library.collection);
  $("library-select").hidden = true;
  $("library-count").textContent = "Thêm vào \"" + picking.album.name + "\"";
}

function cancelPicking() {
  const album = picking && picking.album;
  picking = null;
  $("library-select").hidden = false;
  return album;
}

async function finishPicking() {
  const album = picking.album;
  const items = selectedItems();
  try {
    const result = await apiJSON("/v1/albums/" + album.id + "/assets", "POST", { ids: withLiveVideos(items) });
    toast("Đã thêm " + plural(items.length, "mục") + " vào \"" + album.name + "\"" + (result.changed === 0 ? " (đã có sẵn)" : ""));
  } catch (error) {
    handleError(error);
    return;
  }
  cancelPicking();
  endSelection();
  go("album/" + album.id);
}

// ---------------------------------------------------------------- albums tab

const smartAlbums = {
  videos: { title: "Video", icon: "▶︎", group: "media" },
  live: { title: "Live Photo", icon: "◎", group: "media" },
  selfies: { title: "Ảnh selfie", icon: "☺︎", group: "media" },
  screenshots: { title: "Ảnh màn hình", icon: "▣", group: "media" },
  panoramas: { title: "Ảnh toàn cảnh", icon: "▭", group: "media" },
  favorites: { title: "Yêu thích", icon: "♥", group: "utility" },
  recent: { title: "Mới thêm gần đây", icon: "⇪", group: "utility" },
  places: { title: "Địa điểm", icon: "⌖", group: "utility" },
  hidden: { title: "Đã ẩn", icon: "⊘", group: "utility" },
  trash: { title: "Đã xoá gần đây", icon: "🗑", group: "utility" },
};

let albumsCache = { albums: [], folders: [] };

async function fetchAlbums() {
  albumsCache = await apiJSON("/v1/albums");
  return albumsCache;
}

function coverNode(url, fallback) {
  const cover = el("span", { class: "album-cover", text: fallback });
  if (url) {
    const image = el("img", { alt: "", src: url });
    image.addEventListener("error", () => image.remove());
    cover.append(image);
  }
  return cover;
}

function albumCard(album) {
  const cover = coverNode(album.cover_url, "❏");
  if (album.shared) cover.append(el("span", { class: "shared-mark", text: album.is_owner ? "Đã chia sẻ" : album.owner_name }));
  return el("button", { class: "album-card", onclick: () => go("album/" + album.id) },
    cover,
    el("span", { class: "album-name", text: album.name }),
    el("span", { class: "album-count", text: album.count.toLocaleString("vi-VN") }));
}

function folderCard(folder) {
  const albums = albumsCache.albums.filter((album) => album.folder_id === folder.id).length;
  const folders = albumsCache.folders.filter((child) => child.parent_id === folder.id).length;
  return el("button", { class: "album-card", onclick: () => go("folder/" + folder.id) },
    el("span", { class: "album-cover", text: "📁" }),
    el("span", { class: "album-name", text: folder.name }),
    el("span", { class: "album-count", text: [albums ? albums + " album" : "", folders ? folders + " thư mục" : ""].filter(Boolean).join(", ") || "Trống" }));
}

function placeCard(place) {
  return el("button", { class: "album-card", onclick: () => go("place/" + place.cell) },
    coverNode(place.cover_url, "⌖"),
    el("span", { class: "album-name", text: place.name || coordinateText(place.latitude, place.longitude) }),
    el("span", { class: "album-count", text: place.count.toLocaleString("vi-VN") }));
}

function smartRow(key, counts) {
  const smart = smartAlbums[key];
  const count = counts && counts[key] !== undefined ? counts[key] : "";
  return el("li", {}, el("button", { class: "row-button", onclick: () => go(key === "places" ? "places" : "smart/" + key) },
    el("span", { class: "row-icon", text: smart.icon }),
    el("span", { class: "row-label", text: smart.title }),
    el("span", { class: "row-count", text: count === "" ? "" : Number(count).toLocaleString("vi-VN") }),
    el("span", { class: "row-chevron", text: "›" })));
}

async function loadAlbumsTab() {
  $("albums-error").textContent = "";
  try {
    const [albums, summary] = await Promise.all([fetchAlbums(), apiJSON("/v1/library/summary")]);
    librarySummary = summary;
    const mine = albums.albums.filter((album) => album.is_owner && !album.folder_id);
    const folders = albums.folders.filter((folder) => !folder.parent_id);
    const cards = [...folders.map(folderCard), ...mine.map(albumCard)];
    $("albums-mine").replaceChildren(...(cards.length ? cards : [el("p", { class: "muted small", text: "Chưa có album. Bấm ＋ để tạo." })]));
    $("albums-media").replaceChildren(...["videos", "live", "selfies", "screenshots", "panoramas"].map((key) => smartRow(key, summary.counts)));
    $("albums-utilities").replaceChildren(...["favorites", "recent", "places", "hidden", "trash"].map((key) => smartRow(key, summary.counts)));
    loadMemories();
    loadPlacesPreview();
  } catch (error) {
    handleError(error, $("albums-error"));
  }
}

async function loadMemories() {
  try {
    const now = new Date();
    const query = new URLSearchParams({ view: "on_this_day", month: String(now.getMonth() + 1), day: String(now.getDate()), tzoffset: String(-now.getTimezoneOffset()), limit: "12", tickets: "1" });
    const page = await apiJSON("/v1/assets?" + query);
    const withCover = page.assets.find((item) => item.thumbnail_url) || page.assets[0];
    $("albums-memories-section").hidden = page.assets.length === 0;
    if (!page.assets.length) return;
    const years = [...new Set(page.assets.map((item) => takenDate(item).getFullYear()))];
    const card = el("button", { class: "memory-card", onclick: () => go("smart/on_this_day") },
      withCover && withCover.thumbnail_url ? el("img", { alt: "", src: withCover.thumbnail_url }) : null,
      el("span", { text: "Ngày này năm xưa · " + years.join(", ") }));
    $("albums-memories").replaceChildren(card);
  } catch {
    $("albums-memories-section").hidden = true;
  }
}

async function loadPlacesPreview() {
  try {
    const { places } = await apiJSON("/v1/places");
    $("albums-places-section").hidden = places.length === 0;
    $("albums-places").replaceChildren(...places.slice(0, 4).map(placeCard));
  } catch {
    $("albums-places-section").hidden = true;
  }
}

function openNewMenu(folderID = "") {
  openSheet("Tạo mới", menu([
    { label: "Album mới", icon: "❏", action: () => createAlbum({ folderID }) },
    { label: "Thư mục mới", icon: "📁", action: () => createFolder(folderID) },
    { label: "Album chia sẻ mới", icon: "♡", action: () => createSharedAlbum() },
  ]));
}

async function createAlbum({ folderID = "", assetIDs = [], open = true } = {}) {
  const name = await askText("Album mới", "Tên album", "", "Ví dụ: Tết 2026");
  if (!name) return null;
  try {
    const album = await apiJSON("/v1/albums", "POST", { name, folder_id: folderID, asset_ids: assetIDs });
    if (open) go("album/" + album.id);
    return album;
  } catch (error) {
    handleError(error);
    return null;
  }
}

async function createFolder(parentID = "") {
  const name = await askText("Thư mục mới", "Tên thư mục", "", "Ví dụ: Du lịch");
  if (!name) return;
  try {
    await apiJSON("/v1/album-folders", "POST", { name, parent_id: parentID });
    if (parentID) openCollectionRoute({ name: "folder", arg: parentID });
    else loadAlbumsTab();
  } catch (error) {
    handleError(error);
  }
}

// ---------------------------------------------------------------- shared tab

async function loadSharedTab() {
  $("shared-error").textContent = "";
  try {
    const [albums, feed] = await Promise.all([fetchAlbums(), apiJSON("/v1/activity")]);
    const shared = albums.albums.filter((album) => album.shared);
    shared.sort((a, b) => new Date(b.latest_added_at || b.updated_at) - new Date(a.latest_added_at || a.updated_at));
    $("shared-albums").replaceChildren(...shared.map(albumCard));
    $("shared-empty").hidden = shared.length > 0;
    renderActivity(feed.activity);
    if (feed.activity.length) writeJSON(ACTIVITY_SEEN_KEY, feed.activity[0].at);
    $("shared-badge").hidden = true;
  } catch (error) {
    handleError(error, $("shared-error"));
  }
}

function renderActivity(entries) {
  const rows = entries.map((entry) => {
    const text = entry.kind === "comment"
      ? [el("b", { text: entry.actor_name }), " bình luận trong ", el("b", { text: entry.album_name }), ": “" + entry.body + "”"]
      : [el("b", { text: entry.actor_name }), " đã thêm " + plural(entry.count, "mục") + " vào ", el("b", { text: entry.album_name })];
    const thumbs = el("div", { class: "activity-thumbs" }, (entry.thumbnail_urls || []).map((url) => {
      const image = el("img", { alt: "", src: url });
      image.addEventListener("error", () => image.remove());
      return image;
    }));
    return el("li", { onclick: () => go("album/" + entry.album_id) },
      el("span", { class: "avatar", text: initials(entry.actor_name) }),
      el("div", { class: "activity-text" }, ...text, el("time", { text: capitalize(fullFormat.format(new Date(entry.at))) }), thumbs));
  });
  $("shared-activity").replaceChildren(...(rows.length ? rows : [el("li", { class: "muted small", text: "Chưa có hoạt động nào từ người trong nhà." })]));
}

let activityCheckedAt = 0;

async function checkActivity() {
  if (Date.now() - activityCheckedAt < 60000 || !currentCredential()) return;
  activityCheckedAt = Date.now();
  try {
    const feed = await apiJSON("/v1/activity");
    const seen = readJSON(ACTIVITY_SEEN_KEY);
    $("shared-badge").hidden = !feed.activity.length || (seen && new Date(feed.activity[0].at) <= new Date(seen)) || currentRoute.name === "shared";
  } catch {
    // The badge is a hint; ignore failures.
  }
}

let peopleCache = null;

async function fetchPeople() {
  if (!peopleCache) peopleCache = (await apiJSON("/v1/people")).people;
  return peopleCache;
}

async function createSharedAlbum() {
  let people;
  try {
    people = await fetchPeople();
  } catch (error) {
    handleError(error);
    return;
  }
  const others = people.filter((person) => !person.me);
  const name = el("input", { placeholder: "Ví dụ: Ảnh của bé", maxlength: "100" });
  const canAdd = el("input", { type: "checkbox", checked: true });
  const choices = others.map((person) => ({ person, box: el("input", { type: "checkbox" }) }));
  const error = el("p", { class: "error", role: "alert" });
  openSheet("Album chia sẻ mới",
    el("label", { text: "Tên album" }, name),
    others.length ? el("p", { class: "muted small", text: "Chia sẻ với:" }) : el("p", { class: "muted", text: "Chưa có tài khoản nào khác trên máy chủ. Nhờ người quản trị tạo tài khoản cho người trong nhà." }),
    el("ul", { class: "picker-list" }, choices.map(({ person, box }) => el("li", {}, el("label", {}, box,
      el("span", { class: "avatar", text: initials(person.display_name) }),
      el("span", { class: "picker-text" }, person.display_name, el("small", { text: person.email })))))),
    el("label", { class: "switch-row" }, el("span", { text: "Cho phép người được mời thêm ảnh" }), canAdd),
    el("button", { class: "primary", onclick: async () => {
      if (!name.value.trim()) {
        error.textContent = "Hãy đặt tên album.";
        return;
      }
      try {
        const album = await apiJSON("/v1/albums", "POST", {
          name: name.value.trim(),
          member_ids: choices.filter(({ box }) => box.checked).map(({ person }) => person.id),
        });
        if (!canAdd.checked) await apiJSON("/v1/albums/" + album.id, "PATCH", { members_can_add: false });
        closeSheet();
        go("album/" + album.id);
      } catch (failure) {
        handleError(failure, error);
      }
    } }, "Tạo album"),
    error);
  setTimeout(() => name.focus(), 50);
}

// ---------------------------------------------------------------- album, folder, smart and place views

const collectionView = {
  collection: new Collection("collection"),
  route: null,
  album: null,
  folder: null,
  place: null,
};

function collectionHeader({ title, subtitle = "", back = "‹ Album", add = false, select = true, more = null, banner = null }) {
  $("collection-title").textContent = title;
  $("collection-subtitle").textContent = subtitle;
  $("collection-back").textContent = back;
  $("collection-add").hidden = !add;
  $("collection-select").hidden = !select;
  $("collection-menu").hidden = !more;
  $("collection-menu").onclick = more;
  const bannerBox = $("collection-banner");
  bannerBox.hidden = !banner;
  bannerBox.replaceChildren(...(banner ? [banner].flat() : []));
}

async function openCollectionRoute({ name, arg }) {
  const view = collectionView;
  view.route = { name, arg };
  view.album = null;
  view.folder = null;
  view.place = null;
  $("collection-folders").replaceChildren();
  $("collection-error").textContent = "";
  const collection = view.collection;
  applyGridColumns();
  $("collection-back").onclick = () => {
    if (name === "place") go("places");
    else if (name === "folder" || name === "album") {
      const parent = (view.folder && view.folder.parent_id) || (view.album && view.album.folder_id);
      go(parent ? "folder/" + parent : lastTab);
    } else go(lastTab);
  };
  try {
    if (name === "album") {
      const album = await apiJSON("/v1/albums/" + encodeURIComponent(arg));
      view.album = album;
      renderAlbumHeader(album);
      collection.configure({ album: album.id }, { context: { album }, grouping: album.sort_order === "added" ? "none" : "day", emptyText: album.can_add ? "Album trống. Bấm ＋ để thêm ảnh." : "Album trống." });
    } else if (name === "folder") {
      if (!albumsCache.folders.length) await fetchAlbums();
      const folder = albumsCache.folders.find((candidate) => candidate.id === arg);
      if (!folder) throw new Error("Không tìm thấy thư mục.");
      view.folder = folder;
      renderFolder(folder);
      collection.reset();
    } else if (name === "places") {
      collectionHeader({ title: "Địa điểm", subtitle: "Ảnh có vị trí GPS, gom theo khu vực khoảng 5 km.", select: false });
      collection.reset();
      const { places } = await apiJSON("/v1/places");
      $("collection-folders").replaceChildren(...places.map(placeCard));
      collection.empty.hidden = places.length > 0;
      collection.empty.textContent = "Chưa có ảnh nào có vị trí. Ảnh gửi qua nút Chia sẻ thường bị iPhone bỏ vị trí; ảnh gốc từ máy tính hoặc app vẫn giữ.";
    } else if (name === "place") {
      const { places } = await apiJSON("/v1/places");
      const place = places.find((candidate) => candidate.cell === arg) || { cell: arg, name: "", latitude: Number(arg.split(",")[0]), longitude: Number(arg.split(",")[1]), count: 0 };
      view.place = place;
      collectionHeader({
        title: place.name || coordinateText(place.latitude, place.longitude),
        subtitle: plural(place.count, "mục"),
        back: "‹ Địa điểm",
        more: () => openSheet("Địa điểm", menu([
          { label: place.name ? "Đổi tên địa điểm" : "Đặt tên địa điểm", icon: "✎", action: () => renamePlace(place) },
          place.name ? { label: "Xoá tên", icon: "⌫", action: () => labelPlace(place, "") } : null,
          { label: "Mở trong Bản đồ", icon: "⌖", action: () => window.open(mapsLink(place.latitude, place.longitude, place.name), "_blank", "noopener") },
        ])),
      });
      collection.configure({ place: place.cell }, { context: { view: "place" } });
    } else if (name === "smart") {
      const smart = smartAlbums[arg] || (arg === "on_this_day" ? { title: "Ngày này năm xưa" } : null);
      if (!smart) throw new Error("Không có album này.");
      const params = { view: arg };
      if (arg === "on_this_day") {
        const now = new Date();
        Object.assign(params, { month: String(now.getMonth() + 1), day: String(now.getDate()), tzoffset: String(-now.getTimezoneOffset()) });
      }
      const banner = arg === "trash"
        ? [el("span", { text: "Các mục sẽ bị xoá vĩnh viễn sau 30 ngày kể từ lúc xoá. Gửi lại đúng ảnh đó cũng tự khôi phục." })]
        : arg === "hidden" ? [el("span", { text: "Ảnh đã ẩn không hiện trong Thư viện, album hay album chia sẻ." })] : null;
      collectionHeader({
        title: smart.title,
        banner,
        more: arg === "trash" ? () => openSheet("Đã xoá gần đây", menu([
          { label: "Khôi phục tất cả", icon: "↺", action: () => trashAll("restore") },
          { label: "Xoá tất cả vĩnh viễn", icon: "🗑", destructive: true, action: () => trashAll("empty") },
        ])) : null,
      });
      const empty = { trash: "Không có mục nào bị xoá gần đây.", hidden: "Không có ảnh nào bị ẩn.", favorites: "Bấm ♥ khi xem ảnh để thêm vào Yêu thích." }[arg] || "Chưa có mục nào.";
      collection.configure(params, { context: { view: arg }, grouping: arg === "trash" ? "none" : undefined, emptyText: empty });
    }
  } catch (error) {
    collectionHeader({ title: "", select: false });
    handleError(error, $("collection-error"));
  }
}

function renderAlbumHeader(album) {
  const members = album.members || [];
  const subtitle = [plural(album.count, "mục"), album.is_owner ? (album.shared ? "Đã chia sẻ" : "") : "Của " + album.owner_name].filter(Boolean).join(" · ");
  let banner = null;
  if (album.shared) {
    banner = [
      el("span", { text: album.is_owner
        ? "Chia sẻ với " + members.map((member) => member.display_name).join(", ") + (album.members_can_add ? " · mọi người được thêm ảnh" : " · chỉ bạn thêm ảnh")
        : "Album của " + album.owner_name + (album.can_add ? " · bạn được thêm ảnh của mình" : "") }),
    ];
  }
  collectionHeader({
    title: album.name,
    subtitle,
    back: album.folder_id ? "‹ Thư mục" : lastTab === "shared" ? "‹ Chia sẻ" : "‹ Album",
    add: album.can_add,
    banner,
    more: () => openAlbumMenu(album),
  });
  $("collection-add").onclick = () => startPicking(album);
}

function openAlbumMenu(album) {
  if (!album.is_owner) {
    openSheet(album.name, menu([
      { label: "Tải cả album (ZIP)", icon: "⤓", action: () => downloadAlbum(album) },
      { label: "Rời album", icon: "⎋", destructive: true, action: () => leaveAlbum(album) },
    ]));
    return;
  }
  const sorts = [["newest_first", "Mới nhất trước"], ["oldest_first", "Cũ nhất trước"], ["added", "Theo thứ tự thêm"]];
  openSheet(album.name, menu([
    { label: "Đổi tên", icon: "✎", action: () => renameAlbum(album) },
    { label: album.shared ? "Người được chia sẻ" : "Chia sẻ với người trong nhà", icon: "♡", action: () => openMembers(album) },
    { label: "Chuyển vào thư mục", icon: "📁", action: () => moveAlbum(album) },
    ...sorts.map(([value, label]) => ({ label: "Sắp xếp: " + label, checked: album.sort_order === value, action: () => updateAlbum(album, { sort_order: value }) })),
    { label: "Tải cả album (ZIP)", icon: "⤓", action: () => downloadAlbum(album) },
    { label: "Xoá album", icon: "🗑", destructive: true, action: () => deleteAlbum(album) },
  ]));
}

async function updateAlbum(album, patch) {
  try {
    await apiJSON("/v1/albums/" + album.id, "PATCH", patch);
    openCollectionRoute({ name: "album", arg: album.id });
  } catch (error) {
    handleError(error);
  }
}

async function renameAlbum(album) {
  const name = await askText("Đổi tên album", "Tên mới", album.name);
  if (name && name !== album.name) updateAlbum(album, { name });
}

async function deleteAlbum(album) {
  if (!confirm("Xoá album \"" + album.name + "\"? Ảnh trong album vẫn còn trong Thư viện." + (album.shared ? " Người được chia sẻ sẽ không xem được nữa." : ""))) return;
  try {
    await apiJSON("/v1/albums/" + album.id, "DELETE");
    toast("Đã xoá album");
    go(album.folder_id ? "folder/" + album.folder_id : lastTab);
  } catch (error) {
    handleError(error);
  }
}

async function leaveAlbum(album) {
  if (!confirm("Rời album \"" + album.name + "\"? Ảnh bạn đã thêm vào album này cũng sẽ được bỏ ra.")) return;
  try {
    await apiJSON("/v1/albums/" + album.id + "/leave", "POST", {});
    go("shared");
  } catch (error) {
    handleError(error);
  }
}

async function openMembers(album) {
  let people;
  try {
    people = await fetchPeople();
  } catch (error) {
    handleError(error);
    return;
  }
  const current = new Set((album.members || []).map((member) => member.id));
  const choices = people.filter((person) => !person.me).map((person) => ({ person, box: el("input", { type: "checkbox", checked: current.has(person.id) }) }));
  const canAdd = el("input", { type: "checkbox", checked: album.members_can_add });
  const error = el("p", { class: "error", role: "alert" });
  openSheet("Chia sẻ \"" + album.name + "\"",
    choices.length ? el("ul", { class: "picker-list" }, choices.map(({ person, box }) => el("li", {}, el("label", {}, box,
      el("span", { class: "avatar", text: initials(person.display_name) }),
      el("span", { class: "picker-text" }, person.display_name, el("small", { text: person.email })))))) :
      el("p", { class: "muted", text: "Chưa có tài khoản nào khác trên máy chủ." }),
    el("label", { class: "switch-row" }, el("span", { text: "Người được mời có thể thêm ảnh" }), canAdd),
    el("p", { class: "muted small", text: "Bỏ chọn một người sẽ thu hồi quyền xem và bỏ ảnh họ đã thêm ra khỏi album." }),
    el("button", { class: "primary", onclick: async () => {
      try {
        await apiJSON("/v1/albums/" + album.id + "/members", "PUT", { user_ids: choices.filter(({ box }) => box.checked).map(({ person }) => person.id) });
        if (canAdd.checked !== album.members_can_add) await apiJSON("/v1/albums/" + album.id, "PATCH", { members_can_add: canAdd.checked });
        closeSheet();
        openCollectionRoute({ name: "album", arg: album.id });
      } catch (failure) {
        handleError(failure, error);
      }
    } }, "Lưu"),
    error);
}

// folderOptions lists folders as an indented tree, skipping one subtree.
function folderOptions(skipID = "") {
  const options = [];
  const walk = (parent, depth) => {
    for (const folder of albumsCache.folders.filter((candidate) => (candidate.parent_id || "") === parent)) {
      if (folder.id === skipID) continue;
      options.push({ folder, depth });
      walk(folder.id, depth + 1);
    }
  };
  walk("", 0);
  return options;
}

async function moveAlbum(album) {
  await fetchAlbums();
  openSheet("Chuyển \"" + album.name + "\" vào", menu([
    { label: "Không thuộc thư mục nào", checked: !album.folder_id, action: () => updateAlbum(album, { folder_id: "" }) },
    ...folderOptions().map(({ folder, depth }) => ({
      label: "  ".repeat(depth) + "📁 " + folder.name, checked: album.folder_id === folder.id,
      action: () => updateAlbum(album, { folder_id: folder.id }),
    })),
  ]));
}

function renderFolder(folder) {
  collectionHeader({
    title: folder.name,
    back: folder.parent_id ? "‹ Thư mục" : "‹ Album",
    select: false,
    more: () => openSheet(folder.name, menu([
      { label: "Album mới trong thư mục", icon: "❏", action: () => createAlbum({ folderID: folder.id }) },
      { label: "Thư mục con mới", icon: "📁", action: () => createFolder(folder.id) },
      { label: "Đổi tên", icon: "✎", action: () => renameFolder(folder) },
      { label: "Chuyển vào thư mục khác", icon: "↗︎", action: () => moveFolder(folder) },
      { label: "Xoá thư mục", icon: "🗑", destructive: true, action: () => deleteFolder(folder) },
    ])),
  });
  const folders = albumsCache.folders.filter((candidate) => candidate.parent_id === folder.id);
  const albums = albumsCache.albums.filter((album) => album.folder_id === folder.id);
  $("collection-folders").replaceChildren(...folders.map(folderCard), ...albums.map(albumCard));
  collectionView.collection.empty.hidden = folders.length + albums.length > 0;
  collectionView.collection.empty.textContent = "Thư mục trống. Bấm ⋯ để tạo album trong thư mục.";
}

async function renameFolder(folder) {
  const name = await askText("Đổi tên thư mục", "Tên mới", folder.name);
  if (!name) return;
  try {
    await apiJSON("/v1/album-folders/" + folder.id, "PATCH", { name });
    await fetchAlbums();
    openCollectionRoute({ name: "folder", arg: folder.id });
  } catch (error) {
    handleError(error);
  }
}

function moveFolder(folder) {
  const skip = new Set([folder.id]);
  let grew = true;
  while (grew) {
    grew = false;
    for (const candidate of albumsCache.folders) {
      if (candidate.parent_id && skip.has(candidate.parent_id) && !skip.has(candidate.id)) {
        skip.add(candidate.id);
        grew = true;
      }
    }
  }
  const move = async (parent) => {
    try {
      await apiJSON("/v1/album-folders/" + folder.id, "PATCH", { parent_id: parent });
      await fetchAlbums();
      openCollectionRoute({ name: "folder", arg: folder.id });
    } catch (error) {
      handleError(error);
    }
  };
  openSheet("Chuyển \"" + folder.name + "\" vào", menu([
    { label: "Ngoài cùng", checked: !folder.parent_id, action: () => move("") },
    ...folderOptions(folder.id).filter(({ folder: option }) => !skip.has(option.id)).map(({ folder: option, depth }) => ({
      label: "  ".repeat(depth) + "📁 " + option.name, checked: folder.parent_id === option.id, action: () => move(option.id),
    })),
  ]));
}

async function deleteFolder(folder) {
  if (!confirm("Xoá thư mục \"" + folder.name + "\"? Album và thư mục bên trong được chuyển ra ngoài, không mất ảnh nào.")) return;
  try {
    await apiJSON("/v1/album-folders/" + folder.id, "DELETE");
    await fetchAlbums();
    go(folder.parent_id ? "folder/" + folder.parent_id : "albums");
  } catch (error) {
    handleError(error);
  }
}

async function renamePlace(place) {
  const name = await askText("Đặt tên địa điểm", "Tên (ví dụ: Nhà bà nội)", place.name);
  if (name) labelPlace(place, name);
}

async function labelPlace(place, name) {
  try {
    await apiJSON("/v1/places/" + encodeURIComponent(place.cell), name ? "PUT" : "DELETE", name ? { name } : undefined);
    openCollectionRoute({ name: "place", arg: place.cell });
  } catch (error) {
    handleError(error);
  }
}

async function trashAll(action) {
  const message = action === "empty"
    ? "Xoá vĩnh viễn mọi mục trong Đã xoá gần đây? Không thể hoàn tác."
    : "Khôi phục mọi mục trong Đã xoá gần đây?";
  if (!confirm(message)) return;
  try {
    const result = await apiJSON("/v1/assets/trash/" + (action === "empty" ? "empty" : "restore"), "POST", {});
    toast((action === "empty" ? "Đã xoá vĩnh viễn " : "Đã khôi phục ") + plural(result.changed, "mục"));
    openCollectionRoute({ name: "smart", arg: "trash" });
  } catch (error) {
    handleError(error);
  }
}

// ---------------------------------------------------------------- album picker

async function openAlbumPicker(items) {
  if (!items.length) return;
  let data;
  try {
    data = await fetchAlbums();
  } catch (error) {
    handleError(error);
    return;
  }
  const targets = data.albums.filter((album) => album.can_add);
  const add = async (album) => {
    try {
      const result = await apiJSON("/v1/albums/" + album.id + "/assets", "POST", { ids: withLiveVideos(items) });
      toast(result.changed ? "Đã thêm vào \"" + album.name + "\"" : "Đã có sẵn trong \"" + album.name + "\"");
      endSelection();
    } catch (error) {
      handleError(error);
    }
  };
  openSheet("Thêm vào album",
    el("button", { class: "secondary", onclick: async () => {
      closeSheet();
      const album = await createAlbum({ assetIDs: withLiveVideos(items), open: false });
      if (album) {
        toast("Đã tạo album \"" + album.name + "\"");
        endSelection();
      }
    } }, "＋ Album mới"),
    el("ul", { class: "picker-list" }, targets.map((album) => el("li", {}, el("button", { onclick: () => {
      closeSheet();
      add(album);
    } },
      el("span", { class: "picker-cover" }, album.cover_url ? el("img", { alt: "", src: album.cover_url }) : "❏"),
      el("span", { class: "picker-text" }, album.name, el("small", { text: plural(album.count, "mục") + (album.is_owner ? "" : " · của " + album.owner_name) })))))));
}

// ---------------------------------------------------------------- share & download

function canShareFiles() {
  try {
    return Boolean(navigator.canShare && navigator.canShare({ files: [new File(["x"], "x.jpg", { type: "image/jpeg" })] }));
  } catch {
    return false;
  }
}

// filesFor downloads originals (and Live Photo videos) for the share sheet.
async function filesFor(items, onProgress) {
  const parts = [];
  for (const item of items) {
    parts.push(item);
    if (item.live_video) parts.push({ ...item.live_video, mine: item.mine });
  }
  const total = parts.reduce((sum, part) => sum + (part.byte_size || 0), 0);
  if (total > SHARE_LIMIT_BYTES) throw new Error("Quá lớn để lưu một lần (" + formatBytes(total) + "). Hãy chọn ít hơn hoặc tải ZIP.");
  const files = [];
  let done = 0;
  for (const part of parts) {
    const response = await api(part.original_url || "/v1/assets/" + part.id + "/original");
    if (!response.ok) throw new Error(await problemText(response));
    const blob = await response.blob();
    files.push(new File([blob], part.original_filename, { type: part.media_type }));
    done += part.byte_size || blob.size;
    onProgress(total ? done / total : 1);
  }
  return files;
}

function shareItems(items) {
  if (!items.length) return;
  const entries = [];
  if (canShareFiles()) {
    entries.push({ label: "Lưu vào app Ảnh / Chia sẻ…", icon: "⤴︎", action: () => prepareShare(items) });
  }
  if (items.length === 1) {
    entries.push({ label: "Tải bản gốc", icon: "⤓", action: () => downloadOriginal(items[0]) });
    if (items[0].live_video) entries.push({ label: "Tải video của Live Photo", icon: "◎", action: () => downloadOriginal({ ...items[0].live_video }) });
  }
  entries.push({ label: items.length === 1 ? "Tải về dạng ZIP" : "Tải " + plural(items.length, "mục") + " (ZIP)", icon: "🗜", action: () => downloadZip(items.map((item) => item.id)) });
  openSheet(items.length === 1 ? "Chia sẻ" : "Chia sẻ " + plural(items.length, "mục"), menu(entries),
    el("p", { class: "muted small", text: "Bản gốc được giữ nguyên, không nén. Trên iPhone, chọn \"Lưu hình ảnh/Lưu video\" để cất vào app Ảnh." }));
}

async function prepareShare(items, files = null) {
  const bar = el("progress", { class: "sheet-progress", max: "1", value: "0" });
  const status = el("p", { class: "muted", text: "Đang tải bản gốc…" });
  openSheet("Chuẩn bị lưu", status, bar);
  try {
    files = files || (await filesFor(items, (fraction) => {
      bar.value = fraction;
    }));
  } catch (error) {
    if (error instanceof SignedOut) return showLogin();
    status.textContent = error.message;
    return;
  }
  // Safari needs a fresh tap to open the share sheet after a download.
  openSheet("Sẵn sàng",
    el("p", { text: plural(files.length, "tệp") + " · " + formatBytes(files.reduce((sum, file) => sum + file.size, 0)) }),
    el("button", { class: "primary", onclick: async () => {
      try {
        await navigator.share({ files });
        closeSheet();
        endSelection();
      } catch (error) {
        if (error.name !== "AbortError") toast("Không mở được bảng chia sẻ: " + error.message);
      }
    } }, "Lưu / Chia sẻ"));
}

async function downloadOriginal(item) {
  try {
    const fresh = await ensureFresh(item);
    const link = el("a", { href: fresh.view_url || fresh.original_url, download: fresh.original_filename });
    document.body.append(link);
    link.click();
    link.remove();
  } catch (error) {
    handleError(error);
  }
}

async function downloadZip(ids) {
  try {
    const result = await apiJSON("/v1/assets/download", "POST", { ids });
    toast("Đang tải " + plural(result.count, "tệp") + " (" + formatBytes(result.total_bytes) + ")…");
    const link = el("a", { href: result.url });
    document.body.append(link);
    link.click();
    link.remove();
    endSelection();
  } catch (error) {
    handleError(error);
  }
}

async function downloadAlbum(album) {
  try {
    const ids = [];
    let cursor = "";
    do {
      const query = new URLSearchParams({ album: album.id, limit: "100" });
      if (cursor) query.set("cursor", cursor);
      const page = await apiJSON("/v1/assets?" + query);
      for (const item of page.assets) ids.push(item.id);
      cursor = page.next_cursor || "";
    } while (cursor && ids.length < 1000);
    if (!ids.length) return toast("Album trống.");
    downloadZip(ids);
  } catch (error) {
    handleError(error);
  }
}

// ensureFresh re-issues view tickets for an item opened long after its list
// was loaded (tickets last 10 minutes).
async function ensureFresh(item) {
  if (item.ticketsAt && Date.now() - item.ticketsAt < TICKET_MAX_AGE_MS) return item;
  const fresh = await apiJSON("/v1/assets/" + item.id + "?tickets=1");
  item.view_url = fresh.view_url;
  item.thumbnail_url = fresh.thumbnail_url || item.thumbnail_url;
  if (fresh.live_video && item.live_video) item.live_video.view_url = fresh.live_video.view_url;
  item.ticketsAt = Date.now();
  return item;
}

// ---------------------------------------------------------------- viewer

const viewer = {
  collection: null,
  index: -1,
  item: null,
  media: null,
  scale: 1,
  x: 0,
  y: 0,
  slideshow: null,
  token: 0,
};

function viewerContext() {
  return (viewer.collection && viewer.collection.options.context) || {};
}

function openViewer(collection, index) {
  if (index < 0) return;
  viewer.collection = collection;
  $("viewer").hidden = false;
  document.body.classList.add("no-scroll");
  showItem(index);
}

function closeViewer() {
  if ($("viewer").hidden) return;
  stopSlideshow();
  exitTrim();
  $("viewer").hidden = true;
  $("viewer").classList.remove("chrome-hidden");
  $("viewer-stage").replaceChildren();
  viewer.media = null;
  viewer.item = null;
  if ($("sheet-backdrop").hidden) document.body.classList.remove("no-scroll");
}

async function showItem(index) {
  const collection = viewer.collection;
  if (!collection || index < 0) return;
  if (index >= collection.items.length) {
    if (!collection.done) await collection.loadMore();
    if (index >= collection.items.length) return;
  }
  const token = ++viewer.token;
  viewer.index = index;
  let item = collection.items[index];
  viewer.item = item;
  resetZoom();
  try {
    item = await ensureFresh(item);
  } catch (error) {
    handleError(error);
  }
  if (token !== viewer.token) return;
  renderViewerChrome(item);
  const stage = $("viewer-stage");
  let media;
  if (isVideo(item)) {
    media = el("video", { controls: true, playsinline: true, preload: "metadata", src: item.view_url });
  } else {
    media = el("img", { alt: item.original_filename, src: item.view_url, draggable: "false" });
    media.addEventListener("error", () => {
      stage.replaceChildren(el("p", { text: "Trình duyệt này không hiển thị được định dạng ảnh. Bấm ⤴︎ → Tải bản gốc để mở bằng app Ảnh." }));
    }, { once: true });
  }
  stage.replaceChildren(media);
  viewer.media = media;
  if (viewer.slideshow && isVideo(item)) {
    media.autoplay = true;
    media.muted = false;
    media.addEventListener("ended", () => nextSlide(), { once: true });
  }
  $("viewer-prev").disabled = index === 0;
  $("viewer-next").disabled = collection.done && index >= collection.items.length - 1;
  // Warm the neighbours so swiping feels instant.
  for (const neighbour of [collection.items[index + 1], collection.items[index - 1]]) {
    if (neighbour && !isVideo(neighbour) && neighbour.view_url && Date.now() - (neighbour.ticketsAt || 0) < TICKET_MAX_AGE_MS) {
      new Image().src = neighbour.view_url;
    }
  }
  if (index >= collection.items.length - 5 && !collection.done) collection.loadMore();
}

function renderViewerChrome(item) {
  const taken = item.captured_at ? new Date(item.captured_at) : new Date(item.created_at);
  $("viewer-date").textContent = dayLabel(taken) + " " + timeFormat.format(taken);
  $("viewer-place").textContent = [item.mine ? "" : item.owner_name, placeText(item) || item.original_filename].filter(Boolean).join(" · ");
  $("viewer-live").hidden = !item.live_video;
  const caption = $("viewer-caption");
  caption.hidden = !item.caption;
  caption.textContent = item.caption || "";
  const context = viewerContext();
  const album = context.album;
  const actions = [];
  if (item.trashed_at) {
    actions.push(actionButton("↺", "Khôi phục", () => viewerBatch("restore")));
    actions.push(actionButton("🗑", "Xoá vĩnh viễn", () => viewerBatch("purge"), { danger: true }));
  } else {
    actions.push(actionButton("⤴︎", "Chia sẻ", () => shareItems([item])));
    if (album && album.shared) {
      actions.push(actionButton(item.liked ? "♥" : "♡", item.like_count ? String(item.like_count) : "Thích", () => toggleLike(item), { on: item.liked }));
      actions.push(actionButton("💬", item.comment_count ? String(item.comment_count) : "Bình luận", () => openComments(item)));
    } else if (item.mine) {
      actions.push(actionButton(item.favorite ? "♥" : "♡", "Yêu thích", () => toggleFavorite(item), { on: item.favorite }));
    }
    actions.push(actionButton("ⓘ", "Thông tin", () => openInfo(item)));
    if (isVideo(item)) actions.push(actionButton("✂︎", "Cắt", () => enterTrim(item)));
    if (item.mine) actions.push(actionButton("🗑", "Xoá", () => viewerBatch("trash"), { danger: true }));
  }
  $("viewer-actions").replaceChildren(...actions);
}

function viewerStep(delta) {
  const next = viewer.index + delta;
  if (next < 0) return;
  showItem(next);
}

function nextSlide() {
  if (!viewer.slideshow) return;
  const collection = viewer.collection;
  if (viewer.index >= collection.items.length - 1 && collection.done) {
    stopSlideshow();
    toast("Hết trình chiếu");
    return;
  }
  viewerStep(1);
  scheduleSlide();
}

function scheduleSlide() {
  clearTimeout(viewer.slideshow);
  viewer.slideshow = setTimeout(() => {
    if (viewer.item && isVideo(viewer.item)) return; // videos advance when they end
    nextSlide();
  }, 3500);
}

function startSlideshow() {
  viewer.slideshow = -1;
  $("viewer").classList.add("chrome-hidden");
  scheduleSlide();
  if (viewer.item && isVideo(viewer.item) && viewer.media) {
    viewer.media.addEventListener("ended", () => nextSlide(), { once: true });
    viewer.media.play().catch(() => {});
  }
  toast("Trình chiếu · chạm để dừng");
}

function stopSlideshow() {
  if (!viewer.slideshow) return;
  clearTimeout(viewer.slideshow);
  viewer.slideshow = null;
  $("viewer").classList.remove("chrome-hidden");
}

async function toggleFavorite(item) {
  try {
    await apiJSON("/v1/assets/" + item.id, "PATCH", { favorite: !item.favorite });
    item.favorite = !item.favorite;
    viewer.collection.refresh(item);
    renderViewerChrome(item);
  } catch (error) {
    handleError(error);
  }
}

async function toggleLike(item) {
  const album = viewerContext().album;
  try {
    await apiJSON("/v1/albums/" + album.id + "/likes", "POST", { asset_id: item.id, liked: !item.liked });
    item.liked = !item.liked;
    item.like_count = Math.max(0, (item.like_count || 0) + (item.liked ? 1 : -1));
    renderViewerChrome(item);
  } catch (error) {
    handleError(error);
  }
}

async function viewerBatch(action) {
  const item = viewer.item;
  const messages = {
    trash: "Xoá mục này? Nó sẽ nằm trong \"Đã xoá gần đây\" 30 ngày.",
    purge: "Xoá vĩnh viễn mục này? Không thể hoàn tác.",
  };
  if (messages[action] && !confirm(messages[action])) return;
  try {
    await apiJSON("/v1/assets/batch", "POST", { ids: withLiveVideos([item]), action });
    const collection = viewer.collection;
    const index = viewer.index;
    collection.remove([item.id]);
    toast({ trash: "Đã chuyển vào Đã xoá gần đây", purge: "Đã xoá vĩnh viễn", restore: "Đã khôi phục", hide: "Đã ẩn", unhide: "Đã bỏ ẩn" }[action]);
    if (!collection.items.length) closeViewer();
    else showItem(Math.min(index, collection.items.length - 1));
  } catch (error) {
    handleError(error);
  }
}

function openViewerMenu() {
  const item = viewer.item;
  if (!item) return;
  const context = viewerContext();
  const album = context.album;
  openSheet(item.original_filename, menu([
    item.mine && !item.trashed_at ? { label: "Thêm vào album", icon: "＋", action: () => openAlbumPicker([item]) } : null,
    item.mine && !item.trashed_at ? { label: item.hidden ? "Bỏ ẩn" : "Ẩn", icon: "⊘", action: () => viewerBatch(item.hidden ? "unhide" : "hide") } : null,
    album && album.is_owner ? { label: "Đặt làm ảnh bìa album", icon: "▣", action: () => setCover(album, item) } : null,
    album && (album.is_owner || item.mine) ? { label: "Bỏ khỏi album", icon: "⊖", action: () => removeCurrentFromAlbum(album, item) } : null,
    { label: "Trình chiếu", icon: "▶︎", action: () => startSlideshow() },
    { label: "Tải bản gốc", icon: "⤓", action: () => downloadOriginal(item) },
    item.live_video ? { label: "Tải video của Live Photo", icon: "◎", action: () => downloadOriginal({ ...item.live_video }) } : null,
    item.latitude !== undefined && item.latitude !== null ? { label: "Mở trong Bản đồ", icon: "⌖", action: () => window.open(mapsLink(item.latitude, item.longitude, item.place_name), "_blank", "noopener") } : null,
  ]));
}

async function setCover(album, item) {
  try {
    await apiJSON("/v1/albums/" + album.id, "PATCH", { cover_asset_id: item.id });
    toast("Đã đặt làm ảnh bìa");
  } catch (error) {
    handleError(error);
  }
}

async function removeCurrentFromAlbum(album, item) {
  try {
    await apiJSON("/v1/albums/" + album.id + "/assets/remove", "POST", { ids: withLiveVideos([item]) });
    const index = viewer.index;
    viewer.collection.remove([item.id]);
    toast("Đã bỏ khỏi album");
    if (!viewer.collection.items.length) closeViewer();
    else showItem(Math.min(index, viewer.collection.items.length - 1));
  } catch (error) {
    handleError(error);
  }
}

async function openInfo(item) {
  const body = openSheet("Thông tin", el("p", { class: "muted", text: "Đang tải…" }));
  let detail;
  try {
    detail = await apiJSON("/v1/assets/" + item.id);
  } catch (error) {
    handleError(error, body);
    return;
  }
  const details = detail.details || {};
  const facts = [];
  const fact = (label, value) => {
    if (value) facts.push(el("li", {}, el("span", { text: label }), el("span", { text: value })));
  };
  fact("Chụp lúc", detail.captured_at ? describeTaken(detail) : "Không có trong tệp");
  fact("Tải lên", capitalize(fullFormat.format(new Date(detail.created_at))));
  fact("Tên tệp", detail.original_filename);
  fact("Dung lượng", formatBytes(detail.byte_size));
  if (detail.width && detail.height) fact("Kích thước", detail.width + " × " + detail.height + (isVideo(detail) ? "" : " · " + (detail.width * detail.height / 1e6).toFixed(1).replace(".", ",") + " MP"));
  if (detail.duration_ms) fact("Thời lượng", formatDuration(detail.duration_ms));
  fact("Máy", [details.make, details.model].filter(Boolean).join(" "));
  fact("Ống kính", details.lens);
  fact("Thông số", [details.aperture, details.exposure, details.iso ? "ISO " + details.iso : "", details.focal_length].filter(Boolean).join(" · "));
  if (detail.subtype) fact("Loại", { screenshot: "Ảnh màn hình", selfie: "Ảnh selfie", panorama: "Ảnh toàn cảnh" }[detail.subtype]);
  if (detail.live_video || item.live_video) fact("Live Photo", "Có video kèm theo");
  if (!detail.mine) fact("Của", detail.owner_name);
  if (detail.albums && detail.albums.length) fact("Album", detail.albums.map((album) => album.name).join(", "));
  const content = [el("ul", { class: "facts" }, facts)];
  if (detail.latitude !== undefined && detail.latitude !== null) {
    content.push(el("ul", { class: "facts" }, el("li", {}, el("span", { text: "Vị trí" }), el("span", { text: placeText(detail) }))));
    content.push(el("a", { class: "secondary button", href: mapsLink(detail.latitude, detail.longitude, detail.place_name), target: "_blank", rel: "noopener" }, "Mở trong Bản đồ"));
    if (detail.place_cell) content.push(el("button", { class: "text-button", onclick: () => go("place/" + detail.place_cell) }, "Xem ảnh khác ở đây"));
  }
  if (detail.mine) {
    const caption = el("textarea", { maxlength: "2000", placeholder: "Thêm chú thích…" });
    caption.value = detail.caption || "";
    const status = el("p", { class: "muted small" });
    content.push(el("label", { text: "Chú thích" }, caption), el("button", { class: "secondary", onclick: async () => {
      try {
        await apiJSON("/v1/assets/" + item.id, "PATCH", { caption: caption.value });
        item.caption = caption.value.trim();
        renderViewerChrome(item);
        status.textContent = "Đã lưu ✓";
      } catch (error) {
        handleError(error, status);
      }
    } }, "Lưu chú thích"), status);
  } else if (detail.caption) {
    content.push(el("p", { text: detail.caption }));
  }
  openSheet("Thông tin", ...content);
}

async function openComments(item) {
  const album = viewerContext().album;
  const list = el("ul", { class: "comments" });
  const input = el("input", { placeholder: "Viết bình luận…", maxlength: "1000" });
  const error = el("p", { class: "error", role: "alert" });
  const render = (comments) => {
    list.replaceChildren(...comments.map((comment) => el("li", {},
      el("span", { class: "avatar", text: initials(comment.author_name) }),
      el("div", { class: "comment-body" }, el("b", { text: comment.author_name }), " " + comment.body,
        el("time", { text: capitalize(fullFormat.format(new Date(comment.created_at))) })),
      comment.mine || album.is_owner ? el("button", { class: "text-button danger-text", "aria-label": "Xoá bình luận", onclick: async () => {
        try {
          await apiJSON("/v1/albums/" + album.id + "/comments/" + comment.id, "DELETE");
          load();
        } catch (failure) {
          handleError(failure, error);
        }
      } }, "Xoá") : null)));
    if (!comments.length) list.replaceChildren(el("li", { class: "muted small", text: "Chưa có bình luận." }));
    item.comment_count = comments.length;
    renderViewerChrome(item);
  };
  const load = async () => {
    try {
      render((await apiJSON("/v1/albums/" + album.id + "/comments?asset_id=" + encodeURIComponent(item.id))).comments);
    } catch (failure) {
      handleError(failure, error);
    }
  };
  openSheet("Bình luận", list, el("form", { class: "inline-form", onsubmit: async (event) => {
    event.preventDefault();
    if (!input.value.trim()) return;
    try {
      await apiJSON("/v1/albums/" + album.id + "/comments", "POST", { asset_id: item.id, body: input.value.trim() });
      input.value = "";
      load();
    } catch (failure) {
      handleError(failure, error);
    }
  } }, input, el("button", { type: "submit", class: "secondary" }, "Gửi")), error);
  load();
}

// Live Photos play their motion while the LIVE badge is tapped or the photo
// is pressed and held, like Photos.
function playLive() {
  const item = viewer.item;
  if (!item || !item.live_video || !item.live_video.view_url) return;
  const stage = $("viewer-stage");
  if (stage.querySelector("video.live-motion")) return;
  const video = el("video", { class: "live-motion", playsinline: true, autoplay: true, src: item.live_video.view_url });
  video.addEventListener("ended", stopLive, { once: true });
  video.addEventListener("error", stopLive, { once: true });
  stage.append(video);
  if (viewer.media) viewer.media.hidden = true;
}

function stopLive() {
  const video = $("viewer-stage").querySelector("video.live-motion");
  if (video) {
    video.pause();
    video.remove();
  }
  if (viewer.media) viewer.media.hidden = false;
}

// ---------------------------------------------------------------- viewer gestures

function resetZoom() {
  viewer.scale = 1;
  viewer.x = 0;
  viewer.y = 0;
  applyTransform();
}

function applyTransform(dragX = 0, dragY = 0, dragging = false) {
  const media = viewer.media;
  if (!media) return;
  media.classList.toggle("dragging", dragging);
  media.style.setProperty("transform", "translate(" + (viewer.x + dragX) + "px, " + (viewer.y + dragY) + "px) scale(" + viewer.scale + ")");
}

function enableViewerGestures() {
  const stage = $("viewer-stage");
  const pointers = new Map();
  let gesture = null;
  let lastTap = 0;
  let tapTimer = null;
  let holdTimer = null;

  stage.addEventListener("pointerdown", (event) => {
    if (!$("trimmer").hidden) return;
    pointers.set(event.pointerId, { x: event.clientX, y: event.clientY });
    if (pointers.size === 2) {
      clearTimeout(holdTimer);
      const [a, b] = [...pointers.values()];
      gesture = { kind: "pinch", distance: Math.hypot(a.x - b.x, a.y - b.y), scale: viewer.scale };
      return;
    }
    gesture = { kind: "pending", x: event.clientX, y: event.clientY, time: Date.now(), video: event.target.tagName === "VIDEO" };
    if (viewer.item && viewer.item.live_video && !gesture.video) {
      holdTimer = setTimeout(() => {
        if (gesture && gesture.kind === "pending") {
          gesture.kind = "hold";
          playLive();
        }
      }, 450);
    }
  });

  stage.addEventListener("pointermove", (event) => {
    if (!pointers.has(event.pointerId) || !gesture) return;
    pointers.set(event.pointerId, { x: event.clientX, y: event.clientY });
    if (gesture.kind === "pinch" && pointers.size === 2) {
      const [a, b] = [...pointers.values()];
      viewer.scale = Math.min(5, Math.max(1, gesture.scale * Math.hypot(a.x - b.x, a.y - b.y) / gesture.distance));
      if (viewer.scale === 1) {
        viewer.x = 0;
        viewer.y = 0;
      }
      applyTransform(0, 0, true);
      return;
    }
    const dx = event.clientX - gesture.x;
    const dy = event.clientY - gesture.y;
    if (gesture.kind === "pending" && Math.hypot(dx, dy) > 10) {
      clearTimeout(holdTimer);
      if (viewer.scale > 1) gesture.kind = "pan";
      else if (gesture.video && Math.abs(dy) > Math.abs(dx)) gesture.kind = "none";
      else gesture.kind = Math.abs(dx) > Math.abs(dy) ? "swipe" : "dismiss";
    }
    if (gesture.kind === "pan") applyTransform(dx, dy, true);
    else if (gesture.kind === "swipe") applyTransform(dx, 0, true);
    else if (gesture.kind === "dismiss" && dy > 0) applyTransform(0, dy, true);
  });

  const finish = (event) => {
    clearTimeout(holdTimer);
    if (!pointers.has(event.pointerId)) return;
    const start = gesture;
    pointers.delete(event.pointerId);
    if (!start) return;
    if (start.kind === "pinch") {
      if (pointers.size === 0) gesture = null;
      return;
    }
    gesture = null;
    const dx = event.clientX - start.x;
    const dy = event.clientY - start.y;
    if (start.kind === "hold") {
      stopLive();
    } else if (start.kind === "pan") {
      viewer.x += dx;
      viewer.y += dy;
      applyTransform();
    } else if (start.kind === "swipe") {
      applyTransform();
      if (dx < -70) viewerStep(1);
      else if (dx > 70) viewerStep(-1);
    } else if (start.kind === "dismiss") {
      if (dy > 120) closeViewer();
      else applyTransform();
    } else if (start.kind === "pending" && Date.now() - start.time < 350 && !start.video) {
      if (viewer.slideshow) {
        stopSlideshow();
        return;
      }
      // Double tap zooms in on the spot; single tap shows or hides controls.
      if (Date.now() - lastTap < 300) {
        clearTimeout(tapTimer);
        lastTap = 0;
        if (viewer.scale > 1) resetZoom();
        else {
          const rect = stage.getBoundingClientRect();
          viewer.scale = 2.5;
          viewer.x = (rect.width / 2 - (event.clientX - rect.left)) * 1.5;
          viewer.y = (rect.height / 2 - (event.clientY - rect.top)) * 1.5;
          applyTransform();
        }
      } else {
        lastTap = Date.now();
        tapTimer = setTimeout(() => $("viewer").classList.toggle("chrome-hidden"), 300);
      }
    }
  };
  stage.addEventListener("pointerup", finish);
  stage.addEventListener("pointercancel", finish);
  stage.addEventListener("wheel", (event) => {
    if (!event.ctrlKey || !viewer.media || isVideo(viewer.item)) return;
    event.preventDefault();
    viewer.scale = Math.min(5, Math.max(1, viewer.scale * (event.deltaY < 0 ? 1.1 : 0.9)));
    if (viewer.scale === 1) resetZoom();
    else applyTransform();
  }, { passive: false });

  document.addEventListener("keydown", (event) => {
    if ($("viewer").hidden || !$("sheet-backdrop").hidden || !$("trimmer").hidden) {
      if (event.key === "Escape") closeSheet();
      return;
    }
    if (event.key === "ArrowRight") viewerStep(1);
    else if (event.key === "ArrowLeft") viewerStep(-1);
    else if (event.key === "Escape") closeViewer();
  });
}

// ---------------------------------------------------------------- video cutting

const trim = {
  item: null,
  video: null,
  duration: 0,
  start: 0,
  end: 0,
  frame: 0,
  playing: false,
  filmstripToken: 0,
};

async function enterTrim(item) {
  stopSlideshow();
  try {
    item = await ensureFresh(item);
  } catch (error) {
    handleError(error);
    return;
  }
  trim.item = item;
  const video = el("video", { playsinline: true, preload: "auto", src: item.view_url });
  video.muted = false;
  $("viewer-stage").replaceChildren(video);
  viewer.media = video;
  trim.video = video;
  $("viewer").classList.add("trimming");
  $("viewer-actions").hidden = true;
  $("viewer-live").hidden = true;
  $("trimmer").hidden = false;
  $("trim-status").textContent = "Kéo hai cạnh vàng để chọn đoạn cần giữ.";
  const ready = () => {
    trim.duration = video.duration || (item.duration_ms || 0) / 1000;
    trim.start = 0;
    trim.end = trim.duration;
    layoutTrim();
    buildFilmstrip(item);
  };
  if (video.readyState >= 1) ready();
  else video.addEventListener("loadedmetadata", ready, { once: true });
  video.addEventListener("timeupdate", () => {
    if (trim.playing && video.currentTime >= trim.end) {
      video.pause();
      trim.playing = false;
      $("trim-play").textContent = "▶";
      video.currentTime = trim.start;
    }
    layoutTrim();
  });
}

function exitTrim() {
  if ($("trimmer").hidden) return;
  trim.filmstripToken++;
  trim.playing = false;
  $("trimmer").hidden = true;
  $("viewer").classList.remove("trimming");
  $("viewer-actions").hidden = false;
  $("trim-strip").replaceChildren();
}

function trackGeometry() {
  const track = $("trim-track");
  const width = track.clientWidth - 44;
  return { track, left: 22, width: Math.max(1, width) };
}

// Positions are percentages of the track (between the 22px handles), so the
// trimmer stays correct when the phone rotates.
function trackPosition(fraction) {
  return "calc(22px + (100% - 44px) * " + Math.min(1, Math.max(0, fraction)).toFixed(5) + ")";
}

function trackLength(fraction) {
  return "calc((100% - 44px) * " + Math.min(1, Math.max(0, fraction)).toFixed(5) + ")";
}

function layoutTrim() {
  if (!trim.duration) return;
  const startFraction = trim.start / trim.duration;
  const endFraction = trim.end / trim.duration;
  const windowBox = $("trim-window");
  windowBox.style.setProperty("left", trackPosition(startFraction));
  windowBox.style.setProperty("width", trackLength(endFraction - startFraction));
  $("trim-shade-left").style.setProperty("left", "22px");
  $("trim-shade-left").style.setProperty("width", trackLength(startFraction));
  $("trim-shade-right").style.setProperty("left", trackPosition(endFraction));
  $("trim-shade-right").style.setProperty("width", trackLength(1 - endFraction));
  const current = trim.video ? trim.video.currentTime : 0;
  $("trim-playhead").style.setProperty("left", trackPosition((current || 0) / trim.duration));
  $("trim-start-label").textContent = formatClock(trim.start);
  $("trim-end-label").textContent = formatClock(trim.end);
  $("trim-length").textContent = (trim.end - trim.start).toFixed(1).replace(".", ",") + " giây";
}

async function buildFilmstrip(item) {
  const token = ++trim.filmstripToken;
  const strip = $("trim-strip");
  const { width } = trackGeometry();
  const count = Math.max(6, Math.min(14, Math.floor(width / 44)));
  strip.replaceChildren(...Array.from({ length: count }, () => el("img", { alt: "" })));
  const probe = document.createElement("video");
  try {
    for (let index = 0; index < count; index++) {
      if (token !== trim.filmstripToken) break;
      await videoFrame(item.view_url, ((index + 0.5) / count) * trim.duration, probe);
      if (token !== trim.filmstripToken) break;
      strip.children[index].src = drawScaled(probe, 96).toDataURL("image/jpeg", 0.6);
    }
  } catch {
    // The strip is decoration; the handles work without it.
  } finally {
    releaseMedia(probe);
  }
}

function seekPreview(seconds) {
  const video = trim.video;
  if (!video) return;
  trim.pendingSeek = seconds;
  if (trim.frame) return;
  trim.frame = requestAnimationFrame(() => {
    trim.frame = 0;
    video.currentTime = trim.pendingSeek;
  });
}

function enableTrimGestures() {
  const timeAt = (clientX) => {
    const { track, left, width } = trackGeometry();
    const x = clientX - track.getBoundingClientRect().left - left;
    return Math.min(trim.duration, Math.max(0, (x / width) * trim.duration));
  };
  for (const [id, edge] of [["trim-handle-start", "start"], ["trim-handle-end", "end"]]) {
    const handle = $(id);
    handle.addEventListener("pointerdown", (event) => {
      event.preventDefault();
      event.stopPropagation();
      handle.setPointerCapture(event.pointerId);
      handle.dragging = true;
      if (trim.playing) {
        trim.video.pause();
        trim.playing = false;
        $("trim-play").textContent = "▶";
      }
    });
    handle.addEventListener("pointermove", (event) => {
      if (!handle.dragging || !trim.duration) return;
      const seconds = timeAt(event.clientX);
      if (edge === "start") trim.start = Math.min(seconds, trim.end - MIN_CLIP_SECONDS);
      else trim.end = Math.max(seconds, trim.start + MIN_CLIP_SECONDS);
      trim.start = Math.max(0, trim.start);
      trim.end = Math.min(trim.duration, trim.end);
      seekPreview(edge === "start" ? trim.start : trim.end);
      layoutTrim();
    });
    const stop = () => {
      handle.dragging = false;
    };
    handle.addEventListener("pointerup", stop);
    handle.addEventListener("pointercancel", stop);
    handle.addEventListener("keydown", (event) => {
      const step = event.shiftKey ? 1 : 0.1;
      const delta = event.key === "ArrowLeft" ? -step : event.key === "ArrowRight" ? step : 0;
      if (!delta) return;
      event.preventDefault();
      if (edge === "start") trim.start = Math.min(Math.max(0, trim.start + delta), trim.end - MIN_CLIP_SECONDS);
      else trim.end = Math.max(Math.min(trim.duration, trim.end + delta), trim.start + MIN_CLIP_SECONDS);
      seekPreview(edge === "start" ? trim.start : trim.end);
      layoutTrim();
    });
  }
  $("trim-track").addEventListener("pointerdown", (event) => {
    if (event.target.closest(".trim-handle") || !trim.duration) return;
    const seconds = Math.min(trim.end, Math.max(trim.start, timeAt(event.clientX)));
    seekPreview(seconds);
  });
  $("trim-play").addEventListener("click", () => {
    const video = trim.video;
    if (!video) return;
    if (trim.playing) {
      video.pause();
      trim.playing = false;
      $("trim-play").textContent = "▶";
      return;
    }
    if (video.currentTime < trim.start || video.currentTime >= trim.end - 0.05) video.currentTime = trim.start;
    trim.playing = true;
    $("trim-play").textContent = "❚❚";
    video.play().catch(() => {
      trim.playing = false;
      $("trim-play").textContent = "▶";
    });
  });
  $("trim-cancel").addEventListener("click", () => {
    exitTrim();
    showItem(viewer.index);
  });
  $("trim-save").addEventListener("click", () => saveClip());
}

function clipQuery() {
  return "start=" + trim.start.toFixed(3) + "&end=" + trim.end.toFixed(3);
}

function clipName(item) {
  const dot = item.original_filename.lastIndexOf(".");
  const base = dot > 0 ? item.original_filename.slice(0, dot) : item.original_filename;
  const extension = dot > 0 ? item.original_filename.slice(dot) : ".MOV";
  return base + " (" + formatClock(trim.start).replace(",", ".") + "-" + formatClock(trim.end).replace(",", ".") + ")" + extension;
}

function saveClip() {
  const item = trim.item;
  if (!item) return;
  const entries = [];
  if (canShareFiles()) entries.push({ label: "Lưu đoạn này vào app Ảnh", icon: "⤴︎", action: () => shareClip(item) });
  entries.push({ label: "Tải đoạn này về", icon: "⤓", action: async () => {
    try {
      await ensureFresh(item);
      const ticket = item.view_url.slice(item.view_url.indexOf("ticket="));
      const link = el("a", { href: "/v1/assets/" + item.id + "/clip?" + clipQuery() + "&" + ticket, download: clipName(item) });
      document.body.append(link);
      link.click();
      link.remove();
      toast("Đang tải đoạn " + (trim.end - trim.start).toFixed(1).replace(".", ",") + " giây…");
    } catch (error) {
      handleError(error);
    }
  } });
  openSheet("Lưu đoạn " + formatClock(trim.start) + " – " + formatClock(trim.end), menu(entries),
    el("p", { class: "muted small", text: "Đoạn được cắt thẳng từ bản gốc, không nén lại, giữ nguyên chất lượng. Video gốc không bị thay đổi." }));
}

async function shareClip(item) {
  const bar = el("progress", { class: "sheet-progress", max: "1", value: "0" });
  const status = el("p", { class: "muted", text: "Đang cắt video…" });
  openSheet("Chuẩn bị đoạn video", status, bar);
  try {
    const response = await api("/v1/assets/" + item.id + "/clip?" + clipQuery());
    if (!response.ok) throw new Error(await problemText(response));
    const total = Number(response.headers.get("Content-Length")) || 0;
    const reader = response.body.getReader();
    const chunks = [];
    let received = 0;
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      chunks.push(value);
      received += value.length;
      if (total) bar.value = received / total;
    }
    const file = new File(chunks, clipName(item), { type: item.media_type });
    prepareShare([], [file]);
  } catch (error) {
    if (error instanceof SignedOut) return showLogin();
    status.textContent = "Không cắt được: " + error.message;
  }
}

// ---------------------------------------------------------------- sign in

let mfaChallenge = null;

async function submitLogin(event) {
  event.preventDefault();
  $("login-error").textContent = "";
  const response = await fetch("/v1/auth/login", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      email: $("login-email").value.trim(),
      password: $("login-password").value,
      device_name: deviceName(),
    }),
  }).catch(() => null);
  if (!response) {
    $("login-error").textContent = "Không kết nối được máy chủ.";
    return;
  }
  if (response.status === 202) {
    mfaChallenge = (await response.json()).challenge;
    $("login-form").hidden = true;
    $("mfa-form").hidden = false;
    $("mfa-code").focus();
    return;
  }
  if (!response.ok) {
    $("login-error").textContent =
      response.status === 429 ? "Thử sai quá nhiều lần. Đợi 10 phút rồi thử lại." : "Email hoặc mật khẩu không đúng.";
    return;
  }
  $("login-password").value = "";
  storeTokenResponse(await response.json());
  peopleCache = null;
  route();
}

async function submitMFA(event) {
  event.preventDefault();
  $("mfa-error").textContent = "";
  const code = $("mfa-code").value.trim();
  const body = /^\d{6}$/.test(code)
    ? { challenge: mfaChallenge, totp_code: code }
    : { challenge: mfaChallenge, recovery_code: code };
  const response = await fetch("/v1/auth/mfa/verify", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  }).catch(() => null);
  if (!response || !response.ok) {
    $("mfa-error").textContent = "Mã không đúng hoặc đã hết hạn.";
    if (response && response.status === 401) {
      const problem = await response.json().catch(() => ({}));
      if (problem.code === "invalid_mfa_challenge") showLogin();
    }
    return;
  }
  $("mfa-code").value = "";
  storeTokenResponse(await response.json());
  peopleCache = null;
  route();
}

function deviceName() {
  const ua = navigator.userAgent;
  const kind = /iPhone/.test(ua) ? "iPhone" : /iPad/.test(ua) ? "iPad" : /Android/.test(ua) ? "Android" : "Máy tính";
  return "Web – " + kind;
}

async function logout() {
  const credential = currentCredential();
  if (credential) {
    await fetch("/v1/auth/logout", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ refresh_token: credential.refreshToken }),
    }).catch(() => null);
  }
  credentials.clear();
  library.collection.reset();
  collectionView.collection.reset();
  peopleCache = null;
  showLogin();
}

// ---------------------------------------------------------------- uploads

const stateLabels = {
  created: ["Đang chờ", "busy"],
  uploading: ["Đang tải lên", "busy"],
  received: ["Đang kiểm tra", "busy"],
  verifying: ["Đang kiểm tra", "busy"],
  verified: ["Đang lưu", "busy"],
  committing: ["Đang lưu", "busy"],
  quarantining: ["Lỗi khi gửi", "bad"],
  available: ["Đã sao lưu ✓", "ok"],
  quarantined: ["Lỗi: file bị hỏng khi gửi, hãy gửi lại", "bad"],
  failed: ["Lỗi, hãy gửi lại", "bad"],
  expired: ["Hết hạn, hãy gửi lại", "bad"],
};

function uploadThumb(upload, assets) {
  const thumb = el("span", { class: "upload-thumb", text: upload.media_type.startsWith("video/") ? "🎬" : "🖼" });
  const asset = assets.get(upload.asset_id);
  const add = (node) => {
    node.addEventListener("error", () => node.remove(), { once: true });
    thumb.append(node);
  };
  if (asset && asset.thumbnail_url) add(el("img", { alt: "", src: asset.thumbnail_url }));
  else if (upload.thumbnail_url) add(el("img", { alt: "", src: upload.thumbnail_url }));
  else if (upload.preview_url && upload.media_type.startsWith("video/")) add(el("video", { muted: true, playsinline: true, preload: "metadata", src: upload.preview_url + "#t=0.1" }));
  else if (upload.preview_url && upload.media_type.startsWith("image/")) add(el("img", { alt: "", src: upload.preview_url }));
  return thumb;
}

function uploadMeta(upload) {
  const parts = [];
  if (upload.captured_at) parts.push((upload.media_type.startsWith("video/") ? "Quay " : "Chụp ") + capitalize(fullFormat.format(new Date(upload.captured_at))));
  if (upload.duration_ms) parts.push(formatDuration(upload.duration_ms));
  parts.push(formatBytes(upload.expected_size));
  return parts.join(" · ");
}

async function refreshServerUploads() {
  try {
    const body = await apiJSON("/v1/upload-sessions?limit=50");
    // Thumbnails of what was backed up, and of items stuck uploads duplicate.
    const assetIDs = [...new Set(body.uploads.flatMap((upload) => [upload.asset_id, upload.same_as_asset_id]).filter(Boolean))].slice(0, 200);
    const assets = new Map();
    if (assetIDs.length) {
      try {
        const page = await apiJSON("/v1/assets?" + new URLSearchParams({ ids: assetIDs.join(","), tickets: "1" }));
        for (const asset of page.assets) {
          asset.ticketsAt = Date.now();
          assets.set(asset.id, asset);
        }
      } catch {
        // Thumbnails are optional.
      }
    }
    const rows = body.uploads.map((upload) => {
      let [text, kind] = stateLabels[upload.state] || [upload.state, "busy"];
      if (upload.state === "expired" && upload.error_code === "cancelled") [text, kind] = ["Đã dừng", "dim"];
      const fraction = upload.expected_size ? upload.received_size / upload.expected_size : 0;
      const transferring = upload.state === "uploading" || upload.state === "created";
      const stateLine = text + (transferring ? " " + Math.floor(fraction * 100) + "%" : "");
      const item = el("li", { dataset: { sessionId: upload.id } });
      const asset = assets.get(upload.asset_id);
      const thumb = uploadThumb(upload, assets);
      if (asset) {
        thumb.addEventListener("click", () => openSingle(asset));
      }
      const main = el("div", { class: "upload-main" },
        el("div", { class: "name", text: upload.original_filename }),
        el("div", { class: "state " + kind, text: stateLine }),
        el("div", { class: "upload-meta", text: uploadMeta(upload) }));
      const row = el("div", { class: "upload-row" }, thumb, main);
      if (upload.cancellable) {
        row.append(el("button", { class: "cancel-upload", "aria-label": "Dừng tải " + upload.original_filename, onclick: () => openCancelSheet(upload, assets) }, "✕"));
      }
      item.append(row);
      if (transferring) item.append(el("progress", { max: "1", value: String(fraction) }));
      return item;
    });
    $("server-uploads").replaceChildren(...rows);
    const known = new Set(body.uploads.map((upload) => upload.id));
    for (const local of $("local-uploads").querySelectorAll("li[data-session-id]")) {
      if (known.has(local.dataset.sessionId)) local.remove();
    }
    if (!rows.length) {
      $("server-uploads").replaceChildren(el("li", { class: "muted", text: "Chưa có lần tải lên nào." }));
    }
    $("uploads-error").textContent = "";
  } catch (error) {
    handleError(error, $("uploads-error"));
  }
}

// openSingle shows one item (for example from the upload list) in the viewer.
function openSingle(item) {
  const single = { items: [item], byID: new Map([[item.id, item]]), tiles: new Map(), done: true, options: { context: {} }, refresh() {}, remove() {}, loadMore() {} };
  openViewer(single, 0);
}

// The ✕ on a stuck upload shows what arrived so far (first frame or top of
// the photo, capture time, camera) before it is stopped.
function openCancelSheet(upload, assets) {
  const preview = el("div", { class: "cancel-preview", text: upload.media_type.startsWith("video/") ? "🎬" : "🖼" });
  if (upload.preview_url && upload.media_type.startsWith("video/")) {
    const video = el("video", { muted: true, playsinline: true, controls: true, preload: "metadata", src: upload.preview_url + "#t=0.1" });
    video.addEventListener("error", () => video.remove(), { once: true });
    preview.append(video);
  } else if (upload.thumbnail_url || upload.preview_url) {
    const image = el("img", { alt: "", src: upload.thumbnail_url || upload.preview_url });
    image.addEventListener("error", () => image.remove(), { once: true });
    preview.append(image);
  }
  const facts = [];
  const fact = (label, value) => {
    if (value) facts.push(el("li", {}, el("span", { text: label }), el("span", { text: value })));
  };
  const fraction = upload.expected_size ? upload.received_size / upload.expected_size : 0;
  fact("Tên", upload.original_filename);
  fact("Đã nhận", formatBytes(upload.received_size) + " / " + formatBytes(upload.expected_size) + " (" + Math.floor(fraction * 100) + "%)");
  fact(upload.media_type.startsWith("video/") ? "Quay lúc" : "Chụp lúc", upload.captured_at ? capitalize(fullFormat.format(new Date(upload.captured_at))) : "");
  fact("Thời lượng", upload.duration_ms ? formatDuration(upload.duration_ms) : "");
  fact("Kích thước", upload.width && upload.height ? upload.width + " × " + upload.height : "");
  fact("Máy", upload.camera);
  fact("Bắt đầu gửi", capitalize(fullFormat.format(new Date(upload.created_at))));
  fact("Dữ liệu mới nhất", capitalize(fullFormat.format(new Date(upload.updated_at))));
  const content = [preview, el("ul", { class: "facts" }, facts)];
  const twin = assets.get(upload.same_as_asset_id);
  if (upload.same_as_asset_id) {
    content.push(el("div", { class: "same-as" },
      twin && twin.thumbnail_url ? el("img", { alt: "", src: twin.thumbnail_url }) : null,
      el("span", { text: "Đã có bản sao lưu " + (upload.captured_at ? "chụp/quay cùng lúc" : "cùng tên") + (twin ? " (" + twin.original_filename + ", " + formatBytes(twin.byte_size) + ")" : "") + ". Dừng bản này không làm mất ảnh." })));
  }
  const error = el("p", { class: "error", role: "alert" });
  content.push(
    el("button", { class: "danger", onclick: async () => {
      try {
        await apiJSON("/v1/upload-sessions/" + upload.id, "DELETE");
        closeSheet();
        toast("Đã dừng tải " + upload.original_filename);
        refreshServerUploads();
      } catch (failure) {
        handleError(failure, error);
      }
    } }, "Dừng tải lên"),
    el("button", { class: "secondary", onclick: closeSheet }, "Không"),
    el("p", { class: "muted small", text: "Phần đã nhận sẽ bị xoá. Gửi lại cùng tệp sau sẽ tải lại từ đầu." }),
    error);
  openSheet("Dừng tải lên?", content);
}

async function uploadFiles(files) {
  const media = files.filter((file) => file.type.startsWith("image/") || file.type.startsWith("video/") || /\.(heic|heif|jpe?g|png|gif|webp|tiff?|dng|avif|mov|mp4|m4v|3gp)$/i.test(file.name));
  if (files.length && !media.length) {
    toast("Không có ảnh hoặc video trong các tệp đã chọn.");
    return;
  }
  for (const file of media) {
    const controller = { request: null, cancelled: false };
    const state = el("span", { class: "state busy", text: "Đang tính mã kiểm tra…" });
    const bar = el("progress", { max: "1", value: "0" });
    const cancel = el("button", { class: "cancel-upload", "aria-label": "Huỷ tải " + file.name, onclick: () => {
      controller.cancelled = true;
      if (controller.request) controller.request.abort();
    } }, "✕");
    const thumb = el("span", { class: "upload-thumb", text: file.type.startsWith("video/") ? "🎬" : "🖼" });
    if (file.type.startsWith("image/") && file.size < 30 * 1024 * 1024) {
      const image = el("img", { alt: "" });
      image.addEventListener("error", () => image.remove(), { once: true });
      image.src = URL.createObjectURL(file);
      thumb.append(image);
    }
    const item = el("li", {}, el("div", { class: "upload-row" }, thumb,
      el("div", { class: "upload-main" }, el("div", { class: "name", text: file.name }), state, el("div", { class: "upload-meta", text: formatBytes(file.size) })),
      cancel), bar);
    $("local-uploads").prepend(item);
    try {
      const digest = await sha256Hex(file, (fraction) => {
        bar.value = fraction * 0.2;
      });
      if (controller.cancelled) throw new Error("Đã huỷ");
      let result;
      for (let attempt = 0; attempt < 5; attempt++) {
        state.textContent = "Đang tải lên…";
        result = await sendFile(file, digest, controller, (fraction) => {
          bar.value = 0.2 + fraction * 0.8;
          state.textContent = "Đang tải lên " + Math.floor(fraction * 100) + "%";
        });
        if (result.status !== 429) break;
        state.textContent = "Đang chờ máy chủ…";
        await sleep((result.retryAfter || 5) * 1000);
      }
      if (result.status === 200 || result.status === 202) {
        // From here the server's own row under "Gần đây" tracks this file
        // (checking → Đã sao lưu ✓); the local row is removed once it appears.
        item.dataset.sessionId = result.id;
        state.textContent = result.status === 200 ? "Đã có sẵn ✓" : "Đã gửi, đang kiểm tra";
        if (result.status === 200) state.className = "state ok";
        cancel.remove();
      } else {
        throw new Error(result.detail || "Lỗi " + result.status);
      }
      bar.value = 1;
    } catch (error) {
      if (error instanceof SignedOut) {
        showLogin();
        return;
      }
      state.textContent = controller.cancelled ? "Đã huỷ" : "Lỗi: " + (error.message || error);
      state.className = controller.cancelled ? "state dim" : "state bad";
      cancel.remove();
    }
    refreshServerUploads();
  }
}

function sendFile(file, digest, controller, onProgress) {
  return accessToken().then(
    (token) =>
      new Promise((resolve, reject) => {
        const request = new XMLHttpRequest();
        controller.request = request;
        request.open("POST", "/v1/direct-uploads");
        request.setRequestHeader("Authorization", "Bearer " + token);
        request.setRequestHeader("X-Content-SHA256", digest);
        request.setRequestHeader("X-File-Name", encodeURIComponent(file.name));
        if (file.type.startsWith("image/") || file.type.startsWith("video/")) {
          request.setRequestHeader("X-Media-Type", file.type);
        }
        request.upload.addEventListener("progress", (event) => {
          if (event.lengthComputable) onProgress(event.loaded / event.total);
        });
        request.addEventListener("load", () => {
          let detail = "";
          let id = "";
          try {
            const body = JSON.parse(request.responseText);
            detail = body.detail || "";
            id = body.id || "";
          } catch {
            // not JSON
          }
          if (request.status === 401) {
            credentials.clear();
            reject(new SignedOut());
            return;
          }
          resolve({
            status: request.status,
            id,
            detail,
            retryAfter: Number(request.getResponseHeader("Retry-After")) || 0,
          });
        });
        request.addEventListener("abort", () => reject(new Error("Đã huỷ")));
        request.addEventListener("error", () => reject(new Error("Mất kết nối khi đang gửi")));
        request.send(file);
      }),
  );
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

// SHA-256 of a File. WebCrypto needs the whole file in memory, so large videos
// are hashed incrementally in slices with a small pure-JS implementation.
async function sha256Hex(file, onProgress) {
  if (file.size <= SUBTLE_HASH_LIMIT) {
    const digest = await crypto.subtle.digest("SHA-256", await file.arrayBuffer());
    onProgress(1);
    return hex(new Uint8Array(digest));
  }
  const hasher = new Sha256();
  const slice = 8 * 1024 * 1024;
  for (let offset = 0; offset < file.size; offset += slice) {
    hasher.update(new Uint8Array(await file.slice(offset, offset + slice).arrayBuffer()));
    onProgress(Math.min(1, (offset + slice) / file.size));
  }
  return hex(hasher.digest());
}

function hex(bytes) {
  return Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("");
}

const K = new Uint32Array([
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
  0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
  0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
  0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
  0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
  0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
  0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
  0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
]);

class Sha256 {
  constructor() {
    this.state = new Uint32Array([
      0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19,
    ]);
    this.buffer = new Uint8Array(64);
    this.buffered = 0;
    this.length = 0;
    this.words = new Uint32Array(64);
  }

  update(data) {
    this.length += data.length;
    let offset = 0;
    if (this.buffered) {
      const take = Math.min(64 - this.buffered, data.length);
      this.buffer.set(data.subarray(0, take), this.buffered);
      this.buffered += take;
      offset = take;
      if (this.buffered === 64) {
        this.block(this.buffer, 0);
        this.buffered = 0;
      }
    }
    for (; offset + 64 <= data.length; offset += 64) this.block(data, offset);
    if (offset < data.length) {
      this.buffer.set(data.subarray(offset), 0);
      this.buffered = data.length - offset;
    }
  }

  block(data, offset) {
    const w = this.words;
    for (let i = 0; i < 16; i++) {
      const j = offset + i * 4;
      w[i] = (data[j] << 24) | (data[j + 1] << 16) | (data[j + 2] << 8) | data[j + 3];
    }
    for (let i = 16; i < 64; i++) {
      const a = w[i - 15];
      const b = w[i - 2];
      const s0 = ((a >>> 7) | (a << 25)) ^ ((a >>> 18) | (a << 14)) ^ (a >>> 3);
      const s1 = ((b >>> 17) | (b << 15)) ^ ((b >>> 19) | (b << 13)) ^ (b >>> 10);
      w[i] = (w[i - 16] + s0 + w[i - 7] + s1) | 0;
    }
    let [a, b, c, d, e, f, g, h] = this.state;
    for (let i = 0; i < 64; i++) {
      const s1 = ((e >>> 6) | (e << 26)) ^ ((e >>> 11) | (e << 21)) ^ ((e >>> 25) | (e << 7));
      const ch = (e & f) ^ (~e & g);
      const t1 = (h + s1 + ch + K[i] + w[i]) | 0;
      const s0 = ((a >>> 2) | (a << 30)) ^ ((a >>> 13) | (a << 19)) ^ ((a >>> 22) | (a << 10));
      const maj = (a & b) ^ (a & c) ^ (b & c);
      const t2 = (s0 + maj) | 0;
      h = g;
      g = f;
      f = e;
      e = (d + t1) | 0;
      d = c;
      c = b;
      b = a;
      a = (t1 + t2) | 0;
    }
    const s = this.state;
    s[0] += a; s[1] += b; s[2] += c; s[3] += d;
    s[4] += e; s[5] += f; s[6] += g; s[7] += h;
  }

  digest() {
    const bitLength = this.length * 8;
    const padding = new Uint8Array(this.buffered < 56 ? 64 - this.buffered : 128 - this.buffered);
    padding[0] = 0x80;
    const view = new DataView(padding.buffer);
    view.setUint32(padding.length - 8, Math.floor(bitLength / 0x100000000));
    view.setUint32(padding.length - 4, bitLength >>> 0);
    const length = this.length;
    this.update(padding);
    this.length = length;
    const out = new Uint8Array(32);
    const outView = new DataView(out.buffer);
    for (let i = 0; i < 8; i++) outView.setUint32(i * 4, this.state[i]);
    return out;
  }
}

function enableDropZone() {
  const zone = $("drop-zone");
  const desktop = Boolean(window.matchMedia && window.matchMedia("(hover: hover) and (pointer: fine)").matches);
  $("drop-hint").hidden = !desktop;
  $("folder-button").hidden = !desktop || !("webkitdirectory" in document.createElement("input"));
  for (const type of ["dragenter", "dragover"]) {
    document.addEventListener(type, (event) => {
      if (currentRoute.name !== "uploads" || !event.dataTransfer || ![...event.dataTransfer.types].includes("Files")) return;
      event.preventDefault();
      zone.classList.add("dragging");
    });
  }
  document.addEventListener("dragleave", (event) => {
    if (!event.relatedTarget) zone.classList.remove("dragging");
  });
  document.addEventListener("drop", (event) => {
    if (currentRoute.name !== "uploads" || !event.dataTransfer) return;
    event.preventDefault();
    zone.classList.remove("dragging");
    uploadFiles(Array.from(event.dataTransfer.files || []));
  });
}

// ---------------------------------------------------------------- settings

async function loadSettings({ keepOpenWork = false } = {}) {
  refreshKeys();
  // Don't wipe an MFA setup or recovery codes the person may be copying into
  // another app when they come back.
  if (!keepOpenWork || ($("mfa-setup").hidden && $("mfa-codes").hidden)) refreshMFA();
  loadUsage();
  try {
    const me = (await fetchPeople()).find((person) => person.me);
    if (me && document.activeElement !== $("display-name")) $("display-name").value = me.display_name;
  } catch (error) {
    handleError(error, $("name-error"));
  }
}

async function saveDisplayName() {
  $("name-error").textContent = "";
  try {
    await apiJSON("/v1/people/me", "PUT", { display_name: $("display-name").value.trim() });
    peopleCache = null;
    toast("Đã lưu tên");
  } catch (error) {
    handleError(error, $("name-error"));
  }
}

async function loadUsage() {
  $("usage-error").textContent = "";
  try {
    const summary = await apiJSON("/v1/library/summary");
    const usage = summary.usage;
    const used = usage.photo_bytes + usage.video_bytes + usage.trash_bytes;
    const capacity = usage.quota_bytes || used + (usage.disk_free_bytes || 0) || 1;
    const segment = (kind, bytes) => {
      const bar = el("span", { class: "usage-" + kind });
      bar.style.setProperty("width", Math.min(100, (bytes / capacity) * 100) + "%");
      return bar;
    };
    $("usage-bar").replaceChildren(segment("photo", usage.photo_bytes), segment("video", usage.video_bytes), segment("trash", usage.trash_bytes));
    const row = (kind, label, value) => el("li", {}, el("span", {}, kind ? el("span", { class: "dot usage-" + kind }) : null, label), el("span", { class: "muted", text: value }));
    $("usage-list").replaceChildren(...[
      row("photo", plural(usage.photo_count, "ảnh"), formatBytes(usage.photo_bytes)),
      row("video", plural(usage.video_count, "video"), formatBytes(usage.video_bytes)),
      row("trash", "Đã xoá gần đây (" + usage.trash_count + ")", formatBytes(usage.trash_bytes)),
      usage.quota_bytes ? row("", "Hạn mức của bạn", formatBytes(usage.quota_bytes)) : null,
      usage.disk_free_bytes !== undefined && usage.disk_free_bytes !== null ? row("", "Còn trống trên máy chủ", formatBytes(usage.disk_free_bytes)) : null,
    ].filter(Boolean));
  } catch (error) {
    handleError(error, $("usage-error"));
  }
}

async function refreshKeys() {
  try {
    const body = await apiJSON("/v1/auth/upload-keys");
    const rows = body.upload_keys.map((key) => el("li", {},
      el("div", { class: "row" }, el("span", { class: "name", text: key.name }),
        el("button", { class: "secondary", onclick: () => revokeKey(key) }, "Thu hồi")),
      el("span", { class: "muted", text: key.last_used_at ? "Dùng lần cuối: " + new Date(key.last_used_at).toLocaleString("vi-VN") : "Chưa dùng" })));
    if (!rows.length) rows.push(el("li", { class: "muted", text: "Chưa có khoá nào." }));
    $("key-list").replaceChildren(...rows);
  } catch (error) {
    handleError(error, $("key-error"));
  }
}

async function createKey() {
  $("key-error").textContent = "";
  $("create-key").disabled = true;
  try {
    const body = await apiJSON("/v1/auth/upload-keys", "POST", { name: $("key-name").value.trim() || "iPhone" });
    $("new-key-value").textContent = body.upload_key;
    // One address for everything: POSTing a photo uploads it, opening it
    // shows progress.
    $("upload-url").textContent = location.origin + "/app/#uploads";
    for (const link of document.querySelectorAll(".app-url")) {
      link.textContent = location.origin + "/app/#" + link.dataset.hash;
    }
    $("new-key").hidden = false;
    refreshKeys();
  } catch (error) {
    handleError(error, $("key-error"));
  } finally {
    $("create-key").disabled = false;
  }
}

async function revokeKey(key) {
  if (!confirm("Thu hồi khoá \"" + key.name + "\"? Phím tắt trên máy đó sẽ không gửi ảnh được nữa.")) return;
  try {
    const response = await api("/v1/auth/upload-keys/" + encodeURIComponent(key.id), { method: "DELETE" });
    if (!response.ok && response.status !== 404) throw new Error(await problemText(response));
    refreshKeys();
  } catch (error) {
    handleError(error, $("key-error"));
  }
}

async function copyText(elementID, button) {
  const text = $(elementID).textContent;
  try {
    await navigator.clipboard.writeText(text);
    const original = button.textContent;
    button.textContent = "Đã sao chép ✓";
    setTimeout(() => {
      button.textContent = original;
    }, 2000);
  } catch {
    const range = document.createRange();
    range.selectNodeContents($(elementID));
    const selected = getSelection();
    selected.removeAllRanges();
    selected.addRange(range);
  }
}

// ---------------------------------------------------------------- MFA settings

function showMFASection(name) {
  for (const section of ["mfa-off", "mfa-setup", "mfa-codes", "mfa-on"]) $(section).hidden = section !== name;
}

async function refreshMFA() {
  $("mfa-settings-error").textContent = "";
  try {
    const status = await apiJSON("/v1/auth/mfa");
    if (!status.available) {
      $("mfa-settings-status").textContent = "Máy chủ chưa bật tính năng này.";
      showMFASection(null);
      return;
    }
    $("mfa-settings-status").textContent = status.enabled ? "" : "Đang tắt.";
    showMFASection(status.enabled ? "mfa-on" : "mfa-off");
  } catch (error) {
    handleError(error, $("mfa-settings-error"));
  }
}

async function postMFA(path, body) {
  const response = await api(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (response.status === 429) throw new Error("Thử quá nhiều lần. Đợi vài phút rồi thử lại.");
  if (!response.ok) throw new Error(await mfaProblemText(response));
  return response.status === 204 ? {} : response.json();
}

async function mfaProblemText(response) {
  try {
    const body = await response.json();
    return (
      {
        recent_auth_required: "Mật khẩu không đúng.",
        invalid_mfa_code: "Mã không đúng. Dùng mã đang hiện trong app Mật khẩu.",
        mfa_already_enabled: "Xác thực 2 bước đã được bật.",
        mfa_enrollment_missing: "Hãy bấm 'Bắt đầu bật' lại.",
      }[body.code] || body.detail || "Lỗi " + response.status
    );
  } catch {
    return "Lỗi " + response.status;
  }
}

async function startMFA() {
  $("mfa-settings-error").textContent = "";
  try {
    const enrollment = await postMFA("/v1/auth/mfa/enroll", { password: $("mfa-password").value });
    $("mfa-password").value = "";
    $("mfa-otpauth").href = enrollment.otpauth_uri;
    $("mfa-secret").textContent = enrollment.secret;
    $("mfa-qr").hidden = !enrollment.qr_png_base64;
    if (enrollment.qr_png_base64) $("mfa-qr").src = "data:image/png;base64," + enrollment.qr_png_base64;
    showMFASection("mfa-setup");
  } catch (error) {
    handleError(error, $("mfa-settings-error"));
  }
}

function showRecoveryCodes(codes, signedOut) {
  $("mfa-codes-list").textContent = codes.join("\n");
  $("mfa-relogin-note").hidden = !signedOut;
  $("mfa-relogin").hidden = !signedOut;
  showMFASection("mfa-codes");
}

async function confirmMFA() {
  $("mfa-settings-error").textContent = "";
  try {
    const result = await postMFA("/v1/auth/mfa/confirm", { totp_code: $("mfa-confirm-code").value.trim() });
    $("mfa-confirm-code").value = "";
    // Enabling MFA signs out every device, including this one, so the
    // recovery codes are shown before asking for a new sign-in.
    credentials.clear();
    $("mfa-settings-status").textContent = "Đã bật ✓";
    showRecoveryCodes(result.recovery_codes, true);
  } catch (error) {
    handleError(error, $("mfa-settings-error"));
  }
}

async function rotateRecoveryCodes() {
  $("mfa-settings-error").textContent = "";
  try {
    const result = await postMFA("/v1/auth/mfa/recovery", { totp_code: $("mfa-manage-code").value.trim() });
    $("mfa-manage-code").value = "";
    showRecoveryCodes(result.recovery_codes, false);
  } catch (error) {
    handleError(error, $("mfa-settings-error"));
  }
}

async function disableMFA() {
  if (!confirm("Tắt xác thực 2 bước? Mọi thiết bị sẽ bị đăng xuất.")) return;
  $("mfa-settings-error").textContent = "";
  try {
    await postMFA("/v1/auth/mfa/disable", { totp_code: $("mfa-manage-code").value.trim() });
    credentials.clear();
    showLogin();
    $("login-error").textContent = "Đã tắt xác thực 2 bước. Hãy đăng nhập lại.";
  } catch (error) {
    handleError(error, $("mfa-settings-error"));
  }
}

// ---------------------------------------------------------------- wiring

document.addEventListener("DOMContentLoaded", () => {
  applyGridColumns();
  enableDragSelect(library.collection);
  enableDragSelect(collectionView.collection);
  enableViewerGestures();
  enableTrimGestures();
  enableDropZone();

  $("login-form").addEventListener("submit", submitLogin);
  $("mfa-form").addEventListener("submit", submitMFA);

  $("library-select").addEventListener("click", () => (selection.active ? endSelection() : startSelection(library.collection)));
  $("collection-select").addEventListener("click", () => (selection.active ? endSelection() : startSelection(collectionView.collection)));
  $("library-sort").addEventListener("click", () => library.openSortSheet());
  $("library-search-toggle").addEventListener("click", () => {
    const bar = $("library-search-bar");
    bar.hidden = !bar.hidden;
    if (!bar.hidden) $("library-search").focus();
    else if (library.search) {
      library.search = "";
      $("library-search").value = "";
      library.open();
    }
  });
  $("library-search").addEventListener("input", () => {
    clearTimeout(searchTimer);
    searchTimer = setTimeout(() => {
      library.search = $("library-search").value.trim();
      library.open();
    }, 350);
  });
  $("select-done").addEventListener("click", () => {
    if (picking) {
      const album = cancelPicking();
      endSelection();
      go("album/" + album.id);
      return;
    }
    endSelection();
  });
  $("select-all").addEventListener("click", async () => {
    const collection = selection.collection;
    if (!collection) return;
    const everything = collection.done && selection.ids.size >= collection.items.length;
    if (everything) {
      for (const id of [...selection.ids]) setSelected(id, false);
    } else {
      $("select-count").textContent = "Đang chọn…";
      await collection.loadAll();
      for (const item of collection.items) setSelected(item.id, true);
    }
    updateSelectionBar();
  });
  $("albums-new").addEventListener("click", () => openNewMenu());
  $("shared-new").addEventListener("click", () => createSharedAlbum());

  $("viewer-close").addEventListener("click", () => {
    if (!$("trimmer").hidden) {
      exitTrim();
      showItem(viewer.index);
    } else closeViewer();
  });
  $("viewer-menu").addEventListener("click", openViewerMenu);
  $("viewer-prev").addEventListener("click", () => viewerStep(-1));
  $("viewer-next").addEventListener("click", () => viewerStep(1));
  $("viewer-live").addEventListener("click", playLive);

  $("sheet-close").addEventListener("click", closeSheet);
  $("sheet-backdrop").addEventListener("click", (event) => {
    if (event.target === $("sheet-backdrop")) closeSheet();
  });

  $("file-input").addEventListener("change", (event) => {
    const files = Array.from(event.target.files || []);
    event.target.value = "";
    uploadFiles(files);
  });
  $("folder-input").addEventListener("change", (event) => {
    const files = Array.from(event.target.files || []);
    event.target.value = "";
    uploadFiles(files);
  });

  $("save-name").addEventListener("click", saveDisplayName);
  $("create-key").addEventListener("click", createKey);
  $("copy-key").addEventListener("click", (event) => copyText("new-key-value", event.currentTarget));
  $("copy-url").addEventListener("click", (event) => copyText("upload-url", event.currentTarget));
  $("logout").addEventListener("click", logout);
  $("mfa-start").addEventListener("click", startMFA);
  $("mfa-confirm").addEventListener("click", confirmMFA);
  $("mfa-new-codes").addEventListener("click", rotateRecoveryCodes);
  $("mfa-disable").addEventListener("click", disableMFA);
  $("mfa-copy-secret").addEventListener("click", (event) => copyText("mfa-secret", event.currentTarget));
  $("mfa-copy-codes").addEventListener("click", (event) => copyText("mfa-codes-list", event.currentTarget));
  $("mfa-relogin").addEventListener("click", () => {
    showMFASection(null);
    showLogin();
  });

  for (const tab of document.querySelectorAll(".tab")) {
    tab.addEventListener("click", () => go(tabViews[tab.dataset.tab]));
  }
  window.addEventListener("hashchange", route);
  window.addEventListener("online", () => ($("offline").hidden = true));
  window.addEventListener("offline", () => ($("offline").hidden = false));
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) refreshActiveTab();
  });
  window.addEventListener("focus", refreshActiveTab);
  window.addEventListener("pageshow", (event) => {
    if (event.persisted) refreshActiveTab();
  });
  $("offline").hidden = navigator.onLine;
  route();
});
