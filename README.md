# Option Scope

## 一键安装（推荐）

在任意一台 Ubuntu / Debian / CentOS / RHEL / Rocky / Alma / Alpine 机器上，直接执行这一行（无需事先下载源码、无需手动上传部署）：

```bash
bash <(curl -Ls https://raw.githubusercontent.com/jack2652/31tvpmhdhlngphy59jx1/main/run.sh)
```

脚本会先把仓库克隆到当前目录下的 `us_stocks/`（该目录已是仓库时改跑 `git pull` 更新到最新源码），然后切换进去打开交互菜单：菜单第 7 项仅检测/安装/修复 Python 环境与依赖，不启动应用；第 2 项启动（后台运行 + 看门狗守护）。第 9 项是高级服务管理，第 10 项可结束占用指定 TCP 端口的程序，第 11 项是环境自检。

| 需求 | 命令 |
| --- | --- |
| 打开交互菜单（默认） | `bash <(curl -Ls https://raw.githubusercontent.com/jack2652/31tvpmhdhlngphy59jx1/main/run.sh)` |
| 首次安装完成后启动应用 | `bash <(curl -Ls https://raw.githubusercontent.com/jack2652/31tvpmhdhlngphy59jx1/main/run.sh) 2` |
| 换安装目录 | `INSTALL_DIR=/opt/us_stocks bash <(curl -Ls https://raw.githubusercontent.com/jack2652/31tvpmhdhlngphy59jx1/main/run.sh)` |
| 换远端（镜像 / SSH） | `GIT_REMOTE_URL=git@github.com:jack2652/31tvpmhdhlngphy59jx1.git bash <(curl -Ls <脚本地址>)` |

几点说明：默认安装目录是执行命令时所在目录下的 `us_stocks/`（用 `INSTALL_DIR` 可改成 `/opt/...` 这类路径）；重复执行同一条命令等同于「升级到最新源码」，`.env` 配置与 `data/` 数据库都在忽略名单里，不会被动到；机器上没装 git 时脚本会先用系统包管理器装，装不上则退化为下载源码压缩包（这种安装方式后续不能自动增量更新）；`pip` 依赖只在 `pyproject.toml` 变化时才重装，日常拉新代码几乎不花时间。

一个可快速部署的美股行情与期权分析工具，涵盖现货行情、期权链、成交流向、买方结构和关键价位分析。品牌名称为 Option Scope，页面主标题使用「美股行情与期权分析」。后端使用 FastAPI，行情与期权链来自上游行情接口，快照落盘到 SQLite，前端是无需 Node 构建的原生 HTML/CSS/JavaScript。

## Go 重写分支（`go-rewrite`）

本分支用于逐步把后端迁移到 Go，HTTP 层使用 Gin，不是 Cloudflare Workers/Wasm 版本。Python 基线保留在同级目录 `us_stocks_py/`，作为可运行版本、API 行为参考和迁移期间的回退实现；在 Go 完整覆盖全部接口前不会删除 Python 文件，也不会影响 `main` 分支。该目录中的 `run.sh` 只管理 Python 版本。Go 版静态资源从 `web/static` 通过 `//go:embed` 编进服务二进制；交叉编译发布包只需部署 Go 服务二进制和 `.env` 配置，不需要 Go 工具链、源码或静态目录。Go 生产切换和服务托管脚本后续单独规划。

当前 Go 服务入口为 `cmd/go-server/main.go`，使用：

- Gin 路由、中间件、静态资源和 SSE；活跃 SSE 订阅按美东时段及 XNYS 假日、历史临时休市和提前收盘规则调度快照刷新（约 20/60/300 秒），同标的刷新串行合并，失败时退避；
- `modernc.org/sqlite` 通过标准 `database/sql` 读写现有 `data/options.db`，启动时只做兼容性列检查。驱动完全由 Go 实现，不依赖 CGO 或目标机 libc，数据库使用标准 SQLite 文件格式；
- Yahoo 行情、历史、到期日、财报日历和估值可用能力优先通过 `github.com/wnjoon/go-yfinance` 获取。当前依赖为 `v1.7.0`；期权链只对 SDK 尚未暴露的 Gamma 和缺失值/null 存在性保留窄字段适配，fundamentals timeseries 也只补 SDK 当前未提供的时间序列字段。SDK 增加这些字段/API 后，应优先切回 SDK 并删除对应适配。所有行情适配器共享同一个 `UPSTREAM_CONCURRENCY` 并发门（默认上限 6，由启动时可用内存和 CPU 动态收紧），Yahoo、CBOE、Alpaca、估值与刷新统一受此预算约束；基础报价与期限目录可在预算允许时并行，低内存下 Gamma、估值基准和历史等重任务仍采用串行或小批次策略；
- 启动时读取当前可执行文件所在目录的 `.env` 中未由进程环境设置的变量（支持 `KEY=value`、引号值、注释及 `export KEY=value`）；默认 SQLite 文件位于二进制旁的 `data/options.db`，`.env` 中的相对 `DATABASE_PATH` 也相对于二进制目录解析；原始 Yahoo 补充请求、Cboe、Alpaca 和维护工具 API 请求统一经 `internal/outboundhttp` 构造和发送，Yahoo SDK 请求由 SDK 自己管理连接，但使用同一包提供的 `MARKET_PROXY`、Chrome JA3 和 User-Agent 配置；`RAW_RETENTION_DAYS`（默认 30 天）配置清理保留期；
- 与现有前端兼容的 `/quote`、`/expirations`、`/chain`、`refresh`、`events`、`status`、`health` 路由，以及按 `RAW_RETENTION_DAYS` 清理历史数据、按数据库体积上限阶梯回收冗余快照的 `/api/cleanup`；`SCHEDULER_ENABLED` 开启时还会通过 SQLite 服务租约执行每日维护。

在开发机交叉编译 Linux 发布包（默认 `linux/amd64`）：

```bash
# 直接用 bash 调用，不要求脚本文件具有可执行权限
bash ./scripts/build-go-release.sh linux amd64
# 交叉编译到 ARM64
bash ./scripts/build-go-release.sh linux arm64
```

脚本直接生成 `dist/option-scope-linux-amd64` 单个可执行文件（ARM64 示例见命令），使用 `CGO_ENABLED=0` 和 `-trimpath -ldflags='-s -w'` 构建，并通过 `//go:embed` 将源码目录 `web/static` 内的页面资源打包进去。部署时只需传输该二进制；配置通过进程环境或同目录 `.env` 提供，服务可由 systemd 或其他进程管理器启动。

在开发机运行 Go 验证：

```bash
go test -p 1 ./...
go test -race -p 1 ./...
go vet ./...
```

当前 Go 版已实现 `/api/flow` 的 SQLite 快照分析、Yahoo/Cboe/Alpaca 行情与期权 provider 组件、基于报价的 IV/Gamma、历史日线/极值/Beta/财报缓存协调、SQLite 服务租约、跨实例快照合并、可恢复的 Gamma 后台分批任务，以及由服务协调行情数据和快照输入的 Go `/api/levels` 分析（含斐波那契、筹码峰值、承接候选、期权磁吸、合并排序、历史留出校准和计划输出）；容量上限阶梯清理和每日维护也已迁移。levels 已有锁定时间的端到端 Python golden，覆盖空数据、买卖结构、候选合并及计划输出，并新增多快照聚合、计划条数和历史留出边界用例；盘前/盘后/夜盘/休市组合仍需更广输入回归。市场 regime 已有 20 组 Python v39 固定输入 golden。Go 估值已覆盖金融、REIT、能源/矿业、互联网、公用事业、国防、邮轮、移动科技、重资产转型、芯片及 AI 存储等模型；Python 的 17 项 `industry_key_map`、行业数据质量 warning、市值调和诊断和利息覆盖率精度已补齐；分类矩阵覆盖只提供行业 key、缺少行业文本的 17 类输入，并修复 REIT 标签、AI 暴露诊断及 mobility/financial 优先级差异。全部行业决策与估值公式仍需更广输入差分。Yahoo 行情、历史、期权、估值指标、财务报表和财报日历优先使用 `go-yfinance v1.7.0`；保留的原始 Yahoo 补充请求仅覆盖该 SDK 未建模的 Gamma/null 存在性、行业 key、专用指标和部分历史股份字段。Go 全量测试、race、vet、运行脚本保护、隔离后端 API smoke 及交叉编译发布包切换测试通过；Python 基线回归为 258 passed。实时 OpenAPI 路径定义及 8 组错误/正常 HTTP 请求与 Python 相同。Go 与 Python 在相同 SQLite 副本上执行容量清理后关键表行数、文件大小和 SQLite 完整性结果一致；Go 数据库工具已完成在线备份及隔离恢复验证。Gamma 任务事件清理现按 Python 的 `MAX(id)-20000` 窗口保留，并覆盖 ID 跳号。Yahoo 代理 `http://127.0.0.1:10808` 曾实测连通，但当前 Yahoo 返回 HTTP 429，Cboe/Alpaca 生产权限和可靠上游场景仍需验证。生产数据库备份/回滚、多实例故障恢复和 Go/Python 生产切换演练仍未完成；Go 仍不是生产默认后端。本轮没有访问真实服务器，也未执行合并、提交或推送。

## 能做什么

- 查询指定标的的当前价格、涨跌幅和市场状态。载入标的或切换到期日后，地址栏会写入 `?symbol=` 和 `expiration=`，刷新页面会恢复当前选择；未指定标的时使用 `DEFAULT_SYMBOLS` 的第一项（默认 `QQQ`）。换标的时到期日默认回到新标的的第一个（最近）到期日——各标的到期日序列不同（有的每周五、有的每天、有的半月）；同一标的重复载入则保留当前选择。
- 现货现价按当前时段动态显示：盘前显示盘前价、盘后与夜盘显示盘后价、盘中与休市显示常规价（对应时段没有数据时回退常规价）。卡片同时标注当前时段（盘前 / 正常交易 / 盘后 / 夜盘 / 休市），顶栏也有同样的时段提示。涨跌幅以该时段之前最近一次盘中收盘为基准。优先用行情源正式收盘（含竞价）；它缺失，或与分钟线上一交易日盘中收盘偏离超过 1% 时，改用分钟线收盘，避免日线空掉一根时涨跌幅被放大。
- 现货涨跌幅旁标出计算基准（相对昨收、相对收盘或相对前收）。净 Gamma、零 Gamma、趋势信号、最佳点评分和买方评分都标明为估算，不是成交价或胜率。下一财报若落在未来 5 个交易日内，卡片会提示，相关窗口追加「含财报」；只作提醒，不改变评分。
- 选择到期日查看 Call/Put 期权链的成交量、未平仓量、买价、卖价、Gamma、GEX 和隐含波动率；表格共 9 列（行权价 / 成交量 / 未平仓 / 买价 / 卖价 / Gamma / GEX / IV（估）/ 价内），所有单元格居中显示，不展示合约代码与最新价。类型不再单独占列：行权价文字绿色即看涨、红色即看跌（保持加粗强调）。期权链面板默认折叠，标题行最右侧是「展开 / 收起」；类型筛选下拉框（全部 / 看涨 / 看跌）放在面板内部、收起面板时随内容一起隐藏，切换只重绘表格、不重新请求数据，重绘时底色按当前显示的行重新归一。成交量与未平仓两列用底色表示强弱：各自按「本屏该列最强值」归一（√ 缩放），黑夜模式填充 10%~78%、白天模式 12%~82%，0 值不着色；底色已经够亮的「热点格」文字自动换成深色墨色，保证亮底上仍然读得清；悬停显示数值与本列最强值占比，表格下方图例标注取色口径与两列的归一基准。GEX 列与 Gamma 敞口图同口径（模型 Gamma × 未平仓 × 100 × 现价² × 0.01，即现价每变动 1% 的美元敞口），看涨为正、看跌为负，整列之和就是 Gamma 面板上的净 Gamma。
- 期权链在正常交易时段沿用主行情接口；盘前、盘后、夜盘和休市自动改用 Cboe 免费延迟期权链接口（无需 API key，接口为 `https://cdn.cboe.com/api/global/delayed_quotes/options/{symbol}.json`），返回逐合约未平仓量、报价、成交量、IV 和 Greeks。Cboe 的未平仓量按上一交易日结算数据理解，不承诺盘外实时变化；接口故障时保留 SQLite 最近快照。
- 主题默认白天模式，右上角一键切换黑夜；偏好写入浏览器 `localStorage`，刷新后保持，首屏由内联脚本引导不会闪色。
- 查看 Gamma 敞口、零 Gamma（Gamma Flip）、看涨墙/看跌墙、成交量分布和持仓量分布。
- 期权流向与买方结构分析：按相邻两份期权链快照的成交量增量，结合最新价相对买卖价的位置估算主动买入、主动卖出和中性成交，并展示 Call/Put 汇总与集中执行价；没有新增成交量、接口失败或本轮期权链暂时为空时保留最近一次有效结果。买方结构按当前期权链和价位分析生成候选，仅供研究参考。
- 压力位与支撑位：支撑位在左、压力位在右（窄屏堆叠时支撑位在上），基准价上下各 10 条，综合四类因子——斐波那契回撤、筹码密集区（按价格分布的成交量）、近期承接位（被打下去又被买回的摆动低点）和所选到期日的 Call/Put 持仓。基准价由折叠组标题栏的开关决定：默认「实时价」（当前时段生效的最新价，盘中为最新成交价，跟随快照刷新，突破行情下不会滞后），可切「盘后价」（盘后成交更接近当日结算价，盘前冲高与盘中回落常是「假突破」；没有盘后数据时依次回退盘前价、常规价）。切换只重算压力位/支撑位这一侧，不重新请求上游接口；面板说明里会标注实际基准与口径。同一段价位（基准 0.5% 内）合并成一条并累加权重，按「强度 ÷ (1 + 距基准%)」取前 10，再按离基准由近到远展示；每条标注组成它的因子，悬停可看综合强度（1 为最强）。日线历史抓不到时自动退化为纯期权口径。每行另显示「到达概率」：在所选到期日之前触及该价位的概率，按该到期日隐含波动率、零漂移的首次触及模型（几何布朗运动反射原理）估算，波动率或期限缺失时显示「--」。
- 压力位/支撑位柱状图：位于 Gamma 敞口右侧、同一行两栏，横轴为价位（按价格线性排布），柱高为综合强度（1 为最强），压力位向上（绿）、支撑位向下（红），并用琥珀色竖线标出基准价；悬停显示价位、距现价、到达概率与综合依据。原有的压力位/支撑位表格保留。
- 趋势通道与交易计划：趋势通道按最近 60 根日线收盘价做线性回归，给出上行通道 / 下行通道 / 区间震荡以及通道上下轨与日均斜率，并用 RSI(14) 标出超买、超卖或中性，并列出 52 周最高/最低与历史最高/最低价（取日线最高/最低价，52 周窗口按最后一根日线回推 365 天，历史极值取全量日线，悬停显示发生日期与距现价）；推荐买入价位取现价下方最近的 5 个支撑，加仓价位取更深一档的 5 个支撑，卖出价位取现价上方最近的 5 个压力，三张表的每一行都给出价位、距现价、到达概率与综合依据（组成该价位的因子标签，悬停可看综合强度）。
- 将鼠标或触摸移动到任意图表上，可查看十字虚线（竖线定位价位、横线跟随鼠标高度，两端各带取值标签）和详细数据浮层；Gamma 图额外显示行情价、看涨 GEX、看跌 GEX 和净 GEX。
- 浏览器通过 SSE 保持当前标的连接；仅活跃订阅触发行情自动刷新，服务端按美股交易时段自调度：常规盘约 20 秒、盘前/盘后约 60 秒、休市约 5 分钟；上游连续失败时指数退避，最长 5 分钟。SSE 断开后停止该订阅的刷新；SQLite 租约协调多 worker，避免重复回源。`DEFAULT_SYMBOLS` 仅用于页面默认标的；页面数据随实时推送自动更新。载入标的时优先读取 SQLite 缓存，跨期限 Gamma 窗口在后台异步刷新，不阻塞表格。
- 每天按 `RAW_RETENTION_DAYS` 配置分批清理过期的历史行情、期权链、日线历史和刷新运行记录。
- 首次载入一个从未抓取过的标的时，即使本地 SQLite 还没有它的到期日，也会自动回源补全并正常渲染；如果该标的确实没有挂牌期权（例如 OTC 存托凭证 TCEHY），页面只显示现货行情，期权相关面板统一提示「没有挂牌期权（或可用期限已全部到期）」，刷新按钮仍可用于更新现货。
- SQLite 使用 WAL 和索引，适合单机或单实例快速部署。
- 页面自上而下依次为：标的现货与成交统计卡 → 分析详情折叠组（默认展开：趋势通道与交易计划（推荐买入 / 加仓 / 卖出）→ 压力位/支撑位数据表）→ 图表区（Gamma 敞口、压力位/支撑位柱状图、成交量分布、持仓量分布）→ 期权流向与买方结构分析（默认折叠）→ 期权链明细（默认折叠）。折叠组的展开状态记在 sessionStorage：趋势分析与图表默认展开，期权分析与期权链默认折叠；同一标签页内换标的不会改变，重开页面各自回到默认。

配色沿用美股行情惯例「绿涨红跌」：上涨、看涨（Call）、压力位统一为绿，下跌、看跌（Put）、支撑位统一为红。Go 版颜色令牌集中在 `web/static/common/css/styles.css`：黑夜主题（默认）在 `:root`，白天主题在 `:root[data-theme="light"]`，两套同名令牌一一对应。Python 基线静态资源副本仍位于 `us_stocks_py/app/static`。

主行情接口在这里提供的是行情和盘中期权链快照，Cboe 公共接口提供盘外延迟期权链；两者都不是交易所逐笔成交流，也不保证每分钟都有新的市场数据。Cboe 的未平仓量属于上一交易日数据，适合盘前、盘后和夜盘分析但不是盘外实时 OI。生产使用前请确认所用行情数据源的使用条款与授权范围。

Gamma 敞口使用 Black-Scholes 根据标的价格、执行价、隐含波动率和到期时间估算，并按看涨正值、看跌负值汇总。零 Gamma 将标的价格作为变量，在近 7 天未到期合约上重新计算总 GEX，到期时刻按美东 16:00 处理，使盘中 0DTE 仍计入；扫描现价 ±15% 后二分求根，多根时取距离当前股价最近的根。柱状图、看涨墙和看跌墙仍使用近 45 天，避免墙位被当天到期合约完全淹没。这些指标是分析估算，不代表交易商真实库存或完整市场 Gamma。

压力位/支撑位的四类因子：斐波那契回撤取回看 120 根日线里由收盘价确认的摆动高低点，按 23.6% / 38.2% / 50% / 61.8% / 78.6% 计算（50% 与 61.8% 权重最高），单根插针不会重画整组回撤；筹码分布把每根日线的成交量按当日价格区间做三角分配后汇总成价格剖面，取最密集的价位；承接位取近 60 个交易日内「被砸下去又收回（长下影，或随后反弹达到约 0.5 倍 ATR）、且之后没有被跌破超过约 0.25 倍 ATR」的摆动低点，越近、越放量权重越高；期权持仓按所选到期日聚合绝对 GEX（整链未平仓量不足成交量 20% 时退回成交量），行权价在现价上方视为压力侧磁吸、下方视为支撑侧磁吸，标签仍标明该价位主要是看涨还是看跌。技术面候选同样按价位落在现价哪一侧归入压力或支撑，同段价位合并后按权重和累加强度。

### 零 Gamma 的公开算法依据

项目采用的是公开资料中常见的“spot-shift GEX profile”口径，而不是按执行价柱子的累计和直接猜测。主要参考：

- [SpotGamma：Gamma Flip](https://support.spotgamma.com/hc/en-us/articles/15413261162387-Gamma-Flip)：将 Gamma Flip 定义为做市商估算 Gamma 正负切换的价格。
- [ZeroGEX：Gamma Flip Calculation](https://zerogex.io/guides/gamma-flip-calculation-before-vs-after)：公开说明近似实现，使用 `Gamma × OI × 100 × S² × 0.01`，Call 为正、Put 为负，在假设的标的价格网格上重新计算 Gamma。本工具未采用其 `DTE/5` 多期限权重，因为个股盘中 0DTE 会被过度压低，导致 AAPL 这类标的零点明显偏离现价。
- [Quant Data：What Is Zero Gamma](https://quantdata.uk/guides/what-is-zero-gamma)：给出近 45 天、冻结 IV、现价 ±10% 搜索区间等可复现选择。
- [sgdividends/spx-dealer-gamma](https://github.com/sgdividends/spx-dealer-gamma)：公开 Python 实现，使用 ±15% / 121 点扫描和 Black-Scholes 重算。

不同平台的零 Gamma 不会天然相同：富途、SpotGamma 等可能使用不同的期权数据时刻、到期日范围、隐含波动率来源、未平仓量过滤和 0DTE 权重。当前工具使用上游快照，因此目标是口径透明、结果可复算，而不是承诺与某个商业平台逐分复刻。

## 启动

推荐直接用运维脚本 `run.sh`（幂等，重复执行安全，Ubuntu/Debian、CentOS/RHEL、Alpine 通用）：

```bash
./run.sh            # 交互菜单：启动/停止/重启、配置、数据库、环境安装与自检、更新、高级服务管理、端口释放
./run.sh install   # 按 GO_BACKEND 检测所选后端；Go 只检查预编译程序，不启动应用（等价菜单第 7 项）
./run.sh 2          # 启动应用；若环境缺失会先准备环境，再后台启动并挂看门狗守护
./run.sh status     # 查看运行状态（状态和日志对应主菜单第 1 项）
./run.sh stop       # 停止应用与看门狗
./run.sh restart    # 停止后重新启动应用
GO_BACKEND=go ./run.sh install  # 选择 Go 后端时检查发布包中的预编译程序
./run.sh kill-port 8080  # 确认后结束占用 TCP 8080 的进程；不带端口时交互输入
```

选项 7 只负责检查 Python、创建虚拟环境、安装/修复依赖，不创建 `.env`，也不启动应用；需要再手动选择选项 2 启动。选项 2 会补齐缺少的运行环境和 `.env`，然后启动应用。对应手动命令为 `python -m venv .venv`、`.venv/bin/pip install -e .`、`cp .env.example .env` 和 `.venv/bin/python -m app`；已经就绪的步骤会跳过（依赖指纹变了才重装）。启动后应用与看门狗都以 `setsid` 脱离会话在后台运行，
无 systemd 的环境下看门狗每 15 秒检查一次健康状态，连续 2 次失败或进程消失会自动拉起；
日志写到 `logs/app.log`、`logs/watchdog.log`，超过 5MB 自动轮转。
脚本的退出码沿用对应动作的结果（成功 0、失败非 0），可直接用在计划任务或监控里。

生产服务器建议安装 systemd 托管，避免 SSH 断开、服务器重启或 Python 进程被 OOM 杀掉后出现 Cloudflare 521：

```bash
./run.sh install-service
./run.sh status
```

该命令会生成并启用 `option-scope.service`，以项目目录所属的普通用户运行，进程异常后 3 秒自动重启，并设置为开机启动。
查看服务日志：`journalctl -u option-scope.service -f`。卸载 systemd 托管并恢复 shell 看门狗：`./run.sh remove-service`。

如果仍出现 521，先在服务器本机执行 `curl -fsS http://127.0.0.1:${PORT:-8000}/health`：
本机失败说明应用或端口没有监听，查看 `systemctl status option-scope.service` 和 `journalctl -u option-scope.service`；
本机成功而公网失败，则检查 Cloudflare 到源站端口、防火墙、安全组和反向代理配置，521 不是应用代码能单独修复的。

手动启动方式：

```bash
python -m venv .venv
.venv/bin/pip install -e '.[dev]'
cp .env.example .env
.venv/bin/python -m app
```

服务默认监听 `0.0.0.0:8000`。新版启动脚本会生成 `ACCESS_KEY`，本机访问
`http://127.0.0.1:8000/?key=你的访问密钥`，局域网地址把主机名换成运行主机的实际 IP。
首次带 `?key=` 打开后，页面导航和后续 AJAX 请求都会继续在 URL 上携带 key；
浏览器同时会把 key 写入 `localStorage` 作为备用读取来源。如果浏览器禁用 `localStorage`，
会自动回退 `sessionStorage` 和当前页面内存，不会因为存储策略不同而丢掉访问凭证；
URL 和本地存储都没有 key 时才显示 403。
手动启动时需要自行在 `.env` 填写 `ACCESS_KEY`，留空表示不启用访问保护，适合只在本机临时调试。
URL 中的密钥会进入浏览器历史记录，
请避免在公开截图或聊天中直接分享完整地址。

应用会自动读取可执行文件所在目录下的 `.env`；默认数据库为可执行文件旁 `data/options.db`，也可以在 `.env` 中设置绝对或相对于该目录的 `DATABASE_PATH`。进程启动前导出的环境变量优先于 `.env`。
交互菜单第 5 项会以数字菜单逐项修改配置，包含 `ACCESS_KEY` 和 Alpaca 夜盘凭据：`ALPACA_API_KEY`、`ALPACA_API_SECRET` 输入时不会回显，摘要只显示掩码；输入 `clear` 可清空凭据。修改后应用运行中会询问是否立即重启。第 10 项可按输入的 TCP 端口列出监听进程，经确认后发送 `SIGTERM`；进程仍未退出时再确认是否发送 `SIGKILL`。

也可以直接使用 `.venv/bin/uvicorn app.main:app --host 0.0.0.0 --port 8000`。可通过 `HOST` 和 `PORT` 环境变量覆盖 `python -m app` 的默认值。

## 常见问题

### 安装依赖时提示 `Killed`（内存不足 / OOM）

菜单第 7 项或 `pip install` 中途出现 `Killed`（退出码 137），含义是进程被系统的 OOM Killer 杀掉了：
内存不够用，**不是**网络、软件源或代码问题。脚本检测到这种情况会直接打印当前内存和下面的处理办法：

1. 先停掉正在运行的应用再装：`./run.sh stop`（应用本身常驻 150-250M，装依赖时内存占用会翻倍）；
2. 临时加 1G Swap，装完可保留也可删除：

   ```bash
   fallocate -l 1G /swapfile && chmod 600 /swapfile && mkswap /swapfile && swapon /swapfile
   ```

   没有 `fallocate` 的镜像（busybox）改用 `dd if=/dev/zero of=/swapfile bs=1M count=1024`；
   非特权 LXC 容器可能禁止 `swapon`，那就走第 3 条。
3. 调大容器/主机内存上限到 1G 以上（建模机磁盘 512M 也建议内存 ≥ 1G）；
4. 内存实在加不上时分步安装，降低单次峰值：

   ```bash
   .venv/bin/pip install --no-cache-dir pandas
   .venv/bin/pip install --no-cache-dir -e .
   ```

确认是否 OOM：`dmesg | tail -20`，或 `cat /sys/fs/cgroup/memory.events`（出现 `oom_kill` 计数即可确认）。
Alpine 上如果 `apk` 装包过程中被中断，先执行 `apk fix` 修复半装状态的软件包再重试。

## 配置

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `DATABASE_PATH` | `data/options.db` | SQLite 文件路径 |
| `HOST` | `0.0.0.0` | `python -m app` 的监听地址 |
| `PORT` | `8000` | `python -m app` 的监听端口 |
| `ACCESS_KEY` | 空 | 页面与 API 访问密钥；`run.sh` 首次启动自动生成 16 位小写字母 + 数字 |
| `MARKET_PROXY` | 空 | Go 版出站行情与期权请求使用的代理地址，支持 `http`、`https`、`socks5` 和 `socks5h`，例如 `http://127.0.0.1:10808` |
| `ALPACA_API_KEY` | 空 | Alpaca Basic API Key；配置后仅在美东夜盘补充股票现货行情，未配置时沿用原行情源 |
| `ALPACA_API_SECRET` | 空 | Alpaca Basic API Secret；`run.sh config` 菜单中隐藏输入，摘要不会显示原文 |
| `DEFAULT_SYMBOLS` | `QQQ` | 页面未带 `?symbol=` 时使用的默认标的（兼容逗号分隔配置，取第一项）；不再触发后台刷新 |
| `RAW_RETENTION_DAYS` | `30` | 历史快照保留天数；后台维护任务每天检查并清理超过保留期限的数据 |
| `DATABASE_MAX_MB` | `0` | SQLite 体积上限，纯数字按 MB 解释，也支持 `300M`、`1G`；`0` 表示不限制 |
| `HISTORY_MAX_AGE_SECONDS` | `3600` | 日线历史（斐波那契/筹码分布/承接位）的回源间隔 |
| `EXTREMES_MAX_AGE_SECONDS` | `86400` | 52 周和历史最高/最低价的回源间隔 |
| `SCHEDULER_ENABLED` | `true` | 是否启用定时历史清理和数据库维护；不控制行情刷新 |
| `UPSTREAM_CONCURRENCY` | `auto` | Go 默认按启动时剩余 cgroup/物理可用内存预算（预留 64 MiB 后每槽位估算 160 MiB）和容器可用 CPU 动态计算；整数值作为人工上限。Python 版读取 `auto` 时使用原有默认并发 6 |
| `UPSTREAM_TIMEOUT_SECONDS` | `12` | Go 上游 Yahoo / Alpaca 请求超时秒数，与 Python 默认值一致 |
| `CBOE_TIMEOUT_SECONDS` | `15` | Go 的 Cboe 期权请求超时秒数，与 Python 默认值一致 |
| `UPSTREAM_WAIT_SECONDS` | `20` | 等待上游并发槽位的最长时间，超时后优先使用本地旧快照 |
| `WEB_WORKERS` | `1` | Web worker 数量；增加前请确认部署机器内存足够。低内存模式会把大于 1 的值降回 1 |
| `LOW_MEMORY` | `auto` | Go 后端 `auto` 在内存上限不超过 4GiB 或启动时可用内存不超过 2GiB 时开启保护；也可强制 `true` / `false`。开启后后台重任务采用保守并发、缓存缩小，并按剩余磁盘收紧数据库上限；基础报价与期限目录仍使用共享并发预算 |

表中「默认值」是环境变量缺失时代码的回退值；`.env.example` 面向小磁盘环境，
已经把 `DATABASE_MAX_MB` 预设为 `256M`、`RAW_RETENTION_DAYS` 预设为 `7`，改成 `0` / 更大的天数即可放宽。

### 小磁盘部署（LXC / 512M 磁盘）

512M 磁盘不要按 256M 的数据库来预留。低内存保护开启时会按剩余空间再收紧上限，
并给系统、虚拟环境和 SQLite 临时文件留出余量。显式填写的 `DATABASE_MAX_MB` 只会更小，不会比这个预算更大。

把 `DATABASE_MAX_MB` 设为磁盘容量的一半到六成（512M 磁盘建议 `200`-`300`，1G 磁盘建议 `500`-`600`），
后台维护任务每天检查数据库体积（未超限时只读取文件大小，开销可忽略），
超过上限后按阶梯清理，每一步都先看是否已经降到上限的 80% 以下，够用就停下：

1. 清空 `raw_json` 冗余报文（该列只写不读，旧库里约占文件的 68%）；
2. 删除 24 小时之前、已被新批次覆盖的旧快照，每个 `(标的, 到期日, 天)` 保留一批；
3. 仍未降下来时收窄到每个 `(标的, 到期日, 小时)` 保留一批；
4. 继续超限才退到最狠的一档：每个 `(标的, 到期日)` 只保留最新一批，并删除过期刷新记录；
5. 磁盘剩余空间足够（不小于库文件的 1.2 倍）时执行 `VACUUM` 回收文件体积，空间不足则跳过并记录告警，
   等下一轮再回收，避免把容器磁盘写满。

判断「是否降下来」用的是去掉空洞后的有效数据量，而不是文件大小：SQLite 删除行不会立刻缩小文件，
要等 `VACUUM`（WAL 模式下还需要一次 checkpoint）才会真正腾出磁盘。

清理只影响历史冗余批次：最新快照、日线历史、极值缓存的读取路径不受影响；
盘前未平仓量为 0 时的回退逻辑依赖最近 24 小时的历史批次，第 2、3 步都按时间桶给它们留了样本。

另外，新版本不再写入 `raw_json` 冗余报文（列保留以兼容旧库），服务启动时会自动清空旧库中
遗留的报文并回收文件体积，例如 150 MB 的旧库启动后通常会降到 70 MB 左右。

## API

- `GET /api/quote/{symbol}`
- `GET /api/expirations/{symbol}`
- `GET /api/chain/{symbol}?expiration=YYYY-MM-DD`
- `GET /api/flow/{symbol}?expiration=YYYY-MM-DD`（比较同一期限最近两份链快照，估算成交方向与期权流向）
- `GET /api/events/{symbol}?expiration=YYYY-MM-DD`（SSE 推送活跃订阅标的的快照更新通知）
- `GET /api/gamma/{symbol}?horizon_days=45&refresh=true`（跨近期期限的 Gamma 分析链）
- `GET /api/levels/{symbol}?expiration=YYYY-MM-DD&spot=144.57`（压力位/支撑位：斐波那契 + 筹码密集 + 承接位 + 所选到期日期权持仓综合，每行含「到达概率」；`spot` 为可选基准价，缺省用快照常规价）
- `POST /api/refresh/{symbol}?expiration=YYYY-MM-DD&max_age=60`（本地快照在 `max_age` 秒内直接返回 `skipped=true`，不再请求上游接口；`max_age=0` 表示强制刷新）
- `GET /api/status/{symbol}`
- `POST /api/cleanup`
- `GET /health`

数据库菜单支持在线安全备份，也支持从备份恢复。恢复会先要求确认、停止服务、检查备份 SQLite 完整性和应用表结构，再保留原数据库及 sidecar 作为回滚副本后替换；操作失败会尝试恢复原文件。Python 后端用 Python 标准库执行恢复；Go 后端使用发布包中的 `bin/option-scope-go-db restore <backup-path>`。直接运行 Go 数据库恢复工具时，应用服务仍须先停止。

配置 `ACCESS_KEY` 后，所有页面和 `/api/*` 请求都必须携带正确的 `?key=` 或 `X-Access-Key` 请求头；
只有 `/static/*` 与 `/health` 例外，分别用于加载页面资源和本机看门狗健康检查。

## 本地验证

```bash
.venv/bin/python -m pytest -q
.venv/bin/python -m compileall -q app tests
```

测试使用 mock provider，不会依赖外部行情服务；启动应用后的真实数据请求需要网络连接。
