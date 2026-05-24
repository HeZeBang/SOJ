# 使用在线评测系统

我们使用基于 SSH 协议的在线评测系统。登录方式与登录集群相同。

> [!WARNING]
> 在继续之前，请确保你能成功[登录集群](./index.md)。

> [!WARNING]
> **警告**
> 请勿使用密码登录集群。密码登录充当蜜罐，任何输入的密码将以明文形式记录在日志中。

## 通过 SSH 连接评测系统

在线评测系统的节点名称为 `oj`。例如，你可以使用以下命令登录系统：

```shell
ssh student+oj@clusters.zju.edu.cn -p 443
```

> [!TIP]
> 我们建议修改你的 SSH 配置以获得更流畅的体验。例如：
> 
> ```text title="~/.ssh/config"
> Host oj
>     User student+oj
>     HostName clusters.zju.edu.cn
>     Port 443
> ```
> 
> 以下所有示例均假设你已更新 SSH 配置文件。

你应该看到类似以下的提示：

```shell
$ ssh oj
************************************************************
*                                                          *
*  ███████╗     ██╗██╗   ██╗███████╗ ██████╗████████╗      *
*  ╚══███╔╝     ██║██║   ██║██╔════╝██╔════╝╚══██╔══╝      *
*    ███╔╝      ██║██║   ██║███████╗██║        ██║         *
*   ███╔╝  ██   ██║██║   ██║╚════██║██║        ██║         *
*  ███████╗╚█████╔╝╚██████╔╝███████║╚██████╗   ██║         *
*  ╚══════╝ ╚════╝  ╚═════╝ ╚══════╝ ╚═════╝   ╚═╝         *
*                                                          *
************************************************************
Mon, 05 Aug 2024 12:00:00 +0800
 Your IP: ***.***.***.***
 User: username+oj
%%% 我们仍未理解OpenCAEPoro为什么跑不起来多机
Welcome to SOJ Secure Online Judge , username
2024-08-26 19:39:20 CST
Use 'submit (sub) <problem_id>' to submit a problem
Use 'list (ls) [page]' to list your submissions
Use 'status (st) <submit_id>' to show a submission (fuzzy match)
Use 'rank (rk) ' to show rank list
Use 'my' to show your submission summary
Use 'token' to get token for frontend authentication
```

> [!NOTE]
> 目前，在线评测系统不提供交互式用户界面。信息输出后，你的连接将立即断开。这是正常行为。

## 上传文件

在评测过程中，当你需要提交文件时，我们使用 `sftp`。路径格式为：

```text
<题目>/<提交路径>
```

例如，如果你需要为题目 `hello` 提交 `world.cpp`，可以使用以下命令上传：

```shell
scp /path/to/local/file oj:hello/world.cpp
```

> [!NOTE]
> **如果你无法正常使用 scp 上传**
> 
> 当 OpenSSH 版本 `<8.7` 时，`scp` 不支持 sftp 协议，无法用于上传。请使用 `sftp` 命令代替（具体用法请参考其手册）。
> 
> 当 OpenSSH 版本 `>=8.7 && <9.0` 时，`scp` 支持 sftp 协议但默认未启用。请添加 `-s` 选项，例如：
> 
> ```shell
> scp -s /path/to/local/file oj:hello/world.cpp
> ```
> 
> 当 OpenSSH 版本 `>9.0` 时，你可以安全地直接使用 `scp`。

> [!TIP]
> 如果你未修改 `ssh config`，此处的命令等价于：
> 
> ```shell
> scp -P 443 /path/to/local/file student+oj@clusters.zju.edu.cn:hello/world.cpp
> ```

> [!NOTE]
> 我们支持标准 `sftp` 协议，你可以使用任何你喜欢的 `sftp` 客户端连接。

## 查看我的状态和题目列表

使用命令

```shell
ssh oj my
```

你可以查询当前题目的有效提交状态和总分。

## 提交

准备好所有文件后，你可以提交。提交不会清除已上传的文件。提交命令为：

```shell
ssh oj submit <题目>
```

提交会先进入 exclusive 评测队列，并立即获得提交 ID。如果当前没有其他提交运行，这个 SSH 会话会继续跟随实时评测日志。如果前面已有提交，SOJ 会显示排队位置后退出。关闭 SSH 连接或按 Ctrl+C 只会断开日志观察，不会取消评测。

如果要继续观察一个排队中或运行中的提交，使用：

```shell
ssh oj attach <ID>
```

> [!TIP]
> `attach` = `at`

## 查看提交列表

你可以使用

```shell
ssh oj list
```

获取你的历史提交。每页最多显示 10 条提交。例如，使用以下命令查看第二页：

```shell
ssh oj list 2
```

> [!TIP]
> `list` = `ls`

## 获取提交状态

在提交后的日志和提交列表中，你可以找到提交的 `ID`。使用：

```shell
ssh oj status <ID>
```

获取该提交的详细信息。

排队中的提交会显示 `queued`；运行中的提交可能显示 `running`，也可能显示更具体的评测阶段，例如 `prep_files` 或 `run_workflow-0`。

> [!TIP]
> `status` = `st`

> [!TIP]
> `status` 命令对 `ID` 进行模糊匹配。你可以使用其任意子串查询，它只会返回最新的匹配提交。

> [!NOTE]
> 0 分提交不会包含在统计中。

## 查看排行榜

使用命令

```shell
ssh oj rank
```

查询当前排行榜。

> [!TIP]
> `rank` = `rk`

## 获取认证令牌

要获取前端认证令牌，请使用命令：

```shell
ssh oj token
```
