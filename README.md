# Go 大文件分片上传与断点续传服务

这个项目解决“大文件传到一半断网，如何继续上传，并确认最终文件没有损坏”的问题。它适合用于 Go 后端实习简历，重点是流式 I/O、HTTP Range、数据库事务、幂等、并发状态转换和磁盘与数据库之间的一致性边界。

实现采用 Go 标准库 `net/http`、PostgreSQL 及本地持久化磁盘。文件字节不进入数据库，也不一次性加载到内存。当前没有接入 S3 或 MinIO。

> 2026-10-04 完善轮通过 11 个顶层单元测试、go vet 和编译，未启动 Docker。使用源码一致的临时副本及既有同版本依赖离线验证；原目录联网构建、真实 PostgreSQL、容器部署和压测仍未验收。详见 [完善记录](docs/06-完善记录与验收.md)。

## 学习入口

- [项目设计：需求、架构、数据模型与一致性](docs/01-项目设计.md)
- [学习路线与源码导读](docs/02-学习路线与源码导读.md)
- [接口说明、演示脚本与手工验证清单](docs/03-接口与手工验证.md)
- [完善记录与验收](docs/06-完善记录与验收.md)

## 核心流程

1. 客户端计算整个文件的 SHA-256，声明文件大小与分片大小，创建上传会话。
2. 客户端按从 1 开始的片号提交原始字节，允许乱序发送。
3. 服务端流式接收到临时文件，检查实际长度与可选分片 SHA-256，然后持数据库行锁发布分片。
4. 客户端通过状态接口获取已成功提交的片号，重连后只补发缺失分片。
5. 服务端按片号顺序流式合并，检查整体 SHA-256，再标记完成。
6. 完成的文件可通过 GET、HEAD 和 HTTP Range 下载。

同一片号重复发送相同内容返回成功；不同内容返回冲突。完成接口可重复调用。取消接口只允许取消未完成上传。

## 项目能力与边界

| 已实现 | 需要后续扩展 |
| --- | --- |
| 初始化、上传、状态查询、完成、取消、下载 | 前端拖拽、自动重连 UI |
| 流式限长、分片和整体 SHA-256、路径约束 | 病毒扫描、内容合规、文件类型识别 |
| PostgreSQL 行锁协调写入、完成、取消 | 非阻塞合并队列、租约与后台任务 |
| API Key 对应服务端固定租户 | 动态租户管理、用户登录、权限策略 |
| 本地卷部署、Docker Compose 示例 | S3/MinIO 存储适配、预签名 URL |
| 单元测试通过、每进程上传/合并并发门控 | 真实 SQL 集成、故障注入、累计配额与压测 |

这是单服务、共享磁盘模型。多个副本只有在访问同一数据库和同一个支持所需文件操作的共享文件系统时才可能成立，不能把多个独立本地卷当作共享存储。当前推荐单副本部署。数据库与文件系统没有共同事务，崩溃可能留下不可见的孤儿文件，详见设计文档；当前没有自动垃圾回收。成功完成后分片仍保留用于清晰的恢复边界，文件及分片共占用约两倍原始文件空间。

## 之后如何启动

准备 Docker Desktop 后，在此文件夹执行：

```powershell
docker compose up -d --build
```

服务地址为 `http://localhost:8092`。演示鉴权为 `X-API-Key: local-upload-key`，服务端将此 Key 绑定到 `demo-tenant`，客户端不能通过租户请求头切换所有者。上线前必须更换密钥，并增加 TLS、存储配额、清理机制及真实鉴权。

手工演示（需要 PowerShell 7）：

```powershell
pwsh -File .\scripts\demo.ps1 -FilePath C:\data\example.bin
```

停止但保留数据库和文件卷：

```powershell
docker compose down
```

不要加入 `-v`，除非你明确要删除演示数据。

## 目录结构

```text
cmd/server/           服务启动、配置加载、优雅关闭
internal/config/      环境变量与边界校验
internal/domain/      状态、分片计算、业务错误
internal/store/       PostgreSQL 元数据与事务
internal/service/     流式磁盘操作与一致性流程
internal/api/         鉴权、路由、错误映射、Range 下载
migrations/           首次数据库初始化
scripts/demo.ps1      可续传的客户端演示
docs/                 项目与学习文档
```

## 参考资料

接口设计参考“初始化—上传分片—完成”的成熟模式，但不是 S3 协议实现：[Amazon S3 multipart upload 官方说明](https://docs.aws.amazon.com/AmazonS3/latest/userguide/mpuoverview.html)。下载使用 Go 的标准库行为：[net/http ServeContent](https://pkg.go.dev/net/http#ServeContent)。
