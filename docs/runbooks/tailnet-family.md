# Private Family Access with Tailscale (no domain, no Apple fee)

This path replaces the domain, Cloudflare Tunnel and the App Store/TestFlight
app with three free pieces:

1. **Tailscale** puts the server and each family phone on one private network
   and gives the server an HTTPS name such as
   `https://family-photos.<tailnet>.ts.net`. Nothing is exposed to the public
   Internet.
2. The **web app** at `/app/` signs in, shows the library, plays Live Photos,
   downloads originals and shows upload progress.
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

   It prints the web app address. The key is only needed for this first start;
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

## What is and is not an original

- The server never resizes, transcodes or recompresses anything. Every upload
  is verified against the SHA-256 the phone computed, and downloads return the
  same bytes.
- What iOS hands to the uploader is outside the server's control:
  - **Shortcut from the share sheet**: photos arrive as files. Live Photos are
    passed as the still only.
  - **Web page file picker**: iOS may convert photos and **compresses videos**
    picked from the Photos library. Use the Shortcut for videos.
- Check once on a real iPhone: upload a photo and a video with the Shortcut,
  download them from the web app, and compare size/format with Photos → ⓘ.

## Live Photos

The web app shows a still and a video with the same base name (for example
`IMG_1234.HEIC` and `IMG_1234.MOV`) uploaded within 10 minutes as one **LIVE**
item that plays on tap, with separate downloads for both parts. Getting the
motion part out of iOS through Shortcuts must still be confirmed on a device;
the share sheet sends only the still.

## Operations

- Lost phone: `make revoke-sessions EMAIL=…` (signs out the web app and revokes
  that person's Shortcut keys), or revoke one key in **Cài đặt**.
- Remove a phone from the tailnet in the Tailscale admin console.
- Upload keys can only upload. A stolen key cannot read anyone's photos.
