# Using the Online Judge

We use an online judging system based on the SSH protocol. The login method is identical to logging into the cluster.

> [!WARNING]
> Before you continue, please ensure that you can successfully [log in to the cluster](./index.md).

> [!WARNING]
> **Warning**
> Do not use a password to log in to the cluster. Password login acts as a honeypot, and any entered password will be recorded in plain text in the logs.

## Connect to the Judging System via SSH

The node name for the online judging system is `oj`. For example, you can log in to the system using:

```shell
ssh student+oj@clusters.zju.edu.cn -p 443
```

> [!TIP]
> We recommend modifying your SSH config for a smoother experience. For instance:
> 
> ```text title="~/.ssh/config"
> Host oj
>     User student+oj
>     HostName clusters.zju.edu.cn
>     Port 443
> ```
> 
> All the following examples assume you have already updated your SSH config file.

You should then see a prompt similar to this:

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
> Currently, the online judging system does not provide an interactive user interface. Once the information is output, your connection will be disconnected immediately. This is normal behavior.

## Upload Files

During the judging process, when you need to submit files, we use `sftp`. The path format is:

```text
<problem>/<submit_path>
```

For example, if you need to submit `world.cpp` for the problem `hello`, you can upload it using:

```shell
scp /path/to/local/file oj:hello/world.cpp
```

> [!NOTE]
> **If you cannot normally use scp to upload**
> 
> When OpenSSH version is `<8.7`, `scp` does not support the sftp protocol and cannot be used for uploading. Please use the `sftp` command instead (refer to its manual for specific usage).
> 
> When OpenSSH version is `>=8.7 && <9.0`, `scp` supports the sftp protocol but it's not enabled by default. Please add the `-s` option, like:
> 
> ```shell
> scp -s /path/to/local/file oj:hello/world.cpp
> ```
> 
> When OpenSSH version is `>9.0`, you can safely use `scp` directly.

> [!TIP]
> If you have not modified your `ssh config`, the command here translates to:
> 
> ```shell
> scp -P 443 /path/to/local/file student+oj@clusters.zju.edu.cn:hello/world.cpp
> ```

> [!NOTE]
> We support the standard `sftp` protocol, so you can connect using any `sftp` client you prefer.

## View My Status and Problem List

Using the command

```shell
ssh oj my
```

you can query the effective submission status and total score for current problems.

## Submit

Once you have prepared all the files, you can submit them. Submitting will not clear the uploaded files. The submit command is:

```shell
ssh oj submit <problem>
```

This will create an SSH session providing streaming logs. Closing this SSH connection will not affect the judging process. You can obtain the evaluation logs through [Get Submission Status](#get-submission-status), which are identical to the streaming logs.

## View Submission List

You can use

```shell
ssh oj list
```

to get your historical submissions. It displays up to 10 submissions per page. For example, you can get the second page by using:

```shell
ssh oj list 2
```

> [!TIP]
> `list` = `ls`

## Get Submission Status

In the logs after submission and in the submission list, you can find the `ID` of a submission. You can use:

```shell
ssh oj status <ID>
```

to get detailed information about this submission.

> [!TIP]
> `status` = `st`

> [!TIP]
> The `status` command performs a fuzzy match on the `ID`. You can query using any substring of it, and it will only return the latest matched submission.

> [!NOTE]
> 0-point submissions will not be included in the statistics.

## View Ranklist

Using the command

```shell
ssh oj rank
```

you can query the current ranklist.

> [!TIP]
> `rank` = `rk`

## Get Authentication Token

To retrieve a token for frontend authentication, use the command:

```shell
ssh oj token
```