package machine

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"

	"FlashDock/define"
	"FlashDock/utils"

	"golang.org/x/crypto/ssh"
)

const (
	fileBackendSFTP = "sftp"
	fileBackendSCP  = "scp"
)

func normalizeFileProtocol(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "sftp":
		return "sftp"
	case "scp":
		return "scp"
	default:
		return "auto"
	}
}

// EnsureFileBackend 按机器配置初始化文件后端（SFTP 或 SCP 回退）
func (a *ShellAuxManager) EnsureFileBackend() error {
	a.mu.Lock()
	client := a.client
	backend := a.fileBackend
	a.mu.Unlock()
	if client == nil || !client.IsConnected() || client.remoteMachine == nil {
		return fmt.Errorf("辅助连接未建立")
	}
	if backend != "" {
		return nil
	}

	cfg := resolveMachine(a.machineName)
	proto := "auto"
	sudo := false
	if cfg != nil {
		proto = normalizeFileProtocol(cfg.SftpFileProtocol)
		sudo = cfg.SftpSudo
	}
	if sudo && proto == "scp" {
		return fmt.Errorf("Sudo SFTP 与「仅 SCP」互斥，请改用仅 SFTP 或自动")
	}

	openBrowseSFTP := func() error {
		rm := client.remoteMachine
		if rm.SFTPClient != nil {
			return nil
		}
		if !sudo {
			return rm.EnsureSFTP()
		}
		password := ""
		if cfg != nil {
			if sensitive, err := cfg.GetSensitiveData(); err == nil && sensitive != nil {
				password = sensitive.Password
			}
		}
		sftpClient, err := OpenSFTPClient(rm.SSHClient, true, password)
		if err != nil {
			return err
		}
		rm.SetSFTPClient(sftpClient)
		return nil
	}

	switch proto {
	case "scp":
		a.mu.Lock()
		a.fileBackend = fileBackendSCP
		a.mu.Unlock()
		return nil
	case "sftp":
		if err := openBrowseSFTP(); err != nil {
			return err
		}
		a.initTransferPoolLocked(client, sudo, cfg)
		a.mu.Lock()
		a.fileBackend = fileBackendSFTP
		a.mu.Unlock()
		return nil
	default:
		if err := openBrowseSFTP(); err != nil {
			if sudo {
				return err
			}
			a.mu.Lock()
			a.fileBackend = fileBackendSCP
			a.mu.Unlock()
			return nil
		}
		a.initTransferPoolLocked(client, sudo, cfg)
		a.mu.Lock()
		a.fileBackend = fileBackendSFTP
		a.mu.Unlock()
		return nil
	}
}

func (a *ShellAuxManager) initTransferPoolLocked(client *SSHClient, sudo bool, cfg *define.Machine) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.transferPool != nil || client == nil || client.remoteMachine == nil || client.remoteMachine.SSHClient == nil {
		return
	}
	password := ""
	if sudo && cfg != nil {
		if sensitive, err := cfg.GetSensitiveData(); err == nil && sensitive != nil {
			password = sensitive.Password
		}
	}
	a.transferPool = newSFTPTransferPool(client.remoteMachine.SSHClient, sudo, password)
	a.sftpSudo = sudo
}

func (a *ShellAuxManager) isSCPBackend() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.fileBackend == fileBackendSCP
}

// FileBackendName 当前文件后端名称（sftp/scp/空）
func (a *ShellAuxManager) FileBackendName() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.fileBackend
}

func (a *ShellAuxManager) sshClient() (*ssh.Client, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client == nil || a.client.remoteMachine == nil || a.client.remoteMachine.SSHClient == nil {
		return nil, fmt.Errorf("SSH 未连接")
	}
	return a.client.remoteMachine.SSHClient, nil
}

func shellQuotePath(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// ShellQuotePath 导出给 app 层构造 shell 命令（如 cd）
func ShellQuotePath(s string) string {
	return shellQuotePath(s)
}

func (a *ShellAuxManager) listDirSCP(dirPath string, showHidden bool) ([]define.SftpEntry, error) {
	if dirPath == "" {
		dirPath = "."
	}
	cmd := fmt.Sprintf(
		`cd %s || exit 1; LC_ALL=C ls -la -- 2>/dev/null || LC_ALL=C ls -la`,
		shellQuotePath(dirPath),
	)
	out, err := a.Exec(cmd)
	if err != nil {
		return nil, fmt.Errorf("SCP 列目录失败: %w", err)
	}
	return parseLsLa(dirPath, out, showHidden), nil
}

func parseLsLa(dirPath, out string, showHidden bool) []define.SftpEntry {
	entries := make([]define.SftpEntry, 0)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "total ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 9 {
			continue
		}
		mode := fields[0]
		if len(mode) < 10 {
			continue
		}
		name := strings.Join(fields[8:], " ")
		linkTarget := ""
		if i := strings.Index(name, " -> "); i >= 0 {
			linkTarget = name[i+4:]
			name = name[:i]
		}
		if name == "." || name == ".." {
			continue
		}
		if !showHidden && strings.HasPrefix(name, ".") {
			continue
		}
		size, _ := strconv.ParseInt(fields[4], 10, 64)
		isDir := mode[0] == 'd'
		typ := "文件"
		switch mode[0] {
		case 'd':
			typ = "目录"
		case 'l':
			typ = "链接"
		}
		entries = append(entries, define.SftpEntry{
			Name:       name,
			Path:       path.Join(dirPath, name),
			IsDir:      isDir,
			Size:       size,
			Mode:       mode,
			Type:       typ,
			LinkTarget: linkTarget,
		})
	}
	return entries
}

func (a *ShellAuxManager) mkdirSCP(remotePath string) error {
	_, err := a.Exec("mkdir -p -- " + shellQuotePath(remotePath))
	return err
}

func (a *ShellAuxManager) renameSCP(oldPath, newPath string) error {
	_, err := a.Exec(fmt.Sprintf("mv -- %s %s", shellQuotePath(oldPath), shellQuotePath(newPath)))
	return err
}

func (a *ShellAuxManager) chmodSCP(remotePath string, mode uint32) error {
	_, err := a.Exec(fmt.Sprintf("chmod %04o -- %s", mode&0o7777, shellQuotePath(remotePath)))
	return err
}

func (a *ShellAuxManager) removeSCP(remotePath string) error {
	_, err := a.Exec("rm -rf -- " + shellQuotePath(remotePath))
	return err
}

func (a *ShellAuxManager) statSCP(remotePath string) (*define.SftpEntry, error) {
	cmd := fmt.Sprintf(
		`p=%s; if [ ! -e "$p" ] && [ ! -L "$p" ]; then echo ENOENT; exit 2; fi; `+
			`if [ -L "$p" ]; then t=l; elif [ -d "$p" ]; then t=d; else t=f; fi; `+
			`mode=$(ls -ld -- "$p" 2>/dev/null | awk '{print $1}'); `+
			`size=$(stat -c %%s -- "$p" 2>/dev/null || stat -f %%z -- "$p" 2>/dev/null || echo 0); `+
			`printf "%%s|%%s|%%s\\n" "$t" "${mode:-?}" "${size:-0}"`,
		shellQuotePath(remotePath),
	)
	out, err := a.Exec(cmd)
	if err != nil {
		return nil, err
	}
	line := strings.TrimSpace(out)
	if strings.Contains(line, "ENOENT") {
		return nil, fmt.Errorf("路径不存在")
	}
	parts := strings.Split(line, "|")
	if len(parts) < 3 {
		return nil, fmt.Errorf("stat 解析失败")
	}
	size, _ := strconv.ParseInt(parts[2], 10, 64)
	entry := &define.SftpEntry{
		Name:  path.Base(remotePath),
		Path:  remotePath,
		IsDir: parts[0] == "d",
		Size:  size,
		Mode:  parts[1],
		Type:  "文件",
	}
	switch parts[0] {
	case "d":
		entry.Type = "目录"
	case "l":
		entry.Type = "链接"
	}
	return entry, nil
}

func (a *ShellAuxManager) copyRemoteSCP(src, dst string) error {
	_, err := a.Exec(fmt.Sprintf("cp -a -- %s %s", shellQuotePath(src), shellQuotePath(dst)))
	return err
}

// UploadFileSCP 通过 scp -t 协议上传单文件
func (a *ShellAuxManager) UploadFileSCP(ctx context.Context, localPath, remotePath string, onProgress TransferProgressFunc) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	client, err := a.sshClient()
	if err != nil {
		return err
	}
	src, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return err
	}
	total := info.Size()
	remoteDir := path.Dir(remotePath)
	partRemote := utils.RemoteUploadPartPath(remotePath)
	baseName := path.Base(partRemote)
	if remoteDir != "" && remoteDir != "." && remoteDir != "/" {
		_ = a.mkdirSCP(remoteDir)
	}
	_ = a.removeSCP(partRemote)

	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	stdin, err := session.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		return err
	}

	dest := remoteDir
	if dest == "" || dest == "." {
		dest = "."
	}
	if err := session.Start("scp -t -- " + shellQuotePath(dest)); err != nil {
		return fmt.Errorf("启动 scp 失败: %w", err)
	}
	if err := scpReadAck(stdout); err != nil {
		return err
	}
	mode := info.Mode().Perm()
	if mode == 0 {
		mode = 0o644
	}
	header := fmt.Sprintf("C%04o %d %s\n", mode, total, baseName)
	if _, err := io.WriteString(stdin, header); err != nil {
		return err
	}
	if err := scpReadAck(stdout); err != nil {
		return err
	}
	writer := &countingWriter{ctx: ctx, w: stdin, total: total, onProgress: onProgress}
	if _, err := utils.CopyBuffer(writer, src); err != nil {
		return fmt.Errorf("SCP 上传失败: %w", err)
	}
	if _, err := stdin.Write([]byte{0}); err != nil {
		return err
	}
	if err := scpReadAck(stdout); err != nil {
		return err
	}
	_ = stdin.Close()
	if err := session.Wait(); err != nil {
		_ = a.removeSCP(partRemote)
		return fmt.Errorf("SCP 上传结束失败: %w", err)
	}
	if out, err := a.Exec(utils.ShellCommitRemoteUpload(partRemote, remotePath)); err != nil {
		_ = a.removeSCP(partRemote)
		return fmt.Errorf("SCP 原子替换失败: %w (%s)", err, strings.TrimSpace(out))
	}
	if onProgress != nil {
		onProgress(total, total, writer.speedBPS)
	}
	return nil
}

// DownloadFileSCP 通过 scp -f 协议下载单文件
func (a *ShellAuxManager) DownloadFileSCP(ctx context.Context, remotePath, localPath string, onProgress TransferProgressFunc) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	client, err := a.sshClient()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepathDir(localPath), 0o755); err != nil {
		return err
	}
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	stdin, err := session.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		return err
	}

	if err := session.Start("scp -f -- " + shellQuotePath(remotePath)); err != nil {
		return fmt.Errorf("启动 scp 失败: %w", err)
	}
	if _, err := stdin.Write([]byte{0}); err != nil {
		return err
	}
	ctrl, err := scpReadControlLine(stdout)
	if err != nil {
		return err
	}
	if ctrl.kind != "file" {
		return fmt.Errorf("SCP 期望文件，得到 %s", ctrl.kind)
	}
	if _, err := stdin.Write([]byte{0}); err != nil {
		return err
	}
	dst, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer dst.Close()
	reader := &countingReader{ctx: ctx, r: io.LimitReader(stdout, ctrl.size), total: ctrl.size, onProgress: onProgress}
	if _, err := utils.CopyBuffer(dst, reader); err != nil {
		return fmt.Errorf("SCP 下载失败: %w", err)
	}
	var ack [1]byte
	_, _ = stdout.Read(ack[:])
	if _, err := stdin.Write([]byte{0}); err != nil {
		return err
	}
	_ = stdin.Close()
	_ = session.Wait()
	if onProgress != nil {
		onProgress(ctrl.size, ctrl.size, reader.speedBPS)
	}
	_ = os.Chmod(localPath, os.FileMode(ctrl.mode)&0o777)
	return nil
}

type scpControl struct {
	kind string
	mode int
	size int64
	name string
}

func scpReadAck(r io.Reader) error {
	var b [1]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return fmt.Errorf("读取 SCP ACK 失败: %w", err)
	}
	if b[0] == 0 {
		return nil
	}
	msg, _ := readUntilNewline(r)
	if b[0] == 1 || b[0] == 2 {
		return fmt.Errorf("SCP 远端错误: %s", strings.TrimSpace(msg))
	}
	return fmt.Errorf("意外的 SCP 状态: 0x%x", b[0])
}

func scpReadControlLine(r io.Reader) (*scpControl, error) {
	line, err := readUntilNewline(r)
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "E" {
		return &scpControl{kind: "end"}, nil
	}
	if len(line) == 0 {
		return nil, fmt.Errorf("空 SCP 控制行")
	}
	if line[0] == 'T' {
		return scpReadControlLine(r)
	}
	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 3 || len(parts[0]) < 2 || (parts[0][0] != 'C' && parts[0][0] != 'D') {
		return nil, fmt.Errorf("无效 SCP 控制行: %s", line)
	}
	mode, _ := strconv.ParseInt(parts[0][1:], 8, 32)
	size, _ := strconv.ParseInt(parts[1], 10, 64)
	kind := "file"
	if parts[0][0] == 'D' {
		kind = "directory"
	}
	return &scpControl{kind: kind, mode: int(mode), size: size, name: parts[2]}, nil
}

func readUntilNewline(r io.Reader) (string, error) {
	var buf strings.Builder
	tmp := make([]byte, 1)
	for {
		_, err := r.Read(tmp)
		if err != nil {
			if buf.Len() > 0 {
				return buf.String(), nil
			}
			return "", err
		}
		buf.WriteByte(tmp[0])
		if tmp[0] == '\n' {
			return buf.String(), nil
		}
		if buf.Len() > 8192 {
			return "", fmt.Errorf("SCP 控制行过长")
		}
	}
}

func filepathDir(p string) string {
	i := strings.LastIndexAny(p, `/\`)
	if i < 0 {
		return "."
	}
	if i == 0 {
		return p[:1]
	}
	return p[:i]
}
