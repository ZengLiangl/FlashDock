package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"FlashDock/data"
	"FlashDock/define"
	"FlashDock/machine"

	"github.com/google/uuid"
	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	shellRemoteEditMaxSize = 10 * 1024 * 1024 // 10MB 内置编辑上限
)

// SearchShellCommandHistory 搜索跨会话命令历史
func (a *App) SearchShellCommandHistory(scope, query string, limit int) []string {
	if a.shellCmdHistory == nil {
		return nil
	}
	return a.shellCmdHistory.Search(scope, query, limit)
}

// ClearShellCommandHistory 清空命令历史
func (a *App) ClearShellCommandHistory(scope string) error {
	if a.shellCmdHistory == nil {
		return nil
	}
	return a.shellCmdHistory.Clear(scope)
}

// RecordShellCommandHistory 显式记录一条命令历史（插入未执行的命令也可记入）
func (a *App) RecordShellCommandHistory(scope, command string) error {
	if a.shellCmdHistory == nil {
		return nil
	}
	return a.shellCmdHistory.Record(scope, command)
}

// recordShellCommand 记录命令历史：
// - 含换行：按行记录（终端回车提交）
// - 无换行但为整段文本：记为一次插入（片段「不直接执行」、粘贴等）
func (a *App) recordShellCommand(sessionID, input string) {
	if a.shellCmdHistory == nil || input == "" {
		return
	}
	scope := "global"
	if !machine.IsLocalShellID(sessionID) {
		if name := a.remoteConfigName(sessionID); name != "" {
			scope = name
		}
	}

	if strings.Contains(input, "\n") {
		for _, line := range strings.Split(input, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			_ = a.shellCmdHistory.Record(scope, line)
		}
		return
	}

	// 忽略 ANSI / 控制序列与单字符键入
	if strings.HasPrefix(input, "\x1b") || strings.ContainsRune(input, '\x1b') {
		return
	}
	cmd := strings.TrimSpace(input)
	if cmd == "" || len([]rune(cmd)) < 2 {
		return
	}
	_ = a.shellCmdHistory.Record(scope, cmd)
}

// AddShellTemporaryTunnel 添加临时端口转发
func (a *App) AddShellTemporaryTunnel(sessionID string, spec define.SSHTunnel) error {
	if err := a.requireUnlocked(); err != nil {
		return err
	}
	configName := a.remoteConfigName(sessionID)
	machineConfig := a.configManager.GetMachine(configName)
	if machineConfig == nil {
		return fmt.Errorf("未找到机器配置")
	}
	var client *machine.SSHClient
	if sm := a.shellPool.GetSession(sessionID); sm != nil {
		client = sm.SharedSSHClient()
	}
	if client == nil {
		return fmt.Errorf("SSH 未连接")
	}
	spec.Enabled = true
	return a.tunnelMgr.AddTemporary(configName, spec, client)
}

// RemoveShellTunnel 移除隧道
func (a *App) RemoveShellTunnel(sessionID, tunnelName string) error {
	configName := a.remoteConfigName(sessionID)
	return a.tunnelMgr.RemoveTunnel(configName, tunnelName)
}

// ReadShellRemoteFile 读取远端小文件（用于内置编辑）
func (a *App) ReadShellRemoteFile(machineName, remotePath string) (string, error) {
	if err := a.requireUnlocked(); err != nil {
		return "", err
	}
	aux, err := a.getShellAux(machineName)
	if err != nil {
		return "", err
	}
	remotePath = strings.TrimSpace(remotePath)
	if remotePath == "" {
		return "", fmt.Errorf("路径为空")
	}
	info, err := aux.StatPath(remotePath)
	if err != nil {
		return "", err
	}
	if info.IsDir {
		return "", fmt.Errorf("不能编辑目录")
	}
	if info.Size > shellRemoteEditMaxSize {
		return "", fmt.Errorf("文件超过 %dMB，请用系统应用打开", shellRemoteEditMaxSize/(1024*1024))
	}
	f, err := aux.OpenFile(remotePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, shellRemoteEditMaxSize+1))
	if err != nil {
		return "", err
	}
	if int64(len(raw)) > shellRemoteEditMaxSize {
		return "", fmt.Errorf("文件过大")
	}
	return string(raw), nil
}

// SaveShellRemoteFile 保存远端小文件
func (a *App) SaveShellRemoteFile(machineName, remotePath, content string) error {
	if err := a.requireUnlocked(); err != nil {
		return err
	}
	aux, err := a.getShellAux(machineName)
	if err != nil {
		return err
	}
	return aux.WriteFile(remotePath, []byte(content))
}

// SelectSystemApplication 选择用于打开文件的本地应用程序
func (a *App) SelectSystemApplication() (*data.SftpSystemApp, error) {
	if a.ctx == nil {
		return nil, fmt.Errorf("应用未就绪")
	}
	path, err := a.selectSystemApplicationPath()
	if err != nil {
		return nil, err
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil
	}
	name := filepath.Base(path)
	name = strings.TrimSuffix(name, filepath.Ext(name))
	if name == "" {
		name = filepath.Base(path)
	}
	return &data.SftpSystemApp{Path: path, Name: name}, nil
}

// OpenShellRemoteFileExternal 下载到临时目录并用关联/外置编辑器/系统默认打开；是否回传由 sftpAutoSync 决定
func (a *App) OpenShellRemoteFileExternal(machineName, remotePath string) error {
	if err := a.requireUnlocked(); err != nil {
		return err
	}
	cfg, _ := a.GetGlobalConfig()
	enableWatch := data.SftpAutoSyncEnabled(cfg)
	return a.openRemoteFileExternal(machineName, remotePath, "", false, enableWatch)
}

// OpenShellRemoteFileWithApp 下载到临时目录并用指定应用程序打开
func (a *App) OpenShellRemoteFileWithApp(machineName, remotePath, appPath string, enableWatch bool) error {
	if err := a.requireUnlocked(); err != nil {
		return err
	}
	appPath = strings.TrimSpace(appPath)
	if appPath == "" {
		return fmt.Errorf("请选择应用程序")
	}
	return a.openRemoteFileExternal(machineName, remotePath, appPath, false, enableWatch)
}

// OpenShellRemoteFileSystemDefault 下载到临时目录并用操作系统默认程序打开
func (a *App) OpenShellRemoteFileSystemDefault(machineName, remotePath string, enableWatch bool) error {
	if err := a.requireUnlocked(); err != nil {
		return err
	}
	return a.openRemoteFileExternal(machineName, remotePath, "", true, enableWatch)
}

// UpsertSftpFileAssociation 新增或更新扩展名打开关联
func (a *App) UpsertSftpFileAssociation(extension string, assoc data.SftpFileAssociation) error {
	cfg, err := a.GetGlobalConfig()
	if err != nil {
		return err
	}
	if cfg == nil {
		return fmt.Errorf("配置未加载")
	}
	ext := data.NormalizeSftpFileExtension(extension)
	normalized := data.NormalizeSftpFileAssociations(map[string]data.SftpFileAssociation{ext: assoc})
	entry, ok := normalized[ext]
	if !ok {
		return fmt.Errorf("无效的文件关联")
	}
	if cfg.SftpFileAssociations == nil {
		cfg.SftpFileAssociations = map[string]data.SftpFileAssociation{}
	}
	cfg.SftpFileAssociations[ext] = entry
	cfg.SftpFileAssociations = data.NormalizeSftpFileAssociations(cfg.SftpFileAssociations)
	if err := a.configManager.SaveGlobalConfig(cfg); err != nil {
		return err
	}
	a.emitSftpOpenSettingsChanged(cfg)
	return nil
}

// DeleteSftpFileAssociation 删除扩展名打开关联
func (a *App) DeleteSftpFileAssociation(extension string) error {
	cfg, err := a.GetGlobalConfig()
	if err != nil {
		return err
	}
	if cfg == nil || cfg.SftpFileAssociations == nil {
		return nil
	}
	ext := data.NormalizeSftpFileExtension(extension)
	delete(cfg.SftpFileAssociations, ext)
	if len(cfg.SftpFileAssociations) == 0 {
		cfg.SftpFileAssociations = nil
	}
	if err := a.configManager.SaveGlobalConfig(cfg); err != nil {
		return err
	}
	a.emitSftpOpenSettingsChanged(cfg)
	return nil
}

func (a *App) emitSftpOpenSettingsChanged(cfg *data.GlobalConfig) {
	if a.ctx == nil || cfg == nil {
		return
	}
	wailsRuntime.EventsEmit(a.ctx, "system-settings:changed", map[string]any{
		"sftpAutoSync":         data.SftpAutoSyncEnabled(cfg),
		"sftpDefaultOpener":    data.NormalizeSftpDefaultOpener(cfg.SftpDefaultOpener),
		"sftpDefaultSystemApp": cfg.SftpDefaultSystemApp,
		"sftpFileAssociations": cfg.SftpFileAssociations,
	})
}

func (a *App) openRemoteFileExternal(machineName, remotePath, appPath string, forceOSDefault, enableWatch bool) error {
	aux, err := a.getShellAux(machineName)
	if err != nil {
		return err
	}
	remotePath = strings.TrimSpace(remotePath)
	if remotePath == "" {
		return fmt.Errorf("路径为空")
	}
	base := filepath.Base(remotePath)
	tmpDir, err := os.MkdirTemp("", "flashdock-remote-*")
	if err != nil {
		return err
	}
	localPath := filepath.Join(tmpDir, base)
	if err := aux.DownloadFile(context.Background(), remotePath, localPath, nil); err != nil {
		_ = os.RemoveAll(tmpDir)
		return err
	}
	if enableWatch {
		if err := a.startExternalEditWatch(machineName, remotePath, localPath); err != nil {
			_ = os.RemoveAll(tmpDir)
			return fmt.Errorf("启动编辑监听失败: %w", err)
		}
	}
	var openErr error
	if forceOSDefault {
		openErr = openWithSystemDefault(localPath)
	} else if appPath != "" {
		openErr = openWithApplication(localPath, appPath)
	} else {
		openErr = a.openExternalPath(localPath)
	}
	if openErr != nil {
		if enableWatch {
			a.externalEdits.stop(externalEditKey(machineName, remotePath))
		}
		_ = os.RemoveAll(tmpDir)
		return openErr
	}
	return nil
}

func (a *App) openExternalPath(path string) error {
	ext := strings.ToLower(filepath.Ext(path))
	cfg, _ := a.GetGlobalConfig()
	if cfg != nil && cfg.FileAssociations != nil {
		if cmdTpl := strings.TrimSpace(cfg.FileAssociations[ext]); cmdTpl != "" {
			if err := runEditorCommand(cmdTpl, path); err == nil {
				return nil
			}
		}
	}
	if cfg != nil {
		if cmdTpl := strings.TrimSpace(cfg.ExternalEditorCommand); cmdTpl != "" {
			if err := runEditorCommand(cmdTpl, path); err == nil {
				return nil
			}
		}
	}
	return openWithSystemDefault(path)
}

func runEditorCommand(tpl, path string) error {
	tpl = strings.TrimSpace(tpl)
	if tpl == "" {
		return fmt.Errorf("命令为空")
	}
	replaced := strings.ReplaceAll(tpl, "{path}", path)
	if !strings.Contains(tpl, "{path}") {
		replaced = tpl + " " + path
	}
	parts := strings.Fields(replaced)
	if len(parts) == 0 {
		return fmt.Errorf("命令为空")
	}
	return exec.Command(parts[0], parts[1:]...).Start()
}

func openWithApplication(filePath, appPath string) error {
	filePath = strings.TrimSpace(filePath)
	appPath = strings.TrimSpace(appPath)
	if filePath == "" || appPath == "" {
		return fmt.Errorf("路径为空")
	}
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", "-a", appPath, filePath).Start()
	case "windows":
		return exec.Command(appPath, filePath).Start()
	default:
		return exec.Command(appPath, filePath).Start()
	}
}

func openWithSystemDefault(path string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", path).Start()
	case "windows":
		return exec.Command("cmd", "/C", "start", "", path).Start()
	default:
		return exec.Command("xdg-open", path).Start()
	}
}

// StartShellFolderSync 文件夹同步（递归；按大小+修改时间跳过未变更）
func (a *App) StartShellFolderSync(machineName, localDir, remoteDir, direction string) (string, error) {
	if err := a.requireUnlocked(); err != nil {
		return "", err
	}
	localDir = strings.TrimSpace(localDir)
	remoteDir = strings.TrimSpace(remoteDir)
	if localDir == "" || remoteDir == "" {
		return "", fmt.Errorf("请填写本地与远端目录")
	}
	direction = strings.ToLower(strings.TrimSpace(direction))
	if direction == "" {
		direction = "upload"
	}
	id := uuid.NewString()
	go a.runFolderSync(id, machineName, localDir, remoteDir, direction)
	return id, nil
}

type syncFileJob struct {
	rel        string
	localPath  string
	remotePath string
	action     string // upload | download
}

func (a *App) runFolderSync(id, machineName, localDir, remoteDir, direction string) {
	rec := &define.SftpTransferRecord{
		ID:          id,
		MachineName: machineName,
		Direction:   "sync-" + direction,
		Name:        filepath.Base(localDir) + " ↔ " + remoteDir,
		LocalPath:   localDir,
		RemotePath:  remoteDir,
		IsDir:       true,
		Status:      "running",
		StartedAt:   time.Now().Unix(),
		UpdatedAt:   time.Now().Unix(),
	}
	a.upsertTransfer(rec)

	aux, err := a.getShellAux(machineName)
	if err != nil {
		rec.Status = "error"
		rec.Error = err.Error()
		rec.FinishedAt = time.Now().Unix()
		a.upsertTransfer(rec)
		return
	}

	skipUnchanged := true
	if cfg, err := a.GetGlobalConfig(); err == nil {
		skipUnchanged = data.SftpSkipUnchangedEnabled(cfg)
	}

	localFiles := map[string]define.LocalFileEntry{}
	if err := a.walkLocalFiles(localDir, "", localFiles); err != nil {
		rec.Status = "error"
		rec.Error = err.Error()
		rec.FinishedAt = time.Now().Unix()
		a.upsertTransfer(rec)
		return
	}
	remoteFiles := map[string]define.SftpEntry{}
	if err := a.walkRemoteFiles(aux, remoteDir, "", remoteFiles); err != nil {
		// 远端目录不存在时视为空树（上传场景）
		if direction == "download" || direction == "both" {
			rec.Status = "error"
			rec.Error = err.Error()
			rec.FinishedAt = time.Now().Unix()
			a.upsertTransfer(rec)
			return
		}
	}

	needUpload := direction == "upload" || direction == "both"
	needDownload := direction == "download" || direction == "both"
	jobs := make([]syncFileJob, 0)

	if needUpload {
		for rel, le := range localFiles {
			rp := path.Join(remoteDir, filepath.ToSlash(rel))
			re, ok := remoteFiles[rel]
			if ok && skipUnchanged && machine.SameSizeAndMtime(le.Size, time.Unix(le.ModTime, 0), re.Size, time.Unix(re.ModTime, 0)) {
				continue
			}
			if ok && re.Size == le.Size && !skipUnchanged {
				// 仍同步（强制覆盖）
			}
			jobs = append(jobs, syncFileJob{rel: rel, localPath: le.Path, remotePath: rp, action: "upload"})
		}
	}
	if needDownload {
		for rel, re := range remoteFiles {
			lp := filepath.Join(localDir, filepath.FromSlash(rel))
			le, ok := localFiles[rel]
			if ok && skipUnchanged && machine.SameSizeAndMtime(le.Size, time.Unix(le.ModTime, 0), re.Size, time.Unix(re.ModTime, 0)) {
				continue
			}
			jobs = append(jobs, syncFileJob{rel: rel, localPath: lp, remotePath: re.Path, action: "download"})
		}
	}

	rec.Total = int64(len(jobs))
	a.upsertTransfer(rec)
	ctx := context.Background()
	var done int64
	for _, job := range jobs {
		parentRemote := path.Dir(job.remotePath)
		parentLocal := filepath.Dir(job.localPath)
		if job.action == "upload" {
			_ = aux.MkdirRemotePath(parentRemote)
			if upErr := aux.UploadFileOverwrite(ctx, job.localPath, job.remotePath, nil); upErr != nil {
				rec.Status = "error"
				rec.Error = upErr.Error()
				rec.FinishedAt = time.Now().Unix()
				a.upsertTransfer(rec)
				return
			}
			if info, err := os.Stat(job.localPath); err == nil {
				aux.PreserveRemoteMtime(job.remotePath, info.ModTime())
			}
		} else {
			_ = os.MkdirAll(parentLocal, 0o755)
			if dlErr := aux.DownloadFile(ctx, job.remotePath, job.localPath, nil); dlErr != nil {
				rec.Status = "error"
				rec.Error = dlErr.Error()
				rec.FinishedAt = time.Now().Unix()
				a.upsertTransfer(rec)
				return
			}
		}
		done++
		rec.Transferred = done
		rec.Percent = float64(done) / float64(max64(rec.Total, 1)) * 100
		rec.UpdatedAt = time.Now().Unix()
		a.upsertTransfer(rec)
	}

	rec.Status = "done"
	rec.Percent = 100
	rec.FinishedAt = time.Now().Unix()
	a.upsertTransfer(rec)
}

func (a *App) walkLocalFiles(root, rel string, out map[string]define.LocalFileEntry) error {
	dir := root
	if rel != "" {
		dir = filepath.Join(root, rel)
	}
	entries, err := a.ListLocalFiles(dir, false)
	if err != nil {
		return err
	}
	for _, e := range entries {
		childRel := e.Name
		if rel != "" {
			childRel = filepath.ToSlash(filepath.Join(rel, e.Name))
		}
		if e.IsDir {
			if err := a.walkLocalFiles(root, childRel, out); err != nil {
				return err
			}
			continue
		}
		out[filepath.ToSlash(childRel)] = e
	}
	return nil
}

func (a *App) walkRemoteFiles(aux *machine.ShellAuxManager, root, rel string, out map[string]define.SftpEntry) error {
	dir := root
	if rel != "" {
		dir = path.Join(root, rel)
	}
	entries, err := aux.ListDir(dir, false)
	if err != nil {
		return err
	}
	for _, e := range entries {
		childRel := e.Name
		if rel != "" {
			childRel = path.Join(rel, e.Name)
		}
		if e.IsDir {
			if err := a.walkRemoteFiles(aux, root, childRel, out); err != nil {
				return err
			}
			continue
		}
		out[childRel] = e
	}
	return nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
