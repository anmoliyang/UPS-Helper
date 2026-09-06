# UPS 助手 (UPS-Helper) — 飞牛 fnOS 原生 UPS 远程代理

将一台局域网内运行 NUT 协议的远程 UPS 设备，通过 USB/IP 协议桥接为飞牛 fnOS
本地的 HID UPS 设备，使系统级 UPS 保护（断电安全关机）能在没有物理 USB UPS
的环境下正常工作。

> Go + 标准库 · 零外部依赖 · 静态编译 · 飞牛 fpk 原生应用，本项目参考 https://github.com/iwinmin/fnos-remote-ups。

---

## 一、安装与使用

### 环境要求
- 飞牛 fnOS ≥ 0.9.0，已开启 SSH
- 内核已加载 `vhci-hcd`（未加载则 `modprobe vhci-hcd`）
- USB/IP 客户端由本应用自带，无需系统预装
- 局域网内有一台运行 NUT 的机器（Synology/QNAP NAS、装了 `nut-server` 的 Linux 等）

### 安装与配置
- 飞牛桌面 → 应用中心 → 右上角 ⋮ → 手动安装 → 选择 `UPS 助手-1.0.x.fpk`
- 安装完成后，从桌面图标或应用中心「打开」进入 Web 页面
- 进入「连接配置」，填写 `UPS 名称@IP地址`（如 `ups@192.168.1.50`）；
  主服务器要求认证时开启「NUT 认证」开关并填写账号
- 保存即生效，无需重启

> Web 入口由飞牛反代提供，与飞牛 Web 同源并处于登录会话保护内。
> 后端只监听本机回环，直连设备 IP:53713 会被拒绝（这是刻意的安全设计）。

### 验证
- 飞牛桌面「硬件 → UPS」应能识别到设备并显示实时数据（电量、电压、状态等）
- Web 界面顶部状态指示灯：绿（在线）/ 黄 / 红；实时数据每 3 秒刷新
- 模拟断电：拔掉市电或按 UPS 测试键，状态应从 `OL` 变为 `OB`，飞牛桌面 UPS 状态同步变化

### 升级与卸载
- 升级：上传新版本 fpk 覆盖安装，服务会自动重启生效
- 卸载：应用中心卸载即可，会自动停服并清理

---

## 二、NUT 服务器端开放局域网访问（必做）

UPS 助手只是 **NUT 客户端**，它连接你网络中另一台已接入物理 UPS 的 NUT 服务器
（Synology/QNAP NAS、跑 `nut-server` 的 Linux、或飞牛自身装了 NUT 服务端等）。
若那台服务器的 `upsd` 只监听本机回环，UPS 助手就会报
`dial tcp <ip>:3493: connection refused`（主机可达但端口无服务），或
`i/o timeout`（主机不可达）。

### 飞牛当 NUT 服务器端（飞牛直连 UPS 的场景）

飞牛 fnOS 内置了 NUT 服务端（USB 直连 UPS 时会自动起 `upsd`），
但默认只监听 `127.0.0.1:3493`，且未提供开放局域网监听的 Web 开关，
需 SSH 进那台飞牛一次（改完永久生效）：

```sh
ssh 管理员@<NUT服务器IP>
sudo -i

# ① 查真实 UPS 名（飞牛给的是一串数字，不是 myups！）
cat /etc/nut/ups.conf | grep -E '^\['            # 例如输出 [1234567890]

# ② 改监听地址，允许局域网
cp /etc/nut/upsd.conf /etc/nut/upsd.conf.bak
sed -i 's/^LISTEN 127.0.0.1 3493/LISTEN 0.0.0.0 3493/' /etc/nut/upsd.conf
grep -i listen /etc/nut/upsd.conf                # 确认已改为 0.0.0.0

# ③ 重启 NUT 服务使监听生效
systemctl restart nut-server.service

# ④ （可选）确认飞牛的 NUT 认证账号，UPS 助手要用
grep -E 'password' /etc/nut/upsd.users           # 默认 monuser / trim-secret
```

> ⚠️ **安全提示**：NUT 协议是明文的，`USERNAME`/`PASSWORD` 只做访问控制不加密。
> 改完请确认这台 NUT 服务器没有把 3493 端口映射到公网（UPnP/DMZ/端口转发）。
> 能只监听内网网卡 IP 就不要用 `0.0.0.0`；网段不可信时用防火墙把 3493 限制到 UPS 助手那一台 IP。

在 UPS 助手的 Web UI 里把：
- `远程 NUT UPS` 填成 `<真实ups名>@<NUT服务器IP>`（如 `1234567890@192.168.1.50`）
- `NUT 认证` 用户名 `monuser`、密码 `trim-secret`（飞牛默认值；留空则匿名）
点保存即生效，无需重装/重启。

UPS 助手已内置认证握手，连不上时 `last_error` 会明确提示
`NUT PASSWORD 认证失败` 或 `ACCESS-DENIED`。

### 其他 NUT 服务器
- **群晖**：控制面板 → 硬件和电源 → UPS → 勾选"启用 UPS 网络服务器 / 允许通过网络连接此 UPS"。
- **QNAP**：控制面板 → 外部设备 → UPS → 勾选"允许远程连接"。
- 确认监听地址：`cat /etc/nut/upsd.conf | grep -i listen`，默认多为 `LISTEN 127.0.0.1 3493`，需改为允许局域网后 `systemctl restart nut-server`。

### 连接错误处理预期
UPS 助手对 NUT 连接失败会持续重试 + 优雅降级：Web 状态显示"未连接"，
`last_error` 字段给出具体错误（timeout / refused / auth）。连接恢复后自动重连，
无需重启。

---

## 三、故障排查

| 现象 | 排查 |
|------|------|
| Web 打开 404 | 确认从飞牛桌面/应用中心「打开」进入；`/api/health` 应返回 200 |
| NUT 始终 "未连接" | `nc -vz <NUT主机> 3493`；`upsc <配置>`；看 Web UI 的 `last_error` 字段 |
| 自动挂载失败 | `usbip port` 是否有 `1-1`；`dmesg \| tail` 有无 vhci_hcd 错误；服务日志 `/var/apps/UPS-Helper/var/log/app.log` 中 "自动挂载失败: ..." 的具体原因 |
| 飞牛桌面无 UPS 入口 | 确认已成功 attached；fnOS「硬件 → UPS」刷新 |
| `开放 API` 注入失败 | 确认 `manifest` 含 `micro_app=true`；fnOS 版本 ≥ 0.9.0 |
| NUT 连不上（timeout / refused） | 这是 NUT **服务器端**未开放局域网访问（见第二章），不是本应用问题 |
| 设置页设备名显示 "UPS Remote" | 正常——这是本应用模拟 USB 设备的产品名。只要点下一步能稳定接管、Web/硬件页能看到实时电量，即为成功 |


