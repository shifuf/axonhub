# Baota (宝塔) Docker 无缝更新指南 — AxonHub fork

适用场景：宝塔面板管理 Docker，服务器上已用 docker-compose 部署 AxonHub
（Postgres 或 SQLite 均可），源站可以是上游 looplj/axonhub 也可以是本 fork。

## 核心思路

更新只做四件事，全部不中断（容器原地重建，数据卷不动）：

1. **在线备份**：pg_dump（Postgres）或直接拷贝 SQLite 文件 -> backups/
2. **国内直接下载镜像归档**：Release 里的 `axonhub-dev-<sha>-linux-amd64.tar.gz`
   经 `gh-proxy.com` 下载，不访问 Docker Hub / ghcr.io
3. **校验并导入**：校验 SHA256 后执行 `docker load`
4. **换镜像 + 重建 + 健康检查**：把 AXONHUB_IMAGE 写进 .env，重建容器；
   /health 通过即成功，失败自动回滚旧镜像

数据库保存在 named volume / 挂载目录里，重建容器不会丢数据；
Ent 迁移在启动时自动执行（自动建新表/新列）。

## 一键更新（推荐）

    cd /www/wwwroot/axonhub    # 你的 compose 部署目录

首次：下载更新脚本（走 gh-proxy 加速，无 git 依赖）

    curl -fSL --connect-timeout 10 --max-time 60 -o axonhub-update.sh \
      "https://gh-proxy.com/https://raw.githubusercontent.com/shifuf/axonhub/dev/deploy/axonhub-update.sh"
    sed -i 's/\r$//' axonhub-update.sh && chmod +x axonhub-update.sh

日常更新（下载本 fork dev 分支的最新 Release 镜像归档）：

    bash axonhub-update.sh

或锁定某个 dev 提交版本（推荐生产使用）：

    bash axonhub-update.sh dev-f95e5908

可选环境变量：

- AXONHUB_DIR=/path   指定部署目录（默认自动探测 /www/wwwroot/axonhub 等）
- RELEASE_PROXY=https://gh-proxy.com/   镜像归档下载加速地址；设为空则直连
- IMAGE=ghcr.io/shifuf/axonhub:vX.Y.Z    指定完整镜像并改用 registry 模式
- USE_REGISTRY=1      对 vX.Y.Z 也改用 registry 模式
- PULL_IMAGES=1       registry 模式下执行 docker compose pull（archive 模式不需要）
- SKIP_BACKUP=1       跳过备份（不推荐）

脚本行为：[INFO] online backup... -> [INFO] download (mirror)... ->
[OK] sha256 verified -> [INFO] recreating container... ->
[INFO] health check ... -> [OK] update complete

Release 同时提供两个归档名：

- `axonhub-dev-<sha>-linux-amd64.tar.gz`：固定提交版本，推荐生产使用
- `axonhub-linux-amd64.tar.gz`：最新 dev 别名，供 `bash axonhub-update.sh` 使用

## 首次部署（如果服务器上还没有 compose）

    mkdir -p /www/wwwroot/axonhub && cd /www/wwwroot/axonhub
    curl -fSL -o docker-compose.yml "https://gh-proxy.com/https://raw.githubusercontent.com/shifuf/axonhub/dev/docker-compose.yml"
    cat > .env <<'EOF'
    DB_PASSWORD=改成强密码
    AXONHUB_HOST_PORT=8090
    POSTGRES_IMAGE=postgres:16-alpine
    AXONHUB_IMAGE=ghcr.io/shifuf/axonhub:dev-<sha>
    EOF
    docker compose up -d
    curl http://127.0.0.1:8090/health

SQLite 单容器部署：把 docker-compose.yml 中 axonhub-sqlite 段取消注释，
并注释掉 postgres + axonhub 两个服务（需 mkdir data && chmod 750 data && chown 65532:65532 data）。

## 宝塔侧注意

- 反代：站点 -> 反向代理 -> 目标 http://127.0.0.1:8090（更新不需要改）
- 更新前后容器由 compose 管理，重建后名字/卷不变
- 更新后浏览器 Ctrl+F5 强刷（前端内嵌进二进制，有新构建才需要）

## 与上游同步

仓库已配置两个分支工作流：

- unstable 分支：每天自动镜像上游 looplj/axonhub（sync-upstream.yml）
- dev 分支：你的功能分支；push 到 dev 触发 docker-publish.yml，
  构建 ghcr.io/shifuf/axonhub:latest 与 dev-<sha>，并发布同名 GitHub Release；
  Release 上传 Linux amd64 镜像归档和 SHA256，供国内服务器直接下载并 `docker load`

合并流程：本地 git fetch origin && git merge origin/unstable（解决冲突、
review 后）-> git push origin dev -> Actions 出镜像 -> 服务器
bash axonhub-update.sh dev-<新sha>

## 回滚

    cd /www/wwwroot/axonhub
    ls backups/                     # 找到更新时间点的备份
    cp .env.bak.<stamp> .env        # 或手动把 AXONHUB_IMAGE 改回旧 tag
    docker compose up -d --force-recreate axonhub

数据库回滚（如需）：

- Postgres: docker compose exec -T postgres psql -U axonhub -d axonhub < backups/axonhub-pg-<stamp>.sql
- SQLite:   停容器后用 backups/*.db 覆盖 data/*.db 再启动
