# re — 通用批量命令执行器

按行读取 `.exec` 文件中的命令并逐条执行，支持对每条命令的返回做关键字校验，任一命令失败立即停止。docker、git、go、curl 等任意系统命令均可执行。

## 编译


```bash
# Windows
go build -o re.exe .

# Linux
go build -o re .

# 在 Windows 上交叉编译 Linux 版
$env:GOOS="linux"; $env:GOARCH="amd64"; $env:CGO_ENABLED="0"
go build -o re_linux_amd64 .
```

## 用法

```bash
re                  # 自动执行当前目录下唯一的 .exec 文件
re doccmd.exec      # 显式指定文件
re -h               # 查看帮助
```

当前目录没有 `.exec` 文件时报错；有多个时需明确指定文件名。

## .exec 文件格式

每行一条命令：

```text
[规则] 命令
```

- `[]` 留空：只执行，不校验返回
- `关键字`：返回中**必须包含**该关键字
- `!关键字`：返回中**不得包含**该关键字
- 多条规则用英文逗号分隔，如 `[dev-pro,!Error]`
- 空行和以 `#` 开头的行跳过；文件支持 UTF-8（含 BOM）

### 示例

```text
# 只执行不校验
[] echo hello

# 正向校验
[hello] echo hello world
[go1.] go version

# 负向校验
[!error] go build ./...

# 组合校验：必须包含 dev-pro，且不得出现 Error
[dev-pro,!Error] docker rm -f dev-pro

# 一次完整的交叉编译 + 构建镜像 + 重建容器
[]set GOOS=linux&& set GOARCH=amd64&& set CGO_ENABLED=0&& go build -ldflags="-s -w" -o dev-pro ./cmd/
[]docker build -t dev-pro:latest .
[dev-pro]docker rm -f dev-pro
[]docker run -d --name dev-pro --network dev-net -p 8630:8630 -v D:/Project/test/dev-pro/dm.yml:/home/bin/dm.yml --restart always dev-pro:latest
```

## 校验规则

- **子串包含**，不是完全相等：返回中任意位置出现关键字即通过
- **区分大小写**：Docker 错误一般是 `Error`，小写 `error` 匹配不到
- **退出码优先**：命令退出码非 0 直接判 failed 并停止后续行，不再做关键字校验
- stdout 与 stderr 合并后一起校验

## 输出格式

每条命令一个段落，以分隔线隔开：

```text
-----------------------------------------------------------------
[行数]：5
[命令]：docker rm -f dev-pro
[预期]: 包含: "dev-pro"
[返回]：dev-pro
[结果]：success
-----------------------------------------------------------------
```

- `[返回]`：命令输出，首尾空白会被裁剪；无输出显示 `无`；多行输出续行缩进
- `[结果]`：`success` / `failed`
- `[错误原因]`：仅失败时出现（退出码错误或关键字不匹配）
- 命令执行期间在交互终端显示 `执行中......` 循环动画；输出重定向到文件/管道时自动关闭

进程退出码透传失败命令的真实退出码，便于脚本和 CI 判断。

## 平台注意事项

### Windows

- 命令通过临时 `.bat` 交给 cmd 执行，引号行为与手工在 cmd 窗口输入一致
- **cmd 不识别单引号**：`-ldflags='-s -w'` 会失败，请统一用双引号 `-ldflags="-s -w"`（双引号写法 Windows/Linux 通用）
- 临时批处理会自动执行 `chcp 65001`，把命令输出切到 UTF-8，避免中文乱码
- **`setx` 与 `set` 的区别**：
  - `setx FOO bar` 写入注册表（持久化），但**对当前进程及本次运行的后续命令不生效**，需重开终端
  - `set FOO=bar` 仅当前进程生效，立即有效；但每行命令是独立进程，需与使用命令写在同一行并用 `&&` 连接
  - `&&` 前不要留空格，否则变量值尾部会带空格

### Linux

- 命令通过 `sh -c` 执行，单引号、双引号均可按 shell 语义使用
- `.exec` 文件建议使用 LF 换行（CRLF 也可正常解析）
