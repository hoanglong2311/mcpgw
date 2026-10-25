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
go install github.com/hoanglong2311/mcpgw/cmd/mcpctl@latest   # vào $(go env GOPATH)/bin
```

### Cách 3 — build từ source

```sh
git clone https://github.com/hoanglong2311/mcpgw.git && cd mcpgw
make install        # bin/mcpctl → ~/.local/bin/mcpctl (đổi bằng PREFIX=...)
make install-app    # mcpctl-tray.app (ký adhoc) → ~/Applications
```

Thêm `~/.local/bin` vào `PATH` nếu chưa có. Đặt binary ở chỗ cố định **trước** khi chạy `mcpctl daemon install`, vì LaunchAgent lưu đường dẫn tuyệt đối của binary lúc cài.

## Setup

**1. Config.** Copy file mẫu rồi sửa lại: URL, username, chỉ giữ server mình dùng.

```sh
mkdir -p ~/.config/mcpctl
cp config.example.toml ~/.config/mcpctl/config.toml
chmod 600 ~/.config/mcpctl/config.toml
```

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

**4. Nối client.** In snippet cho từng client rồi dán vào config của client đó:

```sh
mcpctl clients               # snippet cho mọi client
mcpctl clients claude-code   # in lệnh `claude mcp add --transport http ...`
mcpctl clients codex         # block [mcp_servers.*] cho ~/.codex/config.toml
```

| Client | Nơi dán |
|---|---|
| `claude-code` | chạy các lệnh in ra trong shell |
| `codex` | `~/.codex/config.toml` |
| `orca` | Orca MCP settings (TOML) |
| `grok` | `~/.grok/config.toml` |
| `hermes` | `~/.hermes/config.yaml` |
| `claude-desktop` | `~/Library/Application Support/Claude/claude_desktop_config.json` (dùng bridge stdio `mcpctl connect <name>`) |

Nếu client đang khai báo cùng server theo kiểu stdio (có token inline), xoá entry cũ đi rồi mới thêm entry mới. Nhớ backup file config trước. Phiên agent đang mở vẫn giữ process cũ, chỉ phiên mở sau mới đi qua gateway.

**5. Menu bar (tuỳ chọn).** Mở `~/Applications/mcpctl-tray.app`. Từ menu có thể bật/tắt/restart từng server, Copy URL, Reload config, mở config/log, Start gateway và Start at login.

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
