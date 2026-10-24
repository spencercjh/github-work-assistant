# 选择和扩展持久化存储

助手通过 `Persistence` 业务接口访问数据，关系数据库实现使用 GORM。新安装默认把数据保存在本地 SQLite 文件；需要已有数据库或云数据库时，在初始化阶段选择对应驱动和连接。

## 选择后端

| 选择 | 适用情况 | 初始化参数 | 验证情况 |
| --- | --- | --- | --- |
| SQLite | 单机部署，希望免去数据库服务 | `--database sqlite`，默认选项 | 临时文件上的完整业务与并发测试 |
| 新建 MySQL | 本机独立容器和数据卷 | `--database mysql` | MySQL 8.4 完整业务与并发测试 |
| 已有 MySQL | 自行维护数据库服务 | `--database mysql --dsn-file /private/db-dsn` | 使用同一 MySQL 驱动；实际连接须经 `doctor` 验证 |
| seekdb | 沿用现有 seekdb 服务 | `--database seekdb --dsn-file /private/db-dsn` | seekdb 1.4.0.0 完整业务与并发测试 |
| PostgreSQL | 使用现有 PostgreSQL 服务 | `--database postgres --dsn-file /private/db-dsn` | PostgreSQL 18 完整业务与并发测试 |
| TiDB／TiDB Cloud | 使用兼容 MySQL 协议的托管数据库 | `--database tidb --dsn-file /private/db-dsn` | 驱动入口已接入，尚未进行真实 TiDB／云实例验收 |

数据库可以位于本机、其他主机或云端，只要应用能够连接、账号具备所需权限，且后端满足事务和唯一约束要求。SQLite 使用本机文件，不能把数据库文件放在 S3 或多机共享文件系统上。单个数据库仍只供一个助手采集实例使用；增加服务器数据库后端并不等于支持多实例运行。

## 初始化与连接文件

`make setup` 提供三种选择：本地 SQLite、新建 MySQL 容器、已有数据库。`init` 只生成私有实例配置；只有新建 MySQL 选项生成 Compose 文件。`migrate` 创建表结构，`doctor` 检查连接与实例身份。

连接文件是权限为 `0600` 的普通文件，不接受符号链接。服务器连接字符串通过文件传入，不放在命令行参数中。下列字符串中的账号、密码和地址都是占位示例，需替换为部署者自己的连接信息。

### 本地 SQLite

```bash
bin/ghraft init --dir state/assistant \
  --raft-token-file /private/external-token --recipient @reviewer \
  --database sqlite
state/assistant/run migrate
state/assistant/run doctor
```

默认文件为实例目录中的 `private/assistant.sqlite`。增加 `--sqlite-path /srv/my-assistant/data.sqlite` 可以指定其他本机路径；相对路径按执行 `init` 时的工作目录解析。目录与数据库由 `migrate` 创建，数据库权限为 `0600`。`doctor` 不会创建缺失的数据库。

SQLite 使用 WAL、同步写入和写事务等待；每个写事务先取得写锁，再读取待修改状态。编译使用纯 Go SQLite 驱动，无需开启 CGO。文件、WAL 和共享内存文件必须位于同一台主机的本地磁盘。

### MySQL、seekdb 与 TiDB

私有连接文件内容示例：

```text
assistant:REPLACE_ME@tcp(db.example.com:3306)/assistant?charset=utf8mb4&tls=true&timeout=5s&readTimeout=20s&writeTimeout=20s
```

```bash
bin/ghraft init --dir state/assistant \
  --raft-token-file /private/external-token --recipient @reviewer \
  --database seekdb --dsn-file /private/database-dsn
state/assistant/run migrate
state/assistant/run doctor
```

按目标服务将 `seekdb` 改为 `mysql` 或 `tidb`。数据库应预先创建，迁移账号需要建表与修改表结构权限；运行账号需要查询和写入权限。MySQL 表需要支持事务的存储引擎。托管服务使用其要求的账号格式、端口和 TLS 配置；不能用跳过证书验证代替正确的 CA 配置。默认 Go TLS 校验使用系统信任根，私有 CA 应安装到服务账号使用的信任环境。

`seekdb` 和 `tidb` 是 MySQL 驱动的显式别名，不表示已经验证这些产品的所有版本或云服务配置。

### PostgreSQL

私有连接文件内容示例：

```text
postgres://assistant:REPLACE_ME@db.example.com:5432/assistant?sslmode=verify-full&sslrootcert=/private/database-ca.pem&connect_timeout=5
```

```bash
bin/ghraft init --dir state/assistant \
  --raft-token-file /private/external-token --recipient @reviewer \
  --database postgres --dsn-file /private/database-dsn
state/assistant/run migrate
state/assistant/run doctor
```

连接 URL 中的用户名和密码需按 URL 规则编码。也可使用 pgx 支持的键值格式。数据库预先创建，账号需要目标 schema 的使用、建表和数据读写权限。远程连接使用 `verify-full`；测试中的 `sslmode=disable` 仅用于一次性本机容器。

## 配置、备份与更换数据库

`config.json` 的 `database_driver` 决定驱动，`private/dsn` 保存连接信息或 SQLite 路径。未填写 `database_driver` 的旧配置沿用 MySQL。`GHRAFT_DSN_FILE` 可在直接调用二进制时指定其他私有文件；生成的 `run` 入口固定使用本实例文件。

切换配置只会选择另一个数据库，不会复制原数据库中的快照、待发事件、去重记录或加密状态。`migrate` 负责表结构，不能进行跨后端数据迁移。已有实例需要迁移数据时，应单独安排导出、转换、导入和恢复验证；不要把修改连接地址当作迁移完成。

备份须包含数据库、实例配置和 `private/key`。数据库中的加密状态依赖该密钥。执行备份前停止采集服务及访问此实例的 CLI 操作：

- SQLite：所有连接关闭后备份数据库及尚存的 `-wal`、`-shm` 文件；默认路径可随完整实例目录一起归档。在线备份应使用 SQLite 备份 API 或工具，不直接复制正在写入的主文件。
- MySQL／seekdb／TiDB：采用目标服务支持的一致性备份工具；向导创建的 MySQL 容器示例见[快速部署](QUICKSTART.md#备份升级与恢复)。
- PostgreSQL：使用 `pg_dump` 或服务商备份功能，并保留数据库角色与 schema 的恢复条件。密码使用私有连接文件或凭据文件，不写入命令参数。

恢复后先核对文件权限、配置路径和加密密钥，再执行 `doctor`。数据库备份包含处理内容，应与凭据一样保持私有。

## 数据访问实现

调用关系为：业务服务 → `Persistence` → GORM 存储实现 → 数据库驱动。业务接口使用快照、事件、任务关联和处理结果，不暴露 SQL、GORM 对象或事务句柄。主要代码如下：

| 文件 | 职责 |
| --- | --- |
| `internal/app/persistence.go` | 业务存储接口与一致性要求 |
| `internal/app/store*.go`、`migration.go`、`status.go` | 数据访问与业务事务；部分关联查询使用参数化 SQL |
| `internal/app/store_models.go` | GORM 记录类型、表名和字段约束 |
| `internal/database/database.go` | 驱动连接、GORM 初始化、SQLite 文件设置和写事务协调 |

表结构通过 GORM 迁移，冲突更新通过 `clause.OnConflict` 生成，各数据库的参数绑定由 GORM 处理。首次观察同一资源也须串行：服务器数据库在 `READ COMMITTED` 事务中写入并锁定资源锁记录，SQLite 使用 `IMMEDIATE` 写事务。ORM 不替应用决定这些并发规则。

新实现必须保证快照与待发事件原子提交、相同快照去重、处理结果不可被同名内容覆盖，以及提交结果前拒绝过期证据。新增关系数据库驱动后，应运行同一套测试；仅连接成功不代表满足这些要求。非关系存储可以另行实现 `Persistence`，但不能仅凭存在读写 API 就声明支持。

## 验证

```bash
# 临时 SQLite 文件，不需要数据库服务。
make check build test
python3 scripts/smoke-setup.py --database sqlite

# 服务器测试只接受 127.0.0.1 和以 _test 结尾的专用数据库。
# 测试会清空 ghraft_* 数据，禁止使用已有实例数据库。
GHRAFT_TEST_DRIVER=mysql GHRAFT_TEST_DSN_FILE=/private/mysql-test-dsn make integration
GHRAFT_TEST_DRIVER=seekdb GHRAFT_TEST_DSN_FILE=/private/seekdb-test-dsn make integration
GHRAFT_TEST_DRIVER=postgres GHRAFT_TEST_DSN_FILE=/private/postgres-test-dsn make integration

# 先创建单独的空测试数据库，不能复用上面已有测试记录的数据库。
# PostgreSQL 安装测试要求 URL 格式、127.0.0.1 和以 _test 结尾的库名。
python3 scripts/smoke-setup.py --database postgres --dsn-file /private/postgres-smoke-dsn
```

测试包含两个独立连接同时首次观察同一资源、竞争绑定不同任务、数据库约束失败后的事务回滚、重启去重和过期证据拒绝。CI 分别运行 SQLite、MySQL 和 PostgreSQL；seekdb 使用独立本机测试实例验证。GitHub 与 Raft 在这些测试中使用受控模拟服务，不代表完成真实账号收信验收。
