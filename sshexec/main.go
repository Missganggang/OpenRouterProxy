// sshexec 是一个极简的 SSH 执行/上传工具。
//
// 为什么需要它：本机 Windows 自带的 OpenSSH 客户端不支持用参数传密码
// （`-pw` 是 PuTTY 的语法，ssh.exe 会当成非法端口报错），
// 而 WSL 的网络模式（Mirrored）初始化失败，WSL 内无法访问外网，
// 因此既有的 sshpass 方案也用不了。Go 的网络栈在本机工作正常，
// 于是用 x/crypto/ssh 直接实现同样的能力。
//
// 用法：
//
//	sshexec exec   -host 1.2.3.4 -user root -pass **** -script ./install.sh
//	sshexec run    -host 1.2.3.4 -user root -pass **** -cmd "systemctl status openroute"
//	sshexec upload -host 1.2.3.4 -user root -pass **** -local ./a.tar.gz -remote /tmp/a.tar.gz
//	sshexec download -host 1.2.3.4 -user root -pass **** -remote /tmp/log -local ./log
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	action := os.Args[1]

	fs := flag.NewFlagSet(action, flag.ExitOnError)
	host := fs.String("host", "", "服务器地址")
	port := fs.Int("port", 22, "SSH 端口")
	user := fs.String("user", "root", "登录用户")
	pass := fs.String("pass", "", "登录密码")
	keyPath := fs.String("key", "", "私钥路径（优先于密码）")
	timeout := fs.Duration("timeout", 10*time.Minute, "整体超时")

	script := fs.String("script", "", "exec: 要上传并执行的本地脚本")
	cmd := fs.String("cmd", "", "run: 要执行的命令")
	local := fs.String("local", "", "upload/download: 本机文件路径")
	remote := fs.String("remote", "", "upload/download: 远端文件路径")
	showOut := fs.Bool("v", true, "是否回显远端输出")

	_ = fs.Parse(os.Args[2:])

	if *host == "" {
		fail("必须提供 -host")
	}

	client, err := dial(*host, *port, *user, *pass, *keyPath, *timeout)
	if err != nil {
		fail(fmt.Sprintf("连接失败: %v", err))
	}
	defer client.Close()

	switch action {
	case "run":
		if *cmd == "" {
			fail("run 需要 -cmd")
		}
		os.Exit(runCommand(client, *cmd, *showOut))

	case "exec":
		// 把本地脚本传到远端再执行。
		//
		// 不直接把脚本内容塞进 `bash -s` 的 stdin：那样在 Windows 上
		// 容易因为管道编码把中文注释弄坏（本次部署已经踩过一次坑）。
		// 走 sftp 落盘再 bash 执行，行尾与编码都保持原样。
		if *script == "" {
			fail("exec 需要 -script")
		}
		data, err := os.ReadFile(*script)
		if err != nil {
			fail(fmt.Sprintf("读取脚本失败: %v", err))
		}
		// 统一成 LF，避免 CRLF 让远端报 "$'\r': command not found"
		content := strings.ReplaceAll(string(data), "\r\n", "\n")

		remotePath := fmt.Sprintf("/tmp/sshexec-%d.sh", time.Now().Unix())
		if err := uploadBytes(client, remotePath, []byte(content), 0o700); err != nil {
			fail(fmt.Sprintf("上传脚本失败: %v", err))
		}
		code := runCommand(client, "bash "+remotePath, *showOut)
		// 清理临时脚本（失败也不影响退出码）
		_, _ = runCommandSilent(client, "rm -f "+remotePath)
		os.Exit(code)

	case "upload":
		if *local == "" || *remote == "" {
			fail("upload 需要 -local 与 -remote")
		}
		if err := uploadFile(client, *local, *remote); err != nil {
			fail(fmt.Sprintf("上传失败: %v", err))
		}
		fmt.Printf("已上传 %s -> %s\n", *local, *remote)

	case "download":
		if *local == "" || *remote == "" {
			fail("download 需要 -local 与 -remote")
		}
		if err := downloadFile(client, *remote, *local); err != nil {
			fail(fmt.Sprintf("下载失败: %v", err))
		}
		fmt.Printf("已下载 %s -> %s\n", *remote, *local)

	default:
		usage()
		os.Exit(2)
	}
}

// dial 建立 SSH 连接。优先使用私钥，其次使用密码。
func dial(host string, port int, user, pass, keyPath string, timeout time.Duration) (*ssh.Client, error) {
	var auths []ssh.AuthMethod

	if keyPath != "" {
		key, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, fmt.Errorf("读取私钥失败: %w", err)
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			return nil, fmt.Errorf("解析私钥失败: %w", err)
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}
	if pass != "" {
		auths = append(auths, ssh.Password(pass))
		// 兼容需要 keyboard-interactive 的服务器
		auths = append(auths, ssh.KeyboardInteractive(
			func(_, _ string, questions []string, _ []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range questions {
					answers[i] = pass
				}
				return answers, nil
			}))
	}

	if len(auths) == 0 {
		return nil, fmt.Errorf("必须提供 -pass 或 -key")
	}

	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            auths,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // 部署脚本场景：不做主机密钥校验
		Timeout:         30 * time.Second,
	}

	addr := fmt.Sprintf("%s:%d", host, port)
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return nil, err
	}
	_ = timeout
	return client, nil
}

// runCommand 在远端执行命令，把 stdout/stderr 实时回显，返回远端退出码。
func runCommand(client *ssh.Client, cmd string, show bool) int {
	session, err := client.NewSession()
	if err != nil {
		fmt.Fprintf(os.Stderr, "创建会话失败: %v\n", err)
		return 1
	}
	defer session.Close()

	var out io.Writer = os.Stdout
	if !show {
		out = io.Discard
	}

	session.Stdout = out
	session.Stderr = os.Stderr

	err = session.Run(cmd)
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(*ssh.ExitError); ok {
		return exitErr.ExitStatus()
	}
	fmt.Fprintf(os.Stderr, "执行命令失败: %v\n", err)
	return 1
}

// runCommandSilent 执行命令但不输出任何内容。
func runCommandSilent(client *ssh.Client, cmd string) (int, error) {
	session, err := client.NewSession()
	if err != nil {
		return 1, err
	}
	defer session.Close()
	session.Stdout = io.Discard
	session.Stderr = io.Discard
	if err := session.Run(cmd); err != nil {
		if exitErr, ok := err.(*ssh.ExitError); ok {
			return exitErr.ExitStatus(), nil
		}
		return 1, err
	}
	return 0, nil
}

// uploadFile 用 SFTP 上传本地文件。
func uploadFile(client *ssh.Client, local, remote string) error {
	src, err := os.Open(local)
	if err != nil {
		return fmt.Errorf("打开本地文件失败: %w", err)
	}
	defer src.Close()

	sc, err := sftp.NewClient(client)
	if err != nil {
		return fmt.Errorf("创建 SFTP 客户端失败: %w", err)
	}
	defer sc.Close()

	// 确保远端目录存在
	if dir := path.Dir(remote); dir != "" && dir != "/" {
		_ = sc.MkdirAll(dir)
	}

	dst, err := sc.Create(remote)
	if err != nil {
		return fmt.Errorf("创建远端文件失败: %w", err)
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("写入远端文件失败: %w", err)
	}
	return nil
}

// uploadBytes 用 SFTP 上传内存内容并设置权限。
func uploadBytes(client *ssh.Client, remote string, data []byte, mode os.FileMode) error {
	sc, err := sftp.NewClient(client)
	if err != nil {
		return fmt.Errorf("创建 SFTP 客户端失败: %w", err)
	}
	defer sc.Close()

	if dir := path.Dir(remote); dir != "" && dir != "/" {
		_ = sc.MkdirAll(dir)
	}

	dst, err := sc.OpenFile(remote, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return fmt.Errorf("创建远端文件失败: %w", err)
	}
	defer dst.Close()

	if _, err := dst.Write(data); err != nil {
		return fmt.Errorf("写入远端文件失败: %w", err)
	}
	if err := sc.Chmod(remote, mode); err != nil {
		// 权限设置失败不算致命，继续
		fmt.Fprintf(os.Stderr, "警告: 设置权限失败: %v\n", err)
	}
	return nil
}

// downloadFile 用 SFTP 下载远端文件到本地。
func downloadFile(client *ssh.Client, remote, local string) error {
	sc, err := sftp.NewClient(client)
	if err != nil {
		return fmt.Errorf("创建 SFTP 客户端失败: %w", err)
	}
	defer sc.Close()

	src, err := sc.Open(remote)
	if err != nil {
		return fmt.Errorf("打开远端文件失败: %w", err)
	}
	defer src.Close()

	dst, err := os.Create(local)
	if err != nil {
		return fmt.Errorf("创建本地文件失败: %w", err)
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("写入本地文件失败: %w", err)
	}
	return nil
}

func usage() {
	fmt.Fprint(os.Stderr, `sshexec —— 最小化的 SSH 执行/上传工具

用法：
  sshexec run      -host H -user U -pass P -cmd "命令"
  sshexec exec     -host H -user U -pass P -script ./install.sh
  sshexec upload   -host H -user U -pass P -local ./a -remote /tmp/a
  sshexec download -host H -user U -pass P -remote /tmp/a -local ./a

公共参数：-port（默认 22）、-key（私钥路径，优先于 -pass）、-v（是否回显输出）
`)
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "错误: "+msg)
	os.Exit(1)
}
