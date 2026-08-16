# KiemTheDeployForge

Ứng dụng Windows độc lập để đóng gói và cài đặt bộ Kiem The Server hoàn toàn ngoại tuyến.

## Luồng sử dụng

1. Build công cụ: chạy `Build.bat` (chỉ cần cài Go, không cần MinGW/windres). Dùng `Build.bat check` nếu muốn chạy thêm gofmt, `go vet` và `go test`.
2. Chạy `dist\KiemTheDeployForge-Builder.exe`.
3. Chọn thư mục `Client`, thư mục `Server`, một file `jxaccount.sql` và thư mục xuất bản. **Thư mục `Bot` là tuỳ chọn** — để trống nếu không muốn đóng gói bot.
   Trong mục **Cấu hình mở rộng**, nút *Tài khoản MySQL…* cho phép đổi mật khẩu `root`, tên user bot và mật khẩu bot. Bỏ qua thì dùng mặc định `root`/`1234` và `bot_writer`/`1234`.
4. Builder tạo đúng hai artifact phát hành: `Setup.exe` và `KiemTheServer-Offline.iso`. Mọi giai đoạn đều báo phần trăm, kể cả lúc ghi ISO (0–90%) và verify ISO (92–100%).
5. Giữ `Setup.exe` cạnh `KiemTheServer-Offline.iso`, rồi chạy `Setup.exe` bằng quyền Administrator trên máy đích. Thư mục mặc định là `C:\KiemTheServer`.

Setup tạo `Client`, `Server` và — nếu bản phát hành có đóng gói bot — `Bot`; sau đó tạo hai shortcut ngoài Desktop dùng chung (`Kiem The.lnk` trỏ `Client\Game.exe`, `Kiem The AutoPk.lnk` trỏ `Client\AutoPk\wjxtdAutoPro.exe`).

## Giao diện tiến độ

Cả hai cửa sổ dùng chung bộ widget trong `src\internal\guiutil`:

- **Phần trăm nằm bên trong thanh tiến trình** (`guiutil.Meter`, `CustomWidget` vẽ tay ở chế độ `PaintBuffered`). Trước đây phần trăm là một `Label` riêng cạnh `ProgressBar`; độ rộng label đổi theo từng giá trị nên layout bị tính lại liên tục, cộng với repaint không double-buffer, gây nháy.
- **Tên stage và tên file nằm ở hai dòng riêng, mỗi dòng chiếm trọn chiều ngang.** Trước đây cả hai bị ghép vào một `Label` dùng chung `HBox` với label phần trăm, nên tên file dài ngắn khác nhau làm cả dải nhấp nháy. Dòng tên file dùng `EllipsisPath` để đường dẫn dài bị cắt bớt thay vì làm giãn layout.
- **Khung log cuộn kiểu terminal** (`guiutil.Console`): nền tối, chữ monospace, chỉ ghi thêm dòng mới, giữ tối đa 4000 dòng scrollback.
- **`guiutil.Relay` tiết chế tần suất cập nhật xuống tối đa một lần mỗi 90 ms.** Pipeline đóng gói báo tiến độ mỗi block I/O — hàng nghìn lần mỗi giây trên payload lớn — và chính luồng báo đó làm cửa sổ nháy. Relay gộp chúng lại nhưng không bao giờ bỏ lỡ một lần đổi stage hay báo cáo cuối cùng.

Builder và Setup đều nhúng application manifest khai báo Common-Controls 6.0. Thiếu manifest thì `walk` gửi `TTM_ADDTOOL` theo layout comctl32 v6 trong khi Windows nạp comctl32 v5, và cả hai GUI panic trước khi hiện cửa sổ. `scripts\Build-Tools.ps1` sinh lại `rsrc_windows_amd64.syso` từ `Builder.manifest` / `Setup.manifest` bằng `cmd\genrsrc` (Go thuần) ở mỗi lần build.

Thư mục `Server` không bắt buộc phải có `start-all.bat` hay `stop-all.bat`. Việc bật/tắt server là do người vận hành tự mở thư mục `Server` đã cài và chạy file `.bat` có sẵn ở đó.

Builder và Setup đều được build `windows/amd64` (PE machine `0x8664`). Chuỗi `win32` trong manifest chỉ mô tả gói MySQL 5.5.15 legacy chạy thành service DB riêng; nó không phải kiến trúc của Builder, Setup hay GameServer.

`Setup.exe` là bootstrap nhỏ, runnable, chỉ mang manifest dùng để khóa đúng release. Dữ liệu lớn nằm trong `Payload.ktpkg` trên ISO UDF. Khi chạy file Setup bên ngoài, nó tự tìm và mount đúng ISO cùng thư mục; khi chạy bản Setup nằm trong ISO đã mount, nó đọc payload trực tiếp. Setup tự dismount ISO do chính nó mount sau khi plan/cài đặt kết thúc.

File database đầu vào bắt buộc có tên `jxaccount.sql`, phải chứa `CREATE TABLE` cho bảng `account` (kèm cột `loginName` và `password_hash`) và phải nằm ngoài cả ba cây Client, Server và Bot. Builder từ chối input lồng nhau trước khi quét để không đóng gói trùng byte hoặc làm đầy ổ đĩa ngoài ý muốn. File SQL cũng bị từ chối nếu chứa `CREATE USER`, `GRANT`, `USE`, `LOAD DATA` hay các câu lệnh ngoài phạm vi tạo bảng — tài khoản MySQL do Setup tự tạo, không lấy từ file SQL.

Thư mục Bot là tuỳ chọn; nếu có chọn thì bắt buộc phải có `loginprobe.exe` và `loginprobe.env` ngay ở gốc.

## Dung lượng ổ đĩa

Setup kiểm tra hai ổ tách biệt và hiển thị cả hai ngay trên giao diện trước khi cài:

- Ổ chứa thư mục cài đặt: phải đủ chỗ cho payload, phần mở rộng MySQL và import SQL. Thiếu chỗ là lỗi chặn, không cho cài.
- Ổ hệ thống Windows (thường là `C:`): bot cần tối thiểu 20 GiB trống dù cài ở ổ nào. Thiếu chỗ chỉ là cảnh báo — Setup vẫn cho cài và ghi lại cảnh báo vào `install-state.json`.

Con số dung lượng được đọc lại theo đúng thư mục người dùng chọn, kể cả khi họ đổi thư mục sau bước preflight.

Thư mục xuất bản là trường bắt buộc; Builder không dùng ngầm thư mục làm việc hiện tại khi ô này trống. Các đường dẫn nguồn được chuẩn hóa qua junction/symlink trước khi kiểm tra overlap, nên hai alias trỏ vào cùng một cây cũng bị từ chối.

ISO có đúng bốn mục ở root: `Setup.exe`, `Payload.ktpkg`, `README.txt` và `manifests`. Không nhúng payload 9 GB vào PE vì executable lớn hơn 4 GB không thể được Windows `CreateProcess` nạp ổn định.

Toàn máy chỉ cho phép một build đóng gói nặng chạy tại một thời điểm, kể cả khi các tiến trình chọn những thư mục output khác nhau. Builder ghi marker giao dịch trước khi tạo output cuối; nếu tiến trình bị hard-kill hoặc mất điện giữa lúc đã đổi tên `Payload.ktpkg`, `Setup.exe`, `README.txt` hay ISO sang tên chính thức, lần build kế tiếp chỉ thu hồi bốn file đó khi marker chứng minh đúng output. Sau khi giữ mutex toàn máy, Builder cũng dọn ngay các tên tạm riêng của Forge và yêu cầu marker sở hữu đối với thư mục (`.Payload.ktpkg-*.building`, ISO `.building.iso`, `.iso-build-*`, `.mysql-cache-*`...). Payload/ISO nhiều GiB bị bỏ dở vì vậy không bị giữ thêm 24 giờ. Cache tải MySQL dùng chung trong `%LOCALAPPDATA%` vẫn chỉ thu hồi file `.download` dở dang đã cũ ít nhất 24 giờ để không đụng một build khác đang tải.

Cache MySQL riêng của Forge trong `%LOCALAPPDATA%` cũng thu hồi các file `.download` dở dang đã cũ ít nhất 24 giờ; hủy bình thường vẫn xóa file tạm ngay trong lần chạy hiện tại.

## IP LAN tự động

Setup không có ô nhập hoặc tham số thay thế IP. Nó tự ưu tiên IPv4 RFC1918 trên card mạng vật lý đang hoạt động và có default route. Nếu máy LAN cô lập không có gateway, Setup tự fallback sang card vật lý RFC1918 đang `Up`; lựa chọn luôn được xếp hạng ổn định theo metric, loại card, interface index, tên và địa chỉ IP. Loopback, APIPA, VPN và các card ảo phổ biến bị loại bỏ.

Setup cập nhật đúng 21 khóa IP trong 12 file:

- `Server\Gameserver\GS1servercfg.ini` đến `GS9servercfg.ini`: `[GameServer] InIp` và `OutIp` (18 khóa).
- `Client\user\uicommon.ini`: `[Region_0] 1_Address`.
- `Client\user\serverlistdebug.ini`: `[Region_1] 1_Address`.
- `Client\AutoPk\serverlist.ini`: `[Region_0] 0_Address`.

Nếu bản phát hành **không** đóng gói bot, toàn bộ bước cấu hình bot bị bỏ qua và cảnh báo 20 GiB ổ hệ thống cũng không hiện — đó là yêu cầu của bot, không phải của server.

Khi có bot, Setup ghi thêm 6 khóa trong `Bot\loginprobe.env`: `BOT_GAMESERVER_DIR` trỏ đường dẫn tuyệt đối tới `<ThưMụcCàiĐặt>\Server\Gameserver`, cùng `BOT_DB_HOST`, `BOT_DB_PORT`, `BOT_DB_USER`, `BOT_DB_PASSWORD` và `BOT_DB_NAME`. Khóa đang bị comment sẽ được kích hoạt; khóa chưa có sẽ được thêm vào cuối file theo đúng kiểu xuống dòng của file đó. Chỉ `loginprobe.env` bị sửa — phần còn lại của cây Bot, kể cả thư mục dữ liệu `Sever` riêng bên trong nó, được giữ nguyên từng byte.

Bộ vá giữ nguyên BOM, kiểu xuống dòng, dữ liệu ngoài khóa đích, timestamp và thuộc tính file.

## MySQL và jxaccount

- Phiên bản cố định: MySQL `5.5.15 Win32`.
- Service cục bộ: `KiemTheServer-MySQL`.
- Kết nối cục bộ: `root` / `1234` (mặc định), bind `127.0.0.1:3306`.
- Tài khoản bot: `bot_writer` / `1234` (mặc định) cho cả `@'localhost'` và `@'%'`, `GRANT ALL PRIVILEGES ON jxaccount.*`.

Mật khẩu `root`, tên user bot và mật khẩu bot **đổi được lúc đóng gói** qua nút *Tài khoản MySQL…* trong Builder, hoặc `--root-password` / `--bot-user` / `--bot-password` ở chế độ CLI. Giá trị được ghi vào manifest của bản phát hành, nên Setup tạo đúng tài khoản đã đóng gói và ghi luôn vào `loginprobe.env` của bot. Tên user `root` không đổi được — đó là superuser của MySQL, đổi tên nó là việc khác hẳn với đổi mật khẩu.

Bộ ký tự bị giới hạn có chủ đích: `sqlLiteral` chỉ nhân đôi dấu nháy đơn, còn MySQL mặc định coi `\` là ký tự escape, nên mật khẩu chỉ nhận ASCII in được, không khoảng trắng, không nháy và không gạch chéo ngược. Mật khẩu tối đa 32 ký tự; user bot tối đa 16 ký tự vì MySQL 5.5 cắt cụt tên dài hơn, không được trùng `root` và không được bắt đầu bằng `ktf` (tiền tố dành cho tài khoản import tạm thời). Bot tự phát `CREATE TABLE IF NOT EXISTS bot_runtime_state` nên quyền chỉ-ghi-dữ-liệu là không đủ. MySQL 5.5 không có `DROP USER IF EXISTS`, nên Setup dùng `GRANT ... IDENTIFIED BY` — vừa tạo mới vừa đặt lại mật khẩu, an toàn khi chạy lại lúc resume.
- Nguồn chính thức: `https://cdn.mysql.com/archives/mysql-5.5/mysql-5.5.15-win32.zip`.
- Kích thước: `139896749` byte.
- SHA-256: `976571c110a9441a26ccf936407a90376dd30f0adce4cc6870be17fcc5ed001e`.
- MD5 `127cf3abe2fa31b58d91e45e65194f25` chỉ được giữ làm metadata tương thích; SHA-256 là giá trị xác thực.

### Khi máy đã có sẵn MySQL

Nếu 127.0.0.1:3306 đã có MySQL chạy trước đó, Setup **tiếp quản** thay vì dừng lại. Trước đây nó báo lỗi và thoát, nhưng đó là xử lý sai cho trường hợp phổ biến nhất: người dùng đã cài sẵn MySQL của server game, nên không thể bind cổng lần hai và cũng chẳng có gì để sửa bằng cách dừng.

Setup sẽ:

1. Đăng nhập bằng `root` / `1234`; nếu `root` chưa có mật khẩu thì đặt thành `1234` theo đúng cam kết của bản phát hành.
2. Tạo hoặc cập nhật `bot_writer` / `1234` với `GRANT ALL ON jxaccount.*`.
3. Kiểm tra `jxaccount`: nếu đã đủ bảng đúng schema thì **giữ nguyên**, không import đè; nếu chưa có thì import.

Server sẵn có được coi là tài sản của người khác. Khác với MySQL do Setup tự cài, đường tiếp quản **không** xóa tài khoản ẩn danh, **không** drop database `test` và **không** ghi đè file cấu hình nào.

Nếu không đăng nhập được bằng `root`/`1234` lẫn mật khẩu rỗng, Setup dừng và nói rõ hai lựa chọn: tắt MySQL đó để Setup cài service riêng, hoặc đặt mật khẩu `root` thành `1234` rồi chạy lại.

### Chọn thư mục cài đặt

Thư mục cài đặt **không được trùng hoặc bao trùm** thư mục chứa `Setup.exe` và file ISO. Bước commit đổi tên thư mục staging thành thư mục cài đặt, nên thư mục đó bắt buộc chưa tồn tại. Setup phát hiện và báo ngay từ lúc chọn đường dẫn. Cài vào **thư mục con** của chỗ chứa ISO thì vẫn hợp lệ.

Runtime và data template được tạo trong thư mục tạm cùng volume, kiểm tra trước khi rename sang vị trí cuối. Nếu lần cài trước dừng giữa chừng, Setup có thể sửa phần runtime/data dở dang do chính nó tạo và tiếp tục. Service hoặc dữ liệu không chứng minh được thuộc quá trình cài đặt sẽ không bị xóa hay ghi đè.

Mỗi lần resume, toàn bộ file runtime MySQL bất biến được SHA-256 lại và đối chiếu byte với chính ZIP đã pin; file hoặc thư mục runtime không có trong gói bị từ chối. `my.ini` của service cũng phải khớp chính xác policy local-only do installer sinh trước khi binary quản trị database được chạy.

Các vòng copy runtime/data kiểm tra tín hiệu hủy theo từng block I/O. Thư mục chuẩn bị MySQL có marker sở hữu riêng; lần chạy kế tiếp chỉ thu hồi đúng staging có marker hợp lệ, không quét hoặc xóa thư mục lạ chỉ vì trùng tiền tố.

Marker sở hữu staging được giữ xuyên suốt thao tác rename nguyên tử. Nếu tiến trình bị kill đúng lúc commit, lần chạy kế tiếp có thể phân biệt staging cũ với dữ liệu ngoài phạm vi và thu hồi marker sau khi xác nhận đúng release, thay vì để một thư mục mồ côi chặn cài đặt.

Khi Setup bị kill trong lúc giải nén, lần chạy kế tiếp giữ mutex của đúng thư mục cài đặt rồi thu hồi mọi staging có marker sở hữu hợp lệ cho thư mục đó, kể cả staging thuộc release cũ. Dữ liệu không có marker hoặc marker không khớp đường dẫn luôn được giữ nguyên.

`jxaccount.sql` được import vào một database staging tên ngẫu nhiên, kiểm tra đủ tập bảng và khả năng ghi bảng `account`, rồi mới publish bằng một lệnh multi-table `RENAME TABLE`. Marker bền vững gắn với SHA-256 của đúng file SQL giúp Setup resume sau gián đoạn; database không rỗng nhưng không có bằng chứng hoàn tất sẽ được giữ nguyên, không bị `DROP` hoặc import lại mù.

Preflight cài đặt cộng thêm ngân sách mở rộng SQL; sau khi payload commit, Setup đọc central directory của ZIP MySQL để tính chính xác runtime giải nén và bản sao data template. Trong lúc import, watchdog kiểm tra dung lượng mỗi giây, hủy client trước khi phần trống xuống dưới 2 GiB và cố thu hồi ngay database staging chưa hoàn tất. Nếu MySQL chưa nhả tài nguyên kịp, marker vẫn giữ đủ định danh để lần chạy sau thu hồi an toàn.

## Build và kiểm tra

```powershell
.\scripts\Build-Tools.ps1
.\scripts\Build-Tools.ps1 -ValidateSource
# Chỉ chạy smoke riêng khi chủ động cần kiểm tra fixture nhỏ.
.\scripts\Smoke-Test-Packager.ps1
```

`Build-Tools.ps1` mặc định chỉ build hai executable x64 và kiểm tra PE machine `0x8664` trước khi publish từng file. Script khóa `GOFLAGS`, `GOENV`, `GOTOOLCHAIN`, `GOWORK`, `GOEXPERIMENT`, `CGO_ENABLED`, `GOOS`, `GOARCH`, `GOAMD64=v1` và `GOFIPS140=off`, ép module ở chế độ `readonly`, resolve một `go.exe` tuyệt đối rồi bắt buộc chạy `Audit-Build.ps1` bằng chính executable đó trên cả hai file tạm trước publish. Các bước `gofmt`,
`go test` và `go vet` chỉ chạy khi chủ động truyền `-ValidateSource`; smoke test
luôn là lệnh riêng, không tự chạy trong quá trình build hoặc đóng gói ISO.

`Build-Tools.ps1` giữ mutex toàn máy cho toàn lượt publish. Setup mới được đưa vào Builder qua overlay build chỉ định rõ, nên validation hoặc build Builder không cần ghi đè `SetupStub.exe` đang phát hành. Builder tự chứa đúng Setup mới được publish trước; nếu publish Setup thất bại trong luồng bình thường, Builder được rollback. Sau một hard-kill,
lượt kế tiếp thu hồi chính xác các tên tạm riêng `.SetupStub-*.exe`,
`.BuilderOverlay-*.json`, `.Builder-*.exe` và `.publish-backup-*`; nó không quét hoặc xóa artifact ngoài
các pattern nội bộ này.

`Audit-Build.ps1` đọc trực tiếp PE header và từ chối mọi Builder/Setup không có machine `0x8664` (`amd64`), ngoài kiểm tra metadata module Go, vật liệu nhạy cảm và chuỗi ASCII/UTF-16 thuộc scope bot, license, `src-bot`, key activation, phase 8 hoặc phase 9. `Build-Tools.ps1` tự gọi audit trong môi trường đã khóa; chạy audit trực tiếp bên ngoài môi trường đó sẽ chủ động bị từ chối.

Builder và Setup không gọi `powershell.exe` qua current directory hoặc `PATH`.
Các bước dò LAN, mount/dismount và tạo UDF luôn resolve Windows PowerShell từ
system directory thật rồi mới chạy ẩn, giảm đường hijack khi đóng gói/cài đặt.

Smoke test mặc định sao chép riêng `src` và `scripts` vào cây `.smoke-*`, build/chạy Builder ở đó rồi chạy `Setup.exe --cli-plan --verify` với Client/Server/Bot fixture nhỏ và payload rời. Nó không thay `dist\KiemTheDeployForge-Builder.exe` hay `src\cmd\builder\assets\SetupStub.exe` của project chính, không tạo ISO, không cài service và không khởi động MySQL. Chỉ dùng `Smoke-Test-Packager.ps1 -IncludeIso` khi thật sự cần kiểm tra IMAPI/UDF, và không dùng cây Client/Server/Bot thật làm fixture.
Smoke test có mutex toàn máy riêng và marker sở hữu. Toàn bộ cây `.smoke-*`, tool copy, log, payload và ISO (nếu được yêu cầu rõ ràng) được dismount rồi xóa có retry trong `finally`; lượt sau cũng thu hồi cây có marker hợp lệ của một tiến trình đã bị hard-kill.

## Cấu trúc

- `src\cmd\builder`: GUI Builder và chế độ CLI phục vụ kiểm thử.
- `src\cmd\setup`: GUI Setup và chế độ plan/install phục vụ kiểm thử có kiểm soát.
- `src\internal\builder`: quét, hash, ghi ZIP64 payload và Setup bootstrap nhỏ.
- `src\internal\sfx`: đọc manifest pin trong Setup, kiểm tra và giải nén `Payload.ktpkg` ZIP64 Store.
- `src\internal\iso`: tạo và xác minh ISO UDF bằng IMAPI2, không mở trình ghi đĩa.
- `src\internal\install`: tự tìm/mount ISO, staging, journal, resume và commit cài đặt.

## Kiểm thử máy sạch

Trước khi phát hành thật cần chạy UAT trên Windows sạch với quyền Administrator, card LAN vật lý, đủ dung lượng NTFS và không có tiến trình khác chiếm cổng `3306`. Nên ký Authenticode cho Builder và Setup trong pipeline phát hành; vật liệu ký không được đặt trong source hoặc artifact trung gian.
