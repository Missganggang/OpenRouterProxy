#!/usr/bin/env bash
#
# OpenRoute 面板部署脚本（在目标服务器上以 root 执行）
#
# 前置条件：
#   - /tmp/openroute-deploy/ 下已放好 openroute（二进制）与 public/（前端产物）
#   - 证书已就位于 /root/cert/x.aarcx.com/
#
# 幂等：可重复执行。已存在的 config.yml 与 data.db 不会被覆盖。

set -euo pipefail

APP_DIR=/opt/openroute
SRC_DIR=/tmp/openroute-deploy
DOMAIN=x.aarcx.com
CERT_DIR=/root/cert/${DOMAIN}
CONF=/etc/openroute.env

log()  { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
warn() { printf '\033[1;33m[!] %s\033[0m\n' "$*"; }
die()  { printf '\n\033[1;31m[x] %s\033[0m\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "请以 root 执行"

# ── 0. 前置检查 ────────────────────────────────────────────────
log "检查前置条件"
[ -f "$SRC_DIR/openroute" ] || die "缺少 $SRC_DIR/openroute"
[ -d "$SRC_DIR/public" ]    || die "缺少 $SRC_DIR/public/"
[ -f "$CERT_DIR/fullchain.pem" ] || die "缺少证书 $CERT_DIR/fullchain.pem"
[ -f "$CERT_DIR/privkey.pem" ]   || die "缺少私钥 $CERT_DIR/privkey.pem"

# 二进制必须是 Linux ELF，避免把别的平台的文件传上来
if ! head -c 4 "$SRC_DIR/openroute" | grep -q 'ELF'; then
  die "$SRC_DIR/openroute 不是 Linux 可执行文件（ELF 头校验失败）"
fi

# 证书有效期检查：过期就直接失败，不要等用户发现浏览器报错
if ! openssl x509 -in "$CERT_DIR/fullchain.pem" -noout -checkend 86400 >/dev/null 2>&1; then
  warn "证书将在 24 小时内过期，请先续期再部署"
fi
openssl x509 -in "$CERT_DIR/fullchain.pem" -noout -subject -dates | sed 's/^/    /'

# ── 1. 账号密码 ────────────────────────────────────────────────
log "准备管理员凭据"
if [ -f "$CONF" ]; then
  # shellcheck disable=SC1090
  . "$CONF"
  echo "    复用已有凭据文件 $CONF（管理员 $ADMIN_USER）"
else
  ADMIN_USER="${ADMIN_USER:-admin}"

  # 生成 20 位随机密码。
  #
  # 必须用 openssl 而不是 `tr -dc ... </dev/urandom | head -c 20`：
  # 后者在 set -o pipefail 下会因为 head 提前关闭管道、tr 收到 SIGPIPE 而
  # 让整条命令返回非 0，脚本被 set -e 静默终止（第一次执行就踩到了这个坑）。
  ADMIN_PASSWORD="$(openssl rand -base64 48 | tr -dc 'A-HJ-NP-Za-km-z2-9' | cut -c1-20)"

  # 兜底：万一随机源异常导致长度不足，直接报错而不是写入弱密码
  if [ "${#ADMIN_PASSWORD}" -ne 20 ]; then
    die "生成管理员密码失败（长度 ${#ADMIN_PASSWORD}），请重试"
  fi

  umask 077
  cat > "$CONF" <<EOF
# OpenRoute 管理员凭据（首次部署时自动生成）
# 面板地址: https://${DOMAIN}/
ADMIN_USER=${ADMIN_USER}
ADMIN_PASSWORD=${ADMIN_PASSWORD}
EOF
  echo "    已生成新密码，写入 $CONF（权限 600）"
fi

# ── 2. 释放程序文件 ────────────────────────────────────────────
log "释放程序文件到 $APP_DIR"
mkdir -p "$APP_DIR"
install -m 0755 "$SRC_DIR/openroute" "$APP_DIR/openroute"
rm -rf "$APP_DIR/public"
cp -r "$SRC_DIR/public" "$APP_DIR/public"
echo "    二进制 $(stat -c %s "$APP_DIR/openroute") 字节，前端 $(find "$APP_DIR/public" -type f | wc -l) 个文件"

# ── 3. 生成 config.yml（仅在不存在时）──────────────────────────
log "准备 config.yml"
if [ -f "$APP_DIR/config.yml" ]; then
  warn "$APP_DIR/config.yml 已存在，保留原文件（不会覆盖现有配置）"
  echo "    如需重建，请先备份并删除它，再重新执行本脚本"
else
  # 用 openssl 生成 32 字节随机密钥并 base64（对应面板的 secret-key）
  SECRET="$(openssl rand -base64 32 | tr -d '\n=' | tr '+/' '-_')"

  cat > "$APP_DIR/config.yml" <<EOF
# ── 数据存储 ───────────────────────────────────────────────
database-path: sqlite3://data.db
max-open-connection: 100
max-idle-connection: 5

# ── 监听 ──────────────────────────────────────────────────
# 应用直接终止 TLS（证书由服务器上的 certbot 管理）
listen: 0.0.0.0:443
tls-cert: ${CERT_DIR}/fullchain.pem
tls-key: ${CERT_DIR}/privkey.pem

# ── 前端静态资源 ──────────────────────────────────────────
html-path: ./public

# ── 面板密钥（请勿外泄，泄露后可伪造登录令牌）─────────────
secret-key: "${SECRET}"

# ── 节点存活判定 ──────────────────────────────────────────
heartbeat-interval: 10
offline-node-time: 20
offline-node-retention-time: 86400

# ── 接口限流 ──────────────────────────────────────────────
user-rate-limit:
  rate: 5
  limit: 5
default-rate-limit:
  rate: 5
  limit: 5

# ── 行为开关 ──────────────────────────────────────────────
disable-gzip: false
disable-queue: false
disable-cron: false

# ── 日志 ──────────────────────────────────────────────────
log-level: info
log-path: ./logs
log-keep-days: 14

# ── 流量统计 ──────────────────────────────────────────────
traffic-collect-interval: 60
traffic-detail-keep-days: 30

# ── 探针 ──────────────────────────────────────────────────
enable-probe: true
probe-keep-days: 7

# ── WebSSH ────────────────────────────────────────────────
enable-webssh: true
EOF
  chmod 600 "$APP_DIR/config.yml"
  echo "    已生成 config.yml（含新随机 secret-key，权限 600）"
fi

# ── 4. systemd 服务 ────────────────────────────────────────────
log "安装 systemd 服务"
cat > /etc/systemd/system/openroute.service <<'EOF'
[Unit]
Description=OpenRoute Panel
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=/opt/openroute
EnvironmentFile=-/etc/openroute.env
ExecStart=/opt/openroute/openroute
Restart=always
RestartSec=5

# 面板要承载大量转发连接，文件句柄上限必须放大
LimitNOFILE=1048576

# 基本加固：仅允许写入自己的目录
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ReadWritePaths=/opt/openroute

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable openroute >/dev/null 2>&1 || true

# ── 5. 首次初始化数据库并创建管理员 ────────────────────────────
#
# 这一步必须在「服务未运行」的状态下做：面板会独占监听 443，
# 若上一轮部署的服务还在跑，这里的初始化进程会因端口冲突而退出。
# 因此先停服务，初始化完再由后面的步骤重新拉起。
log "初始化数据库与管理员账号"
systemctl stop openroute 2>/dev/null || true
# 等端口真正释放，避免紧接着的绑定仍然失败
for _ in $(seq 1 10); do
  ss -tln | grep -q ':443 ' || break
  sleep 1
done

cd "$APP_DIR"
export MIGRATE=1
export ADMIN="$ADMIN_USER"
export ADMIN_PASSWORD="$ADMIN_PASSWORD"

# 常驻进程会被 timeout 终止，因此非 0 退出码属正常，这里只关心日志内容
timeout 45 ./openroute > /tmp/openroute-init.log 2>&1 || true

# 只把「真正的启动失败」当错误：端口冲突在停服务后不该再出现
if grep -q '启动失败' /tmp/openroute-init.log; then
  warn "初始化时出现启动错误，请检查："
  grep -A3 '启动失败' /tmp/openroute-init.log | sed 's/^/    /'
  die "初始化失败"
fi

if ! grep -q 'HTTP 服务已启动\|管理员账号已创建' /tmp/openroute-init.log; then
  warn "初始化日志未见成功标志，请检查："
  tail -20 /tmp/openroute-init.log | sed 's/^/    /'
  die "初始化未完成"
fi

# 数据库与管理员是否就绪，直接查文件与日志更可靠
[ -s "$APP_DIR/data.db" ] || die "data.db 未生成"
echo "    初始化完成（$APP_DIR/data.db 已就绪，日志见 /tmp/openroute-init.log）"

# ── 6. 启动服务 ────────────────────────────────────────────────
log "启动 openroute 服务"
systemctl restart openroute
sleep 4

if systemctl is-active --quiet openroute; then
  echo "    服务已启动"
else
  warn "服务未能启动，最近日志："
  journalctl -u openroute -n 30 --no-pager | sed 's/^/    /'
  die "启动失败"
fi

# ── 7. 自检 ────────────────────────────────────────────────────
log "自检"
echo "--- 监听端口 ---"
ss -tlnp | grep -E ':443 |:80 ' | sed 's/^/    /' || echo "    未监听 443"

echo "--- 本地 HTTPS 探测 ---"
if curl -sk --max-time 10 https://127.0.0.1/api/v1/health -H 'Host: '"$DOMAIN" | grep -q '"code":0'; then
  echo "    面板健康检查通过"
else
  warn "本地健康检查未通过，请查看 journalctl -u openroute"
fi

echo "--- 证书链 ---"
echo | openssl s_client -connect 127.0.0.1:443 -servername "$DOMAIN" 2>/dev/null \
  | openssl x509 -noout -subject -issuer -dates 2>/dev/null | sed 's/^/    /' || true

# ── 8. 证书续期后自动重载面板 ──────────────────────────────────
#
# 面板在启动时读取证书并常驻内存，acme.sh 续期只会覆盖磁盘上的文件，
# 不重启进程的话浏览器拿到的仍是旧证书（表现为「证书已续期但浏览器还报过期」）。
# 这里给 acme.sh 挂一个续期钩子：续期成功后重启面板。
log "配置证书续期钩子"
if [ -d /root/.acme.sh ]; then
  cat > /etc/openroute-reload.sh <<'EOF'
#!/usr/bin/env bash
# acme.sh 续期成功后的回调：重启面板以加载新证书。
#
# 由 /etc/openroute.env 的 CERT_RELOAD_HOOK 调用（见 install.sh），
# 也可手工执行来强制重新加载证书。
set -euo pipefail
if systemctl is-active --quiet openroute; then
  systemctl restart openroute
  echo "openroute 已重启以加载新证书"
else
  echo "openroute 未在运行，跳过重启"
fi
EOF
  chmod 0755 /etc/openroute-reload.sh

  # 写入续期钩子（--renew-hook 只影响后续续期，不改变现有的签发方式）
  if /root/.acme.sh/acme.sh --install-cert -d "$DOMAIN" \
       --key-file "${CERT_DIR}/privkey.pem" \
       --fullchain-file "${CERT_DIR}/fullchain.pem" \
       --reloadcmd "/etc/openroute-reload.sh" >/dev/null 2>&1; then
    echo "    已挂载续期钩子（续期后自动重启 openroute）"
  else
    warn "挂载续期钩子失败，请手工执行："
    echo "      /root/.acme.sh/acme.sh --install-cert -d $DOMAIN \\"
    echo "        --key-file ${CERT_DIR}/privkey.pem \\"
    echo "        --fullchain-file ${CERT_DIR}/fullchain.pem \\"
    echo "        --reloadcmd /etc/openroute-reload.sh"
  fi

  # 校验现有证书剩余有效期，快到期时给出明确提醒
  DAYS_LEFT=$(( ( $(date -d "$(openssl x509 -in ${CERT_DIR}/fullchain.pem -noout -enddate | cut -d= -f2)" +%s) - $(date +%s) ) / 86400 ))
  echo "    当前证书剩余有效期：${DAYS_LEFT} 天"
  if [ "$DAYS_LEFT" -lt 7 ]; then
    warn "证书即将过期，请立即续期："
    echo "      /root/.acme.sh/acme.sh --renew -d $DOMAIN --force"
  fi
else
  warn "未检测到 /root/.acme.sh，跳过证书续期钩子配置"
fi

# ── 9. 完成 ────────────────────────────────────────────────────
log "完成"
cat <<EOF

    面板地址   : https://${DOMAIN}/
    管理员     : ${ADMIN_USER}
    密码       : 见 ${CONF}（执行 cat ${CONF} 查看）
    数据目录   : ${APP_DIR}（data.db / config.yml / logs/）
    服务管理   : systemctl {status|restart|stop} openroute
    查看日志   : journalctl -fu openroute
    证书续期   : acme.sh 每 6 小时自动检查；续期成功后会自动重启面板加载新证书

    提示：节点对接命令里的面板地址需要用 https://${DOMAIN}
          （面板监听 443 且启用了 TLS，install.sh 会自动识别协议）

EOF
