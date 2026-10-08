"use strict";

// Family Photo Cloud web app: sign in, browse and download originals, upload
// with progress, and manage upload-only keys for the iOS Shortcut.
// No third-party code; the page is served with a script-src 'self' CSP and all
// server-provided text is rendered with textContent, never as HTML.

const CREDENTIAL_KEY = "fpc.credential.v1";
const PAGE_SIZE = 60;
const TICKET_MAX_AGE_MS = 9 * 60 * 1000;
const SUBTLE_HASH_LIMIT = 128 * 1024 * 1024;
// A Live Photo is a still plus a short video with the same base name (for
// example IMG_1234.HEIC + IMG_1234.MOV); pair them when uploaded close together.
const LIVE_PAIR_WINDOW_MS = 10 * 60 * 1000;
// Thumbnails are rendered once by the browser (Safari decodes HEIC and video
// frames), uploaded, and reused by every device afterwards.
const THUMBNAIL_MAX_SIDE = 400;
const THUMBNAIL_CONCURRENCY = 2;

const $ = (id) => document.getElementById(id);

// ---------------------------------------------------------------- credentials

const credentials = {
  load() {
    try {
      return JSON.parse(localStorage.getItem(CREDENTIAL_KEY) || "null");
    } catch {
      return null;
    }
  },
  save(value) {
    try {
      localStorage.setItem(CREDENTIAL_KEY, JSON.stringify(value));
    } catch {
      // Private mode: the session still works until the tab closes.
    }
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

async function problemText(response) {
  try {
    const body = await response.json();
    return body.detail || body.code || "Lỗi " + response.status;
  } catch {
    return "Lỗi " + response.status;
  }
}

function handleError(error, target) {
  if (error instanceof SignedOut) {
    showLogin();
    return;
  }
  if (target) target.textContent = navigator.onLine ? error.message || String(error) : "Không có mạng. Thử lại sau.";
}

// ---------------------------------------------------------------- navigation

const views = ["login", "library", "uploads", "settings"];
let activeTab = null;
let uploadsTimer = null;
let lastAutoRefresh = 0;

function showView(name) {
  for (const view of views) $("view-" + view).hidden = view !== name;
  $("tabs").hidden = name === "login";
  for (const tab of document.querySelectorAll(".tab")) {
    if (tab.dataset.tab === name) tab.setAttribute("aria-current", "page");
    else tab.removeAttribute("aria-current");
  }
}

function showLogin() {
  activeTab = null;
  clearInterval(uploadsTimer);
  $("login-form").hidden = false;
  $("mfa-form").hidden = true;
  showView("login");
}

function openTab(name) {
  if (!views.includes(name) || name === "login") name = "library";
  if (!currentCredential()) {
    showLogin();
    return;
  }
  activeTab = name;
  if (location.hash !== "#" + name) history.replaceState(null, "", "#" + name);
  closeViewer();
  showView(name);
  clearInterval(uploadsTimer);
  // Photos arrive from the Shortcut at any time; every tab switch reloads.
  lastAutoRefresh = Date.now();
  loadTabData(name);
  if (name === "uploads") {
    uploadsTimer = setInterval(() => {
      if (!document.hidden) refreshServerUploads();
    }, 5000);
  }
}

function loadTabData(name, { keepOpenWork = false } = {}) {
  if (name === "library") library.reload();
  if (name === "uploads") refreshServerUploads();
  if (name === "settings") {
    refreshKeys();
    // Don't wipe an MFA setup or recovery codes the person may be copying
    // into another app when they come back.
    if (!keepOpenWork || ($("mfa-setup").hidden && $("mfa-codes").hidden)) refreshMFA();
  }
}

// Coming back to the page (from the Shortcut, Photos or another app, or when
// a Shortcut opens the same address) refreshes what is on screen.
function refreshActiveTab() {
  if (!activeTab || !currentCredential() || Date.now() - lastAutoRefresh < 1500) return;
  if (activeTab === "library" && !$("viewer").hidden) return;
  lastAutoRefresh = Date.now();
  loadTabData(activeTab, { keepOpenWork: true });
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
  openTab(location.hash.slice(1) || "library");
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
  openTab(location.hash.slice(1) || "library");
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
  library.reset();
  showLogin();
}

// ---------------------------------------------------------------- library

const library = {
  assets: [],
  cursor: null,
  loadedAt: 0,
  loading: false,
  generation: 0,

  reset() {
    // A newer generation makes any in-flight page load discard its result.
    this.generation += 1;
    thumbnailQueue.length = 0;
    pendingThumbnailTiles.clear();
    this.loading = false;
    this.assets = [];
    this.cursor = null;
    this.loadedAt = 0;
    $("grid").replaceChildren();
  },

  reload() {
    this.reset();
    this.loadMore();
  },

  async loadMore() {
    if (this.loading) return;
    const generation = this.generation;
    this.loading = true;
    $("library-error").textContent = "";
    try {
      const query = new URLSearchParams({ limit: String(PAGE_SIZE), tickets: "1" });
      if (this.cursor) query.set("cursor", this.cursor);
      const response = await api("/v1/assets?" + query);
      if (!response.ok) throw new Error(await problemText(response));
      const page = await response.json();
      if (generation !== this.generation) return;
      if (!this.cursor) this.loadedAt = Date.now();
      this.cursor = page.next_cursor || null;
      for (const asset of page.assets) this.assets.push(asset);
      $("grid").replaceChildren(...groupLivePhotos(this.assets).map(tileFor));
      $("library-empty").hidden = this.assets.length > 0;
      $("load-more").hidden = !this.cursor;
    } catch (error) {
      if (generation === this.generation) handleError(error, $("library-error"));
    } finally {
      if (generation === this.generation) this.loading = false;
    }
  },
};

function isVideo(asset) {
  return asset.media_type.startsWith("video/");
}

function baseName(name) {
  const dot = name.lastIndexOf(".");
  return (dot > 0 ? name.slice(0, dot) : name).toLowerCase();
}

// groupLivePhotos attaches a Live Photo's motion video to its still so the
// grid shows one tile. Everything else passes through unchanged.
function groupLivePhotos(assets) {
  for (const asset of assets) delete asset.liveVideo;
  const stills = new Map();
  for (const asset of assets) {
    if (!isVideo(asset)) {
      const key = baseName(asset.original_filename);
      if (!stills.has(key)) stills.set(key, []);
      stills.get(key).push(asset);
    }
  }
  const paired = new Set();
  for (const video of assets) {
    if (!isVideo(video)) continue;
    const candidates = stills.get(baseName(video.original_filename)) || [];
    const still = candidates.find(
      (candidate) =>
        !candidate.liveVideo &&
        Math.abs(new Date(candidate.created_at) - new Date(video.created_at)) <= LIVE_PAIR_WINDOW_MS,
    );
    if (still) {
      still.liveVideo = video;
      paired.add(video.id);
    }
  }
  return assets.filter((asset) => !paired.has(asset.id));
}

function tileFor(asset) {
  const tile = document.createElement("button");
  tile.className = "tile";
  tile.type = "button";
  tile.setAttribute("aria-label", asset.original_filename);
  const placeholder = document.createElement("span");
  placeholder.className = "placeholder";
  placeholder.textContent = isVideo(asset) ? "🎬" : "🖼";
  // Thumbnails are a few tens of KB; load them eagerly. (A lazy image that
  // starts hidden would never load.)
  const image = document.createElement("img");
  image.decoding = "async";
  image.alt = "";
  image.hidden = true;
  image.addEventListener("load", () => {
    image.hidden = false;
    placeholder.hidden = true;
  });
  image.addEventListener("error", () => {
    image.hidden = true;
  });
  tile.append(placeholder, image);
  if (asset.thumbnail_url) {
    image.src = asset.thumbnail_url;
  } else {
    tile.assetRecord = asset;
    pendingThumbnailTiles.add(tile);
    scheduleThumbnailScan();
  }
  if (isVideo(asset)) {
    const play = document.createElement("span");
    play.className = "play";
    play.textContent = "▶";
    tile.append(play);
  }
  if (asset.liveVideo) {
    const live = document.createElement("span");
    live.className = "live";
    live.textContent = "LIVE";
    tile.append(live);
  }
  const label = document.createElement("span");
  label.className = "label";
  label.textContent = asset.original_filename;
  tile.append(label);
  tile.addEventListener("click", () => openViewer(asset));
  return tile;
}

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
      if (rect.bottom >= -300 && rect.top <= bottom) {
        pendingThumbnailTiles.delete(tile);
        thumbnailQueue.push(tile);
      }
    }
    pumpThumbnails();
  }, 100);
}

window.addEventListener("scroll", scheduleThumbnailScan, { passive: true });
window.addEventListener("resize", scheduleThumbnailScan);

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
    source = isVideo(asset) ? await videoFrame(asset.view_url) : await decodedImage(asset.view_url);
    const blob = await renderThumbnail(source);
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
    // keep the placeholder and let another device create the thumbnail.
  } finally {
    if (source instanceof HTMLVideoElement) {
      source.removeAttribute("src");
      source.load();
    } else if (source) {
      source.src = "";
    }
  }
}

function decodedImage(url) {
  const image = new Image();
  image.decoding = "async";
  image.src = url;
  return image.decode().then(() => image);
}

function videoFrame(url) {
  return new Promise((resolve, reject) => {
    const video = document.createElement("video");
    const timer = setTimeout(() => reject(new Error("video frame timeout")), 20000);
    video.muted = true;
    video.playsInline = true;
    video.preload = "auto";
    video.addEventListener("error", () => reject(new Error("video decode failed")), { once: true });
    video.addEventListener(
      "loadeddata",
      () => {
        video.currentTime = Math.min(0.5, (video.duration || 1) / 2);
      },
      { once: true },
    );
    video.addEventListener(
      "seeked",
      () => {
        clearTimeout(timer);
        resolve(video);
      },
      { once: true },
    );
    video.src = url;
  });
}

function renderThumbnail(source) {
  const width = source.naturalWidth || source.videoWidth;
  const height = source.naturalHeight || source.videoHeight;
  if (!width || !height) return Promise.reject(new Error("no dimensions"));
  const scale = Math.min(1, THUMBNAIL_MAX_SIDE / Math.max(width, height));
  const canvas = document.createElement("canvas");
  canvas.width = Math.max(1, Math.round(width * scale));
  canvas.height = Math.max(1, Math.round(height * scale));
  canvas.getContext("2d").drawImage(source, 0, 0, canvas.width, canvas.height);
  return new Promise((resolve, reject) =>
    canvas.toBlob((blob) => (blob ? resolve(blob) : reject(new Error("encode failed"))), "image/jpeg", 0.8),
  );
}

async function openViewer(asset) {
  if (Date.now() - library.loadedAt > TICKET_MAX_AGE_MS) {
    library.reset();
    await library.loadMore();
    asset = library.assets.find((candidate) => candidate.id === asset.id) || asset;
  }
  const body = $("viewer-body");
  body.replaceChildren();
  $("viewer-name").textContent = asset.original_filename;
  const download = $("viewer-download");
  download.href = asset.view_url;
  download.setAttribute("download", asset.original_filename);
  const liveButton = $("viewer-live");
  const liveDownload = $("viewer-download-live");
  liveButton.hidden = !asset.liveVideo;
  liveDownload.hidden = !asset.liveVideo;
  if (asset.liveVideo) {
    liveDownload.href = asset.liveVideo.view_url;
    liveDownload.setAttribute("download", asset.liveVideo.original_filename);
    liveButton.onclick = () => playLive(asset);
  }
  if (isVideo(asset)) {
    const video = document.createElement("video");
    video.controls = true;
    video.playsInline = true;
    video.preload = "metadata";
    video.src = asset.view_url;
    body.append(video);
  } else {
    const image = document.createElement("img");
    image.alt = asset.original_filename;
    image.src = asset.view_url;
    image.addEventListener(
      "error",
      () => {
        const note = document.createElement("p");
        note.textContent = "Trình duyệt này không hiển thị được định dạng ảnh. Bấm 'Tải về' để mở bằng app Ảnh.";
        body.replaceChildren(note);
      },
      { once: true },
    );
    body.append(image);
  }
  $("viewer").hidden = false;
}

function playLive(asset) {
  const body = $("viewer-body");
  const video = document.createElement("video");
  video.src = asset.liveVideo.view_url;
  video.autoplay = true;
  video.playsInline = true;
  video.controls = false;
  video.addEventListener("ended", () => openViewer(asset), { once: true });
  body.replaceChildren(video);
}

function closeViewer() {
  $("viewer").hidden = true;
  $("viewer-body").replaceChildren();
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

function formatBytes(bytes) {
  if (bytes < 1024 * 1024) return Math.max(1, Math.round(bytes / 1024)) + " KB";
  if (bytes < 1024 * 1024 * 1024) return (bytes / 1024 / 1024).toFixed(1) + " MB";
  return (bytes / 1024 / 1024 / 1024).toFixed(2) + " GB";
}

function uploadRow(name, stateText, stateClass, progress) {
  const item = document.createElement("li");
  const row = document.createElement("div");
  row.className = "row";
  const title = document.createElement("span");
  title.className = "name";
  title.textContent = name;
  const state = document.createElement("span");
  state.className = "state " + stateClass;
  state.textContent = stateText;
  row.append(title, state);
  item.append(row);
  if (progress !== undefined) {
    const bar = document.createElement("progress");
    bar.max = 1;
    bar.value = progress;
    item.append(bar);
  }
  return { item, state, bar: item.querySelector("progress") };
}

async function refreshServerUploads() {
  try {
    const response = await api("/v1/upload-sessions?limit=50");
    if (!response.ok) throw new Error(await problemText(response));
    const body = await response.json();
    const rows = body.uploads.map((upload) => {
      const [text, kind] = stateLabels[upload.state] || [upload.state, "busy"];
      const fraction = upload.expected_size ? upload.received_size / upload.expected_size : 0;
      const showBar = upload.state === "uploading";
      return uploadRow(
        upload.original_filename,
        text + (showBar ? " " + Math.floor(fraction * 100) + "%" : "") + " · " + formatBytes(upload.expected_size),
        kind,
        showBar ? fraction : undefined,
      ).item;
    });
    $("server-uploads").replaceChildren(...rows);
    const known = new Set(body.uploads.map((upload) => upload.id));
    for (const local of $("local-uploads").querySelectorAll("li[data-session-id]")) {
      if (known.has(local.dataset.sessionId)) local.remove();
    }
    if (!rows.length) {
      const empty = document.createElement("li");
      empty.className = "muted";
      empty.textContent = "Chưa có lần tải lên nào.";
      $("server-uploads").replaceChildren(empty);
    }
    $("uploads-error").textContent = "";
  } catch (error) {
    handleError(error, $("uploads-error"));
  }
}

async function uploadFiles(files) {
  for (const file of files) {
    const row = uploadRow(file.name, "Đang tính mã kiểm tra…", "busy", 0);
    $("local-uploads").prepend(row.item);
    try {
      const digest = await sha256Hex(file, (fraction) => {
        row.bar.value = fraction * 0.2;
      });
      let result;
      for (let attempt = 0; attempt < 5; attempt++) {
        row.state.textContent = "Đang tải lên…";
        result = await sendFile(file, digest, (fraction) => {
          row.bar.value = 0.2 + fraction * 0.8;
          row.state.textContent = "Đang tải lên " + Math.floor(fraction * 100) + "%";
        });
        if (result.status !== 429) break;
        row.state.textContent = "Đang chờ máy chủ…";
        await sleep((result.retryAfter || 5) * 1000);
      }
      if (result.status === 200 || result.status === 202) {
        // From here the server's own row under "Gần đây" tracks this file
        // (checking → Đã sao lưu ✓); the local row is removed once it appears.
        row.item.dataset.sessionId = result.id;
        row.state.textContent = result.status === 200 ? "Đã có sẵn ✓" : "Đã gửi, đang kiểm tra";
        if (result.status === 200) row.state.className = "state ok";
      } else {
        throw new Error(result.detail || "Lỗi " + result.status);
      }
      row.bar.value = 1;
    } catch (error) {
      if (error instanceof SignedOut) {
        showLogin();
        return;
      }
      row.state.textContent = "Lỗi: " + (error.message || error);
      row.state.className = "state bad";
    }
    refreshServerUploads();
  }
}

function sendFile(file, digest, onProgress) {
  return accessToken().then(
    (token) =>
      new Promise((resolve, reject) => {
        const request = new XMLHttpRequest();
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

// ---------------------------------------------------------------- settings

async function refreshKeys() {
  try {
    const response = await api("/v1/auth/upload-keys");
    if (!response.ok) throw new Error(await problemText(response));
    const body = await response.json();
    const rows = body.upload_keys.map((key) => {
      const item = document.createElement("li");
      const row = document.createElement("div");
      row.className = "row";
      const name = document.createElement("span");
      name.className = "name";
      name.textContent = key.name;
      const revoke = document.createElement("button");
      revoke.className = "secondary";
      revoke.textContent = "Thu hồi";
      revoke.addEventListener("click", () => revokeKey(key));
      row.append(name, revoke);
      const used = document.createElement("span");
      used.className = "muted";
      used.textContent = key.last_used_at
        ? "Dùng lần cuối: " + new Date(key.last_used_at).toLocaleString("vi-VN")
        : "Chưa dùng";
      item.append(row, used);
      return item;
    });
    if (!rows.length) {
      const empty = document.createElement("li");
      empty.className = "muted";
      empty.textContent = "Chưa có khoá nào.";
      rows.push(empty);
    }
    $("key-list").replaceChildren(...rows);
  } catch (error) {
    handleError(error, $("key-error"));
  }
}

async function createKey() {
  $("key-error").textContent = "";
  $("create-key").disabled = true;
  try {
    const response = await api("/v1/auth/upload-keys", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name: $("key-name").value.trim() || "iPhone" }),
    });
    if (!response.ok) throw new Error(await problemText(response));
    const body = await response.json();
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
    const selection = getSelection();
    selection.removeAllRanges();
    selection.addRange(range);
  }
}

// ---------------------------------------------------------------- MFA settings

function showMFASection(name) {
  for (const section of ["mfa-off", "mfa-setup", "mfa-codes", "mfa-on"]) $(section).hidden = section !== name;
}

async function refreshMFA() {
  $("mfa-settings-error").textContent = "";
  try {
    const response = await api("/v1/auth/mfa");
    if (!response.ok) throw new Error(await problemText(response));
    const status = await response.json();
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
  $("login-form").addEventListener("submit", submitLogin);
  $("mfa-form").addEventListener("submit", submitMFA);
  $("load-more").addEventListener("click", () => library.loadMore());
  $("viewer-close").addEventListener("click", closeViewer);
  $("file-input").addEventListener("change", (event) => {
    const files = Array.from(event.target.files || []);
    event.target.value = "";
    uploadFiles(files);
  });
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
    tab.addEventListener("click", () => openTab(tab.dataset.tab));
  }
  window.addEventListener("hashchange", () => openTab(location.hash.slice(1)));
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
  openTab(location.hash.slice(1) || "library");
});
