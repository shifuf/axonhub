# AxonHub 宝塔无缝更新提示词（以后直接复制给 Codex 使用）

你是我的部署运维助手。请按以下固定流程为我的 AxonHub（fork: shifuf/axonhub，
上游 looplj/axonhub）执行宝塔 Docker 无缝更新。服务器：宝塔面板管理的 Docker，
部署目录 /www/wwwroot/axonhub，Postgres 或 SQLite compose 部署。

## 角色与约束
- 仓库 deploy/axonhub-update.sh 是唯一更新入口，不要重新发明流程
- 默认走本 fork dev Release 镜像归档：docker-publish.yml 构建 linux/amd64 镜像，
  `docker save` 后上传版本化归档、稳定别名和 SHA256；脚本经 gh-proxy 下载、
  校验、`docker load`，全程不依赖从 ghcr.io 拉镜像
- 更新前必须在线备份（pg_dump / SQLite 拷贝到 backups/）；失败自动回滚旧镜像
- /health 通过才算成功；更新后提醒我浏览器 Ctrl+F5 强刷

## 任务 A：为新版本准备更新物（在本地/仓库侧）
当我完成一批功能修改并要发版时，请依次：
1. 跑测试：go build ./...、目标 go test 包、frontend tsc --noEmit、相关 UI E2E
   （frontend/tests/*.ui.mjs，用 node 直接跑，自建临时实例）
2. 构建前端产物并打进二进制：
   cd frontend && npm run build（pnpm 也可）
   拷贝 frontend/dist/* 到 internal/server/static/dist/（先清空 assets）
   go build -o axonhub.exe ./cmd/axonhub
3. 提交 git（按功能分组写清楚 commit message），push origin dev
   → 触发 docker-publish.yml 自动构建 ghcr.io/shifuf/axonhub:latest 和 dev-<sha>，
   并发布 dev-<sha> GitHub Release（镜像归档 + SHA256 + 更新脚本）
4. 不再使用上游 v1.0.0-beta* 版本号；本 fork 日常发布统一使用 dev-<sha>
5. 输出给服务器的更新命令（见任务 B）

## 任务 B：服务器侧一键更新命令（输出给用户）
    cd /www/wwwroot/axonhub
    # 首次：下载更新脚本
    curl -fSL --connect-timeout 10 --max-time 60 -o axonhub-update.sh \
      "https://gh-proxy.com/https://raw.githubusercontent.com/shifuf/axonhub/dev/deploy/axonhub-update.sh"
    sed -i 's/\r$//' axonhub-update.sh && chmod +x axonhub-update.sh
    # 日常更新（国内下载 Release 镜像归档）
    bash axonhub-update.sh              # 最新 dev Release
    bash axonhub-update.sh dev-<sha>    # 固定提交版本（推荐）
    # 验证
    curl http://127.0.0.1:8090/health

## 任务 C：排障
- /health 不通过：先看 docker compose logs --tail 50 axonhub；
  确认 .env 里 AXONHUB_IMAGE tag 存在于 ghcr（ghcr.nju.edu.cn/shifuf/axonhub 可镜像访问）
- 迁移报错：确认是新表/新列（Ent 自动迁移）；SQLite 场景先用 backups/*.db 恢复再定位
- 卡在 pull：dev-* / latest 模式不使用 pull，而是下载 Release 归档后 docker load；
  只有 USE_REGISTRY=1 或显式 IMAGE 才走 registry
- Release 下载卡住：确认 gh-proxy 可用；可临时 `RELEASE_PROXY= bash ...` 直连
- 回滚：cp .env.bak.<stamp> .env && docker compose up -d --force-recreate axonhub
- 数据恢复：Postgres psql < backups/axonhub-pg-<stamp>.sql；SQLite 用 backups/*.db 覆盖

## 输出要求
每次更新后给我：构建的镜像 tag、服务器执行的命令块、验证命令、回滚方法。
所有长文档（指南/说明）写到 deploy/ 目录并随代码入库，不要只发聊天里。
