# 缠论算法回退操作

本文说明从 `chan-recursive-v4-preview.4` 回退到兼容 `strict-old-bi-v3` 的应用版本。算法结构和前端资源随 Go 二进制一起发布；回退时更换应用二进制即可恢复旧客户端和旧 API 消费路径，不需要转换、重算或删除原始行情。

## 发布前准备

1. 从已知兼容 `strict-old-bi-v3` 的发布版本保留与服务器架构相符的完整二进制（`linux-amd64` 或 `linux-arm64`），并记录其发布版本及校验值。不要用仅替换静态页面的方式回退，因为静态资源已嵌入二进制。
2. 确认 `.env`、`DATABASE_PATH` 和当前数据库路径，部署时不覆盖它们。应用回退不需要迁移行情表。
3. 按常规 SQLite 备份流程为数据库建立一份可恢复备份，记录文件位置。备份用于应对版本间 schema 不兼容；正常回退不需要恢复备份。
4. 在预发布环境用该旧版本和当前 schema 启动一次，确认旧进程能打开数据库并读取旧 API。应选用已知兼容当前 SQLite schema 的旧版本；不要假设任意历史构建都能使用新版本 schema。

## 执行回退

在应用目录保留当前 v4 二进制以便后续排查，再把经验证的旧版本二进制原子替换为 `run.sh` 所使用的架构文件名：

```bash
./run.sh stop
cp option-scope-linux-amd64 option-scope-linux-amd64.v4-rollback-copy
cp /path/to/verified-legacy-binary option-scope-linux-amd64.rollback-new
chmod 0755 option-scope-linux-amd64.rollback-new
mv option-scope-linux-amd64.rollback-new option-scope-linux-amd64
./run.sh start
./run.sh status
./run.sh logs 100
```

ARM64 部署时将文件名中的 `amd64` 改为 `arm64`。如果服务由 systemd 管理，也可用 `./run.sh restart` 重启；部署前仍需先把已验证的旧二进制放到 `run.sh` 识别的活动文件名。不要同时运行两个二进制访问同一个 SQLite 文件。

## 回退后核验

1. `./run.sh status` 显示服务运行，日志没有数据库迁移、schema 或启动错误；`curl -fsS http://127.0.0.1:8000/health` 返回成功。若监听端口或访问保护配置不同，使用 `.env` 中的实际值。
2. 用旧客户端请求 `GET /api/chan/{symbol}`，确认 `rule_version` 为 `strict-old-bi-v3`，`daily.strokes` 与 `daily.centers` 可读取。不要让旧客户端再请求 `/adjusted` 或 `/intraday`；嵌入的旧版页面应继续使用旧字段。
3. 保持现有 `.env` 和 SQLite 文件原样。`chan_adjusted_history`、结构快照及修订表属于新版本的附加数据；旧版本不需要读取它们。回退本身不修改原始 `price_history` 和 `intraday_history` 行情。

若旧二进制不能打开当前数据库或旧 API 核验失败，立即停止服务；不要手工删表或改写行情。先将活动二进制恢复为保留的 v4 版本以恢复服务，再在维护窗口按数据库恢复流程从发布前备份恢复，或准备并验证与现有 schema 兼容的旧二进制后重试。保留回退前数据库和日志供排查。

## 恢复 v4

确认回退原因已解决后，停止旧进程，把保留的 v4 二进制放回 `run.sh` 识别的活动文件名并启动。原来的 `.env` 和 SQLite 数据可继续使用；启动后确认 `/api/chan/{symbol}/adjusted` 返回 `chan-recursive-v4-preview.4`，旧 `/api/chan/{symbol}` 仍返回 `strict-old-bi-v3`。
