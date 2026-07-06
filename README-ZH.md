# teledrop（中文说明）

一个 Telegram 机器人：把发给它的所有文件下载到本地并按规则整理，可选地上传到 S3 协议的对象存储（腾讯云 COS / MinIO / AWS S3 等）。上传能力用 `Uploader` 接口抽象，方便扩展其它后端；提供 `overwrite` 覆盖开关。

文件的说明文字（caption）以及纯文本消息，会作为 `.txt` 文件保存在二进制文件旁边，开启上传时也会一并上传。

## 功能

- 自动接收并下载：document / photo（取最大图）/ video / audio / voice / video note / animation / sticker
- 本地整理规则可切换：`flat` `by_date` `by_type` `by_chat` `by_chat_date`（默认 `by_chat_date`）
- 转发隔离：开启 `separate_forwards` 后，转发文件额外放进 `forwarded/` 段，与直发区分
- 文字也会保存：文件的 caption 存为同名 `.txt` 旁注文件；纯文本消息存为 `text_<id>.txt`，含发送者/时间等元信息
- 用户白名单：`allowed_users` 非空即生效，避免陌生人往你的 COS 投文件；空=不限制
- 上传后端可插拔：`s3` / `local` / `none`，默认关闭
- 覆盖开关：`overwrite=false` 时远程已存在则跳过，返回 `ErrExists`
- 并发受限（默认 4）；单文件失败不影响其它；错误不吞，会回用户提示
- **两阶段回复**：收到文件先发 `📥 开始处理 N 个文件...`，全部完成后发 `✅ X/Y 成功` 汇总（含每个文件大小、耗时、上传状态），均引用原消息
- **大文件支持**：>20MB 文件使用独立 HTTP client（30 分钟超时），避免慢连接或上限导致的下载失败
- **SQLite 追踪**：所有下载记录（成功 / 失败 / 跳过）存入 `<base_dir>/.teledrop.db`，支持按时间/用户/文件结构化查询
- **S3 容错**：endpoint 自动补全 `https://`，提供 `health_check` 可选启动连通检测，S3 初始化失败不会导致 bot 崩溃（自动降级为不上传模式）

## 目录结构（单一 package，已打平）

```
main.go        装配 + 命令行参数 + 信号退出
config.go      配置加载 + ${ENV} 展开 + 校验
organizer.go   由消息算本地相对目录与远程 key
downloader.go  抽取消息内所有文件 + 下载 / 保存文字到本地
uploader.go    Uploader 接口 + 工厂
s3.go          S3/COS 上传器
local.go       本地镜像上传器
noop.go        关闭态占位
store.go       SQLite 持久层（已处理/已见/失败队列/下载记录）
state.go       去重 + 失败队列 + 下载记录（基于 store）
bot.go         轮询消息、白名单校验、分发处理
```

## 通过 @BotFather 创建 Telegram bot

teledrop 需要一个 bot token。在 Telegram 内即可免费创建：

1. 打开 Telegram，搜索 **@BotFather**（官方的 bot 管理账号，带蓝色认证勾）。
2. 进入对话，发送 `/start` 打开菜单。
3. 发送 `/newbot`，BotFather 会先询问显示名称。
4. 回复一个好读的**名称**，例如 `My File Drop`。
5. 再回复一个全局唯一的**用户名**，必须以 `bot` 结尾（不区分大小写），例如 `my_file_drop_bot`。若被占用换一个。
6. BotFather 会回一条消息，里面包含 HTTP API token，形如：
   ```
   123456789:AAEhBOwe...longstring...
   ```
   复制这串 token——这是 teledrop 接收并下载文件所需的唯一凭据。
7. （建议）完善 bot 资料：
   - `/setdescription` / `/setabouttext` —— 资料页简介文字。
   - `/setuserpic` —— 头像。
   - `/setcommands` —— 注册 `start` 命令，让它作为建议按钮出现：
     ```
     start - 确认 bot 在线
     ```
8. （可选）若要用 `allowed_users` 白名单，先和 **@userinfobot** 或 **@RawDataBot** 对话拿到自己的 Telegram 用户 `id`，再把它填到 `telegram.allowed_users`。

请妥善保管 token：拿到它的人就能控制你的 bot。在 teledrop 中，token 通过环境变量 `TELEGRAM_TOKEN` 注入（见 `config.yaml` 里的 `${TELEGRAM_TOKEN}` 占位符），绝不写进仓库。

## 接收消息：长轮询

teledrop 使用**长轮询**（`getUpdates`）：与 Telegram 维持一条长连接，有新消息就拉取回来。**无需配置回调 URL，也不用反代**——直接运行即可。

## 去重、失败重试与下载记录

teledrop 在 `<base_dir>/.teledrop.db`（SQLite）中维护状态，重启后依然生效（旧版使用 JSON 文件，首次运行会自动迁移）：

- **已处理消息**（`chat_id:message_id`）：重启后若 Telegram 重投了同一条更新，会直接跳过，避免重复下载。
- **已见过文件**（Telegram `FileUniqueID`）：同一个文件发两次，只下载一次。
- **失败下载**：下载（或写文本/ caption）失败时，先指数退避重试最多 3 次，仍失败则入队。由于 Telegram 更新一旦被确认就一次性失效，失败项靠其**长期有效的 `file_id`** 重新拉取，而不是重拉消息。
- **下载记录**：每次下载（成功、失败、跳过）都会写入 `downloads` 表，含 chat ID、发送者、文件名、大小、状态、耗时、上传状态和时间戳。可直接查询数据库，例如：
  ```sql
  sqlite3 downloads/.teledrop.db "SELECT name,size,status,datetime(created_at,'unixepoch') FROM downloads ORDER BY created_at DESC LIMIT 20;"
  ```

随时发送 **`/retry`** 手动重跑失败队列；teledrop 启动时也会（静默地）自动重跑一次。一条消息只有在**所有部分都成功**后才会标记为已处理，因此局部失败会留在队列里等待后续重试。

## 大文件下载（MTProto）

Telegram Bot API 拒绝 `getFile` 下载大于 20 MB 的文件（返回 `Bad Request: file is too big`）。
teledrop 可选用 **Bot 自身的 MTProto 客户端**来下载——使用与 Bot API 相同的 `telegram.token`，
无需手机号、验证码或 2FA，零额外配置。

**配置步骤：**

1. 在 https://my.telegram.org/apps 创建应用 — 免费，30 秒完成
2. 在 `config.yaml` 中填写 `download.mtproto`：
   ```yaml
   telegram:
     mtproto:
       app_id: 12345
       app_hash: "abc..."
       socks5: "127.0.0.1:1086"
   ```
3. 启动 Bot — 自动用 `telegram.token` 登录，无需任何交互

**原理：** MTProto 与 Bot API 使用同一条 Bot Token，消息 ID 完全一致，直接用 ID
定位和下载文件，无需做 peer 映射或内容搜索。

若未配置（`app_id=0`），大文件与之前一样报错。

## 快速开始

```bash
# 1. 准备配置
cp config.yaml.example config.yaml   # 按需修改
export TELEGRAM_TOKEN="你的 bot token"      # 来自 @BotFather
# 若开启 s3 上传：
export COS_ACCESS_KEY="..."
export COS_SECRET_KEY="..."

# 2. 运行
go run . -config config.yaml
```

向 bot 发送 `/start` 确认在线，随后发任意文件即可。

## 整理规则示例（默认 by_chat_date + separate_forwards）

```
downloads/
├── alice/                    # chat 目录（优先 username，否则 title / chat id）
│   └── 2026/07/06/
│       ├── photo_xxx.jpg
│       ├── photo_xxx.txt      # caption 旁注文件（若发送时带了说明文字）
│       └── doc_xxx.pdf
└── forwarded/                # 转发文件隔离段
    └── some_channel/
        └── 2026/07/06/
            └── video_yyy.mp4
```

远程 key 与本地相对路径一致，仅用 `/` 分隔，结构在 COS 中镜像。

## 扩展新的上传后端

实现 `Uploader` 接口，并在 `NewUploader` 的工厂里加一个分支：

```go
type Uploader interface {
    Upload(ctx context.Context, src, key string, overwrite bool) error
}
```

## 配置说明

| 字段 | 含义 |
|------|------|
| `telegram.token` | bot token（@BotFather 获取），支持 `${ENV}` |
| `telegram.allowed_users` | 白名单用户 ID 列表；空=不限制 |
| `download.base_dir` | 本地下载根目录 |
| `download.organize` | 整理规则 |
| `download.separate_forwards` | 转发的文件是否隔离到 `forwarded/` |
| `telegram.mtproto.app_id` | Telegram API ID，用于大文件下载（可选，0=关闭） |
| `telegram.mtproto.app_hash` | Telegram API hash |
| `telegram.mtproto.socks5` | SOCKS5 代理地址，国内必需（如 `127.0.0.1:1086`） |
| `upload.enabled` | 是否上传（默认 false） |
| `upload.overwrite` | 远程已存在时是否覆盖 |
| `upload.type` | `s3` / `local` / `none` |
| `upload.s3.*` | S3 兼容配置；COS/MinIO 填写自定义 `endpoint`。`health_check`（可选布尔值）启动时 ping bucket 验证连通性 |
| `upload.local.mirror_dir` | `type=local` 时的镜像目录 |
