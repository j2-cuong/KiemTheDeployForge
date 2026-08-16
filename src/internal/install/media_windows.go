package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kiemthedeployforge/internal/release"
	"kiemthedeployforge/internal/sfx"
	"kiemthedeployforge/internal/winprocess"
)

const offlineISOName = "KiemTheServer-Offline.iso"

type installMedia struct {
	bootstrap *sfx.Package
	payload   *sfx.Package
	setupPath string
	mediaPath string
	isoPath   string
	mounted   bool
	dismount  func(string) error
}

func (m *installMedia) sourcePath() string {
	if m.isoPath != "" {
		return m.isoPath
	}
	return m.mediaPath
}

func openInstallMedia(ctx context.Context, setupPath string) (*installMedia, error) {
	absoluteSetup, err := filepath.Abs(setupPath)
	if err != nil {
		return nil, err
	}
	bootstrap, err := sfx.OpenManifest(absoluteSetup)
	if err != nil {
		return nil, fmt.Errorf("read Setup.exe release pin: %w", err)
	}
	media := &installMedia{bootstrap: bootstrap, setupPath: absoluteSetup}
	directPayload := filepath.Join(filepath.Dir(absoluteSetup), sfx.PayloadFileName)
	if info, statErr := os.Stat(directPayload); statErr == nil && info.Mode().IsRegular() {
		if err := media.openPayload(directPayload); err != nil {
			return media, err
		}
		return media, nil
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return media, statErr
	}

	isoPath := filepath.Join(filepath.Dir(absoluteSetup), offlineISOName)
	info, err := os.Stat(isoPath)
	if err != nil || !info.Mode().IsRegular() {
		if err == nil {
			err = fmt.Errorf("not a regular file")
		}
		return media, fmt.Errorf("offline payload not found: keep Setup.exe next to %s (%v)", offlineISOName, err)
	}
	mediaRoot, mounted, err := mountOfflineISO(ctx, isoPath)
	media.isoPath = isoPath
	media.mounted = mounted
	if err != nil {
		return media, err
	}
	if err := media.openPayload(filepath.Join(mediaRoot, sfx.PayloadFileName)); err != nil {
		return media, err
	}
	manifest, digest, err := release.Load(mediaRoot, bootstrap.ManifestSHA256, false)
	if err != nil {
		return media, fmt.Errorf("verify ISO release manifest: %w", err)
	}
	if manifest.ReleaseID != media.payload.Manifest.ReleaseID || !strings.EqualFold(digest, media.payload.ManifestSHA256) {
		return media, fmt.Errorf("ISO manifest and payload package identify different releases")
	}
	return media, nil
}

func (m *installMedia) openPayload(path string) error {
	payload, err := sfx.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", sfx.PayloadFileName, err)
	}
	m.payload = payload
	m.mediaPath = path
	if !strings.EqualFold(payload.ManifestSHA256, m.bootstrap.ManifestSHA256) {
		return fmt.Errorf("%s does not match this Setup.exe release pin", sfx.PayloadFileName)
	}
	return nil
}

func (m *installMedia) Close() error {
	if m == nil {
		return nil
	}
	var closeErrors []error
	if m.payload != nil {
		if err := m.payload.Close(); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("close payload package: %w", err))
		} else {
			m.payload = nil
		}
	}
	if m.bootstrap != nil {
		if err := m.bootstrap.Close(); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("close Setup.exe release pin: %w", err))
		} else {
			m.bootstrap = nil
		}
	}
	if m.mounted && m.isoPath != "" {
		dismount := m.dismount
		if dismount == nil {
			dismount = dismountOfflineISO
		}
		if err := dismount(m.isoPath); err != nil {
			closeErrors = append(closeErrors, err)
		} else {
			m.mounted = false
		}
	}
	return errors.Join(closeErrors...)
}

func mountOfflineISO(ctx context.Context, isoPath string) (string, bool, error) {
	command, err := winprocess.PowerShellCommandContext(ctx, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", mountISOSource)
	if err != nil {
		return "", false, err
	}
	winprocess.Hide(command)
	command.Env = append(os.Environ(), "KIEMTHE_OFFLINE_ISO="+isoPath)
	output, commandErr := command.CombinedOutput()
	var root string
	var owned bool
	for _, raw := range strings.Split(string(output), "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "MEDIA_ROOT="):
			root = strings.TrimSpace(strings.TrimPrefix(line, "MEDIA_ROOT="))
		case line == "MOUNTED_BY_SETUP=1":
			owned = true
		}
	}
	if commandErr != nil {
		return root, owned, fmt.Errorf("mount offline ISO: %w (%s)", commandErr, strings.TrimSpace(string(output)))
	}
	if root == "" {
		return "", owned, fmt.Errorf("mounted ISO did not expose a readable UDF volume")
	}
	return filepath.Clean(root), owned, nil
}

func dismountOfflineISO(isoPath string) error {
	var failures []error
	for attempt := 1; attempt <= 4; attempt++ {
		if err := dismountOfflineISOOnce(isoPath); err == nil {
			return nil
		} else {
			failures = append(failures, fmt.Errorf("attempt %d: %w", attempt, err))
		}
		if attempt < 4 {
			time.Sleep(time.Duration(attempt) * 350 * time.Millisecond)
		}
	}
	return errors.Join(failures...)
}

func dismountOfflineISOOnce(isoPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command, err := winprocess.PowerShellCommandContext(ctx, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command",
		`$ErrorActionPreference='Stop'; $p=$env:KIEMTHE_OFFLINE_ISO; if($p){$image=Get-DiskImage -ImagePath $p -ErrorAction SilentlyContinue; if($image -and $image.Attached){Dismount-DiskImage -ImagePath $p -ErrorAction Stop|Out-Null}}`)
	if err != nil {
		return err
	}
	winprocess.Hide(command)
	command.Env = append(os.Environ(), "KIEMTHE_OFFLINE_ISO="+isoPath)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("dismount offline ISO: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

const mountISOSource = `$ErrorActionPreference='Stop'
$path=$env:KIEMTHE_OFFLINE_ISO
if(-not $path){throw 'KIEMTHE_OFFLINE_ISO is missing'}
$image=Get-DiskImage -ImagePath $path -ErrorAction SilentlyContinue
$owned=$false
if(-not $image -or -not $image.Attached){
 $image=Mount-DiskImage -ImagePath $path -PassThru -ErrorAction Stop
 $owned=$true
}
Write-Output ('MOUNTED_BY_SETUP='+[int]$owned)
$deadline=[DateTime]::UtcNow.AddSeconds(20)
do {
 $volume=$image|Get-Volume|Where-Object {$_.DriveLetter -and $_.FileSystem -eq 'UDF'}|Select-Object -First 1
 if($volume){break}
 Start-Sleep -Milliseconds 250
} while([DateTime]::UtcNow -lt $deadline)
if(-not $volume -or -not $volume.DriveLetter){
 throw 'ISO has no readable UDF volume'
}
Write-Output ('MEDIA_ROOT='+$volume.DriveLetter+':\')
`
