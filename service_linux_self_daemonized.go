//go:build linux
// +build linux

package service

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

const DUMMY_SIGNAL = syscall.Signal(0)

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

func redirectStandardFdsToDevNull() error {
	devNull, err := syscall.Open(os.DevNull, syscall.O_WRONLY, 0644)
	if err != nil {
		return err
	}

	fds := []int{syscall.Stdin, syscall.Stdout, syscall.Stderr}
	for _, fd := range fds {
		if err := syscall.Dup2(devNull, fd); err != nil {
			return err
		}
	}

	return nil
}

func (s *selfDaemonizedLinuxService) lockFilePath() string {
	path := s.Option.string(optionLockFile, "")
	if path == "" {
		dirname, err := os.UserHomeDir()
		if err != nil {
			dirname = "/tmp"
		}
		filename := fmt.Sprintf("%s-service.lock", s.Name)
		path = filepath.Join(dirname, filename)
	}
	return path
}

func (s *selfDaemonizedLinuxService) prepareExecInputs() (executablePath string, args []string, envVarStrings []string, err error) {
	executablePath, err = s.execPath()
	if err != nil {
		return
	}

	args = []string{executablePath}
	args = append(args, s.Arguments...)

	envVarStrings = envVarMapToStringArray(s.EnvVars)
	envVarStrings = append(envVarStrings, os.Environ()...)

	return
}

func (s *selfDaemonizedLinuxService) lockForServiceOperations() (fd int, err error) {
	otherLockFile := "/tmp/lalala.lock" // TODO rename this
	fd, err = syscall.Open(otherLockFile, syscall.O_WRONLY|syscall.O_CREAT, 0644)
	if err != nil {
		return -1, err
	}

	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		return -1, err
	}

	return fd, nil
}

func (s *selfDaemonizedLinuxService) unlockForServiceOperations(fd int) error {
	return syscall.Close(fd)
}

func (s *selfDaemonizedLinuxService) Run() error {
	executablePath, args, envVarStrings, err := s.prepareExecInputs()
	if err != nil {
		return err
	}

	lockFilePath := s.lockFilePath()

	// IMPORTANT: do not close this file, since it will release the flock
	fd, err := syscall.Open(lockFilePath, syscall.O_WRONLY|syscall.O_CREAT, 0644)
	if err != nil {
		return err
	}

	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if err.(syscall.Errno) == syscall.EWOULDBLOCK {
			return errors.New("service already running")
		}

		return err
	}

	if err = syscall.Exec(executablePath, args, envVarStrings); err != nil {
		return err
	}

	return nil
}

func (s *selfDaemonizedLinuxService) Start() error {
	serviceOperationsLockFd, err := s.lockForServiceOperations()
	if err != nil {
		return err
	}

	executablePath, args, envVarStrings, err := s.prepareExecInputs()
	if err != nil {
		return err
	}

	lockFilePath := s.lockFilePath()

	// IMPORTANT: do not close this file, since it will release the flock
	fd, err := syscall.Open(lockFilePath, syscall.O_WRONLY|syscall.O_CREAT, 0644)
	if err != nil {
		return err
	}

	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if err.(syscall.Errno) == syscall.EWOULDBLOCK {
			return errors.New("service already running")
		}

		return err
	}

	ret, _, fdErr := syscall.Syscall(syscall.SYS_FORK, 0, 0, 0)
	if fdErr != 0 {
		return fdErr
	}

	if ret == 0 {
		// Running in child process. Calling setsid is required for the daemon to be an independent process.
		if err, _ := syscall.Setsid(); err < 0 {
			os.Exit(2)
		}

		// IMPORTANT: do not close this file, since it will release the flock
		f := os.NewFile(uintptr(fd), lockFilePath)
		if err := f.Truncate(0); err != nil {
			os.Exit(2)
		}
		if _, err := f.WriteString(strconv.Itoa(os.Getpid())); err != nil {
			os.Exit(2)
		}
		if err := f.Sync(); err != nil {
			os.Exit(2)
		}

		if err := s.unlockForServiceOperations(serviceOperationsLockFd); err != nil {
			os.Exit(2)
		}

		if err = redirectStandardFdsToDevNull(); err != nil {
			os.Exit(2)
		}

		if err = syscall.Exec(executablePath, args, envVarStrings); err != nil {
			os.Exit(2)
		}
	} else {
		defer s.unlockForServiceOperations(serviceOperationsLockFd)

		// Closing the lockfile FD here so that it will only stay open in the child process from now on, to avoid
		// holding the lock more than necessary
		defer syscall.Close(fd)
	}

	return nil
}

func (s *selfDaemonizedLinuxService) Stop() error {
	serviceOperationsLockFd, err := s.lockForServiceOperations()
	if err != nil {
		return err
	}
	defer s.unlockForServiceOperations(serviceOperationsLockFd)

	lockFilePath := s.lockFilePath()

	fd, err := syscall.Open(lockFilePath, syscall.O_RDWR, 0644)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // Lock file does not exist - service is not running
		}
		return err
	}
	defer syscall.Close(fd)

	// Expecting the flock to fail, since the service should be running and locking the file.
	// If the flock does not fail, it means that the service is not running.
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		return nil // Service is not running
	}

	f := os.NewFile(uintptr(fd), lockFilePath)
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}

	pidString := string(data)
	if pidString == "" {
		return nil // Service is probably not running
	}

	pid, err := strconv.Atoi(pidString)
	if err != nil {
		return err
	}

	// For linux, os.FindProcess always return a process
	process, _ := os.FindProcess(pid)
	if err := process.Signal(DUMMY_SIGNAL); err != nil {
		return nil // Could not find service process, service is probably not running
	}

	if err := process.Signal(unix.SIGTERM); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return nil // Could not find service process, service is probably not running
		}

		return err
	}

	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		if err.(syscall.Errno) == syscall.EWOULDBLOCK {
			return errors.New("service is running, could not update lock file")
		}

		return err
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
	lockFilePath := s.lockFilePath()

	if _, err := os.Stat(lockFilePath); !errors.Is(err, os.ErrNotExist) {
		return errors.New("service already installed")
	}

	f, err := os.Create(lockFilePath)
	if err != nil {
		return err
	}
	return f.Close()
}

func (s *selfDaemonizedLinuxService) Uninstall() error {
	lockFilePath := s.lockFilePath()

	if _, err := os.Stat(lockFilePath); errors.Is(err, os.ErrNotExist) {
		return errors.New("service not installed")
	}

	return os.Remove(lockFilePath)
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

func (s *selfDaemonizedLinuxService) Status() (Status, error) {
	serviceOperationsLockFd, err := s.lockForServiceOperations()
	if err != nil {
		return StatusUnknown, err
	}
	defer s.unlockForServiceOperations(serviceOperationsLockFd)

	lockFilePath := s.lockFilePath()
	fd, err := syscall.Open(lockFilePath, syscall.O_RDONLY, 0644)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return StatusStopped, nil
		}
		return StatusUnknown, err
	}

	// Expecting the flock to fail, since the service should be running and locking the file.
	// If the flock does not fail, it means that the service is not running.
	err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)

	if err == nil {
		return StatusStopped, nil
	}

	if err.(syscall.Errno) != syscall.EWOULDBLOCK {
		return StatusUnknown, err
	}

	f := os.NewFile(uintptr(fd), lockFilePath)
	data, err := io.ReadAll(f)
	if err != nil {
		return StatusUnknown, err
	}

	pidString := string(data)
	if pidString == "" {
		return StatusStopped, nil // Service is probably not running
	}

	pid, err := strconv.Atoi(pidString)
	if err != nil {
		return StatusUnknown, err
	}

	// For linux, os.FindProcess always return a process
	process, _ := os.FindProcess(pid)
	if err := process.Signal(DUMMY_SIGNAL); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return StatusStopped, nil
		}

		return StatusUnknown, err
	}

	return StatusRunning, nil
}
