# GPU Glint

在一个终端里查看多台 SSH 主机的 GPU 利用率、显存和温度。Windows 和 Linux 客户端均使用系统 OpenSSH，远端复用已有的 GPU 工具。人可以看实时面板，agent 可以直接查询同一次采集产生的 JSON。

## 开始使用

从 [GitHub Releases](https://github.com/lyingloong/gpuglint/releases) 下载对应系统和架构的程序，Windows 上命名为 `gpuglint.exe`，Linux 上命名为 `gpuglint` 并运行 `chmod +x gpuglint`。将它放在 PATH 中，或者用 `.\gpuglint.exe`、`./gpuglint` 启动。已有 Go 1.23 或更新版本也可以从源码安装。

```sh
go install github.com/lyingloong/gpuglint@latest
```

```sh
gpuglint hosts
gpuglint watch --host "train-*" --interval 15s
```

默认选择所有具名主机，每 30 秒采样，最多同时轮询 4 台，每台的连接和后端回退共用 20 秒超时。面板展示每块卡的型号、负载、显存（GiB）、温度和上次成功采样的年龄。`--sort free` 将剩余显存最多的卡排在前面，过期和显存未知的记录排在最后。同一主机最多有一次正在进行的采集。

## 命令

| 命令 | 用途 |
| --- | --- |
| `gpuglint hosts --json` | 列出选中的 SSH 别名，无需联网 |
| `gpuglint scan --host gpu-01 --json` | 立即采集一次，结果写到标准输出 |
| `gpuglint watch --exclude "login-*" --sort free` | 打开实时面板并维护状态文件 |
| `gpuglint watch --headless --interval 10s` | 在前台持续采集，可由进程管理器运行 |
| `gpuglint status --follow` | 读取正在更新的状态文件并打开只读面板，按 Ctrl+C 关闭这个面板 |
| `gpuglint status --json` | 验证采集进程存活并读取最新快照，不触发 SSH |

`hosts`、`scan`、`watch` 支持重复传入 `--host` 和 `--exclude`。模式支持 `*` 和 `?`，建议加引号。多条包含规则取并集，排除规则优先。`scan` 和 `watch` 可用 `--concurrency` 和 `--timeout` 调整并发和超时。运行 `gpuglint <命令> -h` 可查看该命令的全部参数。

### SSH 配置与采集

默认从 `~/.ssh/config` 发现主机，`--ssh-config PATH` 可指定其他配置。发现器读取具名 `Host` 和 `Include` 文件，保留声明顺序。通配规则无法枚举具体机器，可以用 `--host gpu-07` 指定匹配该规则的目标。依赖运行时 `Match` 条件、主机专属路径变量或其他用户主目录的 Include 也可采用显式别名。相对 Include 路径以用户的 `~/.ssh` 为基准。

实际连接由 PATH 中的 `ssh` 完成，读取已有的用户名、端口、密钥、跳板机和主机密钥配置。采集运行在非交互会话中；首次使用一台机器时，可先用相同别名手动登录，确认主机密钥和 SSH 认证可用。远端的非交互 PATH 需要能找到至少一个采集后端。

| 优先级 | 后端 | 数据来源 |
| --- | --- | --- |
| 1 | nvitop | 用远端 `python3` 或 `python` 启动探针，直接导入 nvitop 或从 PATH 中的常见 pipx、uv、venv 启动器定位其环境，通过设备 API 读取 NVML 数据 |
| 2 | nvtop | 读取 `nvtop --snapshot` 的一次性输出；需要支持可解析快照的版本 |
| 3 | nvidia-smi | 通过 `--query-gpu` 和 CSV 输出读取 NVIDIA 驱动指标 |

工具缺失或响应无法解析时继续尝试下一种后端。单机断线只影响该主机。缺失的数值用 JSON `null` 和面板中的 `–` 表示。nvtop 报告的设备取决于其编译选项和驱动支持。一个成功后端有设备时即采用它的结果。

## 状态文件与 agent

`watch` 启动时创建 `~/.ssh/.gpuglint/state`，Windows 对应 `%USERPROFILE%\.ssh\.gpuglint\state`。初始快照列出所有目标，状态均为 `pending`。每当一台主机完成采集，程序用同目录临时文件原子替换整个快照。每个状态路径只允许一个写者。`--state-file PATH` 可为独立监控实例指定不同的路径，在 `status` 中使用同一参数即可读取。`--ssh-config` 仅更改 SSH 配置来源。

按 Ctrl+C 或在 Linux 上发送 SIGTERM 后，采集进程删除自己写入的状态和锁文件，并清理本次新建且仍为空的状态目录。断电或强制结束进程可能留下文件；此时 `status` 会报告采集进程未运行，下次启动会回收旧状态。需要判断进程存活的 agent 应优先调用 `status --json`。

[JSON Schema](schema/state-v1.json) 描述了 `status --json`、`scan --json` 和状态文件共用的快照格式。时间戳使用 UTC；利用率单位为 %，显存为 MiB，温度为 °C。

| 主机状态 | 含义 |
| --- | --- |
| `pending` | 本次启动后还没有采集结果 |
| `ok` | 后端返回了 GPU 数据 |
| `no_gpu` | 后端报告设备数为零 |
| `error` | 最近一次采集失败，原因在 `error.code` 和 `error.message` 中。若有上次成功结果，保留其 `backend`、`gpus`、`last_success_at` 和 `fresh_until` |

`last_attempt_at` 是最近一次已完成尝试的开始时间。持续采集时，`fresh_until` 是成功采样时间加上轮询间隔和单机超时。选择 GPU 的 agent 应使用 `status == "ok"`、未过期且所需指标非空的记录。例如，安装了 jq 的环境可以列出各卡剩余的 MiB。

```sh
gpuglint status --json | jq -r '
  .hosts[] | select(.status == "ok")
  | select((.fresh_until | sub("[.][0-9]+Z$"; "Z") | fromdateiso8601) > now)
  | .alias as $host | .gpus[]
  | select(.memory_total_mib != null and .memory_used_mib != null)
  | [$host, .index, (.memory_total_mib - .memory_used_mib)] | @tsv'
```

JSON 写入 stdout，诊断写入 stderr。退出码 `0` 表示命令成功；`1` 表示参数、配置或运行时错误；`2` 表示单次 `scan` 中至少一台主机采集失败，此时 stdout 仍包含完整快照；`3` 表示 `status` 找不到活跃的采集进程。`status` 成功读取快照时返回 `0`，agent 可以逐台检查主机状态。

## 开发

```sh
go test ./...
go vet ./...
go build .
```

CI 在 Windows 和 Linux 上运行测试，并构建 amd64 和 arm64 单文件程序。以 `v` 开头的版本标签触发 GitHub Release，提供各平台程序及 SHA-256 校验文件。
