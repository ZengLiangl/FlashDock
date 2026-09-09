package utils

import (
	"fmt"
	"path"
	"strings"
)

// RemoteUploadCommitter 完成原子上传替换所需的最小 SFTP 接口。
type RemoteUploadCommitter interface {
	PosixRename(oldname, newname string) error
	Rename(oldname, newname string) error
	Remove(path string) error
}

// CommitRemoteUpload 将 .part 暂存文件原子替换到目标路径（优先 posix-rename 覆盖，避免先删目标）。
func CommitRemoteUpload(c RemoteUploadCommitter, partRemote, remotePath string) error {
	if err := c.PosixRename(partRemote, remotePath); err == nil {
		return nil
	}
	_ = c.Remove(remotePath)
	if err := c.Rename(partRemote, remotePath); err != nil {
		return fmt.Errorf("原子替换失败: %w", err)
	}
	return nil
}

const remoteUploadPartSuffix = ".flashdock.part"

// RemoteUploadPartPath 返回远端原子上传的隐藏暂存路径（与目标同目录）。
func RemoteUploadPartPath(remotePath string) string {
	dir := path.Dir(remotePath)
	base := path.Base(remotePath)
	if dir == "" || dir == "." {
		return "." + base + remoteUploadPartSuffix
	}
	return path.Join(dir, "."+base+remoteUploadPartSuffix)
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// ShellCommitRemoteUpload 返回在远端 shell 上将 .part 原子替换为目标文件的命令（供 SCP 等无 SFTP rename 场景）。
func ShellCommitRemoteUpload(partRemote, remotePath string) string {
	return "mv -f " + shellSingleQuote(partRemote) + " " + shellSingleQuote(remotePath)
}

// RemoteAtomicUnzipCandidates 远端解压命令候选：先解到 staging，再 mv -f 覆盖目标文件，
// 避免 unzip/extractall 原地截断正在被 Docker/JVM 打开的 jar。
func RemoteAtomicUnzipCandidates(remoteZip, targetDir string) []string {
	tq := shellSingleQuote(targetDir)
	zq := shellSingleQuote(remoteZip)
	staging := strings.TrimRight(targetDir, "/") + ".flashdock.extract"
	sq := shellSingleQuote(staging)
	promote := fmt.Sprintf(
		`find %s -type f -print0 | while IFS= read -r -d '' f; do rel="${f#%s/}"; mkdir -p %s/"$(dirname "$rel")"; mv -f "$f" %s/"$rel"; done && rm -rf %s && rm -f %s`,
		sq, staging, tq, tq, sq, zq,
	)
	return []string{
		fmt.Sprintf("rm -rf %s && mkdir -p %s %s && unzip -o %s -d %s && %s", sq, sq, tq, zq, sq, promote),
		fmt.Sprintf("rm -rf %s && mkdir -p %s %s && busybox unzip -o %s -d %s && %s", sq, sq, tq, zq, sq, promote),
		fmt.Sprintf(
			"rm -rf %s && mkdir -p %s %s && python3 -c %s && %s",
			sq, sq, tq,
			shellSingleQuote(fmt.Sprintf("import zipfile; zipfile.ZipFile(%q).extractall(%q)", remoteZip, staging)),
			promote,
		),
		fmt.Sprintf(
			"rm -rf %s && mkdir -p %s %s && python -c %s && %s",
			sq, sq, tq,
			shellSingleQuote(fmt.Sprintf("import zipfile; zipfile.ZipFile(%q).extractall(%q)", remoteZip, staging)),
			promote,
		),
	}
}
