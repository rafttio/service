//go:build linux
// +build linux

package service

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"syscall"
)

const SIGTERM = syscall.Signal(15)

type selfDaemonizedLinuxService struct {
	i        Interface
	platform string
	*Config
}

func newSelfDaemonizedLinuxService(i Interface, platform string, c *Config) (Service, error) {
	s := &selfDaemonizedLinuxService{
		i:        i,
		platform: platform,
		Config:   c,
	}

	return s, nil
}

func envVarMapToStringArray(envVarMap map[string]string) []string {
	envStrings := make([]string, 0, len(envVarMap))
	for k, v := range envVarMap {
		envStrings = append(envStrings, k+"="+v)
	}

	return envStrings
}

func closeStandardPipes() error {
	pipes := []int{syscall.Stdin, syscall.Stdout, syscall.Stderr}
	for _, pipe := range pipes {
		if err := syscall.Close(pipe); err != nil {
			return err
		}
	}

	return nil
}

func (s *selfDaemonizedLinuxService) lockFilePath() string {
	path := s.Option.string(optionLockFile, "")
	if path == "" {
		path = fmt.Sprintf("/tmp/%s-service.lock", s.Name)
	}
	return path
}

func (s *selfDaemonizedLinuxService) Run() error { return nil } // TODO simply run the service

func (s *selfDaemonizedLinuxService) Start() error {
	envVarStrings := envVarMapToStringArray(s.EnvVars)
	envVarStrings = append(envVarStrings, os.Environ()...)

	executablePath, err := s.execPath()
	if err != nil {
		return err
	}

	args := []string{executablePath}
	args = append(args, s.Arguments...)

	lockFilePath := s.lockFilePath()

	// IMPORTANT: do not close this file, since it will release the flock
	fd, err := syscall.Open(lockFilePath, syscall.O_WRONLY|syscall.O_CREAT, 0644)
	if err != nil {
		return err
	}

	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("service already running")
	}

	ret, _, fdErr := syscall.Syscall(syscall.SYS_FORK, 0, 0, 0)
	if fdErr != 0 {
		return fdErr
	}

	if ret == 0 {
		// Running in child process. Calling setsid is required for the daemon to be an independent process.
		if err, _ := syscall.Setsid(); err < 0 {
			return fmt.Errorf("setsid error: %d", err)
		}

		// IMPORTANT: do not close this file, since it will release the flock
		f := os.NewFile(uintptr(fd), lockFilePath)
		if err := f.Truncate(0); err != nil {
			return err
		}
		if _, err := f.WriteString(strconv.Itoa(os.Getpid())); err != nil {
			return err
		}

		if err = closeStandardPipes(); err != nil {
			return err
		}

		if err = syscall.Exec(executablePath, args, envVarStrings); err != nil {
			return err
		}
	}

	return nil
}

func (s *selfDaemonizedLinuxService) Stop() error {
	lockFilePath := s.lockFilePath()

	fd, err := syscall.Open(lockFilePath, syscall.O_RDWR|syscall.O_CREAT, 0644)
	if err != nil {
		return err
	}

	// Expecting the flock to fail, since the service should be running and locking the file.
	// If the flock does not fail, it means that the service is not running.
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		return errors.New("service is not running")
	}

	f := os.NewFile(uintptr(fd), lockFilePath)
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}

	pidString := string(data)
	if pidString == "" {
		return errors.New("could not find service pid, service is probably not running")
	}

	pid, err := strconv.Atoi(pidString)
	if err != nil {
		return err
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		return errors.New("could not find service process, service is probably not running")
	}

	if err := process.Signal(SIGTERM); err != nil {
		return err
	}

	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		return errors.New("service is running, could not update lock file")
	}

	if err := f.Truncate(0); err != nil {
		return errors.New("failed to update lock file")
	}

	if err := f.Close(); err != nil {
		return err
	}

	return nil
}

func (s *selfDaemonizedLinuxService) Restart() error {
	if err := s.Stop(); err != nil {
		return err
	}

	return s.Start()
}

func (s *selfDaemonizedLinuxService) Install() error {
	// TODO create pidfile
	return nil
}

func (s *selfDaemonizedLinuxService) Uninstall() error {
	// TODO remove pidfile
	return nil
}

func (s *selfDaemonizedLinuxService) Logger(errs chan<- error) (Logger, error) {
	if system.Interactive() {
		return ConsoleLogger, nil
	}
	return s.SystemLogger(errs)
}

func (s *selfDaemonizedLinuxService) SystemLogger(errs chan<- error) (Logger, error) {
	return newSysLogger(s.Name, errs)
}

func (s *selfDaemonizedLinuxService) String() string {
	if len(s.DisplayName) > 0 {
		return s.DisplayName
	}
	return s.Name
}

func (s *selfDaemonizedLinuxService) Platform() string {
	return s.platform
}

func (s *selfDaemonizedLinuxService) Status() (Status, error) { return 0, nil } // TODO check running processes and pidfile
