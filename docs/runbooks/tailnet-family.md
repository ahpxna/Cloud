# Private Family Access with Tailscale (no domain, no Apple fee)

This path replaces the domain, Cloudflare Tunnel and the App Store/TestFlight
app with three free pieces:

1. **Tailscale** puts the server and each family phone on one private network
   and gives the server an HTTPS name such as
   `https://family-photos.<tailnet>.ts.net`. Nothing is exposed to the public
   Internet.
2. The **web app** at `/app/` works like the iPhone Photos app: timeline by
   date taken, albums and folders, albums shared with the family (likes,
   comments, contributions), favourites, hidden items, a 30-day Recently
   Deleted, places, Live Photos, cutting part of a video, saving to the Photos
   app, ZIP downloads, and upload progress with ✕ to stop a stuck upload.
3. An **iOS Shortcut** appears in the Photos share sheet (and on the Home
   Screen with a menu) and uploads originals with an upload-only key.

## One-time server setup

1. Create a free Tailscale account. In the admin console:
   - **DNS** → enable **MagicDNS** and **HTTPS Certificates**.
   - **Settings → Keys** → generate an auth key (one-time use is fine).
2. Put the key in `.env` as `TAILSCALE_AUTHKEY=` (optionally change
   `TAILSCALE_HOSTNAME`), then:

   ```bash
   make tailnet-up
   ```

   It prints the web app address and records it as `CANONICAL_HOST`, so
   `http://…` and the short name `http://family-photos/` redirect to it over
   HTTPS (the certificate covers only the full name). The key is only needed for this first start;
   the node identity is kept in `.data/tailscale` (back it up with the host, do
   not commit it). You may clear `TAILSCALE_AUTHKEY` afterwards.
3. In the admin console **Machines** list, open the server → **Disable key
   expiry**, otherwise the server drops off the tailnet after 180 days.
4. Create each family account on the server: `make create-user EMAIL=…`.

## On each family iPhone (once)

1. Install **Tailscale** from the App Store and sign in with your Tailscale
   account (or invite them as a tailnet user; the free plan covers six).
2. Tailscale → profile picture → **VPN On Demand** → set Wi-Fi and Cellular to
   **Always**, so the phone reconnects after restarts and updates. Ask them not
   to switch the VPN off.
3. Open the web app address in **Safari**, sign in, and (optional) Share →
   **Add to Home Screen**. A Home Screen web app keeps its own sign-in, separate
   from Safari.
4. In the web app: **Cài đặt → Tạo khoá cho Phím tắt**, then build the
   Shortcut by following the on-screen steps (the address and key are filled
   in for that phone).
5. Test: Photos → select a photo → Share → **Ảnh nhà**, then open the
   **Tải lên** tab and wait for **Đã sao lưu ✓**.

## Two-step sign-in (optional, recommended for the operator)

In the web app: **Cài đặt → Xác thực 2 bước → Bắt đầu bật**, enter the current
password, then on the same iPhone tap **Thêm vào app Mật khẩu** (no QR scan
needed; the QR is for enrolling from a computer), enter the 6-digit code, and
save the recovery codes. Every device is then signed out; sign in again and
Safari fills the code from the Passwords app. Tailnet-only access already keeps
strangers out, so MFA is optional for parents.

## Sharing between family members

Every person gets their own account (`make create-user EMAIL=…`) and sets a
display name in **Cài đặt → Tên của bạn**. Libraries stay private. To share,
create an album in **Chia sẻ → ＋** (or **⋯ → Chia sẻ với người trong nhà** on an
existing album) and tick people. Members see the album, like and comment, and
add their own photos unless the owner turned that off. Removing someone (or
them leaving) takes back their access and the photos they added. New activity
shows as a dot on the **Chia sẻ** tab.

## Recently Deleted and hidden items

Deleting moves items to **Album → Đã xoá gần đây**, where they stay 30 days
(the gateway purges older ones hourly) and can be restored. Sending the same
photo again also restores it. **Đã ẩn** keeps items out of the library and every
album. Permanent deletion removes the original from the media volume; older
restic snapshots still contain it until they are pruned.

## What is and is not an original

- The server never resizes, transcodes or recompresses anything. Every upload
  is verified against the SHA-256 the phone computed, and downloads return the
  same bytes. Cutting a video copies the original's frames without re-encoding.
- What iOS hands to the uploader is outside the server's control. Checked on
  an iPhone 14 Pro (iOS 26.5): the **Shortcut from the share sheet** sends a
  converted **JPEG** (not the HEIC), only the **still** of a Live Photo, and
  **without GPS**; videos arrive as an exported copy (same capture time, other
  bytes, location kept). The **web page file picker** may convert photos and
  **compresses videos**.
- For true originals (HEIC, Live Photo video, location): on a Mac, select the
  photos in Photos → **File → Export → Export Unmodified Originals**, then
  upload the folder in **Tải lên → Tải cả thư mục** (or drag it in). The server
  pairs each Live Photo's still and video by Apple's content identifier.

## Live Photos

A still and its motion video show as one item with **◎ LIVE**: press and hold
the photo (or tap LIVE) to play it. They are paired by the content identifier
both files carry, or for older uploads by base name (`IMG_1234.HEIC` +
`IMG_1234.MOV`) within 10 minutes. The share sheet sends only the still, so a
Live Photo needs the export above to keep its motion.

## Slow videos over Tailscale

`tailscale ping <phone>` from the server shows whether traffic is direct or
relayed (`via DERP(nyc)`). A relay works everywhere but is slow. On Docker
Desktop (macOS) the container sits behind two NATs and usually stays relayed.
On the always-on Linux host, prefer a direct path: run the `tailscale`
service with `network_mode: host` (or publish `41641/udp` and start
tailscaled with `--port=41641`), and allow UDP 41641 through the router. When
the phone is on the same Wi-Fi as the server, a direct LAN path is normal.

## Time zone

Photos store their capture time with an offset (iPhone does), and the web app
shows them in the viewer's time zone. Files without an offset are read in
`PHOTO_TIMEZONE` (an IANA name such as `Asia/Ho_Chi_Minh`; set it in `.env`
when the server's zone differs from the family's).

## Operations

- Lost phone: `make revoke-sessions EMAIL=…` (signs out the web app and revokes
  that person's Shortcut keys), or revoke one key in **Cài đặt**.
- Remove a phone from the tailnet in the Tailscale admin console.
- Upload keys can only upload. A stolen key cannot read anyone's photos.
