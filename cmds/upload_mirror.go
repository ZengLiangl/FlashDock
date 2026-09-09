package cmds

import (
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/pkg/sftp"
)

type remoteDirFileMeta struct {
	size int64
}

func collectLocalDirFilesForMirror(root string) (map[string]remoteDirFileMeta, error) {
	root = filepath.Clean(root)
	out := make(map[string]remoteDirFileMeta)
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = remoteDirFileMeta{size: info.Size()}
		return nil
	})
	return out, err
}

func collectRemoteDirFilesForMirror(c *sftp.Client, root string) (map[string]remoteDirFileMeta, error) {
	root = path.Clean(strings.TrimSpace(root))
	out := make(map[string]remoteDirFileMeta)
	var walk func(string, string) error
	walk = func(base, rel string) error {
		dir := base
		if rel != "" {
			dir = path.Join(base, rel)
		}
		entries, err := c.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			childRel := e.Name()
			if rel != "" {
				childRel = path.Join(rel, e.Name())
			}
			if e.IsDir() {
				if err := walk(base, childRel); err != nil {
					return err
				}
				continue
			}
			out[childRel] = remoteDirFileMeta{size: e.Size()}
		}
		return nil
	}
	if err := walk(root, ""); err != nil {
		return nil, err
	}
	return out, nil
}
