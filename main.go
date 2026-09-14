package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// maxLineSize 放宽单行长度上限，避免超长 docker run 等命令触发 Scanner 默认 64KiB 限制
const maxLineSize = 1 << 20 // 1 MiB

// sepLine 每条命令执行前打印的分隔线，让多命令输出的段落边界清晰
var sepLine = strings.Repeat("-", 65)

// 行格式: [规则] 命令；[] 内留空表示不校验输出
// 多条规则用英文逗号分隔；以 ! 开头的关键字表示“输出不得包含”，其余表示“输出必须包含”
// 例:
//
//	[hello] echo hello
//	[] echo 不校验这一行的输出
//	[!error] some-command       输出中不得出现 error
//	[ready,!error] start-server 输出必须包含 ready，且不得包含 error
var lineRe = regexp.MustCompile(`^\s*\[([^\]]*)\]\s*(.+)$`)

func main() {
	args := os.Args[1:]
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		printUsage(os.Stderr)
		os.Exit(2)
	}
	if len(args) > 1 {
		fmt.Fprintln(os.Stderr, "❌ 参数过多，最多指定一个 .exec 文件")
		printUsage(os.Stderr)
		os.Exit(2)
	}

	// 未指定文件时在当前工作目录自动查找 .exec 文件
	explicit := ""
	if len(args) == 1 {
		explicit = args[0]
	}
	filePath, err := resolveRunFile(explicit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}

	if err := runFile(filePath); err != nil {
		// 每条命令的详细结果已在 runFile 内打印，这里只提示终止
		fmt.Fprintln(os.Stderr, "⛔ 已停止执行后续命令。")

		// 透传子命令的真实退出码，便于脚本 / CI 判断失败类型
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		os.Exit(1)
	}

	fmt.Println("✅ success")
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "用法: re [文件名.exec]")
	fmt.Fprintln(w, "示例: re doccmd.exec")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "文件每行格式: [规则] 命令（[] 内留空表示不校验）")
	fmt.Fprintln(w, "规则用逗号分隔: 普通关键字=返回必须包含；以 ! 开头=返回不得包含，如 [ready,!error]")
	fmt.Fprintln(w, "示例:")
	fmt.Fprintln(w, "  [] echo hello                              # 只执行，不校验返回")
	fmt.Fprintln(w, "  [hello] echo hello world                  # 返回必须包含 hello")
	fmt.Fprintln(w, "  [!error] go build ./...                   # 返回不得包含 error")
	fmt.Fprintln(w, "  [go1.] go version                         # 校验 go 版本号")
	fmt.Fprintln(w, "  [dev-pro,!Error] docker rm -f dev-pro     # 必须包含 dev-pro，且不得出现 Error")
	fmt.Fprintln(w, "  [] go build -ldflags=\"-s -w\" -o app .      # 编译；双引号 Windows/Linux 通用")
	fmt.Fprintln(w, "  [200] curl -s -o /dev/null -w \"%{http_code}\" URL  # 校验 HTTP 状态码（Linux 写法）")
	fmt.Fprintln(w, "  [] git pull origin main                   # git 等任意命令均可")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "说明: # 之后为注释，可省略；匹配区分大小写，为子串包含（非完全相等）")
	fmt.Fprintln(w, "      命令退出码非 0 直接判 failed 并停止执行后续行")
}

// resolveRunFile 确定要执行的 .exec 文件：
// 显式指定时原样使用；未指定时只在当前工作目录（不递归）查找 .exec 文件，
// 0 个报错、多个时列出候选并要求明确指定、恰好 1 个时返回它
func resolveRunFile(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		return "", fmt.Errorf("读取当前目录失败: %w", err)
	}

	var found []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(strings.ToLower(e.Name()), ".exec") {
			found = append(found, e.Name())
		}
	}

	switch len(found) {
	case 0:
		return "", errors.New("当前目录下没有找到 .exec 文件，请显式指定: runner <文件名.exec>")
	case 1:
		// 明确回显实际生效的文件，避免“到底执行了哪个”的歧义
		fmt.Printf("📄 未指定文件，自动执行当前目录下唯一的: %s\n\n", found[0])
		return found[0], nil
	default:
		return "", fmt.Errorf("当前目录下找到 %d 个 .exec 文件，请明确指定其中一个:\n  %s",
			len(found), strings.Join(found, "\n  "))
	}
}

// runFile 逐行读取并执行命令文件，遇到第一条失败命令即返回
func runFile(filePath string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("无法打开文件: %w", err)
	}
	defer file.Close()

	// 用 Reader 包一层以跳过 UTF-8 BOM（Windows 记事本默认会加 BOM，否则第一行解析失败）
	reader := bufio.NewReader(file)
	if head, err := reader.Peek(3); err == nil && bytes.Equal(head, []byte{0xEF, 0xBB, 0xBF}) {
		_, _ = reader.Discard(3)
	}

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineSize)

	lineNum := 0
	started := false
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())

		// 跳过空行和纯注释
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !started {
			fmt.Println(sepLine)
			started = true
		}

		mustContain, mustNotContain, command, ok := parseLine(line)
		if !ok {
			printBlockStart(lineNum, line, nil, nil)
			printBlockEnd("", false, "格式错误，应为 [规则] 命令")
			return errors.New("命令文件存在格式错误的行")
		}

		printBlockStart(lineNum, command, mustContain, mustNotContain)

		output, err := runCommand(command)
		if err != nil {
			printBlockEnd(output, false, fmt.Sprintf("命令执行失败: %v", err))
			return fmt.Errorf("命令执行失败: %w", err)
		}

		// 规则列表为空（[]）时不校验输出
		for _, kw := range mustContain {
			if !strings.Contains(output, kw) {
				printBlockEnd(output, false, fmt.Sprintf("返回不匹配，预期包含 %q", kw))
				return errors.New("返回校验未通过")
			}
		}
		for _, kw := range mustNotContain {
			if strings.Contains(output, kw) {
				printBlockEnd(output, false, fmt.Sprintf("返回不匹配，预期不包含 %q", kw))
				return errors.New("返回校验未通过")
			}
		}

		printBlockEnd(output, true, "")
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("读取文件出错: %w", err)
	}
	return nil
}

// printBlockStart 打印结果块的开头：行号、原命令、校验规则。
// 分隔线由调用方在首条命令前打印一次，之后由 printBlockEnd 收尾分隔
func printBlockStart(lineNum int, command string, mustContain, mustNotContain []string) {
	fmt.Printf("[行数]：%d\n", lineNum)
	fmt.Printf("[命令]：%s\n", command)
	for _, kw := range mustContain {
		fmt.Printf("[预期]: 包含: %q\n", kw)
	}
	for _, kw := range mustNotContain {
		fmt.Printf("[预期]: 不包含: %q\n", kw)
	}
}

// printBlockEnd 打印结果块的收尾：命令返回（无则“无”）、结果、错误原因、分隔线：
//
//	[返回]：<命令输出，无输出时为“无”>
//	[结果]：success / failed
//	[错误原因]：仅失败时打印
func printBlockEnd(output string, success bool, reason string) {
	// 裁掉首尾空白/换行：部分命令（如 setx）输出被重定向时开头自带空行
	output = strings.TrimSpace(output)
	if output == "" {
		fmt.Println("[返回]：无")
	} else {
		fmt.Println("[返回]：" + strings.ReplaceAll(output, "\n", "\n   "))
	}

	if success {
		fmt.Println("[结果]：success")
	} else {
		fmt.Println("[结果]：failed")
		if reason != "" {
			fmt.Printf("[错误原因]：%s\n", reason)
		}
	}
	fmt.Println(sepLine)
}

// parseLine 解析 "[规则] command" 格式的一行。
// 规则以英文逗号分隔：普通关键字加入 mustContain（输出必须包含），
// 以 ! 开头的关键字加入 mustNotContain（输出不得包含）。
func parseLine(line string) (mustContain, mustNotContain []string, command string, ok bool) {
	matches := lineRe.FindStringSubmatch(line)
	if matches == nil {
		return nil, nil, "", false
	}

	for _, part := range strings.Split(matches[1], ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.HasPrefix(part, "!") {
			if kw := strings.TrimSpace(part[1:]); kw != "" {
				mustNotContain = append(mustNotContain, kw)
			}
		} else {
			mustContain = append(mustContain, part)
		}
	}

	return mustContain, mustNotContain, strings.TrimSpace(matches[2]), true
}

// runCommand 通过系统 shell 执行单行命令，输出同时透传到终端并捕获用于关键字校验
func runCommand(command string) (string, error) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		// 不能直接 exec.Command("cmd", "/c", command)：Go 的 os/exec 会先按 C 运行时规则
		// 重组命令行，把命令串中的双引号转义成 \"，而 cmd.exe 不识别这种转义，
		// 会导致 -ldflags="-s -w" 这类带引号参数变形（手工在 cmd 执行却正常）。
		// 改为写入临时批处理文件：批处理由 cmd 按交互输入的原样解析，引号行为与手工执行一致。
		batPath, err := writeTempBatch(command)
		if err != nil {
			return "", err
		}
		defer os.Remove(batPath)
		cmd = exec.Command("cmd", "/c", batPath)
	} else {
		cmd = exec.Command("sh", "-c", command)
	}

	// 输出统一捕获后在结果块的 [返回] 中展示（同一段输出只出现一次）。
	// stdout/stderr 由 exec 包在两个独立 goroutine 并发写入，捕获缓冲必须加锁
	var captured syncBuffer
	cmd.Stdout = &captured
	cmd.Stderr = &captured
	cmd.Stdin = os.Stdin

	// 执行期间在终端显示“执行中.”循环点动画，命令结束后清除该行
	stopSpinner := startSpinner()
	err := cmd.Run()
	stopSpinner()

	return captured.String(), err
}

// startSpinner 在交互式终端启动“执行中.”点号循环动画，返回停止函数。
// 输出被重定向（非字符设备）时不启动，避免把 \r 动画写进日志/管道
func startSpinner() func() {
	if fi, err := os.Stderr.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return func() {}
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		frames := []string{".", "..", "...", "....", ".....", "......"}
		ticker := time.NewTicker(300 * time.Millisecond)
		defer ticker.Stop()
		i := 0
		for {
			select {
			case <-done:
				// 清除动画行，让后续 [返回] 从行首开始覆盖
				fmt.Fprint(os.Stderr, "\r"+strings.Repeat(" ", 30)+"\r")
				return
			case <-ticker.C:
				fmt.Fprintf(os.Stderr, "\r执行中%-6s", frames[i])
				i = (i + 1) % len(frames)
			}
		}
	}()

	stop := func() {
		close(done)
		wg.Wait()
	}
	return stop
}

// writeTempBatch 把单行命令写入临时批处理文件，返回文件路径。
// @echo off 避免批处理把命令本身回显到输出；chcp 65001 把代码页切到 UTF-8，
// 否则 setx/docker 等命令的中文输出按 GBK 编码，在 UTF-8 终端里会乱码；
// CRLF 是批处理的标准行尾
func writeTempBatch(command string) (string, error) {
	f, err := os.CreateTemp("", "re-exec-*.bat")
	if err != nil {
		return "", fmt.Errorf("创建临时批处理失败: %w", err)
	}
	defer f.Close()

	content := "@echo off\r\nchcp 65001 >nul\r\n" + command + "\r\n"
	if _, err := f.WriteString(content); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("写入临时批处理失败: %w", err)
	}
	return f.Name(), nil
}

// syncBuffer 是并发安全的输出捕获缓冲
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
