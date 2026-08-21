# Báo cáo tiến độ Family Photo Cloud

Ngày chốt: 2026-08-20  
Trạng thái: tiếp tục phát triển, chưa sẵn sàng production/App Store

## 1. Kết luận ngắn

Dự án đã đi từ tài liệu kiến trúc sang một backend vertical slice có code thật:

- tài khoản family dạng invite/admin-only;
- password Argon2id;
- access JWT ngắn hạn và refresh token opaque được hash, rotate khi dùng;
- gateway Go nhúng `tusd` v2.10.0;
- upload TUS có resume, owner isolation và giới hạn concurrency;
- kiểm tra lại byte count + SHA-256 từ file trên disk;
- quarantine khi sai integrity;
- commit không overwrite và có đường recovery sau process/database interruption;
- Compose profile riêng cho local gateway và Cloudflare Tunnel, với network
  segmentation để `cloudflared` không truy cập PostgreSQL.
- verified library listing và authorized original download có HTTP Range.

Đây chưa phải sản phẩm hoàn thiện. Đã có scaffold iOS/Share Extension và
asset listing/download + signed-manifest CLI, nhưng chưa thể build trên máy này
vì không có full Xcode. Thumbnail, manifest scheduler, backup, restore drill và
lần deploy Cloudflare thật vẫn chưa có.

## 2. Quyết định kỹ thuật đã chốt

### Upload protocol

Không tự phát minh resumable protocol. MVP dùng:

- tus 1.0;
- `tusd` v2.10.0 embedded trong gateway;
- TUSKit cho iOS;
- PATCH tối đa 32 MiB để nằm dưới Cloudflare Free/Pro request-body limit;
- hai PATCH đồng thời mỗi user, sáu PATCH toàn hệ thống;
- `HEAD` lấy offset rồi resume đúng phần còn thiếu.

Gateway tự sở hữu phần quan trọng của sản phẩm: authentication, authorization,
session state, expected SHA-256, durable commit, quarantine và audit events.

### Integrity

- Client phải gửi exact byte size và SHA-256 trước upload.
- Upload hoàn tất ở tầng TUS chưa làm asset visible.
- Origin đọc lại file, tính SHA-256 và so byte count.
- File sai bị chuyển vào `.quarantine` và session thành `quarantined`.
- File đúng được `fsync`, tạo hard link cùng filesystem tại content-addressed
  destination mà không replace file cũ, `fsync` directory, unlink staging, rồi
  mới commit metadata `available` trong PostgreSQL.
- Nếu destination đã tồn tại, backend re-hash destination trước khi deduplicate.
- Staging và originals bắt buộc nằm trên cùng media filesystem có hard-link và
  directory-fsync semantics; Linux nên dùng ext4/XFS, không dùng FAT/exFAT.

### Authentication

- Không có public signup.
- Admin tạo user local bằng `make create-user EMAIL=...`.
- Argon2id hiện dùng `m=65536 KiB, t=3, p=1`, salt 16 bytes, output 32 bytes.
- Access token TTL 15 phút, xác thực issuer/audience/algorithm cố định HS256.
- Refresh token 32 random bytes, chỉ SHA-256 của token được lưu trong database,
  TTL 30 ngày và rotate mỗi lần refresh.
- Access token bị revoke có thể còn hiệu lực tối đa 15 phút; đây là trade-off MVP.

### Network/public access

- MVP: Cloudflare Tunnel, không cần public IP và hoạt động qua CGNAT.
- DNS hostname thuộc Cloudflare zone và route vào `http://upload-gateway:8080`.
- `cloudflared` chỉ nằm trên ingress + egress network; PostgreSQL nằm ở network
  khác và connector không join network đó.
- Cloudflare terminate TLS nên nằm trong trust boundary của MVP; đây không phải
  E2EE tuyệt đối.
- Dài hạn: public IP trực tiếp hoặc VPS Việt Nam + WireGuard + HAProxy TCP
  passthrough. Cách này là end-to-origin TLS, không phải app-level E2EE và VPS
  vẫn thấy IP/timing/traffic volume.

## 3. Thành phần đã tạo

### Backend

- `cmd/upload-gateway`: process HTTP chính, PostgreSQL connection, graceful
  shutdown và config validation.
- `cmd/admin`: CLI tạo member/admin, password đọc từ terminal/stdin.
- `internal/auth`: access-token validation và scoped upload-capability token.
- `internal/account`: Argon2id, login, refresh rotation, logout, login limiter,
  PostgreSQL repository.
- `internal/upload`: upload-session API, in-memory test repository, PostgreSQL
  repository, verifier, quarantine, durable commit và recovery.
- `internal/gateway`: TUS embedding, per-method owner checks, 32 MiB enforcement,
  per-user/global backpressure, health endpoint và security headers.
- `internal/library`: paginated per-user asset list và authenticated original
  download/Range support; storage key không bao giờ ra API.
- `internal/integrity`: canonical signed-manifest v1 format, deterministic
  sorting và Ed25519 sign/verify; scheduled job/scrubber còn pending.
- `cmd/manifest`: one-shot generator đọc verified inventory, ký Ed25519, write
  atomically, rồi record payload hash/signature trong PostgreSQL.
- `db/migrations/0001_core.sql`: users, sessions, upload sessions, assets,
  append-only upload events, integrity checks và signed-manifest inventory.

### Deployment/operations

- `Dockerfile` multi-stage, non-root runtime và admin binary.
- `.dockerignore`, `.gitignore`, `.env.example`.
- GitHub Actions CI: Go race/vet, Compose model, PostgreSQL 18 integration và
  clean image build trên Linux.
- `compose.yaml` với database, gateway, protocol-lab và edge profiles.
- `Makefile` cho config, DB, protocol lab, gateway, edge, user creation và tests.
- Architecture, threat model, four ADRs, API contract, Cloudflare/public-IP
  runbooks, upstream survey và verification status.

### PKI Sentinel reuse

Repo `/Users/phanan/pki-sentinel` không bị sửa. File untracked có sẵn
`docs/family-photo-cloud-reuse-assessment.md` vẫn nguyên trạng.

Đã reuse ý tưởng/cấu trúc, không reuse PKI product code:

- Compose/health/dependency-gate conventions;
- CI/security/release layout để dùng ở phase sau;
- signed-baseline SHA-256 + Ed25519 cho future asset manifests;
- `tc netem` chaos-test pattern cho interrupted/resumed upload;
- ADR/threat-model/runbook structure.

Không đưa Vault, Wazuh, private CA, OCSP hay AppRole user-auth vào MVP.

## 4. Kết quả validation

Các kiểm tra sau hiện pass:

- `go test -race ./...`;
- `go vet ./...`;
- `docker compose --env-file .env.example --profile gateway --profile edge config --quiet`;
- access-token algorithm/audience validation;
- Argon2id hash/verify;
- refresh rotation, old-token replay rejection và logout;
- upload-session idempotency;
- TUS upload một phần, `HEAD` thấy offset, resume phần còn lại;
- unauthenticated PATCH bị `401`;
- token user khác không thể `HEAD/PATCH` upload và nhận `404`;
- PATCH vượt configured chunk bị `413`;
- full SHA-256 match chuyển session thành `available`;
- mismatch chuyển file vào quarantine;
- recovery khi destination đã tồn tại sau interrupted commit;
- recovery khi quarantine move xong nhưng database update chưa xong.
- scoped upload capability không được dùng vào general API và không dùng được
  cho upload session khác;
- paginated asset list, original byte-range download, và cross-user download
  rejection.
- signed-manifest canonical payload không phụ thuộc record order và reject mọi
  inventory tampering sau ký.

Docker runtime chưa được chạy vì `docker pull postgres:18.4-alpine` vẫn lỗi:

```text
error getting credentials - err: exit status 1, out: `Keychain Error. (-67674)`
```

Vì vậy migration SQL, image build và full Compose stack chưa được chứng minh ở
runtime. Không có Docker credential nào bị sửa. Một embedded PostgreSQL 18
integration test cũng download binary thành công nhưng sandbox chặn `sysv`
shared memory trong `initdb`; GitHub Actions Linux chạy test này thay cho local
environment khi repo được push.

## 5. Capability-token hardening hoàn tất

Thay đổi cuối cùng đang giảm quyền của credential mà TUSKit lưu trên disk:
backend cấp capability JWT chỉ dùng được cho đúng một upload thay vì để TUSKit
persist access token tổng quát.

Middleware đã được sửa: general API chỉ chấp nhận access token; TUS chấp nhận
access token hoặc scoped capability token. `PrincipalFrom` phân biệt principal
có session (general API) với principal có exact upload ID (TUS). Toàn bộ
`go test -race ./...` và `go vet ./...` hiện pass.

## 6. iOS scaffold đã tạo nhưng chưa build

- Máy hiện chỉ active Xcode Command Line Tools; `xcodebuild` không chạy vì chưa
  cài/chọn full Xcode.
- Swift hiện có: Apple Swift 6.1.2.
- XcodeGen chưa được cài.
- Đã kiểm tra upstream TUSKit tag 3.7.1, commit ngày 2026-02-11.
- TUSKit hỗ trợ background URLSession, resume, retry và dynamic header callback,
  nhưng upstream cảnh báo chunking không lý tưởng cho background scheduling.
- Source TUSKit 3.7.1 persist `appliedCustomHeaders` trong upload metadata. Đây là
  lý do capability-token-per-upload được chọn thay vì persist access token.
- Đã tạo `ios/project.yml`, app SwiftUI và Share Extension. Extension đăng ký
  `com.apple.share-services` nên sẽ hiện trong Photos Share Sheet cho image và
  video. Nó copy `NSItemProvider` temporary file vào App Group queue và không
  đọc Keychain/không upload mạng.
- Main app scaffold dùng Keychain cho credential, hash SHA-256 stream trước
  khi create session, giữ queue không chứa bearer token tổng quát và dùng
  TUSKit background URLSession. Header TUSKit là capability theo đúng session;
  dynamic header provider refresh capability bằng access token chỉ ở memory.
- Main app có Library tab gọi `GET /v1/assets` bằng access token trong memory;
  original chỉ được tải khi mở chi tiết bằng request authenticated rồi cache
  trong sandbox app sau khi SHA-256 stream ở iPhone khớp asset digest đã verify
  ở server. Image và video đều mở từ file cache, không có stable public download
  URL. Upload queue phân biệt transfer/verify/available/quarantined và
  có nút resume/check status không gửi lại byte đã có trên origin.
- Chưa chạy `xcodegen`/`xcodebuild`; source iOS phải được compile và test trên
  device sau khi cài full Xcode, đăng ký Apple Team, bundle ID và App Group.

## 7. Việc còn lại theo thứ tự

### P0 — đưa backend về green

1. Khắc phục Docker Desktop Keychain, chạy PostgreSQL migration thật.
2. Chạy integration test với PostgreSQL repository thay vì memory repository.
3. Build image và chạy full gateway Compose profile.
4. Tạo user, login, upload file thật qua localhost, restart gateway giữa upload,
   xác nhận resume + final SHA-256.

### P1 — Cloudflare MVP

1. Mua/dùng domain trong Cloudflare DNS zone.
2. Tạo named Tunnel và scoped connector token.
3. Route duy nhất public vào gateway.
4. Chạy acceptance với ảnh/video trên 100 MB, Wi-Fi/cellular interruption,
   connector restart và cross-user attacks.
5. Xác nhận logs không chứa bearer token, filename, EXIF hay body.

### P1 — iOS/App Store

1. Cài full Xcode và chốt bundle ID, App Group, Apple Team/signing.
2. Tạo SwiftUI app + Share Extension xuất hiện trong Photos Share Sheet.
3. Extension chỉ copy resource vào App Group queue; không giữ refresh token.
4. Main app hash original, tạo upload session, đưa scoped capability vào TUSKit,
   resume/retry và poll tới `available`.
5. Refresh token lưu Keychain; queue không chứa secret tổng quát.
6. Test background trên thiết bị thật, low-storage, airplane mode, cellular/Wi-Fi
   switching, reboot và extension termination.
7. Hoàn thiện privacy manifest, App Store privacy labels, screenshots, account
   deletion flow và review notes.

### P1/P2 — sản phẩm và vận hành

- thumbnails và EXIF worker ngoài request path;
- quota/free-space gate;
- Ed25519 signed manifests và scheduled integrity scrub;
- encrypted off-site backup + restore drills;
- metrics/alerts/dashboards;
- CI, SBOM, image signing và provenance theo pattern PKI Sentinel;
- chaos tests với `tc netem`;
- benchmark Cloudflare so với VPS/WireGuard trước khi đổi ingress.

## 8. Vấn đề workspace

`/Users/phanan/Cloud_project` chưa được khởi tạo Git. Hai lần `git init -b main`
đều trả về:

```text
/Users/phanan/Cloud_project/.git: Operation not permitted
```

Các file hiện nằm trực tiếp trong directory nhưng chưa có commit/history.

## 9. Production security baseline (bổ sung)

Yêu cầu “dùng thật, ISO/NIST” đã được chuyển thành baseline có thể audit thay
vì lời hứa certification không có căn cứ:

- ADR-0005 chốt NIST CSF 2.0 Tier 2 làm target ban đầu, map ISO/IEC 27001:2022,
  NIST SP 800-53 và NIST SSDF.
- Có control matrix, risk register, security disclosure policy, incident runbook
  và host-hardening runbook. P0 chưa implement được đánh dấu rõ là launch
  blocker; không được nhận family originals như một bản backup đáng tin cậy cho
  tới khi backup/restore, MFA, host hardening, audit export và alert test có
  evidence.
- Có GitHub CI/security workflow template: Go race/vet, Compose, PostgreSQL
  integration, Docker build, secret scan, Trivy filesystem/image và CodeQL.
  Workflows chỉ chạy sau khi project được khởi tạo Git và push lên GitHub.
- Gateway bare-process default đã đổi sang loopback và thêm read/write/idle
  timeout có cấu hình; Compose explicit bind/profiles giữ nguyên.
