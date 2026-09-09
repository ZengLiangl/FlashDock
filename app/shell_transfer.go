package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	"FlashDock/data"
	"FlashDock/define"
	"FlashDock/machine"

	"github.com/google/uuid"
	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

const shellTransferEvent = "shell:transfer"

type shellTransferStore struct {
	mu            sync.Mutex
	items         map[string]*define.SftpTransferRecord
	cancels       map[string]context.CancelFunc
	activeRunning int
}

func (a *App) ensureTransferStore() *shellTransferStore {
	if a.transfers == nil {
		a.transfers = &shellTransferStore{
			items:   make(map[string]*define.SftpTransferRecord),
			cancels: make(map[string]context.CancelFunc),
		}
	}
	return a.transfers
}

func (a *App) emitTransfer(rec *define.SftpTransferRecord) {
	if a.ctx == nil || rec == nil {
		return
	}
	cp := *rec
	wailsRuntime.EventsEmit(a.ctx, shellTransferEvent, cp)
}

func (a *App) upsertTransfer(rec *define.SftpTransferRecord) {
	store := a.ensureTransferStore()
	store.mu.Lock()
	defer store.mu.Unlock()
	store.items[rec.ID] = rec
	a.emitTransfer(rec)
}

func (a *App) registerTransferCancel(id string, cancel context.CancelFunc) {
	store := a.ensureTransferStore()
	store.mu.Lock()
	defer store.mu.Unlock()
	store.cancels[id] = cancel
}

func (a *App) unregisterTransferCancel(id string) {
	store := a.ensureTransferStore()
	store.mu.Lock()
	defer store.mu.Unlock()
	delete(store.cancels, id)
}

func (a *App) updateTransferProgress(id string, transferred, total int64, speedBPS float64) {
	store := a.ensureTransferStore()
	store.mu.Lock()
	rec, ok := store.items[id]
	if !ok || rec.Status == "paused" || rec.Status == "done" || rec.Status == "error" {
		store.mu.Unlock()
		return
	}
	rec.Transferred = transferred
	if total > 0 {
		rec.Total = total
		rec.Percent = float64(transferred) / float64(total) * 100
	}
	rec.SpeedBPS = speedBPS
	rec.Status = "running"
	rec.UpdatedAt = time.Now().Unix()
	cp := *rec
	store.mu.Unlock()
	// 异步推送进度，避免 Wails IPC 阻塞传输热路径
	go a.emitTransfer(&cp)
}

func (a *App) setTransferPhase(id, phase string) {
	store := a.ensureTransferStore()
	store.mu.Lock()
	rec, ok := store.items[id]
	if !ok || rec.Status == "paused" || rec.Status == "done" || rec.Status == "error" {
		store.mu.Unlock()
		return
	}
	if rec.Phase == phase {
		store.mu.Unlock()
		return
	}
	rec.Phase = phase
	rec.UpdatedAt = time.Now().Unix()
	cp := *rec
	store.mu.Unlock()
	go a.emitTransfer(&cp)
}

func (a *App) finishTransfer(id string, err error) {
	a.unregisterTransferCancel(id)
	store := a.ensureTransferStore()
	store.mu.Lock()
	rec, ok := store.items[id]
	if !ok {
		store.mu.Unlock()
		a.pumpTransferQueue()
		return
	}
	wasRunning := rec.Status == "running" || rec.Status == "pending"
	// 已被暂停则不覆盖为 error（Pause 已扣减 activeRunning）
	if rec.Status == "paused" {
		store.mu.Unlock()
		a.pumpTransferQueue()
		return
	}
	_ = wasRunning
	now := time.Now().Unix()
	rec.UpdatedAt = now
	rec.FinishedAt = now
	rec.Phase = ""
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, define.ErrTransferPaused) {
			rec.Status = "paused"
			rec.Error = ""
			rec.SpeedBPS = 0
		} else {
			rec.Status = "error"
			rec.Error = err.Error()
		}
	} else {
		rec.Status = "done"
		rec.Percent = 100
		if rec.Total > 0 {
			rec.Transferred = rec.Total
		}
	}
	if store.activeRunning > 0 {
		store.activeRunning--
	}
	cp := *rec
	store.mu.Unlock()
	a.emitTransfer(&cp)
	a.pumpTransferQueue()
}

// PickShellUploadPaths 选择要上传的本地文件（可多选）
func (a *App) PickShellUploadPaths() ([]string, error) {
	files, err := wailsRuntime.OpenMultipleFilesDialog(a.ctx, wailsRuntime.OpenDialogOptions{
		Title: "选择要上传的文件",
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// PickShellUploadFolder 选择要上传的本地文件夹
func (a *App) PickShellUploadFolder() (string, error) {
	dir, err := wailsRuntime.OpenDirectoryDialog(a.ctx, wailsRuntime.OpenDialogOptions{
		Title: "选择要上传的文件夹",
	})
	if err != nil {
		return "", err
	}
	return dir, nil
}

// GetShellDownloadDir 返回 Downloads/fddownload
func (a *App) GetShellDownloadDir() (string, error) {
	return machine.ResolveFDDownloadDir(resolveDownloadsDir)
}

// OpenShellDownloadDir 打开 fddownload 目录
func (a *App) OpenShellDownloadDir() error {
	dir, err := a.GetShellDownloadDir()
	if err != nil {
		return err
	}
	var cmd *exec.Cmd
	switch goruntime.GOOS {
	case "windows":
		cmd = exec.Command("explorer", dir)
	case "darwin":
		cmd = exec.Command("open", dir)
	default:
		cmd = exec.Command("xdg-open", dir)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("打开下载目录失败: %w", err)
	}
	return nil
}

// ListShellTransfers 返回传输记录（新到旧）
func (a *App) ListShellTransfers() []define.SftpTransferRecord {
	store := a.ensureTransferStore()
	store.mu.Lock()
	defer store.mu.Unlock()
	out := make([]define.SftpTransferRecord, 0, len(store.items))
	for _, rec := range store.items {
		out = append(out, *rec)
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].StartedAt > out[i].StartedAt {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// ClearFinishedShellTransfers 清除已完成/失败/已暂停的记录
func (a *App) ClearFinishedShellTransfers() {
	store := a.ensureTransferStore()
	store.mu.Lock()
	defer store.mu.Unlock()
	for id, rec := range store.items {
		if rec.Status == "done" || rec.Status == "error" || rec.Status == "paused" {
			delete(store.items, id)
			delete(store.cancels, id)
		}
	}
}

// PauseShellTransfer 暂停进行中的传输
func (a *App) PauseShellTransfer(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("参数无效")
	}
	store := a.ensureTransferStore()
	store.mu.Lock()
	rec, ok := store.items[id]
	if !ok {
		store.mu.Unlock()
		return fmt.Errorf("记录不存在")
	}
	if rec.Status != "running" && rec.Status != "pending" && rec.Status != "queued" {
		store.mu.Unlock()
		return fmt.Errorf("当前状态无法暂停")
	}
	wasActive := rec.Status == "running" || rec.Status == "pending"
	cancel := store.cancels[id]
	rec.Status = "paused"
	rec.SpeedBPS = 0
	rec.UpdatedAt = time.Now().Unix()
	rec.FinishedAt = rec.UpdatedAt
	if wasActive && store.activeRunning > 0 {
		store.activeRunning--
	}
	cp := *rec
	store.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	a.unregisterTransferCancel(id)
	a.emitTransfer(&cp)
	a.pumpTransferQueue()
	return nil
}

// ResumeShellTransfer 继续已暂停的传输（单文件断点续传；目录跳过已完成文件）
func (a *App) ResumeShellTransfer(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("参数无效")
	}
	store := a.ensureTransferStore()
	store.mu.Lock()
	rec, ok := store.items[id]
	if !ok {
		store.mu.Unlock()
		return fmt.Errorf("记录不存在")
	}
	if rec.Status != "paused" && rec.Status != "error" {
		store.mu.Unlock()
		return fmt.Errorf("仅已暂停/失败的任务可继续")
	}
	cp := *rec
	store.mu.Unlock()

	a.launchTransfer(&cp)
	return nil
}

// RemoveShellTransfer 删除传输记录；若进行中则先暂停
func (a *App) RemoveShellTransfer(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("参数无效")
	}
	store := a.ensureTransferStore()
	store.mu.Lock()
	rec, ok := store.items[id]
	if !ok {
		store.mu.Unlock()
		return nil
	}
	cancel := store.cancels[id]
	active := rec.Status == "running" || rec.Status == "pending"
	delete(store.items, id)
	delete(store.cancels, id)
	store.mu.Unlock()

	if active && cancel != nil {
		cancel()
	}
	if a.ctx != nil {
		wailsRuntime.EventsEmit(a.ctx, shellTransferEvent, map[string]interface{}{
			"id":     id,
			"status": "removed",
		})
	}
	return nil
}

func (a *App) launchTransfer(seed *define.SftpTransferRecord) {
	if seed == nil || seed.ID == "" {
		return
	}
	store := a.ensureTransferStore()
	store.mu.Lock()
	rec, ok := store.items[seed.ID]
	if !ok {
		rec = seed
		store.items[seed.ID] = rec
	}
	rec.Status = "queued"
	rec.Error = ""
	rec.SpeedBPS = 0
	rec.FinishedAt = 0
	rec.UpdatedAt = time.Now().Unix()
	cp := *rec
	store.mu.Unlock()
	a.emitTransfer(&cp)
	a.pumpTransferQueue()
}

// pumpTransferQueue 按优先级启动排队中的传输，遵守固定并发上限
func (a *App) pumpTransferQueue() {
	store := a.ensureTransferStore()
	for {
		store.mu.Lock()
		max := define.TransferMaxConcurrent
		if cfg, err := a.GetGlobalConfig(); err == nil {
			max = data.SftpTransferMaxConcurrentValue(cfg)
		}
		if store.activeRunning >= max {
			store.mu.Unlock()
			return
		}
		var next *define.SftpTransferRecord
		for _, rec := range store.items {
			if rec.Status != "queued" {
				continue
			}
			if next == nil ||
				rec.Priority > next.Priority ||
				(rec.Priority == next.Priority && rec.StartedAt < next.StartedAt) {
				next = rec
			}
		}
		if next == nil {
			store.mu.Unlock()
			return
		}
		next.Status = "pending"
		next.UpdatedAt = time.Now().Unix()
		store.activeRunning++
		cp := *next
		store.mu.Unlock()
		a.emitTransfer(&cp)
		a.startTransferWorker(&cp)
	}
}

func (a *App) startTransferWorker(cp *define.SftpTransferRecord) {
	go func() {
		if cp.Direction == "copy" {
			a.updateTransferProgress(cp.ID, cp.Transferred, cp.Total, 0)
			a.finishTransfer(cp.ID, a.runCrossHostCopyTransfer(cp))
			return
		}
		aux, err := a.shellAuxPool.Get(cp.MachineName)
		if err != nil {
			a.finishTransfer(cp.ID, err)
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		a.registerTransferCancel(cp.ID, cancel)
		defer a.unregisterTransferCancel(cp.ID)

		a.updateTransferProgress(cp.ID, cp.Transferred, cp.Total, 0)
		progress := func(transferred, total int64, speedBPS float64) {
			a.updateTransferProgress(cp.ID, transferred, total, speedBPS)
		}
		var transferErr error
		if cp.Direction == "download" {
			if cp.IsDir {
				actual, err := aux.DownloadDirectory(ctx, cp.RemotePath, cp.LocalPath, progress)
				transferErr = err
				if err == nil && actual != "" && actual != cp.LocalPath {
					store := a.ensureTransferStore()
					store.mu.Lock()
					if r, ok := store.items[cp.ID]; ok {
						r.LocalPath = actual
						r.Name = filepath.Base(actual)
					}
					store.mu.Unlock()
				}
			} else {
				skip := false
				if cfg, err := a.GetGlobalConfig(); err == nil && data.SftpSkipUnchangedEnabled(cfg) {
					if ok, _ := aux.ShouldSkipUnchangedDownload(cp.RemotePath, cp.LocalPath); ok {
						skip = true
					}
				}
				if skip {
					if info, err := os.Stat(cp.LocalPath); err == nil {
						a.updateTransferProgress(cp.ID, info.Size(), info.Size(), 0)
					}
					transferErr = nil
				} else {
					transferErr = aux.DownloadFile(ctx, cp.RemotePath, cp.LocalPath, progress)
				}
			}
		} else {
			forceReplace := strings.EqualFold(cp.ConflictAction, "replace") || strings.EqualFold(cp.ConflictAction, "merge")
			forceMirror := strings.EqualFold(cp.ConflictAction, "mirror")
			if cp.IsDir {
				// 目录上传：合并覆盖同名文件（.part 原子替换）；仅 mirror 策略才 prune 远端多余文件（如前端 dist）。
				if cp.UseCompress {
					a.setTransferPhase(cp.ID, "compressing")
					// RemotePath 已是最终目标目录（含 duplicate 分配的唯一名）
					transferErr = aux.UploadDirectoryZip(ctx, cp.LocalPath, cp.RemotePath, func(transferred, total int64, speedBPS float64) {
						if transferred > 0 || total > 0 {
							a.setTransferPhase(cp.ID, "uploading")
						}
						progress(transferred, total, speedBPS)
					}, func(phase string) {
						a.setTransferPhase(cp.ID, phase)
					})
				} else {
					transferErr = aux.UploadDirectoryRecursive(ctx, cp.LocalPath, cp.RemotePath, progress)
				}
				if transferErr == nil && forceMirror {
					if pruneErr := aux.PruneRemoteDirToMirror(cp.LocalPath, cp.RemotePath); pruneErr != nil {
						transferErr = fmt.Errorf("清理多余远端文件失败: %w", pruneErr)
					} else if verifyErr := aux.VerifyRemoteDirMirror(cp.LocalPath, cp.RemotePath); verifyErr != nil {
						transferErr = fmt.Errorf("上传完整性校验失败: %w", verifyErr)
					}
				}
			} else {
				skip := false
				if !forceReplace {
					if cfg, err := a.GetGlobalConfig(); err == nil && data.SftpSkipUnchangedEnabled(cfg) {
						if ok, _ := aux.ShouldSkipUnchangedUpload(cp.LocalPath, cp.RemotePath); ok {
							skip = true
						}
					}
				}
				if skip {
					if info, err := os.Stat(cp.LocalPath); err == nil {
						a.updateTransferProgress(cp.ID, info.Size(), info.Size(), 0)
					}
					transferErr = nil
				} else if forceReplace {
					transferErr = aux.UploadFileOverwrite(ctx, cp.LocalPath, cp.RemotePath, progress)
					if transferErr == nil {
						if info, err := os.Stat(cp.LocalPath); err == nil {
							aux.PreserveRemoteMtime(cp.RemotePath, info.ModTime())
						}
					}
				} else {
					transferErr = aux.UploadFile(ctx, cp.LocalPath, cp.RemotePath, progress)
					if transferErr == nil {
						if info, err := os.Stat(cp.LocalPath); err == nil {
							aux.PreserveRemoteMtime(cp.RemotePath, info.ModTime())
						}
					}
				}
			}
		}
		a.finishTransfer(cp.ID, transferErr)
	}()
}

// PrioritizeShellTransfer 提高任务优先级并尝试立即调度
func (a *App) PrioritizeShellTransfer(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("参数无效")
	}
	store := a.ensureTransferStore()
	store.mu.Lock()
	rec, ok := store.items[id]
	if !ok {
		store.mu.Unlock()
		return fmt.Errorf("记录不存在")
	}
	maxPri := 0
	for _, r := range store.items {
		if r.Priority > maxPri {
			maxPri = r.Priority
		}
	}
	rec.Priority = maxPri + 1
	rec.UpdatedAt = time.Now().Unix()
	cp := *rec
	store.mu.Unlock()
	a.emitTransfer(&cp)
	a.pumpTransferQueue()
	return nil
}

// PauseAllShellTransfers 暂停全部进行中/排队中的传输
func (a *App) PauseAllShellTransfers() int {
	store := a.ensureTransferStore()
	store.mu.Lock()
	ids := make([]string, 0)
	for id, rec := range store.items {
		if rec.Status == "running" || rec.Status == "pending" || rec.Status == "queued" {
			ids = append(ids, id)
		}
	}
	store.mu.Unlock()
	n := 0
	for _, id := range ids {
		if err := a.PauseShellTransfer(id); err == nil {
			n++
		}
	}
	return n
}

// ResumeAllShellTransfers 继续全部已暂停/失败的传输
func (a *App) ResumeAllShellTransfers() int {
	store := a.ensureTransferStore()
	store.mu.Lock()
	ids := make([]string, 0)
	for id, rec := range store.items {
		if rec.Status == "paused" || rec.Status == "error" {
			ids = append(ids, id)
		}
	}
	store.mu.Unlock()
	n := 0
	for _, id := range ids {
		if err := a.ResumeShellTransfer(id); err == nil {
			n++
		}
	}
	return n
}

// StartShellDownload 下载远端路径到 Downloads/fddownload（异步）
func (a *App) StartShellDownload(machineName, remotePath string) (string, error) {
	if err := a.requireUnlocked(); err != nil {
		return "", err
	}
	remotePath = strings.TrimSpace(remotePath)
	if machineName == "" || remotePath == "" || remotePath == "/" {
		return "", fmt.Errorf("参数无效")
	}
	aux, err := a.shellAuxPool.Get(machineName)
	if err != nil {
		return "", err
	}

	downloadDir, err := a.GetShellDownloadDir()
	if err != nil {
		return "", err
	}

	isDir, err := aux.DirExists(remotePath)
	if err != nil {
		return "", err
	}
	if !isDir {
		parent := path.Dir(remotePath)
		base := path.Base(remotePath)
		entries, listErr := aux.ListDir(parent, true)
		if listErr != nil {
			return "", listErr
		}
		found := false
		for _, e := range entries {
			if e.Name == base {
				found = true
				isDir = e.IsDir
				break
			}
		}
		if !found {
			return "", fmt.Errorf("远端路径不存在: %s", remotePath)
		}
	}

	name := path.Base(remotePath)
	localPath := machine.UniqueLocalPath(filepath.Join(downloadDir, name))

	id := uuid.NewString()
	now := time.Now().Unix()
	rec := &define.SftpTransferRecord{
		ID:          id,
		MachineName: machineName,
		Direction:   "download",
		Name:        filepath.Base(localPath),
		LocalPath:   localPath,
		RemotePath:  remotePath,
		IsDir:       isDir,
		Status:      "pending",
		StartedAt:   now,
		UpdatedAt:   now,
	}
	a.upsertTransfer(rec)
	a.launchTransfer(rec)
	return id, nil
}

// StartShellUpload 上传本地路径到远端目录（异步）。
// conflictAction: replace | duplicate | merge | mirror | ""(默认 replace)；目录默认压缩上传（可在设置关闭）。
func (a *App) StartShellUpload(machineName, localPath, remoteDir, conflictAction string) (string, error) {
	if err := a.requireUnlocked(); err != nil {
		return "", err
	}
	localPath = strings.TrimSpace(localPath)
	remoteDir = strings.TrimSpace(remoteDir)
	conflictAction = strings.ToLower(strings.TrimSpace(conflictAction))
	if conflictAction == "" {
		conflictAction = "replace"
	}
	switch conflictAction {
	case "replace", "duplicate", "merge", "mirror":
	default:
		return "", fmt.Errorf("不支持的冲突策略: %s", conflictAction)
	}
	if machineName == "" || localPath == "" {
		return "", fmt.Errorf("参数无效")
	}
	if remoteDir == "" {
		remoteDir = "."
	}
	info, err := os.Stat(localPath)
	if err != nil {
		return "", fmt.Errorf("本地路径不存在: %w", err)
	}
	aux, err := a.shellAuxPool.Get(machineName)
	if err != nil {
		return "", err
	}

	name := info.Name()
	remotePath := path.Join(remoteDir, name)
	if conflictAction == "duplicate" {
		unique, err := aux.AllocateUniqueRemotePath(remotePath)
		if err != nil {
			return "", err
		}
		remotePath = unique
		name = path.Base(remotePath)
	}
	if (conflictAction == "merge" || conflictAction == "mirror") && !info.IsDir() {
		return "", fmt.Errorf("合并/镜像覆盖仅适用于目录")
	}

	useCompress := true
	if cfg, err := a.GetGlobalConfig(); err == nil {
		useCompress = data.SftpUseCompressedUploadEnabled(cfg)
	}
	if !info.IsDir() {
		useCompress = false
	}

	id := uuid.NewString()
	now := time.Now().Unix()
	rec := &define.SftpTransferRecord{
		ID:             id,
		MachineName:    machineName,
		Direction:      "upload",
		Name:           name,
		LocalPath:      localPath,
		RemotePath:     remotePath,
		IsDir:          info.IsDir(),
		UseCompress:    useCompress,
		ConflictAction: conflictAction,
		Status:         "pending",
		Total:          0,
		StartedAt:      now,
		UpdatedAt:      now,
	}
	if !info.IsDir() {
		rec.Total = info.Size()
	}
	a.upsertTransfer(rec)
	a.launchTransfer(rec)
	return id, nil
}

