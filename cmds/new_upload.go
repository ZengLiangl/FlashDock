package cmds

import (
	"FlashDock/define"
	"FlashDock/utils"
	"bufio"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

func RegUploadCmd() {
	CmdManager.RegSpecialCmd("upload", doUpload)
}

func doUpload(rm *define.RemoteMachine, c []string, outputChan chan<- string) error {
	if len(c) != 3 {
		return errors.New("参数错误: upload <本地路径> <远程路径>")
	}

	localPath := c[1]
	remotePath := c[2]

	// 检查本地文件是否存在
	fileInfo, err := os.Stat(localPath)
	if err != nil {
		return fmt.Errorf("本地文件不存在: %w", err)
	}

	if fileInfo.IsDir() {
		// 文件夹上传，使用SFTP模式
		return uploadDirectory(rm, localPath, remotePath, fileInfo.Name(), outputChan)
	} else {
		// 单文件上传，使用SFTP模式
		return uploadFile(rm, localPath, remotePath, fileInfo.Name(), outputChan)
	}
}

// uploadFile 上传单个文件（先写隐藏 .part，完成后再替换目标，避免原地截断正在运行的 jar）
func uploadFile(rm *define.RemoteMachine, localPath, remotePath, targetFileName string, outputChan chan<- string) error {
	// 打开本地文件
	srcFile, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("打开本地文件失败: %w", err)
	}
	defer srcFile.Close()

	// 获取文件信息
	fileInfo, err := srcFile.Stat()
	if err != nil {
		return fmt.Errorf("获取文件信息失败: %w", err)
	}

	// 处理远程路径
	if strings.HasSuffix(remotePath, "/") {
		remotePath = remotePath + fileInfo.Name()
	}

	transferID := fmt.Sprintf("task-upload-%d", time.Now().UnixNano())
	reportTransfer(&define.SftpTransferRecord{
		ID: transferID, Direction: "upload", Name: targetFileName,
		LocalPath: localPath, RemotePath: remotePath, Status: "running",
		Total: fileInfo.Size(), StartedAt: time.Now().Unix(),
	})

	fail := func(err error) error {
		reportTransfer(&define.SftpTransferRecord{
			ID: transferID, Direction: "upload", Name: targetFileName, Status: "error",
			Error: err.Error(), FinishedAt: time.Now().Unix(),
		})
		return err
	}

	remoteDir := path.Dir(remotePath)
	if remoteDir != "" && remoteDir != "." && remoteDir != "/" {
		_ = rm.SFTPClient.MkdirAll(remoteDir)
	}

	partRemote := utils.RemoteUploadPartPath(remotePath)
	_ = rm.SFTPClient.Remove(partRemote)

	// 写入暂存文件，禁止对目标路径 Create/O_TRUNC
	dstFile, err := rm.SFTPClient.Create(partRemote)
	if err != nil {
		return fail(fmt.Errorf("创建远程暂存文件失败: %w", err))
	}

	totalSize := fileInfo.Size()
	startTime := time.Now()
	progressID := fmt.Sprintf("upload_%d_%s", time.Now().UnixNano(), targetFileName)
	var written atomic.Int64
	stopProgress := make(chan struct{})
	go runUploadProgressDisplay(outputChan, progressID, targetFileName, totalSize, startTime, stopProgress, written.Load)

	reader := utils.NewCountingReader(srcFile, totalSize, 0, func(transferred, total int64, _ float64) {
		written.Store(transferred)
		if total <= 0 {
			return
		}
		reportTransfer(&define.SftpTransferRecord{
			ID: transferID, Direction: "upload", Name: targetFileName,
			LocalPath: localPath, RemotePath: remotePath, Status: "running",
			Total: total, Transferred: transferred,
			Percent:   float64(transferred) / float64(total) * 100,
			UpdatedAt: time.Now().Unix(),
		})
	})

	_, err = utils.CopySFTPUpload(dstFile, reader)
	_ = dstFile.Close()
	close(stopProgress)
	if err != nil {
		_ = rm.SFTPClient.Remove(partRemote)
		return fail(fmt.Errorf("文件传输失败: %w", err))
	}
	if err := utils.CommitRemoteUpload(rm.SFTPClient, partRemote, remotePath); err != nil {
		return fail(err)
	}
	reportTransfer(&define.SftpTransferRecord{
		ID: transferID, Direction: "upload", Name: targetFileName,
		LocalPath: localPath, RemotePath: remotePath, Status: "done",
		Total: totalSize, Transferred: totalSize, Percent: 100,
		FinishedAt: time.Now().Unix(),
	})
	return nil
}

// uploadDirectory 上传文件夹
func uploadDirectory(rm *define.RemoteMachine, localPath, remotePath, targetFileName string, outputChan chan<- string) error {
	fmt.Printf("正在上传文件夹: %s -> %s\n", localPath, remotePath)

	// 使用 zip 管道上传
	err := uploadDirectoryZip(rm, localPath, remotePath, targetFileName, outputChan)
	if err != nil {
		utils.SendOutput(outputChan, fmt.Sprintf("文件夹上传失败: %s", err.Error()))
		return fmt.Errorf("文件夹上传失败: %w", err)
	}

	fmt.Printf("文件夹上传完成: %s -> %s\n", localPath, remotePath)

	return nil
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// uploadDirectoryZip 使用 ZIP 压缩上传文件夹；远端无解压工具时回退 SFTP 递归上传
func uploadDirectoryZip(rm *define.RemoteMachine, localPath, remotePath, targetFileName string, outputChan chan<- string) error {
	tempZipFile, err := os.CreateTemp("", "upload_*.zip")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tempZipPath := tempZipFile.Name()
	_ = tempZipFile.Close()
	defer os.Remove(tempZipPath)

	if err := utils.LocalZip(localPath, tempZipPath); err != nil {
		return fmt.Errorf("创建ZIP压缩包失败: %w", err)
	}

	tempRemotePath := fmt.Sprintf("/tmp/upload_%d.zip", time.Now().UnixNano())
	if err := uploadFile(rm, tempZipPath, tempRemotePath, targetFileName, outputChan); err != nil {
		return fmt.Errorf("上传压缩包失败: %w", err)
	}

	if err := extractZipOnRemote(rm, tempRemotePath, remotePath, outputChan); err != nil {
		_ = runRemoteCommand(rm, "rm -f "+shellSingleQuote(tempRemotePath), nil)
		utils.SendOutput(outputChan, "远端无可用解压工具，回退为 SFTP 递归上传…")
		if fallbackErr := uploadDirectoryRecursive(rm, localPath, remotePath, outputChan); fallbackErr != nil {
			return fmt.Errorf("远程解压失败且递归上传失败: %v / %v", err, fallbackErr)
		}
	} else {
		_ = runRemoteCommand(rm, "rm -f "+shellSingleQuote(tempRemotePath), nil)
	}
	// 任务模式目录 upload 为「合并覆盖同名文件」，不 prune 远端多余文件（瘦包场景常只传 target/jar 增量）。
	return nil
}

// extractZipOnRemote 尝试 unzip / busybox / python 在远端解压（staging + mv，避免原地截断）
func extractZipOnRemote(rm *define.RemoteMachine, remoteZipPath, targetPath string, outputChan chan<- string) error {
	candidates := utils.RemoteAtomicUnzipCandidates(remoteZipPath, targetPath)
	var lastErr error
	for _, cmd := range candidates {
		if err := runRemoteCommand(rm, cmd, outputChan); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("远端无可用解压工具")
	}
	return lastErr
}

func runRemoteCommand(rm *define.RemoteMachine, cmd string, outputChan chan<- string) error {
	session, err := rm.NewSession()
	if err != nil {
		return fmt.Errorf("创建SSH会话失败: %w", err)
	}
	defer session.Close()

	stdout, err := session.StdoutPipe()
	if err != nil {
		return fmt.Errorf("获取stdout管道失败: %w", err)
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		return fmt.Errorf("获取stderr管道失败: %w", err)
	}

	if err := session.Start(cmd); err != nil {
		return fmt.Errorf("启动命令失败: %w", err)
	}

	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			utils.SendOutput(outputChan, scanner.Text())
		}
	}()
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			utils.SendOutput(outputChan, fmt.Sprintf("[STDERR] %s", scanner.Text()))
		}
	}()

	if err := session.Wait(); err != nil {
		return fmt.Errorf("命令执行失败: %w", err)
	}
	return nil
}

// uploadDirectoryRecursive 通过 SFTP 递归上传本地目录内容到远端目标路径
func uploadDirectoryRecursive(rm *define.RemoteMachine, localPath, remotePath string, outputChan chan<- string) error {
	if rm.SFTPClient == nil {
		return fmt.Errorf("SFTP 未连接")
	}
	if err := rm.SFTPClient.MkdirAll(remotePath); err != nil {
		return fmt.Errorf("创建远程目录失败: %w", err)
	}

	base, err := filepath.Abs(filepath.Clean(localPath))
	if err != nil {
		return fmt.Errorf("解析本地目录失败: %w", err)
	}

	var fileCount int
	err = filepath.Walk(base, func(p string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(base, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		remoteFile := path.Join(remotePath, filepath.ToSlash(rel))
		if info.IsDir() {
			return rm.SFTPClient.MkdirAll(remoteFile)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		fileCount++
		utils.SendOutput(outputChan, fmt.Sprintf("递归上传: %s", filepath.ToSlash(rel)))
		return uploadFile(rm, p, remoteFile, info.Name(), outputChan)
	})
	if err != nil {
		return err
	}
	utils.SendOutput(outputChan, fmt.Sprintf("SFTP 递归上传完成，共 %d 个文件", fileCount))
	return nil
}

// bytesToHumanReadable 将字节数转换为人类可读的格式
func bytesToHumanReadable(bytes int64) string {
	if bytes == 0 {
		return "0 KB"
	}

	unit := int64(1024)
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}

	unitLabels := []string{"KB", "MB", "GB", "TB", "PB"}
	for _, label := range unitLabels {
		if bytes < unit*1024 {
			return fmt.Sprintf("%.2f %s", float64(bytes)/float64(unit), label)
		}
		unit *= 1024
	}
	return fmt.Sprintf("%.2f PB", float64(bytes)/float64(unit))
}

// runUploadProgressDisplay 每 500ms 刷新任务输出区进度行（不占用 SFTP 写热路径）。
func runUploadProgressDisplay(
	outputChan chan<- string,
	progressID, fileName string,
	totalSize int64,
	startTime time.Time,
	stop <-chan struct{},
	written func() int64,
) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			n := written()
			elapsed := time.Since(startTime).Seconds()
			if elapsed > 0 && totalSize > 0 && n >= totalSize {
				avg := utils.FormatTransferSpeed(float64(n) / elapsed)
				line := fmt.Sprintf("[%s] 传输完成: 100%%, 文件大小: %s, 平均速度: %s, 耗时: %.2f秒",
					fileName, bytesToHumanReadable(totalSize), avg, elapsed)
				utils.SendOutput(outputChan, fmt.Sprintf("PROGRESS_UPDATE:%s:%s", progressID, line))
			}
			return
		case <-ticker.C:
			n := written()
			elapsed := time.Since(startTime).Seconds()
			if elapsed <= 0 || totalSize <= 0 || n >= totalSize {
				continue
			}
			speed := utils.FormatTransferSpeed(float64(n) / elapsed)
			progress := float64(n) / float64(totalSize) * 100
			line := fmt.Sprintf("[%s] 进度: %.2f%%, 速度: %s", fileName, progress, speed)
			utils.SendOutput(outputChan, fmt.Sprintf("PROGRESS_UPDATE:%s:%s", progressID, line))
		}
	}
}
