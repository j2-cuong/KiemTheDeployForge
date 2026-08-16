package install

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"golang.org/x/sys/windows"
)

const serviceMutexName = `Global\KiemTheDeployForge-KiemTheServer-MySQL`

type installLocks struct {
	locks []*namedMutex
}

type namedMutex struct {
	release chan struct{}
	done    chan struct{}
	once    sync.Once
	errMu   sync.Mutex
	err     error
}

type mutexResult struct {
	mutex *namedMutex
	err   error
}

func acquireInstallLocks(installRoot string) (*installLocks, error) {
	normalized := strings.ToLower(filepath.Clean(installRoot))
	digest := sha256.Sum256([]byte(normalized))
	names := []string{
		serviceMutexName,
		`Global\KiemTheDeployForge-InstallRoot-` + hex.EncodeToString(digest[:16]),
	}
	set := &installLocks{}
	for _, name := range names {
		lock, err := acquireNamedMutex(name)
		if err != nil {
			_ = set.Close()
			return nil, err
		}
		set.locks = append(set.locks, lock)
	}
	return set, nil
}

func acquireNamedMutex(name string) (*namedMutex, error) {
	result := make(chan mutexResult, 1)
	lock := &namedMutex{release: make(chan struct{}), done: make(chan struct{})}
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		namePointer, err := windows.UTF16PtrFromString(name)
		if err != nil {
			result <- mutexResult{err: err}
			return
		}
		handle, err := windows.CreateMutex(nil, false, namePointer)
		if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			result <- mutexResult{err: fmt.Errorf("create installer mutex %s: %w", name, err)}
			return
		}
		event, waitErr := windows.WaitForSingleObject(handle, 0)
		if waitErr != nil || (event != windows.WAIT_OBJECT_0 && event != windows.WAIT_ABANDONED) {
			windows.CloseHandle(handle)
			if event == uint32(windows.WAIT_TIMEOUT) {
				result <- mutexResult{err: fmt.Errorf("another Kiem The installation or verification is already running")}
				return
			}
			result <- mutexResult{err: fmt.Errorf("acquire installer mutex %s: event=%d error=%w", name, event, waitErr)}
			return
		}
		result <- mutexResult{mutex: lock}
		<-lock.release
		releaseErr := windows.ReleaseMutex(handle)
		closeErr := windows.CloseHandle(handle)
		lock.errMu.Lock()
		if releaseErr != nil {
			lock.err = releaseErr
		} else {
			lock.err = closeErr
		}
		lock.errMu.Unlock()
		close(lock.done)
	}()
	acquired := <-result
	return acquired.mutex, acquired.err
}

func (l *namedMutex) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() { close(l.release) })
	<-l.done
	l.errMu.Lock()
	defer l.errMu.Unlock()
	return l.err
}

func (l *installLocks) Close() error {
	if l == nil {
		return nil
	}
	var firstErr error
	for index := len(l.locks) - 1; index >= 0; index-- {
		if err := l.locks[index].Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	l.locks = nil
	return firstErr
}
