# mcpctl

Gateway MCP chạy local trên macOS. Mỗi MCP server stdio (Jira, GitLab, MongoDB, MySQL…) chỉ được khai báo **một lần** trong `~/.config/mcpctl/config.toml`. Một daemon dưới launchd giữ các server này và expose mỗi cái thành một endpoint Streamable HTTP `http://127.0.0.1:8765/s/<name>`. Mọi MCP client (Claude Code, Codex, Grok, Hermes, Orca, Claude Desktop) đều trỏ về cùng URL đó.

Lợi ích:
- Mỗi server chỉ có một process dùng chung cho mọi client, thay vì mỗi phiên agent spawn một bản `npx`/`uvx` riêng.
- Token nằm trong macOS Keychain. Không còn token inline trong `~/.claude.json` hay `~/.codex/config.toml`.
- Bật/tắt/restart server bằng CLI hoặc menu bar, không phải sửa config của từng client.
- Gateway chỉ listen trên loopback.

## Yêu cầu

- macOS 13+ (Apple Silicon hoặc Intel).
- Runtime mà các MCP server cần: thường là `node` (cho `npx`) và `uv` (cho `uvx`).
  ```sh
  brew install node uv
  ```
- Chỉ cần Go 1.27+ khi build từ source (`brew install go`).

## Cài đặt

### Cách 1 — tải bản release (không cần Go)

Tải `mcpctl_<version>_darwin_arm64.tar.gz` (Apple Silicon) hoặc `..._darwin_amd64.tar.gz` (Intel) từ [Releases](https://github.com/hoanglong2311/mcpgw/releases):

```sh
tar -xzf mcpctl_*_darwin_*.tar.gz && cd mcpctl_*_darwin_*/
xattr -dr com.apple.quarantine mcpctl mcpctl-tray.app   # binary chưa notarize
mkdir -p ~/.local/bin ~/Applications
install -m 755 mcpctl ~/.local/bin/mcpctl
cp -R mcpctl-tray.app ~/Applications/
```

### Cách 2 — `go install`

```sh
go install github.com/hoanglong2311/mcpgw/cmd/mcpctl@latest        # vào $(go env GOPATH)/bin, thường là ~/go/bin
go install github.com/hoanglong2311/mcpgw/cmd/mcpctl-tray@latest   # menu bar, tuỳ chọn
```

Cách này chỉ ra binary, không có file config mẫu (lấy config ở bước Setup). `mcpctl-tray` cài theo cách này là binary trần, không có `.app`, nên không dùng được **Start at login**. Muốn có mục đó thì lấy `mcpctl-tray.app` từ bản release (Cách 1). Nếu `mcpctl` báo `command not found`, thêm `export PATH="$HOME/go/bin:$PATH"` vào `~/.zshrc`.

### Cách 3 — build từ source

```sh
git clone https://github.com/hoanglong2311/mcpgw.git && cd mcpgw
make install        # bin/mcpctl → ~/.local/bin/mcpctl (đổi bằng PREFIX=...)
make install-app    # mcpctl-tray.app (ký adhoc) → ~/Applications
```

Thêm `~/.local/bin` vào `PATH` nếu chưa có. Đặt binary ở chỗ cố định **trước** khi chạy `mcpctl daemon install`, vì LaunchAgent lưu đường dẫn tuyệt đối của binary lúc cài.

## Setup

**1. Config.** Tải file mẫu về, rồi sửa lại: URL, username, chỉ giữ server mình dùng. Cách này dùng được với cả ba cách cài ở trên.

```sh
mkdir -p ~/.config/mcpctl
curl -fsSL https://raw.githubusercontent.com/hoanglong2311/mcpgw/main/config.example.toml -o ~/.config/mcpctl/config.toml
chmod 600 ~/.config/mcpctl/config.toml
```

Nếu team của bạn có sẵn repo config dùng chung thì lấy file từ repo đó thay cho file mẫu.

- Tên server: `[a-z0-9-]`, tối đa 32 ký tự. Tên này là path trong URL `/s/<name>`.
- `command` nên ghi đường dẫn tuyệt đối. Mac Intel dùng `/usr/local/bin/...` thay cho `/opt/homebrew/bin/...`.
- `env` chứa giá trị không bí mật. `keychain_env` map tên biến môi trường sang `service/account` trong Keychain.
- `enabled = false` để khai báo server nhưng chưa chạy.

**2. Secret.** Mỗi ref trong `keychain_env` cần được lưu một lần. Lệnh sẽ hỏi giá trị, không echo ra màn hình:

```sh
mcpctl secret set mcpctl/jira-pat
mcpctl secret set mcpctl/gitlab-pat
```

Secret được lưu với ACL cho `/usr/bin/security`, nên daemon dưới launchd đọc được mà không bật prompt.

**3. Daemon.**

```sh
mcpctl daemon install     # tạo ~/Library/LaunchAgents/local.mcpctl.plist và start
mcpctl status             # mọi server phải ở ready hoặc idle, cột ERROR là "-"
```

**4. Nối client.** Mỗi server có một URL `http://127.0.0.1:8765/s/<name>`. Lấy danh sách tên server bằng `mcpctl list`. Client nào cũng làm theo 5 bước:

1. Backup file config của client.
2. Nếu client đã khai báo server cùng tên theo kiểu stdio cũ (`npx`/`uvx` + token inline) thì **xoá entry đó trước**.
3. Thêm entry HTTP trỏ về gateway.
4. Kiểm tra lại bằng lệnh `list` của client.
5. Mở **phiên mới**. Phiên đang mở vẫn giữ process cũ.

`mcpctl clients <client>` in sẵn snippet đúng định dạng của từng client, dùng tên và URL lấy từ config của mình.

#### Claude Code

```sh
cp -p ~/.claude.json ~/.claude.json.bak

claude mcp remove gitlab -s user                  # chỉ khi đã có bản stdio cũ cùng tên
claude mcp add --transport http --scope user gitlab http://127.0.0.1:8765/s/gitlab

claude mcp list                                   # gitlab: http://127.0.0.1:8765/s/gitlab (HTTP) - ✔ Connected
```

Thêm tất cả server một lần (sau khi đã xoá các entry trùng tên): `mcpctl clients claude-code | sh`.

#### Codex CLI

```sh
cp -p ~/.codex/config.toml ~/.codex/config.toml.bak

codex mcp remove gitlab                           # chỉ khi đã có bản stdio cũ cùng tên
codex mcp add gitlab --url http://127.0.0.1:8765/s/gitlab

codex mcp list
```

Hoặc dán output của `mcpctl clients codex` vào `~/.codex/config.toml`.

#### Grok CLI

```sh
cp -p ~/.grok/config.toml ~/.grok/config.toml.bak

grok mcp remove gitlab                            # chỉ khi đã có bản stdio cũ cùng tên
grok mcp add --scope user gitlab http://127.0.0.1:8765/s/gitlab

grok mcp list
```

Grok tự đọc thêm MCP server từ config của Claude Code. Để không bị trùng server, thêm đoạn sau vào `~/.grok/config.toml`:

```toml
[compat.claude]
mcps = false
```

Chạy `grok inspect` trong một repo để xem Grok đang nạp server từ nguồn nào. Output của `mcpctl clients grok` có sẵn cả hai phần trên.

#### Hermes

```sh
hermes mcp remove gitlab                          # chỉ khi đã có bản cũ cùng tên
hermes mcp add gitlab --url http://127.0.0.1:8765/s/gitlab
hermes mcp test gitlab
```

Hoặc dán output của `mcpctl clients hermes` vào `~/.hermes/config.yaml`.

#### Orca

Dán output của `mcpctl clients orca` (TOML `[mcp_servers.*]`) vào phần MCP settings của Orca.

#### Claude Desktop

Claude Desktop chỉ chạy được MCP stdio, nên `mcpctl connect <name>` làm cầu nối stdio sang gateway. Dán output của `mcpctl clients claude-desktop` vào `mcpServers` trong `~/Library/Application Support/Claude/claude_desktop_config.json`, rồi tắt hẳn Claude Desktop và mở lại.

Sau khi mọi client đã chạy ổn qua gateway: xoá token inline còn sót trong file config cũ của client, và xoá các file `.bak` có chứa token.

**5. Menu bar app `mcpctl-tray` (tuỳ chọn, nên dùng).** Đây là icon trên menu bar để xem và điều khiển gateway, khỏi phải mở terminal. Tray chỉ là giao diện, nói chuyện với daemon qua control socket. Tắt tray thì gateway **vẫn chạy**.

```sh
open ~/Applications/mcpctl-tray.app      # cài theo Cách 1 hoặc make install-app
~/go/bin/mcpctl-tray &                   # cài theo Cách 2 (go install)
```

Bật **Start at login** ngay trong menu để lần sau tự mở (cần bản `.app`, macOS 13+). Nếu macOS báo app "is damaged" thì chạy `xattr -dr com.apple.quarantine ~/Applications/mcpctl-tray.app`.

| Mục trong menu | Tác dụng |
|---|---|
| `Gateway: running on 127.0.0.1:8765` | Trạng thái daemon. Nếu là `stopped`/`unreachable` thì có thêm mục **Start gateway** (cần đã chạy `mcpctl daemon install`) |
| `✓ <server> — ready pid … · N sessions` | Mỗi dòng một server. Dấu ✓ là đang bật, bấm vào để bật/tắt (giống `mcpctl enable/disable`). `idle` là chưa có client nào nối vào, process sẽ được khởi động khi có client đầu tiên. Nếu server lỗi, lỗi hiện ngay bên dưới, kèm thời gian retry |
| **Restart ›** | Restart một server, ví dụ sau khi đổi secret |
| **Copy URL ›** | Copy `http://127.0.0.1:8765/s/<name>` để dán vào config client |
| **Reload config** | Đọc lại `config.toml` sau khi sửa (giống `mcpctl reload`) |
| **Open config** / **Open logs folder** | Mở `~/.config/mcpctl/config.toml` / `~/.local/state/mcpctl/logs` |
| **Start at login** | Tự mở tray khi đăng nhập macOS |
| **Quit tray** | Chỉ tắt icon, **không** tắt gateway. Muốn tắt hẳn gateway thì chạy `mcpctl daemon uninstall` |

## Dùng hằng ngày

```sh
mcpctl status                 # trạng thái daemon + từng server
mcpctl list                   # server đã khai báo (chạy được khi daemon tắt)
mcpctl enable|disable <name>
mcpctl restart <name>         # sau khi đổi secret của server đó
mcpctl reload                 # sau khi sửa config.toml
mcpctl daemon status
```

Log: `~/.local/state/mcpctl/logs/daemon.log` (xoay vòng 5 MiB), `launchd.out` / `launchd.err` nằm cùng thư mục.

## Gỡ cài đặt

```sh
mcpctl daemon uninstall
rm -rf ~/.local/bin/mcpctl ~/Applications/mcpctl-tray.app ~/.local/state/mcpctl
# tuỳ chọn: rm -rf ~/.config/mcpctl ; xoá các item "mcpctl/*" trong Keychain Access
```

Sau đó trả các client về cấu hình MCP cũ.

## Xử lý sự cố

- **`mcpctl status` báo daemon không chạy:** xem `launchd.err`, sau đó `mcpctl daemon install` lại.
- **Server `error`, `keychain ... not found`:** chưa chạy `mcpctl secret set` cho ref đó.
- **Server `error`, `executable file not found`:** `command` sai đường dẫn. Kiểm tra bằng `which npx` / `which uvx`.
- **macOS báo app "is damaged" / "cannot be opened":** chạy `xattr -dr com.apple.quarantine` với file đã tải về.
- **Cổng 8765 bị chiếm:** đổi `[gateway] listen`, chạy `mcpctl daemon install` lại rồi lấy snippet client mới.

## Phát triển

```sh
make test     # go test -race ./...
make lint     # go vet + gofmt
make release VERSION=v0.1.0   # tarball darwin arm64 + amd64 trong release/
```

Push tag `v*` thì GitHub Actions build và đăng release.
