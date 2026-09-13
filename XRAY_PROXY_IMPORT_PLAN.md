# Xray 代理功能搬运计划

## 背景

需要将 smmooooonn/sub2api-xray 的代理相关功能搬运到两个目标仓库：
1. suixincrazy/sub2api (origin)
2. suixincrazy/sub2api-xray (自己的 xray fork)

## 分析结果

### 代码分叉情况
- **共同祖先**: 98d86915b (官方 v0.2.4)
- **xray 独有提交**: 56 个
- **main 独有提交**: 5 个（包括 3da3f682f 超时修复）
- **冲突文件**: 61 个

### 核心代理功能提交

按时间顺序（从早到晚）：

1. **8613f838e** - `feat: add user-owned resources and xray proxy support`
   - 基础代理实体和 Xray 支持
   - 新增 Proxy 和 ProxySource schema
   - 新增用户资源管理 handler
   - 修改 167 个文件

2. **68caa2a0a** - `feat: align user proxy workflow with admin UI`
   - 对齐用户端和管理端代理工作流

3. **2e38170b2** - `feat(proxy-source): 订阅源管理（一键同步／节点数筛选／启停开关／机场配额）与工具栏、顶栏排版修复`
   - 订阅源管理完整功能
   - 一键同步、节点筛选、启停开关
   - 机场配额显示

### 关键文件差异

代理相关核心文件（main 缺失）：

**Schema**:
- `backend/ent/schema/proxy_source.go` (新增)
- `backend/ent/schema/proxy.go` (扩展：新增 owner_user_id, is_public, kind, extra 等字段)

**Backend**:
- `backend/internal/handler/my_resource_handler.go` (新增：用户端资源管理)
- `backend/internal/handler/admin/resource_owner_scope.go` (新增：归属范围过滤)
- `backend/internal/pkg/proxyutil/dialer.go` (扩展：Xray 拨号器支持)
- `backend/internal/service/admin_proxy.go` (扩展：订阅源同步等)
- `backend/ent/proxysource*.go` (新增：代理源实体，约 8 个生成文件)

**Frontend**:
- `frontend/src/api/myResources.ts` (新增：用户端 API)
- `frontend/src/components/proxy/ProxySourceManager.vue` (新增：订阅源管理组件)
- 多个现有组件的扩展

## 搬运策略

### 方案 A: 直接合并 xray/main（推荐）

**优点**:
- 一次性获得所有代理功能
- 保持提交历史完整
- 包含所有 bug 修复

**缺点**:
- 需要解决 61 个冲突
- 会带入一些非代理相关的 xray 版本更新

**执行步骤**:
1. 在 main 上创建合并分支
2. `git merge xray/main`
3. 解决冲突（优先保留 main 的超时修复）
4. 运行测试
5. 构建验证

### 方案 B: 选择性 cherry-pick

**优点**:
- 只搬运代理相关功能
- 避免无关变更

**缺点**:
- 需要人工识别依赖链
- 仍然有大量冲突（基础提交就有 61 个冲突）
- 可能遗漏关键修复

**不推荐原因**: 基础提交 8613f838e 已经修改 167 个文件，冲突数量与直接合并相当

## 决策：采用方案 A

理由：
1. 代理功能是紧密集成的，难以拆分
2. xray/main 已经合并了官方 v0.2.4，与 main 的基线接近
3. 冲突集中在生成文件（ent/wire）和版本标记，容易解决
4. 可以保留 3da3f682f 超时修复（main 独有）

## 执行计划

### Phase 1: 合并到 origin (suixincrazy/sub2api)

```bash
git checkout main
git pull origin main
git checkout -b merge-xray-proxy
git merge xray/main --no-commit
# 解决冲突，优先保留：
# - main 的超时修复 (3da3f682f)
# - main 的其他 4 个独有提交的修复
# 接受 xray 的：
# - 所有代理相关新文件
# - 代理功能扩展
git commit -m "merge: import xray proxy features from xray/main"
git push origin merge-xray-proxy
```

### Phase 2: 推送到 suixincrazy/sub2api-xray

需要先添加这个 remote：

```bash
git remote add my-xray https://github.com/suixincrazy/sub2api-xray.git
git fetch my-xray
git push my-xray merge-xray-proxy:main
```

### Phase 3: 实施 6 项预防措施

在合并完成后，修改 docker-compose.yml 和配置：

1. 固定项目名
2. 持久化 .env
3. 配置数据库备份
4. 外部命名卷
5. 添加恢复文档
6. 部署脚本更新

## 预期工作量

- 冲突解决: 2-3 小时
- 测试验证: 1 小时
- 部署更新: 1 小时
- 文档更新: 30 分钟

总计: 约 5 小时
