#!/bin/sh
# app/ui/index.cgi — 飞牛微应用的 CGI 反代入口。
#
# 应用入口走飞牛反代（app/ui/config 不声明 port），飞牛 Nginx 会把
# 请求路由到 /cgi/ThirdParty/UPS-Helper/index.cgi/，由本脚本转发给后端。
# 这样应用页面与飞牛 Web 同源，并处于飞牛登录会话保护之内；后端只需监听
# 127.0.0.1，不再对局域网/公网暴露端口。
#
# 复用主二进制而非再打一个独立代理二进制：代理需要 net/http，独立编译会让 fpk
# 从 3.5MB 涨到 6MB；改为 exec 主程序的 -cgi 模式后体积基本不变。
# 注意：fpk 是 zip 解包，不保留可执行位，由 cmd/install_init 显式赋权。
exec /var/apps/UPS-Helper/target/UPS-Helper -cgi
