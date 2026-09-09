package machine

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"FlashDock/utils"

	"github.com/pkg/sftp"
)

// TransferProgressFunc 传输进度回调
type TransferProgressFunc func(transferred, total int64, speedBPS float64)

type countingReader struct {
	ctx         context.Context
	r           io.Reader
	total       int64
	transferred int64
	onProgress  TransferProgressFunc
	lastReport  time.Time
	windowStart time.Time
	windowBytes int64
	speedBPS    float64
}

// Size 返回剩余字节，供 pkg/sftp.ReadFrom 估算并发度。
func (c *countingReader) Size() int64 {
	remain := c.total - c.transferred
	if remain < 0 {
		return 0
	}
	return remain
}

func (c *countingReader) Read(p []byte) (int, error) {
	if err := ctxErr(c.ctx); err != nil {
		return 0, err
	}
	n, err := c.r.Read(p)
	if n > 0 {
		c.transferred += int64(n)
		c.windowBytes += int64(n)
		now := time.Now()
		if c.windowStart.IsZero() {
			c.windowStart = now
		}
		elapsed := now.Sub(c.windowStart).Seconds()
		if elapsed >= 0.4 {
			c.speedBPS = float64(c.windowBytes) / elapsed
			c.windowStart = now
			c.windowBytes = 0
		}
		if c.onProgress != nil && (now.Sub(c.lastReport) >= 400*time.Millisecond || err == io.EOF || (c.total > 0 && c.transferred >= c.total)) {
			c.lastReport = now
			c.onProgress(c.transferred, c.total, c.speedBPS)
		}
	}
	return n, err
}

type countingWriter struct {
	ctx         context.Context
	w           io.Writer
	total       int64
	transferred int64
	onProgress  TransferProgressFunc
	lastReport  time.Time
	windowStart time.Time
	windowBytes int64
	speedBPS    float64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	if err := ctxErr(c.ctx); err != nil {
		return 0, err
	}
	n, err := c.w.Write(p)
	if n > 0 {
		c.transferred += int64(n)
		c.windowBytes += int64(n)
		now := time.Now()
		if c.windowStart.IsZero() {
			c.windowStart = now
		}
		elapsed := now.Sub(c.windowStart).Seconds()
		if elapsed >= 0.4 {
			c.speedBPS = float64(c.windowBytes) / elapsed
			c.windowStart = now
			c.windowBytes = 0
		}
		if c.onProgress != nil && (now.Sub(c.lastReport) >= 400*time.Millisecond || (c.total > 0 && c.transferred >= c.total)) {
			c.lastReport = now
			c.onProgress(c.transferred, c.total, c.speedBPS)
		}
	}
	return n, err
}

func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func (a *ShellAuxManager) sftpClient() (*sftp.Client, error) {
	if err := a.EnsureFileBackend(); err != nil {
		return nil, err
	}
	if a.isSCPBackend() {
		return nil, fmt.Errorf("当前为 SCP 模式，不支持该 SFTP 操作")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client == nil || a.client.remoteMachine == nil || a.client.remoteMachine.SFTPClient == nil {
		return nil, fmt.Errorf("SFTP 未连接")
	}
	return a.client.remoteMachine.SFTPClient, nil
}

// withTransferSFTP 租用传输专用 SFTP 通道（与浏览通道分离）；失败时回退浏览通道。
func (a *ShellAuxManager) withTransferSFTP(fn func(*sftp.Client) error) error {
	if err := a.EnsureFileBackend(); err != nil {
		return err
	}
	if a.isSCPBackend() {
		return fmt.Errorf("当前为 SCP 模式，不支持该 SFTP 操作")
	}
	a.mu.Lock()
	pool := a.transferPool
	a.mu.Unlock()
	if pool == nil {
		c, err := a.sftpClient()
		if err != nil {
			return err
		}
		return fn(c)
	}
	c, err := pool.Acquire()
	if err != nil {
		c, err = a.sftpClient()
		if err != nil {
			return err
		}
		return fn(c)
	}
	err = fn(c)
	pool.Release(c)
	return err
}

const downloadPartSuffix = ".flashdock.part"

func downloadPartPath(localPath string) string {
	return localPath + downloadPartSuffix
}

func uploadPartPath(remotePath string) string {
	return utils.RemoteUploadPartPath(remotePath)
}

// DownloadFile 下载远端文件到本地路径（写入 .flashdock.part，成功后原子改名；支持暂停后续传）
func (a *ShellAuxManager) DownloadFile(ctx context.Context, remotePath, localPath string, onProgress TransferProgressFunc) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if err := a.EnsureFileBackend(); err != nil {
		return err
	}
	if a.isSCPBackend() {
		return a.DownloadFileSCP(ctx, remotePath, localPath, onProgress)
	}
	return a.withTransferSFTP(func(sftpClient *sftp.Client) error {
		src, err := sftpClient.Open(remotePath)
		if err != nil {
			return fmt.Errorf("打开远端文件失败: %w", err)
		}
		defer src.Close()

		info, err := src.Stat()
		if err != nil {
			return fmt.Errorf("获取远端文件信息失败: %w", err)
		}
		total := info.Size()
		if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
			return err
		}

		partPath := downloadPartPath(localPath)
		var offset int64
		// 优先续传 .part；兼容旧版直接写目标文件的半成品
		if st, err := os.Stat(partPath); err == nil && st.Size() > 0 {
			if st.Size() >= total && total > 0 {
				if err := os.Rename(partPath, localPath); err != nil {
					return fmt.Errorf("完成下载改名失败: %w", err)
				}
				if onProgress != nil {
					onProgress(total, total, 0)
				}
				return nil
			}
			if st.Size() < total {
				offset = st.Size()
			}
		} else if st, err := os.Stat(localPath); err == nil && st.Size() > 0 {
			if st.Size() >= total && total > 0 {
				if onProgress != nil {
					onProgress(total, total, 0)
				}
				return nil
			}
			if st.Size() < total {
				// 迁移旧半成品到 .part
				if renErr := os.Rename(localPath, partPath); renErr == nil {
					offset = st.Size()
				}
			}
		}

		var dst *os.File
		if offset > 0 {
			if _, err := src.Seek(offset, io.SeekStart); err != nil {
				return fmt.Errorf("远端定位失败: %w", err)
			}
			dst, err = os.OpenFile(partPath, os.O_WRONLY, 0o644)
			if err != nil {
				return fmt.Errorf("打开本地临时文件失败: %w", err)
			}
			if _, err := dst.Seek(offset, io.SeekStart); err != nil {
				dst.Close()
				return fmt.Errorf("本地定位失败: %w", err)
			}
		} else {
			dst, err = os.Create(partPath)
			if err != nil {
				return fmt.Errorf("创建本地临时文件失败: %w", err)
			}
		}
		defer dst.Close()

		writer := &countingWriter{ctx: ctx, w: dst, total: total, transferred: offset, onProgress: onProgress}
		if onProgress != nil && offset > 0 {
			onProgress(offset, total, 0)
		}
		if _, err := utils.CopySFTPDownload(writer, src); err != nil {
			return fmt.Errorf("下载失败: %w", err)
		}
		if err := dst.Sync(); err != nil {
			return fmt.Errorf("刷写本地文件失败: %w", err)
		}
		_ = dst.Close()
		_ = os.Remove(localPath)
		if err := os.Rename(partPath, localPath); err != nil {
			return fmt.Errorf("完成下载改名失败: %w", err)
		}
		if onProgress != nil {
			onProgress(writer.transferred, total, writer.speedBPS)
		}
		return nil
	})
}

// UploadFile 上传本地文件到远端路径（支持断点续传；SCP 模式不支持续传）
func (a *ShellAuxManager) UploadFile(ctx context.Context, localPath, remotePath string, onProgress TransferProgressFunc) error {
	return a.uploadFile(ctx, localPath, remotePath, onProgress, false)
}

// UploadFileOverwrite 强制覆盖上传（忽略远端同尺寸文件的跳过逻辑）
func (a *ShellAuxManager) UploadFileOverwrite(ctx context.Context, localPath, remotePath string, onProgress TransferProgressFunc) error {
	return a.uploadFile(ctx, localPath, remotePath, onProgress, true)
}

func (a *ShellAuxManager) uploadFile(ctx context.Context, localPath, remotePath string, onProgress TransferProgressFunc, forceOverwrite bool) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if err := a.EnsureFileBackend(); err != nil {
		return err
	}
	if a.isSCPBackend() {
		return a.UploadFileSCP(ctx, localPath, remotePath, onProgress)
	}
	return a.withTransferSFTP(func(sftpClient *sftp.Client) error {
		src, err := os.Open(localPath)
		if err != nil {
			return fmt.Errorf("打开本地文件失败: %w", err)
		}
		defer src.Close()

		info, err := src.Stat()
		if err != nil {
			return fmt.Errorf("获取本地文件信息失败: %w", err)
		}
		total := info.Size()

		remoteDir := path.Dir(remotePath)
		if remoteDir != "" && remoteDir != "." && remoteDir != "/" {
			_ = sftpClient.MkdirAll(remoteDir)
		}

		var offset int64
		useAtomic := total > 0
		partRemote := uploadPartPath(remotePath)
		if forceOverwrite {
			// 只清暂存；目标文件等写完后再替换，避免上传过程中截断/删除正在运行的 jar
			_ = sftpClient.Remove(partRemote)
		}
		// 优先续传远端隐藏 .part；兼容旧版直接写目标文件
		if !forceOverwrite {
			if rst, err := sftpClient.Stat(partRemote); err == nil && rst.Size() > 0 {
				if rst.Size() >= total && total > 0 {
					_ = sftpClient.Remove(remotePath)
					if err := sftpClient.Rename(partRemote, remotePath); err != nil {
						return fmt.Errorf("完成上传改名失败: %w", err)
					}
					if onProgress != nil {
						onProgress(total, total, 0)
					}
					return nil
				}
				if rst.Size() < total {
					offset = rst.Size()
					useAtomic = false
				}
			} else if rst, err := sftpClient.Stat(remotePath); err == nil && rst.Size() > 0 {
				if rst.Size() >= total && total > 0 {
					if onProgress != nil {
						onProgress(total, total, 0)
					}
					return nil
				}
				if rst.Size() < total {
					offset = rst.Size()
					useAtomic = false
				}
			}
		} else {
			// 强制覆盖时始终走完整 atomic 写入，不续传半成品目标
			useAtomic = total > 0
			offset = 0
		}
		stagingPath := remotePath
		if useAtomic {
			stagingPath = partRemote
		} else if offset > 0 {
			// 若在 .part 上续传
			if _, err := sftpClient.Stat(partRemote); err == nil {
				stagingPath = partRemote
			}
		}

		var dst *sftp.File
		targetWrite := stagingPath
		if offset > 0 {
			if _, err := src.Seek(offset, io.SeekStart); err != nil {
				return fmt.Errorf("本地定位失败: %w", err)
			}
			dst, err = sftpClient.OpenFile(targetWrite, os.O_WRONLY)
			if err != nil {
				return fmt.Errorf("打开远端文件失败: %w", err)
			}
			if _, err := dst.Seek(offset, io.SeekStart); err != nil {
				dst.Close()
				return fmt.Errorf("远端定位失败: %w", err)
			}
		} else {
			dst, err = sftpClient.Create(targetWrite)
			if err != nil {
				return fmt.Errorf("创建远端文件失败: %w", err)
			}
		}
		defer dst.Close()

		reader := &countingReader{ctx: ctx, r: src, total: total, transferred: offset, onProgress: onProgress}
		if onProgress != nil && offset > 0 {
			onProgress(offset, total, 0)
		}
		if _, err := utils.CopySFTPUpload(dst, reader); err != nil {
			return fmt.Errorf("上传失败: %w", err)
		}
		_ = dst.Close()
		finalStaging := targetWrite
		if finalStaging != remotePath {
			if err := utils.CommitRemoteUpload(sftpClient, finalStaging, remotePath); err != nil {
				return err
			}
		}
		if onProgress != nil {
			onProgress(reader.transferred, total, reader.speedBPS)
		}
		return nil
	})
}

// UploadDirectoryZip 将本地目录打成 zip 上传后解压到 remoteTarget（完整远端目录路径）。
// remoteTarget 必须是最终目录（如 /home/u/foo 或 /home/u/foo (1)），不再用本地目录名拼接。
// onPhase 可选：上报 compressing/uploading/extracting（由调用方决定何时 compressing）。
// 若远端无 unzip/python，则回退为 SFTP 递归上传。
func (a *ShellAuxManager) UploadDirectoryZip(ctx context.Context, localDir, remoteTarget string, onProgress TransferProgressFunc, onPhase func(string)) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	info, err := os.Stat(localDir)
	if err != nil {
		return fmt.Errorf("本地目录不存在: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("不是目录: %s", localDir)
	}
	targetDir := strings.TrimSpace(remoteTarget)
	if targetDir == "" || targetDir == "." {
		targetDir = path.Join(".", info.Name())
	}
	folderName := path.Base(targetDir)
	if folderName == "" || folderName == "." || folderName == "/" {
		folderName = info.Name()
	}

	tempZip, err := os.CreateTemp("", "shell_upload_*.zip")
	if err != nil {
		return fmt.Errorf("创建临时压缩包失败: %w", err)
	}
	tempZipPath := tempZip.Name()
	_ = tempZip.Close()
	defer os.Remove(tempZipPath)

	if err := utils.LocalZip(localDir, tempZipPath); err != nil {
		return fmt.Errorf("压缩目录失败: %w", err)
	}
	if err := ctxErr(ctx); err != nil {
		return err
	}

	remoteZip := fmt.Sprintf("/tmp/flashdock_upload_%d_%s.zip", time.Now().UnixNano(), sanitizeName(folderName))
	if err := a.UploadFile(ctx, tempZipPath, remoteZip, onProgress); err != nil {
		return err
	}

	if onPhase != nil {
		onPhase("extracting")
	}
	if err := a.extractRemoteZip(remoteZip, targetDir); err != nil {
		_, _ = a.Exec("rm -f " + shellSingleQuote(remoteZip))
		if fallbackErr := a.uploadDirectoryRecursive(ctx, localDir, targetDir, onProgress); fallbackErr != nil {
			return fmt.Errorf("远端解压失败且递归上传失败: %v / %v", err, fallbackErr)
		}
		return nil
	}
	return nil
}

// extractRemoteZip 尝试多种方式在远端解压 zip（staging + mv，避免原地截断）
func (a *ShellAuxManager) extractRemoteZip(remoteZip, targetDir string) error {
	candidates := utils.RemoteAtomicUnzipCandidates(remoteZip, targetDir)
	var lastOut string
	var lastErr error
	for _, cmd := range candidates {
		out, err := a.Exec(cmd)
		if err == nil {
			return nil
		}
		lastOut = strings.TrimSpace(out)
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("远端无可用解压工具")
	}
	if lastOut != "" {
		return fmt.Errorf("%w (%s)", lastErr, lastOut)
	}
	return lastErr
}

// UploadDirectoryRecursive 通过 SFTP 递归上传本地目录（不压缩）
func (a *ShellAuxManager) UploadDirectoryRecursive(ctx context.Context, localDir, remoteDir string, onProgress TransferProgressFunc) error {
	return a.uploadDirectoryRecursive(ctx, localDir, remoteDir, onProgress)
}

// uploadDirectoryRecursive 通过 SFTP 递归上传本地目录到远端目标路径
func (a *ShellAuxManager) uploadDirectoryRecursive(ctx context.Context, localDir, remoteDir string, onProgress TransferProgressFunc) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	sftpClient, err := a.sftpClient()
	if err != nil {
		return err
	}
	total, err := estimateLocalDirSize(localDir)
	if err != nil {
		total = 0
	}
	var transferred int64
	var lastReport time.Time
	var windowStart time.Time
	var windowBytes int64
	var speedBPS float64
	progress := func(n int64) {
		transferred += n
		windowBytes += n
		now := time.Now()
		if windowStart.IsZero() {
			windowStart = now
		}
		elapsed := now.Sub(windowStart).Seconds()
		if elapsed >= 0.5 {
			speedBPS = float64(windowBytes) / elapsed
			windowStart = now
			windowBytes = 0
		}
		if onProgress == nil {
			return
		}
		done := total > 0 && transferred >= total
		if now.Sub(lastReport) >= 400*time.Millisecond || done {
			lastReport = now
			onProgress(transferred, total, speedBPS)
		}
	}
	if err := sftpClient.MkdirAll(remoteDir); err != nil {
		return fmt.Errorf("创建远端目录失败: %w", err)
	}
	return a.copyLocalDir(ctx, sftpClient, localDir, remoteDir, progress)
}

func estimateLocalDirSize(dir string) (int64, error) {
	var total int64
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

func (a *ShellAuxManager) copyLocalDir(ctx context.Context, c *sftp.Client, localDir, remoteDir string, onChunk func(int64)) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	entries, err := os.ReadDir(localDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := ctxErr(ctx); err != nil {
			return err
		}
		lpath := filepath.Join(localDir, e.Name())
		rpath := path.Join(remoteDir, e.Name())
		info, err := e.Info()
		if err != nil {
			return err
		}
		if info.IsDir() {
			if err := c.MkdirAll(rpath); err != nil {
				return fmt.Errorf("创建远端目录失败: %w", err)
			}
			if err := a.copyLocalDir(ctx, c, lpath, rpath, onChunk); err != nil {
				return err
			}
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if err := a.uploadFileWithChunk(ctx, c, lpath, rpath, onChunk); err != nil {
			return fmt.Errorf("上传 %s 失败: %w", lpath, err)
		}
	}
	return nil
}

func (a *ShellAuxManager) uploadFileWithChunk(ctx context.Context, c *sftp.Client, localPath, remotePath string, onChunk func(int64)) error {
	src, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer src.Close()

	remoteDir := path.Dir(remotePath)
	if remoteDir != "" && remoteDir != "." && remoteDir != "/" {
		_ = c.MkdirAll(remoteDir)
	}
	partRemote := uploadPartPath(remotePath)
	_ = c.Remove(partRemote)
	dst, err := c.Create(partRemote)
	if err != nil {
		return err
	}

	buf := make([]byte, utils.TransferBufferSize)
	for {
		if err := ctxErr(ctx); err != nil {
			_ = dst.Close()
			_ = c.Remove(partRemote)
			return err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				_ = dst.Close()
				_ = c.Remove(partRemote)
				return werr
			}
			if onChunk != nil {
				onChunk(int64(n))
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			_ = dst.Close()
			_ = c.Remove(partRemote)
			return readErr
		}
	}
	if err := dst.Close(); err != nil {
		_ = c.Remove(partRemote)
		return err
	}
	if err := utils.CommitRemoteUpload(c, partRemote, remotePath); err != nil {
		return err
	}
	return nil
}

// DownloadDirectory 下载远端目录到本地目录。
// 优先远端 zip 打包后本机解压；若无 zip 则 SFTP 递归下载。
func (a *ShellAuxManager) DownloadDirectory(ctx context.Context, remoteDir, localDir string, onProgress TransferProgressFunc) (string, error) {
	if err := ctxErr(ctx); err != nil {
		return "", err
	}
	sftpClient, err := a.sftpClient()
	if err != nil {
		return "", err
	}
	info, err := sftpClient.Stat(remoteDir)
	if err != nil {
		return "", fmt.Errorf("远端目录不存在: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("不是目录: %s", remoteDir)
	}

	parent := path.Dir(remoteDir)
	base := path.Base(remoteDir)
	if parent == "" || parent == "." {
		parent = "/"
	}

	finalDir := localDir
	if err := os.MkdirAll(filepath.Dir(finalDir), 0o755); err != nil {
		return "", err
	}

	if err := a.downloadDirectoryViaZip(ctx, parent, base, finalDir, onProgress); err == nil {
		return finalDir, nil
	}

	if err := a.downloadDirectoryRecursive(ctx, remoteDir, finalDir, onProgress); err != nil {
		_ = os.RemoveAll(finalDir)
		return "", err
	}
	return finalDir, nil
}

func (a *ShellAuxManager) downloadDirectoryViaZip(ctx context.Context, parent, base, finalDir string, onProgress TransferProgressFunc) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	remoteZip := fmt.Sprintf("/tmp/flashdock_dl_%d_%s.zip", time.Now().UnixNano(), sanitizeName(base))
	cmd := fmt.Sprintf("cd %s && zip -rq %s %s",
		shellSingleQuote(parent),
		shellSingleQuote(remoteZip),
		shellSingleQuote(base),
	)
	out, err := a.Exec(cmd)
	if err != nil {
		_, _ = a.Exec("rm -f " + shellSingleQuote(remoteZip))
		return fmt.Errorf("远端 zip 失败: %w (%s)", err, strings.TrimSpace(out))
	}
	defer func() { _, _ = a.Exec("rm -f " + shellSingleQuote(remoteZip)) }()

	tempZip, err := os.CreateTemp("", "flashdock_dl_*.zip")
	if err != nil {
		return err
	}
	tempPath := tempZip.Name()
	_ = tempZip.Close()
	defer os.Remove(tempPath)

	if err := a.DownloadFile(ctx, remoteZip, tempPath, onProgress); err != nil {
		return err
	}

	staging := finalDir + ".extracting"
	_ = os.RemoveAll(staging)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	if err := utils.LocalUnzip(tempPath, staging); err != nil {
		return err
	}
	return promoteExtractedDir(staging, base, finalDir)
}

func (a *ShellAuxManager) downloadDirectoryRecursive(ctx context.Context, remoteDir, localDir string, onProgress TransferProgressFunc) error {
	sftpClient, err := a.sftpClient()
	if err != nil {
		return err
	}
	total, err := a.estimateRemoteDirSize(sftpClient, remoteDir)
	if err != nil {
		total = 0
	}
	var transferred int64
	var lastReport time.Time
	var windowStart time.Time
	var windowBytes int64
	var speedBPS float64
	progress := func(n int64) {
		transferred += n
		windowBytes += n
		now := time.Now()
		if windowStart.IsZero() {
			windowStart = now
		}
		elapsed := now.Sub(windowStart).Seconds()
		if elapsed >= 0.5 {
			speedBPS = float64(windowBytes) / elapsed
			windowStart = now
			windowBytes = 0
		}
		if onProgress == nil {
			return
		}
		done := total > 0 && transferred >= total
		if now.Sub(lastReport) >= 400*time.Millisecond || done {
			lastReport = now
			onProgress(transferred, total, speedBPS)
		}
	}
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		return err
	}
	return a.copyRemoteDir(ctx, sftpClient, remoteDir, localDir, progress)
}

func (a *ShellAuxManager) estimateRemoteDirSize(c *sftp.Client, dir string) (int64, error) {
	var total int64
	var walk func(string) error
	walk = func(p string) error {
		entries, err := c.ReadDir(p)
		if err != nil {
			return err
		}
		for _, e := range entries {
			full := path.Join(p, e.Name())
			if e.IsDir() {
				if err := walk(full); err != nil {
					return err
				}
				continue
			}
			total += e.Size()
		}
		return nil
	}
	if err := walk(dir); err != nil {
		return 0, err
	}
	return total, nil
}

func (a *ShellAuxManager) copyRemoteDir(ctx context.Context, c *sftp.Client, remoteDir, localDir string, onChunk func(int64)) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	entries, err := c.ReadDir(remoteDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := ctxErr(ctx); err != nil {
			return err
		}
		rpath := path.Join(remoteDir, e.Name())
		lpath := filepath.Join(localDir, e.Name())
		if e.IsDir() {
			if err := os.MkdirAll(lpath, 0o755); err != nil {
				return err
			}
			if err := a.copyRemoteDir(ctx, c, rpath, lpath, onChunk); err != nil {
				return err
			}
			continue
		}
		if err := a.downloadFileWithChunk(ctx, c, rpath, lpath, onChunk); err != nil {
			return fmt.Errorf("下载 %s 失败: %w", rpath, err)
		}
	}
	return nil
}

func (a *ShellAuxManager) downloadFileWithChunk(ctx context.Context, c *sftp.Client, remotePath, localPath string, onChunk func(int64)) error {
	ri, err := c.Stat(remotePath)
	if err != nil {
		return err
	}
	total := ri.Size()
	if st, err := os.Stat(localPath); err == nil {
		if st.Size() == total && total >= 0 {
			if onChunk != nil {
				onChunk(total)
			}
			return nil
		}
		// 不完整文件：截断后重下该文件（目录任务按文件粒度续传）
		if st.Size() > total {
			_ = os.Remove(localPath)
		}
	}

	src, err := c.Open(remotePath)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return err
	}
	dst, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer dst.Close()

	writer := &chunkCountingWriter{ctx: ctx, w: dst, onChunk: onChunk}
	if _, err := utils.CopySFTPDownload(writer, src); err != nil {
		return err
	}
	return nil
}

// chunkCountingWriter 把增量字节交给目录下载汇总进度，同时保留 ctx 取消。
type chunkCountingWriter struct {
	ctx     context.Context
	w       io.Writer
	onChunk func(int64)
}

func (c *chunkCountingWriter) Write(p []byte) (int, error) {
	if err := ctxErr(c.ctx); err != nil {
		return 0, err
	}
	n, err := c.w.Write(p)
	if n > 0 && c.onChunk != nil {
		c.onChunk(int64(n))
	}
	return n, err
}

func promoteExtractedDir(staging, expectName, finalDir string) error {
	_ = expectName
	entries, err := os.ReadDir(staging)
	if err != nil {
		return err
	}
	var meaningful []os.DirEntry
	for _, e := range entries {
		name := e.Name()
		if name == "." || name == ".." {
			continue
		}
		meaningful = append(meaningful, e)
	}
	if len(meaningful) == 1 && meaningful[0].IsDir() {
		src := filepath.Join(staging, meaningful[0].Name())
		_ = os.RemoveAll(finalDir)
		if err := os.Rename(src, finalDir); err != nil {
			if copyErr := copyDir(src, finalDir); copyErr != nil {
				return fmt.Errorf("移动解压目录失败: %v / %v", err, copyErr)
			}
		}
		return nil
	}
	_ = os.RemoveAll(finalDir)
	if err := os.Rename(staging, finalDir); err != nil {
		if copyErr := copyDir(staging, finalDir); copyErr != nil {
			return fmt.Errorf("整理解压目录失败: %v / %v", err, copyErr)
		}
	}
	return nil
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
}

func sanitizeName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "item"
	}
	replacer := strings.NewReplacer(
		"/", "_", "\\", "_", ":", "_", "*", "_", "?", "_",
		"\"", "_", "<", "_", ">", "_", "|", "_", " ", "_",
	)
	return replacer.Replace(name)
}

// UniqueLocalPath 若目标已存在则追加 (1)(2)...
func UniqueLocalPath(path string) string {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return path
	}
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	info, err := os.Stat(path)
	if err == nil && info.IsDir() {
		base = path
		ext = ""
	}
	for i := 1; i < 10000; i++ {
		candidate := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
	return fmt.Sprintf("%s_%d%s", base, time.Now().UnixNano(), ext)
}

// ResolveFDDownloadDir 返回 Downloads/fddownload，必要时创建
func ResolveFDDownloadDir(resolveDownloads func() (string, error)) (string, error) {
	downloads, err := resolveDownloads()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(downloads, "fddownload")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}
