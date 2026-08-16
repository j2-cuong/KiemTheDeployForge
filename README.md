# KiemTheDeployForge

Bộ công cụ Windows đóng gói và cài đặt server Kiếm Thế **hoàn toàn ngoại tuyến**.

Máy đóng gói chạy **Builder** để gom Client, Server, Bot, MySQL 5.5.15 và `jxaccount.sql` thành đúng hai file phát hành: `Setup.exe` và `KiemTheServer-Offline.iso`. Máy đích chỉ cần hai file đó — không cần Internet, không cần cài sẵn gì.

*Developed by CuongNH — a gift to the Hội Quán Võ Lâm brotherhood.*

---

## Mục lục

- [Luồng sử dụng](#luồng-sử-dụng)
- [Builder — đóng gói](#builder--đóng-gói)
- [Setup — cài đặt](#setup--cài-đặt)
- [MySQL và tài khoản](#mysql-và-tài-khoản)
- [Dung lượng ổ đĩa](#dung-lượng-ổ-đĩa)
- [Build từ mã nguồn](#build-từ-mã-nguồn)
- [Cấu trúc mã nguồn](#cấu-trúc-mã-nguồn)
- [Thiết kế đáng lưu ý](#thiết-kế-đáng-lưu-ý)
- [Trước khi phát hành thật](#trước-khi-phát-hành-thật)

---

## Luồng sử dụng

```
┌─ Máy đóng gói ────────────────┐        ┌─ Máy đích ──────────────────┐
│                               │        │                             │
│  Client\                      │        │  Setup.exe  (Administrator) │
│  Server\        ──► Builder ──┼───────►│         │                   │
│  Bot\ (tuỳ chọn)              │  ISO   │         ▼                   │
│  jxaccount.sql                │        │  Client\  Server\  Bot\     │
│                               │        │  MySQL + jxaccount          │
│  ► Setup.exe                  │        │  2 shortcut ngoài Desktop   │
│  ► KiemTheServer-Offline.iso  │        │                             │
└───────────────────────────────┘        └─────────────────────────────┘
```

1. Build công cụ: chạy **`Build.bat`** (chỉ cần cài Go).
2. Chạy `dist\KiemTheDeployForge-Builder.exe`, chọn các thư mục nguồn, bấm **TẠO SETUP + ISO**.
3. Chép **cả hai** file `Setup.exe` và `KiemTheServer-Offline.iso` sang máy đích, đặt **cạnh nhau**.
4. Chạy `Setup.exe` bằng quyền Administrator, chọn thư mục cài đặt, bấm **CÀI ĐẶT**.
5. Vào thư mục `Server` đã cài, chạy file `.bat` khởi động server theo cách bạn vẫn làm.

---

## Builder — đóng gói

### Đầu vào

| Trường | Bắt buộc | Ghi chú |
|---|:---:|---|
| Thư mục **Client** | ✔ | Phải có `Game.exe`, `AutoPk\wjxtdAutoPro.exe`, `user\uicommon.ini`, `user\serverlistdebug.ini`, `AutoPk\serverlist.ini` |
| Thư mục **Server** | ✔ | Phải có `Gameserver\GS1..GS9.exe` và `GS1..GS9servercfg.ini` |
| Thư mục **Bot** | — | **Tuỳ chọn.** Nếu chọn thì phải có `loginprobe.exe` và `loginprobe.env` ở gốc |
| File **jxaccount.sql** | ✔ | Bắt buộc đúng tên, phải có `CREATE TABLE` cho bảng `account` |
| Thư mục **xuất bản** | ✔ | Ổ NTFS, cần khoảng 2,2 lần payload + 2 GiB |

Server **không** bắt buộc có `start-all.bat` hay `stop-all.bat`. Bật/tắt server là việc của người vận hành, Builder không can thiệp.

`jxaccount.sql` phải nằm **ngoài** cả ba cây Client/Server/Bot. Builder từ chối input lồng nhau trước khi quét, để không đóng gói trùng byte hoặc làm đầy ổ đĩa ngoài ý muốn. Các đường dẫn được chuẩn hoá qua junction/symlink trước khi kiểm tra, nên hai alias trỏ cùng một cây cũng bị từ chối.

File SQL bị từ chối nếu chứa `CREATE USER`, `GRANT`, `USE`, `LOAD DATA` hay các lệnh ngoài phạm vi tạo bảng — tài khoản MySQL do Setup tự tạo, không lấy từ file SQL.

### Cấu hình mở rộng

Nút **Tài khoản MySQL…** cho phép đổi mật khẩu `root`, tên user bot và mật khẩu bot. Giá trị được ghi vào manifest của bản phát hành. Bỏ qua thì dùng mặc định.

### Đầu ra

Đúng hai file trong thư mục xuất bản:

- `Setup.exe` — bootstrap nhỏ, chỉ mang manifest để khoá đúng bản phát hành.
- `KiemTheServer-Offline.iso` — UDF, gồm đúng bốn mục ở gốc: `Setup.exe`, `Payload.ktpkg`, `README.txt`, `manifests\`.

Dữ liệu lớn nằm trong `Payload.ktpkg` (ZIP64 Store) trên ISO, **không** nhúng vào PE: executable lớn hơn 4 GB không được Windows `CreateProcess` nạp ổn định.

Mọi giai đoạn đều báo phần trăm, kể cả lúc ghi ISO (0–90 %) và verify ISO (92–100 %).

### Chế độ CLI

```powershell
KiemTheDeployForge-Builder.exe --cli `
  --client  D:\Client `
  --server  D:\server `
  --bot     D:\bot `           # tuỳ chọn
  --sql     D:\jxaccount.sql `
  --output  D:\release `
  --root-password 1234 --bot-user bot_writer --bot-password 1234 `
  --skip-iso                   # bỏ qua bước tạo ISO
```

---

## Setup — cài đặt

Chạy bằng quyền Administrator. Setup tự tìm và mount ISO cùng thư mục; nếu chính nó đang nằm trong ISO đã mount thì đọc payload trực tiếp. ISO do Setup tự mount sẽ được tự dismount khi xong.

Thư mục cài đặt mặc định là `C:\KiemTheServer` và **không được trùng hoặc bao trùm** thư mục chứa `Setup.exe` + ISO. Bước commit đổi tên thư mục staging thành thư mục cài đặt, nên thư mục đó bắt buộc chưa tồn tại. Setup báo ngay từ lúc chọn đường dẫn. Cài vào **thư mục con** của chỗ chứa ISO thì hợp lệ.

### Setup làm gì

1. Giải nén `Client`, `Server` và `Bot` (nếu bản phát hành có bot), verify SHA-256 từng file.
2. Tự nhận IP LAN rồi ghi vào **21 khoá** cấu hình.
3. Ghi 6 khoá trong `Bot\loginprobe.env` (nếu có bot).
4. Cài hoặc tiếp quản MySQL 5.5.15, tạo tài khoản, import `jxaccount`.
5. Tạo 2 shortcut ngoài Desktop dùng chung.

### IP LAN tự động

Setup tự dò IPv4 theo năm tầng, từ chặt tới lỏng, và dừng ở tầng đầu tiên tìm được kết quả. **Ô "Địa chỉ IPv4" điền sẵn kết quả dò được nhưng vẫn sửa được** trước khi bấm cài.

| Tầng | Nguồn | Điều kiện | Dành cho |
|:---:|---|---|---|
| 1 | `Get-NetRoute` | Card vật lý, IP RFC1918, có default route | Máy LAN bình thường |
| 2 | `Get-NetAdapter` | Card vật lý, IP RFC1918, đang `Up` | LAN cô lập không có gateway |
| 3 | `Get-NetRoute` | Card bất kỳ kể cả card ảo hoá, IP bất kỳ, có default route | **Máy chủ thuê (VPS)** |
| 4 | `Get-NetAdapter` | Card bất kỳ đang `Up`, IP bất kỳ | VPS không có default route |
| 5 | `net.Interfaces()` | Mọi địa chỉ Windows đang gán | Windows cũ không có cmdlet `Get-Net*` |

Tầng 5 đọc thẳng từ Go runtime — cùng một API mà `ipconfig` dùng, nhưng trả về dữ liệu có cấu trúc. Không parse text `ipconfig` vì output bị dịch theo ngôn ngữ hệ điều hành: Windows tiếng Việt in `Địa chỉ IPv4` chứ không phải `IPv4 Address`.

Máy LAN vẫn ưu tiên IP riêng như trước — tầng 1 và 2 chạy trước nên hành vi không đổi. Tầng 3 và 4 mới là phần cứu VPS: ở đó card mạng thường là thiết bị ảo hoá (VirtIO, VMXNET3, Hyper-V, Xen) mang **địa chỉ công cộng**, mà cả hai đặc điểm đó đều bị tầng 1–2 loại.

Loại **ở mọi tầng**: loopback, APIPA `169.254.x`, multicast, dải reserved, và các card overlay — `vEthernet`, VPN, TAP, tunnel, WSL, WireGuard, OpenVPN, Tailscale, ZeroTier, Hamachi, Docker, npcap, Bluetooth, WAN Miniport. Đó không bao giờ là đường mạng thật của máy.

Lựa chọn trong mỗi tầng được xếp hạng ổn định theo metric, loại card, interface index, tên rồi địa chỉ, nên cùng một máy luôn cho cùng một kết quả.

Nếu địa chỉ chọn được là **công cộng**, Setup hiện cảnh báo trước khi cài — đúng với VPS nhưng là dấu hiệu sai nếu bạn tưởng đang cài trong LAN. Khi không tìm được gì, thông báo lỗi **liệt kê đúng những địa chỉ máy đã báo** thay vì chỉ nói "không tìm thấy".

**Dò thất bại không còn là ngõ cụt.** Setup vẫn hiện thông tin gói và dung lượng, để trống ô địa chỉ và mời bạn tự nhập. Đây là cách duy nhất xử lý VPS nằm sau NAT của nhà cung cấp (AWS, GCP, Azure): ở đó IP công cộng do nhà cung cấp giữ, không hề gán lên card nào của máy nên không có cách nào dò ra.

Giá trị nhập tay được kiểm tra ngay khi gõ và một lần nữa trước khi cài. Nút CÀI ĐẶT chỉ bật khi địa chỉ hợp lệ — sai một chữ số là ra một server không ai kết nối được. Chế độ CLI dùng `--lan-address`.

### 21 khoá được ghi

| File | Section | Khoá |
|---|---|---|
| `Server\Gameserver\GS1..GS9servercfg.ini` | `[GameServer]` | `InIp`, `OutIp` (18 khoá) |
| `Client\user\uicommon.ini` | `[Region_0]` | `1_Address` |
| `Client\user\serverlistdebug.ini` | `[Region_1]` | `1_Address` |
| `Client\AutoPk\serverlist.ini` | `[Region_0]` | `0_Address` |

Bộ vá giữ nguyên BOM, kiểu xuống dòng, dữ liệu ngoài khoá đích, timestamp và thuộc tính file. Mỗi khoá phải khớp **đúng một lần**, nếu không Setup dừng thay vì đoán.

### Cấu hình bot

Khi bản phát hành có bot, Setup ghi 6 khoá trong `Bot\loginprobe.env`:

- `BOT_GAMESERVER_DIR` → đường dẫn tuyệt đối tới `<ThưMụcCàiĐặt>\Server\Gameserver`
- `BOT_DB_HOST`, `BOT_DB_PORT`, `BOT_DB_USER`, `BOT_DB_PASSWORD`, `BOT_DB_NAME`

Khoá đang bị comment sẽ được kích hoạt; khoá chưa có được thêm vào cuối file theo đúng kiểu xuống dòng của file đó.

> **Chỉ `loginprobe.env` bị sửa.** Phần còn lại của cây Bot — kể cả thư mục dữ liệu tên `Sever` nằm bên trong nó — được giữ nguyên từng byte. Đừng nhầm nó với thư mục `Server` cài đặt ở ngoài.

### Shortcut Desktop

| Shortcut | Trỏ tới |
|---|---|
| `Kiem The.lnk` | `Client\Game.exe` |
| `Kiem The AutoPk.lnk` | `Client\AutoPk\wjxtdAutoPro.exe` |

Đặt ở **Public Desktop** chứ không phải Desktop của admin, để user thường cũng thấy được — Setup chạy elevated nên nếu dùng Desktop cá nhân thì shortcut sẽ rơi vào tài khoản admin.

---

## MySQL và tài khoản

- Phiên bản cố định: **MySQL 5.5.15 Win32**, pin theo SHA-256 `976571c1…d001e`, 139 896 749 byte.
- Nguồn: `https://cdn.mysql.com/archives/mysql-5.5/mysql-5.5.15-win32.zip` — Builder tải một lần rồi cache trong `%LOCALAPPDATA%`, sau đó **đóng luôn vào ISO**. Máy đích không cần mạng.
- Service: `KiemTheServer-MySQL`, bind `127.0.0.1:3306`.

> Chuỗi `win32` chỉ mô tả gói MySQL legacy chạy thành service DB riêng. Builder, Setup và GameServer đều là **x64** (PE machine `0x8664`).

### Tài khoản

| Tài khoản | Mặc định | Quyền |
|---|---|---|
| `root` | `1234` | Toàn quyền |
| `bot_writer` | `1234` | `GRANT ALL ON jxaccount.*` cho cả `@'localhost'` và `@'%'` |

Bot tự phát `CREATE TABLE IF NOT EXISTS bot_runtime_state`, nên quyền chỉ-ghi-dữ-liệu là không đủ — phải có DDL.

MySQL 5.5 không có `DROP USER IF EXISTS`, nên Setup dùng `GRANT … IDENTIFIED BY` — vừa tạo mới vừa đặt lại mật khẩu, an toàn khi chạy lại lúc resume.

### Đổi tài khoản lúc đóng gói

Mật khẩu `root`, tên user bot và mật khẩu bot đổi được qua nút *Tài khoản MySQL…*, hoặc `--root-password` / `--bot-user` / `--bot-password` ở CLI. Tên `root` **không** đổi được — đó là superuser của MySQL, đổi tên nó là việc khác hẳn với đổi mật khẩu.

Bộ ký tự bị giới hạn có chủ đích:

- **Mật khẩu**: ASCII in được, ≤ 32 ký tự, không khoảng trắng, không `'` `"` `` ` `` `\`
- **User bot**: chữ/số/`_`, không bắt đầu bằng số, ≤ 16 ký tự, không trùng `root`, không bắt đầu bằng `ktf`

Lý do: hàm `sqlLiteral` chỉ nhân đôi dấu nháy đơn, mà MySQL mặc định coi `\` là ký tự escape — mật khẩu chứa `\` sẽ bị diễn giải sai. MySQL 5.5 cắt cụt tên user dài quá 16 ký tự, sẽ tạo ra account mà bot không đăng nhập được. Tiền tố `ktf` dành cho các tài khoản import tạm thời.

### Khi máy đã có sẵn MySQL

Nếu `127.0.0.1:3306` đã có MySQL chạy trước đó, Setup **tiếp quản** thay vì dừng. Đó là trường hợp phổ biến nhất — người dùng đã cài sẵn MySQL của server game — và dừng lại thì chẳng sửa được gì, vì không thể bind cổng lần hai.

Setup sẽ:

1. Đăng nhập `root`/mật khẩu của bản phát hành; nếu `root` chưa có mật khẩu thì đặt theo đúng cam kết.
2. Tạo hoặc cập nhật user bot với `GRANT ALL ON jxaccount.*`.
3. Kiểm tra `jxaccount`: đủ bảng đúng schema thì **giữ nguyên**, chưa có thì import.

Server sẵn có được coi là tài sản của người khác. Khác với MySQL do Setup tự cài, đường tiếp quản **không** xoá tài khoản ẩn danh, **không** drop database `test`, **không** ghi đè file cấu hình nào.

Không đăng nhập được bằng cả mật khẩu của bản phát hành lẫn mật khẩu rỗng thì Setup dừng và nêu rõ hai lựa chọn: tắt MySQL đó để Setup cài service riêng, hoặc đặt lại mật khẩu `root` rồi chạy lại.

### Import jxaccount

`jxaccount.sql` được import vào một database staging tên ngẫu nhiên, kiểm tra đủ tập bảng và khả năng ghi bảng `account`, rồi mới publish bằng một lệnh multi-table `RENAME TABLE`.

Marker bền vững gắn với SHA-256 của đúng file SQL giúp Setup resume sau gián đoạn. Database không rỗng nhưng không có bằng chứng hoàn tất sẽ được **giữ nguyên**, không `DROP` hay import lại mù.

Trong lúc import, watchdog kiểm tra dung lượng mỗi giây và huỷ client trước khi phần trống xuống dưới 2 GiB.

---

## Dung lượng ổ đĩa

Setup kiểm tra hai ổ tách biệt và hiển thị cả hai trước khi cài:

| Ổ | Yêu cầu | Không đủ thì |
|---|---|---|
| Ổ chứa thư mục cài đặt | Payload + phần mở rộng MySQL + import SQL | **Chặn**, không cho cài |
| Ổ hệ thống Windows (thường là `C:`) | 20 GiB trống cho bot | **Cảnh báo**, vẫn cho cài |

Bot ghi dữ liệu làm việc lên ổ hệ thống bất kể bạn cài ở ổ nào, nên nó được kiểm tra riêng. Bản phát hành **không có bot** thì cảnh báo này không hiện.

Con số dung lượng được đọc lại theo đúng thư mục bạn chọn, kể cả khi đổi thư mục sau bước preflight.

---

## Build từ mã nguồn

Chỉ cần cài [Go](https://go.dev/dl/). Không cần MinGW, windres hay công cụ ngoài nào khác.

```powershell
.\Build.bat          # build + audit + publish ra dist\
.\Build.bat check    # thêm gofmt, go vet, go test trước khi build
```

Hoặc gọi thẳng script:

```powershell
.\scripts\Build-Tools.ps1
.\scripts\Build-Tools.ps1 -ValidateSource
.\scripts\Smoke-Test-Packager.ps1              # chỉ chạy khi chủ động cần
.\scripts\Smoke-Test-Packager.ps1 -IncludeIso  # chỉ khi cần kiểm tra IMAPI/UDF
```

`Build-Tools.ps1` khoá môi trường Go (`GOFLAGS`, `GOENV`, `GOTOOLCHAIN`, `GOWORK`, `GOEXPERIMENT`, `CGO_ENABLED`, `GOOS`, `GOARCH`, `GOAMD64=v1`, `GOFIPS140=off`), ép module ở chế độ `readonly`, resolve một `go.exe` tuyệt đối, sinh lại resource từ manifest, build cả hai executable, kiểm tra PE machine `0x8664`, chạy `Audit-Build.ps1`, rồi mới publish. Publish thất bại thì rollback.

`Audit-Build.ps1` đọc trực tiếp PE header và từ chối binary không phải amd64, ngoài việc kiểm tra metadata module Go và quét chuỗi ASCII/UTF-16 thuộc phạm vi nội bộ không được lọt ra bản phát hành.

Smoke test sao chép riêng `src` và `scripts` vào cây `.smoke-*`, build/chạy Builder ở đó rồi chạy `Setup.exe --cli-plan --verify` với fixture nhỏ. Nó **không** thay `dist\KiemTheDeployForge-Builder.exe`, không cài service, không khởi động MySQL. Đừng dùng cây Client/Server/Bot thật làm fixture.

### Application manifest

Cả hai GUI nhúng application manifest khai báo **Common-Controls 6.0**. Thiếu manifest thì `walk` gửi `TTM_ADDTOOL` theo layout comctl32 v6 trong khi Windows nạp comctl32 v5, và cả hai cửa sổ panic trước khi hiện ra.

`cmd\genrsrc` (Go thuần) sinh `rsrc_windows_amd64.syso` từ `Builder.manifest` / `Setup.manifest` ở mỗi lần build, và validate XML trước — một manifest hỏng vẫn link được nhưng Windows sẽ từ chối chạy với lỗi *"side-by-side configuration is incorrect"* rất khó truy. Các file `.syso` được commit vào repo để `go build` trần cũng ra GUI chạy được.

`src\cmd\builder\assets\SetupStub.exe` **không** được commit — nó là build output. Placeholder `README.txt` cạnh nó được commit để `//go:embed assets/*` luôn khớp ít nhất một file, nếu không thì clone mới không compile nổi.

---

## Cấu trúc mã nguồn

```
Build.bat                     Điểm vào duy nhất để build
scripts/
  Build-Tools.ps1             Build, audit, publish có rollback
  Audit-Build.ps1             Kiểm tra PE header và phạm vi chuỗi
  Smoke-Test-Packager.ps1     Kiểm thử end-to-end với fixture nhỏ
src/
  cmd/builder/                GUI Builder + chế độ CLI
  cmd/setup/                  GUI Setup + chế độ plan/install
  cmd/genrsrc/                Sinh .syso từ application manifest
  internal/
    builder/                  Quét, hash, ghi ZIP64 payload, Setup bootstrap
    install/                  Mount ISO, staging, journal, resume, commit, shortcut
    database/                 MySQL 5.5.15, tài khoản, import jxaccount
    configpatch/              Vá INI và dotenv giữ nguyên định dạng
    iso/                      Tạo và xác minh ISO UDF bằng IMAPI2
    sfx/                      Đọc manifest pin, giải nén Payload.ktpkg
    release/                  Manifest, tài khoản, verify file
    network/                  Dò IP LAN
    guiutil/                  Theme, progress meter, console, throttle
    winfile/  winprocess/     Tiện ích Windows
```

---

## Thiết kế đáng lưu ý

### Giao diện tiến độ

Cả hai cửa sổ dùng chung bộ widget trong `internal/guiutil`:

- **Phần trăm nằm bên trong thanh tiến trình** (`Meter`, `CustomWidget` vẽ tay ở chế độ `PaintBuffered`). Dùng `Label` riêng cạnh `ProgressBar` khiến độ rộng label đổi theo từng giá trị, layout bị tính lại liên tục, cộng với repaint không double-buffer — kết quả là nháy liên hồi.
- **Stage và tên file ở hai dòng riêng, mỗi dòng chiếm trọn chiều ngang**, nên text dài ngắn khác nhau không làm co giãn hàng xóm. Dòng tên file dùng `EllipsisPath`.
- **Console cuộn kiểu terminal** (`Console`): nền tối, monospace, chỉ append, giữ 4000 dòng scrollback.
- **`Relay` tiết chế cập nhật xuống tối đa một lần mỗi 90 ms.** Pipeline báo tiến độ mỗi block I/O — hàng nghìn lần mỗi giây trên payload lớn. Relay gộp chúng lại nhưng không bao giờ bỏ lỡ một lần đổi stage hay báo cáo cuối cùng.

### An toàn khi bị gián đoạn

- Toàn máy chỉ cho phép **một** build đóng gói nặng tại một thời điểm, kể cả khi các tiến trình chọn thư mục output khác nhau.
- Builder ghi marker giao dịch trước khi tạo output cuối. Bị hard-kill hoặc mất điện giữa chừng thì lần build kế tiếp **chỉ** thu hồi đúng bốn file output khi marker chứng minh được quyền sở hữu.
- Setup dùng staging + `rename` nguyên tử, có journal và marker sở hữu. Dữ liệu không có marker, hoặc marker không khớp đường dẫn, **luôn được giữ nguyên**.
- Mỗi lần resume, toàn bộ file runtime MySQL bất biến được SHA-256 lại và đối chiếu byte với chính ZIP đã pin. File hoặc thư mục runtime không có trong gói bị từ chối. `my.ini` cũng phải khớp chính xác policy local-only do installer sinh.
- Service hoặc dữ liệu không chứng minh được thuộc quá trình cài đặt sẽ **không** bị xoá hay ghi đè.
- Cache tải MySQL dùng chung trong `%LOCALAPPDATA%` chỉ thu hồi file `.download` dở dang đã cũ ít nhất 24 giờ, để không đụng một build khác đang tải.

### Chống hijack

Builder và Setup **không** gọi `powershell.exe` qua current directory hoặc `PATH`. Các bước dò LAN, mount/dismount, tạo UDF và tạo shortcut đều resolve Windows PowerShell từ system directory thật rồi mới chạy ẩn.

---

## Trước khi phát hành thật

Chạy UAT trên Windows sạch với:

- Quyền Administrator
- Card LAN vật lý
- Đủ dung lượng NTFS
- Không có tiến trình khác chiếm cổng `3306` (hoặc có, để kiểm tra đúng đường tiếp quản)

Nên ký Authenticode cho Builder và Setup trong pipeline phát hành. **Vật liệu ký không được đặt trong source hoặc artifact trung gian** — `.gitignore` đã chặn `*.pfx`, `*.p12`, `*.pem`, `*.key`, nhưng đừng dựa vào đó thay cho quy trình.
