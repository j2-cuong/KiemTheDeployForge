package install

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type VolumeInfo struct {
	Root       string `json:"root"`
	FileSystem string `json:"fileSystem"`
	DriveType  uint32 `json:"driveType"`
}

func RequireFixedNTFS(path, label string) (VolumeInfo, error) {
	probe := path
	for {
		if _, err := os.Stat(probe); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return VolumeInfo{}, err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return VolumeInfo{}, fmt.Errorf("cannot resolve %s volume for %s", label, path)
		}
		probe = parent
	}
	probePointer, err := windows.UTF16PtrFromString(probe)
	if err != nil {
		return VolumeInfo{}, err
	}
	volumeBuffer := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumePathName(probePointer, &volumeBuffer[0], uint32(len(volumeBuffer))); err != nil {
		return VolumeInfo{}, fmt.Errorf("resolve %s volume: %w", label, err)
	}
	volumeRoot := windows.UTF16ToString(volumeBuffer)
	volumePointer, err := windows.UTF16PtrFromString(volumeRoot)
	if err != nil {
		return VolumeInfo{}, err
	}
	fileSystemBuffer := make([]uint16, 64)
	var flags uint32
	if err := windows.GetVolumeInformation(volumePointer, nil, 0, nil, nil, &flags, &fileSystemBuffer[0], uint32(len(fileSystemBuffer))); err != nil {
		return VolumeInfo{}, fmt.Errorf("read %s filesystem: %w", label, err)
	}
	fileSystem := windows.UTF16ToString(fileSystemBuffer)
	driveType := windows.GetDriveType(volumePointer)
	info := VolumeInfo{Root: volumeRoot, FileSystem: fileSystem, DriveType: driveType}
	if driveType != windows.DRIVE_FIXED {
		return info, fmt.Errorf("%s must be on a fixed local drive, got drive type %d at %s", label, driveType, volumeRoot)
	}
	if !strings.EqualFold(fileSystem, "NTFS") {
		return info, fmt.Errorf("%s must use NTFS, got %s at %s", label, fileSystem, volumeRoot)
	}
	if flags&windows.FILE_READ_ONLY_VOLUME != 0 {
		return info, fmt.Errorf("%s volume is read-only: %s", label, volumeRoot)
	}
	return info, nil
}

var (
	kernel32Install     = syscall.NewLazyDLL("kernel32.dll")
	getDiskFreeSpaceExW = kernel32Install.NewProc("GetDiskFreeSpaceExW")
	shell32Install      = syscall.NewLazyDLL("shell32.dll")
	shellExecuteW       = shell32Install.NewProc("ShellExecuteW")
	advapi32Install     = syscall.NewLazyDLL("advapi32.dll")
	allocateSID         = advapi32Install.NewProc("AllocateAndInitializeSid")
	checkTokenMember    = advapi32Install.NewProc("CheckTokenMembership")
	freeSID             = advapi32Install.NewProc("FreeSid")
)

type sidIdentifierAuthority struct {
	Value [6]byte
}

func AvailableBytes(path string) (uint64, error) {
	probe := path
	for {
		if _, err := os.Stat(probe); err == nil {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	p, err := syscall.UTF16PtrFromString(probe)
	if err != nil {
		return 0, err
	}
	var available, total, free uint64
	r1, _, callErr := getDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&available)),
		uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&free)),
	)
	if r1 == 0 {
		return 0, callErr
	}
	return available, nil
}

// SystemDriveFree reports the root and free space of the Windows system drive.
// The bot always writes its working data there, whatever install directory the
// operator picks, so this is checked independently of the target volume.
func SystemDriveFree() (string, uint64, error) {
	windowsDirectory, err := windows.GetSystemWindowsDirectory()
	if err != nil {
		return "", 0, fmt.Errorf("resolve the Windows system directory: %w", err)
	}
	root := filepath.VolumeName(windowsDirectory)
	if root == "" {
		return "", 0, fmt.Errorf("resolve the system drive for %s", windowsDirectory)
	}
	root += string(filepath.Separator)
	available, err := AvailableBytes(root)
	if err != nil {
		return root, 0, fmt.Errorf("read free space on %s: %w", root, err)
	}
	return root, available, nil
}

func IsAdministrator() bool {
	authority := sidIdentifierAuthority{Value: [6]byte{0, 0, 0, 0, 0, 5}}
	var administratorsSID uintptr
	created, _, _ := allocateSID.Call(
		uintptr(unsafe.Pointer(&authority)), 2, 0x20, 0x220,
		0, 0, 0, 0, 0, 0, uintptr(unsafe.Pointer(&administratorsSID)),
	)
	if created == 0 {
		return false
	}
	defer freeSID.Call(administratorsSID)
	var member int32
	checked, _, _ := checkTokenMember.Call(0, administratorsSID, uintptr(unsafe.Pointer(&member)))
	return checked != 0 && member != 0
}

func Elevate(executable string, args []string) error {
	verb, _ := syscall.UTF16PtrFromString("runas")
	file, err := syscall.UTF16PtrFromString(executable)
	if err != nil {
		return err
	}
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = syscall.EscapeArg(arg)
	}
	parameters, err := syscall.UTF16PtrFromString(strings.Join(quoted, " "))
	if err != nil {
		return err
	}
	directory, _ := syscall.UTF16PtrFromString(filepath.Dir(executable))
	r1, _, callErr := shellExecuteW.Call(
		0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(file)),
		uintptr(unsafe.Pointer(parameters)), uintptr(unsafe.Pointer(directory)), 1,
	)
	if r1 <= 32 {
		return fmt.Errorf("UAC elevation failed: %w", callErr)
	}
	return nil
}
