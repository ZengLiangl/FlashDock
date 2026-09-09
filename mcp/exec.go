package mcp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"FlashDock/define"
	"FlashDock/machine"
	"FlashDock/utils"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// ExecResult SSH 执行结果
type ExecResult struct {
	ExitCode int    `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

func (s *Service) machineByAlias(alias string) (*define.Machine, error) {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return nil, wrapErr("[notfound]", "服务器别名为空，请先 list_servers")
	}
	if s.cfg == nil {
		return nil, wrapErr("[notfound]", "配置未加载")
	}
	for _, m := range s.cfg.GetAllMachinesFromGlobal() {
		if m.Name == alias || m.ID == alias {
			cp := m
			return &cp, nil
		}
	}
	return nil, wrapErr("[notfound]", "未找到服务器别名 "+alias+"，请先 list_servers")
}

func (s *Service) displayMachine(m define.Machine) define.Machine {
	out := m
	if !out.ApplyListFieldsForDisplay() {
		if sd, err := m.GetSensitiveData(); err == nil && sd != nil {
			out.Host = sd.Host
			out.Port = sd.Port
			if out.Port <= 0 {
				out.Port = 22
			}
			out.User = sd.User
		}
	}
	return out
}

func (s *Service) preparedMachine(m *define.Machine) (*define.Machine, error) {
	if s.cfg == nil {
		return m, nil
	}
	return s.cfg.MachineForConnect(m)
}

func (s *Service) workVars() map[string]string {
	if s.cfg == nil {
		return nil
	}
	return s.cfg.GetWorkPathVars()
}

// SetSSHShare 注入已连接 SSH（如 Shell 会话池），MCP / 任务优先复用。
func (s *Service) SetSSHShare(fn func(configName string) *machine.SSHClient) {
	s.shareSSH = fn
}

// OwnedClient 返回 MCP 自己持有的空闲 SSH（供任务模式复用）。
func (s *Service) OwnedClient(configName string) *machine.SSHClient {
	configName = strings.TrimSpace(configName)
	if configName == "" {
		return nil
	}
	s.sshMu.Lock()
	defer s.sshMu.Unlock()
	c := s.ownedSSH[configName]
	if c != nil && c.IsConnected() {
		return c
	}
	return nil
}

func (s *Service) hostLock(name string) *sync.Mutex {
	v, _ := s.sshLocks.LoadOrStore(name, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func (s *Service) rememberOwned(name string, cli *machine.SSHClient) {
	if name == "" || cli == nil {
		return
	}
	s.sshMu.Lock()
	defer s.sshMu.Unlock()
	if s.ownedSSH == nil {
		s.ownedSSH = make(map[string]*machine.SSHClient)
	}
	if old := s.ownedSSH[name]; old != nil && old != cli {
		_ = old.Close()
	}
	s.ownedSSH[name] = cli
}

func (s *Service) closeOwnedSSH() {
	s.sshMu.Lock()
	defer s.sshMu.Unlock()
	for name, c := range s.ownedSSH {
		if c != nil {
			_ = c.Close()
		}
		delete(s.ownedSSH, name)
	}
}

func (s *Service) liveShare(name string) *machine.SSHClient {
	if s.shareSSH != nil {
		if c := s.shareSSH(name); c != nil && c.IsConnected() {
			return c
		}
	}
	s.sshMu.Lock()
	defer s.sshMu.Unlock()
	c := s.ownedSSH[name]
	if c != nil && c.IsConnected() {
		return c
	}
	if c != nil {
		_ = c.Close()
		delete(s.ownedSSH, name)
	}
	return nil
}

func ensureSFTP(cli *machine.SSHClient, withSFTP bool) error {
	if !withSFTP || cli == nil {
		return nil
	}
	rm := cli.SharedRemoteMachine()
	if rm == nil {
		return fmt.Errorf("SSH 未连接")
	}
	return rm.EnsureSFTP()
}

func (s *Service) withSSH(alias string, withSFTP bool, fn func(*machine.SSHClient, *define.Machine) error) error {
	raw, err := s.machineByAlias(alias)
	if err != nil {
		return err
	}
	prep, err := s.preparedMachine(raw)
	if err != nil {
		return err
	}
	vars := s.workVars()
	name := strings.TrimSpace(raw.Name)
	if name == "" {
		name = strings.TrimSpace(alias)
	}

	lk := s.hostLock(name)
	lk.Lock()
	cli, borrowed, err := s.acquireSSHLocked(name, prep, vars, withSFTP)
	lk.Unlock()
	if err != nil {
		return err
	}
	if borrowed {
		defer cli.Close()
	}
	return fn(cli, raw)
}

func (s *Service) acquireSSHLocked(name string, prep *define.Machine, vars map[string]string, withSFTP bool) (*machine.SSHClient, bool, error) {
	if shared := s.liveShare(name); shared != nil {
		cli := machine.NewSSHClient(prep, vars)
		cli.AttachRemote(shared.SharedRemoteMachine(), prep, vars)
		if err := ensureSFTP(cli, withSFTP); err != nil {
			cli.Close()
			return nil, false, err
		}
		return cli, true, nil
	}

	cli := machine.NewSSHClient(prep, vars)
	if err := cli.ConnectAutoTrustOnce(prep, withSFTP); err != nil {
		return nil, false, fmt.Errorf("连接失败: %w", err)
	}
	s.rememberOwned(name, cli)
	if err := ensureSFTP(cli, withSFTP); err != nil {
		return nil, false, err
	}
	return cli, false, nil
}

func (s *Service) execSSH(ctx context.Context, alias, command string, timeout time.Duration) (ExecResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var res ExecResult
	err := s.withSSH(alias, false, func(cli *machine.SSHClient, _ *define.Machine) error {
		rm := cli.SharedRemoteMachine()
		if rm == nil || rm.SSHClient == nil {
			return fmt.Errorf("SSH 未连接")
		}
		session, err := rm.NewSession()
		if err != nil {
			return err
		}
		defer session.Close()
		var stdout, stderr bytes.Buffer
		session.Stdout = &stdout
		session.Stderr = &stderr
		done := make(chan error, 1)
		go func() { done <- session.Run(command) }()
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case err := <-done:
			res.Stdout = stdout.String()
			res.Stderr = stderr.String()
			if err == nil {
				res.ExitCode = 0
				return nil
			}
			if ee, ok := err.(*ssh.ExitError); ok {
				res.ExitCode = ee.ExitStatus()
				return nil
			}
			return err
		case <-timer.C:
			_ = session.Close()
			res.Stdout = stdout.String()
			res.Stderr = stderr.String()
			return wrapErr("[timeout]", "命令超时（多数是交互式命令在等 stdin）")
		}
	})
	res.Stdout = s.redactText(ctx, res.Stdout, alias)
	res.Stderr = s.redactText(ctx, res.Stderr, alias)
	return res, err
}

func clampTimeout(v *int64, def, min, max int64) time.Duration {
	n := def
	if v != nil && *v > 0 {
		n = *v
	}
	if n < min {
		n = min
	}
	if n > max {
		n = max
	}
	return time.Duration(n) * time.Second
}

func (s *Service) sftpClient(alias string, fn func(*sftp.Client, *define.Machine) error) error {
	return s.withSSH(alias, true, func(cli *machine.SSHClient, raw *define.Machine) error {
		rm := cli.SharedRemoteMachine()
		if rm == nil {
			return fmt.Errorf("SSH 未连接")
		}
		if err := rm.EnsureSFTP(); err != nil {
			return err
		}
		if rm.SFTPClient == nil {
			return fmt.Errorf("SFTP 未连接")
		}
		return fn(rm.SFTPClient, raw)
	})
}

func isMostlyUTF8(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	return utf8.Valid(b)
}

func copyLocalToSFTP(cli *sftp.Client, local, remote string) error {
	src, err := os.Open(local)
	if err != nil {
		return err
	}
	defer src.Close()
	remote = strings.ReplaceAll(remote, "\\", "/")
	remoteDir := path.Dir(remote)
	if remoteDir != "" && remoteDir != "." && remoteDir != "/" {
		_ = cli.MkdirAll(remoteDir)
	}
	partRemote := utils.RemoteUploadPartPath(remote)
	_ = cli.Remove(partRemote)
	dst, err := cli.Create(partRemote)
	if err != nil {
		return err
	}
	if _, err = utils.CopySFTPUpload(dst, src); err != nil {
		_ = dst.Close()
		_ = cli.Remove(partRemote)
		return err
	}
	_ = dst.Close()
	return utils.CommitRemoteUpload(cli, partRemote, remote)
}

func readSFTPFile(cli *sftp.Client, path string, max int64) ([]byte, error) {
	st, err := cli.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Size() > max {
		return nil, fmt.Errorf("文件 %d 字节超过上限 %d，请用 tail_log 或 ssh_exec", st.Size(), max)
	}
	f, err := cli.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, max+1))
}

func (s *Service) withLocalForward(alias, remoteHost string, remotePort int, fn func(localHost string, localPort int) error) error {
	return s.withSSH(alias, false, func(cli *machine.SSHClient, _ *define.Machine) error {
		rm := cli.SharedRemoteMachine()
		if rm == nil || rm.SSHClient == nil {
			return fmt.Errorf("SSH 未连接")
		}
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		defer l.Close()
		_, portStr, _ := net.SplitHostPort(l.Addr().String())
		port := atoi(portStr)
		done := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				wg.Add(1)
				go func(c net.Conn) {
					defer wg.Done()
					defer c.Close()
					select {
					case <-done:
						return
					default:
					}
					rc, err := rm.SSHClient.Dial("tcp", fmt.Sprintf("%s:%d", remoteHost, remotePort))
					if err != nil {
						return
					}
					defer rc.Close()
					go func() { _, _ = io.Copy(rc, c) }()
					_, _ = io.Copy(c, rc)
				}(c)
			}
		}()
		err = fn("127.0.0.1", port)
		close(done)
		_ = l.Close()
		wg.Wait()
		return err
	})
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}
