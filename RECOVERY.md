# Sub2API 数据恢复文档

本文档说明 Sub2API 系统的数据恢复流程和注意事项。

## 预防措施总览

Sub2API 部署了六项预防措施防止数据丢失：

1. **固定项目名称** (`name: sub2api`) - 确保卷名称固定，避免随机后缀
2. **环境文件备份** (`scripts/ensure-env.sh`) - 自动备份和恢复 `.env` 文件
3. **PostgreSQL 自动备份** (`scripts/backup-postgres.sh`) - 每日备份数据库
4. **危险命令警告** - 文档中警告避免使用 `docker compose down -v`
5. **外部命名卷** - 使用 `external: true` 确保卷在容器删除后保留
6. **本文档** - 提供完整的数据恢复流程

## ⚠️ 危险命令警告

**永远不要运行以下命令：**

```bash
# 危险！会删除所有卷和数据
docker compose down -v

# 危险！会删除指定的卷
docker volume rm sub2api_data
docker volume rm sub2api_postgres_data
docker volume rm sub2api_redis_data
```

这些命令会**不可逆地删除**所有用户数据、数据库记录和配置。

**正确的停止方式：**

```bash
# 安全：停止容器但保留卷
docker compose down

# 安全：停止单个服务
docker compose stop backend
docker compose stop postgres
```

## 首次部署前准备

在首次运行 `docker compose up` 之前，必须手动创建外部卷：

```bash
cd /root/sub2api/deploy

# 创建三个外部命名卷
docker volume create sub2api_data
docker volume create sub2api_postgres_data
docker volume create sub2api_redis_data

# 验证卷已创建
docker volume ls | grep sub2api
```

如果不创建这些卷，`docker compose up` 会失败并提示卷不存在。

## 数据备份

### 1. PostgreSQL 数据库备份

系统提供了自动备份脚本：

```bash
# 手动执行备份
/root/sub2api/scripts/backup-postgres.sh

# 设置每日自动备份（推荐）
# 添加到 crontab（每天凌晨 3 点）
0 3 * * * /root/sub2api/scripts/backup-postgres.sh >> /root/backups/postgres/backup.log 2>&1
```

备份文件位置：`/root/backups/postgres/sub2api-YYYYMMDD-HHMMSS.sql.gz`

备份保留策略：自动删除 7 天前的旧备份

### 2. 环境文件备份

系统自动维护 `.env` 文件备份：

```bash
# 主环境文件
/root/sub2api/deploy/.env

# 自动备份文件
/root/sub2api/.env.backup
```

`scripts/ensure-env.sh` 脚本会在以下情况自动运行：
- 服务器重启时（如果配置了开机自启动）
- 手动执行时

### 3. 完整系统备份

除了数据库和环境文件，建议定期备份以下内容：

```bash
# 备份整个部署目录
tar -czf /root/backups/sub2api-full-$(date +%Y%m%d).tar.gz \
  /root/sub2api/deploy \
  /root/sub2api/.env.backup \
  /root/sub2api/scripts

# 备份 Docker 卷（可选，数据库备份已覆盖主要数据）
docker run --rm \
  -v sub2api_postgres_data:/data \
  -v /root/backups:/backup \
  alpine tar -czf /backup/postgres_volume-$(date +%Y%m%d).tar.gz -C /data .
```

## 数据恢复流程

### 场景 1：环境文件丢失

如果 `.env` 文件意外丢失：

```bash
# 方法 1：从自动备份恢复
/root/sub2api/scripts/ensure-env.sh

# 方法 2：手动恢复
cp /root/sub2api/.env.backup /root/sub2api/deploy/.env

# 验证恢复
cat /root/sub2api/deploy/.env | grep POSTGRES_PASSWORD
```

### 场景 2：数据库损坏或数据丢失

使用备份恢复数据库：

```bash
# 1. 列出可用备份
ls -lh /root/backups/postgres/

# 2. 选择要恢复的备份文件
BACKUP_FILE="/root/backups/postgres/sub2api-20260913.sql.gz"

# 3. 执行恢复（需要确认）
/root/sub2api/scripts/restore-postgres.sh "$BACKUP_FILE"

# 脚本会提示：
# 警告: 此操作将覆盖当前数据库！
# 确认继续？(输入 YES 继续):

# 4. 验证恢复
docker exec sub2api-postgres psql -U sub2api -d sub2api -c "SELECT COUNT(*) FROM accounts;"
```

**注意事项：**
- 恢复会**完全覆盖**当前数据库内容
- 恢复前建议先备份当前状态（如果有价值）
- 恢复后需要重启后端服务：`docker compose restart backend`

### 场景 3：Docker 卷意外删除

如果 Docker 卷被删除（例如误运行 `docker compose down -v`）：

#### 3.1 PostgreSQL 数据恢复

```bash
# 1. 重新创建卷
docker volume create sub2api_postgres_data

# 2. 启动数据库容器
docker compose up -d postgres

# 3. 等待数据库初始化完成
docker compose logs -f postgres
# 看到 "database system is ready to accept connections" 后按 Ctrl+C

# 4. 从备份恢复数据
/root/sub2api/scripts/restore-postgres.sh /root/backups/postgres/sub2api-YYYYMMDD.sql.gz

# 5. 重启所有服务
docker compose down
docker compose up -d
```

#### 3.2 Redis 数据恢复

Redis 主要用于缓存，数据丢失影响相对较小：

```bash
# 重新创建卷
docker volume create sub2api_redis_data

# 启动 Redis（会自动初始化）
docker compose up -d redis
```

Redis 的数据会在系统运行过程中自动重建。

#### 3.3 应用数据恢复

```bash
# 重新创建卷
docker volume create sub2api_data

# 如果有卷备份，可以恢复
docker run --rm \
  -v sub2api_data:/data \
  -v /root/backups:/backup \
  alpine sh -c "cd /data && tar -xzf /backup/data_volume-YYYYMMDD.tar.gz"
```

### 场景 4：完全重建系统

如果需要在新服务器上或完全重建系统：

```bash
# 1. 恢复代码和配置
cd /root
git clone https://github.com/suixincrazy/sub2api.git
cd sub2api

# 2. 恢复环境文件
# 从备份复制 .env 文件到 deploy/ 目录
cp /path/to/backup/.env deploy/.env

# 3. 创建 Docker 卷
docker volume create sub2api_data
docker volume create sub2api_postgres_data
docker volume create sub2api_redis_data

# 4. 启动基础服务
cd deploy
docker compose up -d postgres redis

# 5. 等待数据库就绪
sleep 30

# 6. 恢复数据库
/root/sub2api/scripts/restore-postgres.sh /path/to/backup/sub2api-YYYYMMDD.sql.gz

# 7. 启动全部服务
docker compose up -d

# 8. 验证服务
docker compose ps
curl http://localhost:8080/health
```

## 验证数据完整性

恢复后，执行以下检查确保数据完整：

```bash
# 1. 检查容器状态
docker compose ps

# 2. 检查数据库连接
docker exec sub2api-postgres psql -U sub2api -d sub2api -c "\dt"

# 3. 检查关键表记录数
docker exec sub2api-postgres psql -U sub2api -d sub2api -c "
SELECT 
  'accounts' AS table_name, COUNT(*) FROM accounts
UNION ALL
SELECT 'users', COUNT(*) FROM users
UNION ALL
SELECT 'subscriptions', COUNT(*) FROM subscriptions;"

# 4. 检查 Redis 连接
docker exec sub2api-redis redis-cli ping

# 5. 检查后端服务
curl http://localhost:8080/health

# 6. 检查日志无错误
docker compose logs --tail=50 backend
docker compose logs --tail=50 postgres
```

## 定期维护任务

建议设置以下定期任务：

```bash
# 编辑 crontab
crontab -e

# 添加以下任务：

# 每天凌晨 3 点备份数据库
0 3 * * * /root/sub2api/scripts/backup-postgres.sh >> /root/backups/postgres/backup.log 2>&1

# 每周日凌晨 4 点完整备份
0 4 * * 0 tar -czf /root/backups/sub2api-full-$(date +\%Y\%m\%d).tar.gz /root/sub2api/deploy /root/sub2api/.env.backup /root/sub2api/scripts

# 每月 1 号清理 30 天前的完整备份
0 5 1 * * find /root/backups -name "sub2api-full-*.tar.gz" -mtime +30 -delete
```

## 灾难恢复检查清单

定期（建议每月）执行灾难恢复演练：

- [ ] 验证最新备份文件存在且完整
- [ ] 测试环境文件恢复流程
- [ ] 测试数据库恢复流程（在测试环境）
- [ ] 验证备份文件可以解压和读取
- [ ] 检查备份脚本的 crontab 任务正常运行
- [ ] 验证备份文件的权限设置正确（600）
- [ ] 确认备份目录有足够的磁盘空间

## 联系支持

如果遇到无法通过本文档解决的问题：

1. 检查系统日志：`docker compose logs`
2. 查看详细错误信息
3. 保留原始错误现场（不要立即重启）
4. 联系技术支持并提供详细的错误信息

## 附录：常用命令速查

```bash
# 查看卷列表
docker volume ls

# 查看卷详细信息
docker volume inspect sub2api_postgres_data

# 查看卷大小
docker system df -v | grep sub2api

# 检查数据库大小
docker exec sub2api-postgres psql -U sub2api -d sub2api -c "
SELECT pg_size_pretty(pg_database_size('sub2api'));"

# 查看容器日志
docker compose logs -f backend
docker compose logs --tail=100 postgres

# 进入容器 shell
docker exec -it sub2api-postgres /bin/sh
docker exec -it sub2api-redis /bin/sh
```
