AxonHub 二开完整说明文档
项目：基于 looplj/axonhub 的定制化二开
文档版本：v1.2
上游分支：unstable
本地开发分支：dev
适用对象：个人开发者，fork 后在 dev 分支定制功能，需要每日自动同步上游 unstable，并且仅在 dev 有推送时构建发布 Docker 镜像（以便先审核合并冲突）。

第一部分：Fork 仓库与上游同步策略
1.1 核心原则
用于镜像上游的分支（unstable）不提交个人代码，个人功能全部放在 dev 分支上开发。上游同步与 Docker 构建是两个独立事件，互不触发。

上游同步：每天定时把 looplj/axonhub 的 unstable 同步到你 fork 的 unstable 分支，仅此而已。

Docker 构建：只在你自己向 dev 推送代码时触发。你手动把 unstable 合并进 dev、解决冲突、审核确认后，再 git push 到 dev，才会构建镜像。这样你有完整的审核窗口。

1.2 分支模型
分支	用途	是否提交个人代码	触发事件
unstable（fork 中）	镜像上游 looplj/axonhub 的 unstable	否	每日自动同步（不构建镜像）
dev	所有二开功能	是	手动推送时触发 Docker 构建
冲突暴露时机：你在本地把最新的 unstable 合并进 dev 时，Git 检测到与个人改动的冲突会停下提示。你解决冲突、确认无误后推送 dev，此时才触发镜像构建。这正是你需要的“先审核、后构建”。

1.3 首次配置（一次性）
bash
git remote add upstream https://github.com/looplj/axonhub.git
git remote -v

git checkout -b unstable upstream/unstable
git push origin unstable

git checkout -b dev unstable
git push origin dev
1.4 每日自动同步 unstable（不触发构建）
在 fork 仓库创建 .github/workflows/sync-upstream.yml：

yaml
name: Sync Upstream unstable
on:
  schedule:
    - cron: '0 0 * * *'   # 每天 UTC 0 点
  workflow_dispatch:

jobs:
  sync:
    runs-on: ubuntu-latest
    if: ${{ github.event.repository.fork }}
    steps:
      - uses: actions/checkout@v4
        with:
          ref: unstable
          persist-credentials: false
      - name: Sync upstream changes
        uses: aormsby/Fork-Sync-With-Upstream-action@v3.4.1
        with:
          target_sync_branch: unstable
          target_repo_token: ${{ secrets.GITHUB_TOKEN }}
          upstream_sync_branch: unstable
          upstream_sync_repo: looplj/axonhub
关键点：这个 workflow 只同步 unstable 分支，不触碰 dev，也不会触发 Docker 构建。同步成功后，你需要在本地手动把 unstable 合并进 dev。

1.5 将更新合并到 dev（冲突在此暴露，由你审核）
bash
git fetch origin
git checkout dev
git merge origin/unstable
# 如有冲突，解决后 git add + git commit
git push origin dev   # 推送后才触发 Docker 构建
1.6 冲突最小化建议
需求	冲突风险	降低冲突的做法
需求 1（删除确认改 4 位数字）	低	纯前端局部组件改动
需求 3（模型关联拖拽排序）	低	拖拽逻辑封装为独立 hook 或组件
需求 2（API Key 分组）	中高	不修改 APIKeys []string 类型，只新增 KeyGroups 字段；调度器优先读新字段、回退读旧字段
第二部分：Docker 镜像构建与发布（仅由 dev 推送触发）
2.1 目标
只有当你向 dev 分支推送代码时，才自动构建并推送 Docker 镜像。上游 unstable 的同步不会触发构建，确保你在解决冲突、审核代码之后才发布镜像。

2.2 前置准备：配置 Docker Hub 凭据
在 GitHub 仓库 Settings → Secrets and variables → Actions 中添加：

类型	名称	值
Secret	DOCKER_PASSWORD	Docker Hub Access Token（Read & Write）
Variable	DOCKER_USERNAME	Docker Hub 用户名
2.3 Workflow：构建并推送镜像
在 fork 仓库创建 .github/workflows/docker-publish.yml：

yaml
name: Build and Push Docker Image

on:
  push:
    branches:
      - dev          # 仅 dev 分支推送触发
  workflow_dispatch: # 允许手动触发

jobs:
  build-and-push:
    runs-on: ubuntu-latest
    steps:
      - name: Checkout
        uses: actions/checkout@v4
        with:
          ref: dev

      - name: Log in to Docker Hub
        uses: docker/login-action@v4
        with:
          username: ${{ vars.DOCKER_USERNAME }}
          password: ${{ secrets.DOCKER_PASSWORD }}

      - name: Extract Docker metadata
        id: meta
        uses: docker/metadata-action@v6
        with:
          images: ${{ vars.DOCKER_USERNAME }}/axonhub-dev
          tags: |
            type=sha,prefix=dev-
            type=raw,value=latest

      - name: Set up Docker Buildx
        uses: docker/setup-buildx-action@v4

      - name: Build and push
        uses: docker/build-push-action@v7
        with:
          context: .
          file: ./Dockerfile
          push: true
          tags: ${{ steps.meta.outputs.tags }}
          labels: ${{ steps.meta.outputs.labels }}
          platforms: linux/amd64
关键点：

on.push.branches: [dev] 确保只有 dev 的推送会触发。

sync-upstream.yml 只操作 unstable，不会触发这个 workflow。

镜像打 latest 和 dev-<sha> 标签，便于回滚。

2.4 验收标准
□ 上游同步 workflow 运行后，Docker 构建不被触发
□ 向 dev 推送代码后，Docker 构建自动触发
□ 构建成功，Docker Hub 出现 latest 和 dev-<sha> 标签
□ 手动触发 workflow 也能正常构建
第三部分：二开功能需求
需求 1：渠道删除确认改为 4 位数字
行为：删除渠道弹窗展示随机 4 位数字，用户输入匹配后确认删除；输入错误时按钮禁用。

涉及文件：frontend/src/features/channels/ 下的删除确认组件。

实现：generatedCode = Math.floor(1000 + Math.random() * 9000).toString()；按钮 disabled={inputCode !== generatedCode}；后端无改动。

验收：每次打开生成不同数字；输入错误不可点击；输入正确成功删除。

需求 2：渠道内多 API Key 分组管理
目标：每个 API Key 可配自定义名称（如“某某公益站-codex分组”），支持按 Key 获取和绑定模型列表。

数据模型：新增 KeyGroup{Name, APIKey, Models, Enabled} 与 ChannelCredentials.KeyGroups；保留 APIKey/APIKeys 兼容旧数据，读取时自动转换。

GraphQL：新增 KeyGroup 类型与 KeyGroupInput，ChannelCredentials 增加 keyGroups。

调度：从 enabled=true 的 KeyGroups 选 Key，按 Models 过滤请求模型，保留故障转移。

获取模型：每个 KeyGroup 行独立按钮，调用 fetchModelsForKey，结果仅填入该 Key 的 Models。

前端 UI：分组表格（名称、API Key、获取模型、模型列表 tag、启用、删除）。

验收：可填名称；可独立获取模型；按模型匹配 Key；旧数据兼容；失败自动切换。

需求 3：模型关联拖拽排序与优先级同步
行为：右侧渠道/域名列表可拖拽，拖拽后 priority 按新顺序重写（第一行=0，第二行=1…），输入框同步更新。

涉及文件：frontend/src/features/models/components/models-association-dialog.tsx。

实现：@dnd-kit/core + @dnd-kit/sortable；拖拽结束 form.setValue(associations.${index}.priority, index)；后端无改动。

验收：可拖拽；优先级输入框自动更新；保存后路由按新优先级生效。

附录 A：关于“最新模型名称”
AxonHub 上游模型数据通过其注册表同步机制更新。二开中不应硬编码 gpt-6、deepseek-v4.1-flash 等名称。需求 2 的“按 Key 获取模型”设计目的就是从上游 /models 接口动态拉取实际可用模型 ID，以实际返回为准。

附录 B：改动影响评估与推进顺序
需求	前端工作量	后端工作量	冲突风险	建议顺序
需求 1（删除确认）	小	无	低	1
需求 3（拖拽排序）	中	无	低	2
需求 2（Key 分组）	大	中	中高	3
同步 workflow（unstable）	—	—	无	先配置
构建 workflow（dev）	—	—	无	dev 创建后配置
建议：先配置两个 workflow 并验证“同步不触发构建、推送 dev 才构建”；再按 需求 1 → 需求 3 → 需求 2 在 dev 上推进，每完成一项提交一次，便于合并上游时定位冲突来源。