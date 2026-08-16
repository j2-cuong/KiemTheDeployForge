package iso

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"kiemthedeployforge/internal/winprocess"
)

type ProgressFunc func(percent int, stage, detail string)

const (
	WorkMarkerName    = ".kiemthedeployforge-iso-work"
	WorkMarkerContent = "KiemTheDeployForge\r\nkind=iso-work\r\n"
)

type Options struct {
	SetupPath      string
	SetupSHA256    string
	PayloadPath    string
	PayloadSHA256  string
	Manifest       []byte
	ManifestSHA256 string
	ReadmePath     string
	OutputPath     string
}

func Create(ctx context.Context, options Options, report ProgressFunc) (isoHash string, err error) {
	workRoot, err := os.MkdirTemp(filepath.Dir(options.OutputPath), ".iso-build-")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(workRoot, WorkMarkerName), []byte(WorkMarkerContent), 0o600); err != nil {
		_ = os.RemoveAll(workRoot)
		return "", fmt.Errorf("mark ISO work directory: %w", err)
	}
	workRootCleaned := false
	defer func() {
		if !workRootCleaned {
			err = errors.Join(err, cleanupWorkRoot(workRoot))
		}
	}()
	stage := filepath.Join(workRoot, "stage")
	if err := os.Mkdir(stage, 0o700); err != nil {
		return "", err
	}
	link := func(source, name string) error {
		if err := os.Link(source, filepath.Join(stage, name)); err != nil {
			return fmt.Errorf("create NTFS hard link for ISO staging (%s): %w", name, err)
		}
		return nil
	}
	if err := link(options.SetupPath, "Setup.exe"); err != nil {
		return "", err
	}
	if err := link(options.PayloadPath, "Payload.ktpkg"); err != nil {
		return "", err
	}
	readmeTarget := filepath.Join(stage, "README.txt")
	readme, err := os.ReadFile(options.ReadmePath)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(readmeTarget, readme, 0o600); err != nil {
		return "", err
	}
	manifestTarget := filepath.Join(stage, "manifests", "release.json")
	if err := os.MkdirAll(filepath.Dir(manifestTarget), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(manifestTarget, options.Manifest, 0o600); err != nil {
		return "", err
	}
	setupInfo, err := os.Stat(options.SetupPath)
	if err != nil {
		return "", err
	}
	payloadInfo, err := os.Stat(options.PayloadPath)
	if err != nil {
		return "", err
	}
	mediaSize := setupInfo.Size() + payloadInfo.Size() + int64(len(readme)) + int64(len(options.Manifest))
	extension := filepath.Ext(options.OutputPath)
	if !strings.EqualFold(extension, ".iso") {
		return "", fmt.Errorf("ISO output must use the .iso extension: %s", options.OutputPath)
	}
	reservation, err := os.CreateTemp(filepath.Dir(options.OutputPath), "."+strings.TrimSuffix(filepath.Base(options.OutputPath), extension)+"-*.building.iso")
	if err != nil {
		return "", err
	}
	tempISO := reservation.Name()
	published := false
	defer func() {
		if !published {
			err = errors.Join(err, cleanupBuildISO(tempISO))
		}
	}()
	if err := reservation.Close(); err != nil {
		return "", err
	}
	if err := os.Remove(tempISO); err != nil {
		return "", err
	}
	scriptPath := filepath.Join(workRoot, "build-iso.ps1")
	if err := os.WriteFile(scriptPath, []byte(powerShellSource), 0o600); err != nil {
		return "", err
	}
	command, err := winprocess.PowerShellCommandContext(ctx, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", scriptPath,
		"-SourceDirectory", stage,
		"-OutputIso", tempISO,
		"-ExpectedSetupSize", strconv.FormatInt(setupInfo.Size(), 10),
		"-ExpectedSetupHash", options.SetupSHA256,
		"-ExpectedPayloadSize", strconv.FormatInt(payloadInfo.Size(), 10),
		"-ExpectedPayloadHash", options.PayloadSHA256,
		"-ExpectedManifestSize", strconv.Itoa(len(options.Manifest)),
		"-ExpectedManifestHash", options.ManifestSHA256,
		"-ExpectedMediaSize", strconv.FormatInt(mediaSize, 10))
	if err != nil {
		return "", err
	}
	winprocess.Hide(command)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return "", err
	}
	command.Stderr = command.Stdout
	if err := command.Start(); err != nil {
		return "", err
	}
	var outputLines []string
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		outputLines = append(outputLines, line)
		if strings.HasPrefix(line, "PERCENT=") {
			value, _ := strconv.Atoi(strings.TrimPrefix(line, "PERCENT="))
			if report != nil {
				report(value, "Build ISO UDF", "Writing and verifying offline ISO")
			}
		}
		if strings.HasPrefix(line, "ISO_SHA256=") {
			isoHash = strings.TrimPrefix(line, "ISO_SHA256=")
		}
	}
	scanErr := scanner.Err()
	waitErr := command.Wait()
	if waitErr != nil || scanErr != nil {
		if waitErr != nil {
			return "", fmt.Errorf("UDF ISO build failed: %w (%s)", waitErr, strings.Join(outputLines, " | "))
		}
		return "", fmt.Errorf("read UDF ISO builder output: %w", scanErr)
	}
	if isoHash == "" {
		return "", fmt.Errorf("ISO builder did not return a checksum")
	}
	if err := cleanupWorkRoot(workRoot); err != nil {
		return "", err
	}
	workRootCleaned = true
	if err := os.Rename(tempISO, options.OutputPath); err != nil {
		return "", fmt.Errorf("publish verified ISO: %w", err)
	}
	published = true
	return isoHash, nil
}

func cleanupWorkRoot(path string) error {
	return cleanupWorkRootWith(path, os.RemoveAll, time.Sleep)
}

func cleanupWorkRootWith(path string, remove func(string) error, sleep func(time.Duration)) error {
	var failures []error
	for attempt := 1; attempt <= 4; attempt++ {
		if err := remove(path); err == nil || os.IsNotExist(err) {
			return nil
		} else {
			failures = append(failures, fmt.Errorf("remove ISO work directory %s (attempt %d): %w", path, attempt, err))
		}
		if attempt < 4 {
			sleep(time.Duration(attempt) * 250 * time.Millisecond)
		}
	}
	return errors.Join(failures...)
}

func cleanupBuildISO(path string) error {
	return cleanupBuildISOWith(path, dismountBuildISO, os.Remove)
}

// RemoveStaleBuildISO dismounts and deletes only a caller-validated partial ISO path.
func RemoveStaleBuildISO(path string) error {
	return cleanupBuildISO(path)
}

func cleanupBuildISOWith(path string, dismount func(string) error, remove func(string) error) error {
	var cleanupErrors []error
	if err := dismount(path); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	if err := remove(path); err != nil && !os.IsNotExist(err) {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("remove incomplete ISO %s: %w", path, err))
	}
	return errors.Join(cleanupErrors...)
}

func dismountBuildISO(path string) error {
	var failures []error
	for attempt := 1; attempt <= 3; attempt++ {
		if err := dismountBuildISOOnce(path); err == nil {
			return nil
		} else {
			failures = append(failures, fmt.Errorf("attempt %d: %w", attempt, err))
		}
		if attempt < 3 {
			time.Sleep(time.Duration(attempt) * 300 * time.Millisecond)
		}
	}
	return errors.Join(failures...)
}

func dismountBuildISOOnce(path string) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command, err := winprocess.PowerShellCommandContext(cleanupCtx, "-NoProfile", "-NonInteractive", "-Command",
		`$ErrorActionPreference='Stop'; $p=$env:KIEMTHE_ISO_CLEANUP_PATH; if($p){$image=Get-DiskImage -ImagePath $p -ErrorAction SilentlyContinue; if($image -and $image.Attached){Dismount-DiskImage -ImagePath $p -ErrorAction Stop|Out-Null}}`)
	if err != nil {
		return err
	}
	winprocess.Hide(command)
	command.Env = append(os.Environ(), "KIEMTHE_ISO_CLEANUP_PATH="+path)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("dismount incomplete ISO %s: %w (%s)", path, err, strings.TrimSpace(string(output)))
	}
	return nil
}

const powerShellSource = `#requires -version 5.1
param(
 [string]$SourceDirectory,[string]$OutputIso,
 [long]$ExpectedSetupSize,[string]$ExpectedSetupHash,
 [long]$ExpectedPayloadSize,[string]$ExpectedPayloadHash,
 [long]$ExpectedManifestSize,[string]$ExpectedManifestHash,
 [long]$ExpectedMediaSize
)
$ErrorActionPreference='Stop'
$source=@'
using System;
using System.IO;
using System.Runtime.InteropServices;
using System.Runtime.InteropServices.ComTypes;
using System.Security.Cryptography;
public static class ForgeIsoWriter {
 public static string Write(object source,string path,int blockSize,long blocks,Action<long,long> progress) {
  IStream input=(IStream)source; long total=(long)blockSize*blocks,written=0; byte[] buffer=new byte[4194304]; IntPtr p=Marshal.AllocHGlobal(4);
  try { using(FileStream output=new FileStream(path,FileMode.CreateNew,FileAccess.Write,FileShare.None,buffer.Length,FileOptions.SequentialScan)) using(SHA256 sha=SHA256.Create()) {
   while(written<total){ int request=(int)Math.Min(buffer.Length,total-written); Marshal.WriteInt32(p,0); input.Read(buffer,request,p); int read=Marshal.ReadInt32(p); if(read<=0)break; output.Write(buffer,0,read); sha.TransformBlock(buffer,0,read,null,0); written+=read; progress(written,total); }
   sha.TransformFinalBlock(new byte[0],0,0); output.Flush(true); if(written!=total)throw new IOException("ISO stream ended early"); return BitConverter.ToString(sha.Hash).Replace("-","").ToLowerInvariant();
  }} finally { Marshal.FreeHGlobal(p); }
 }
 public static string HashFile(string path,Action<long,long> progress) {
  using(FileStream input=new FileStream(path,FileMode.Open,FileAccess.Read,FileShare.Read,4194304,FileOptions.SequentialScan)) using(SHA256 sha=SHA256.Create()) {
   long total=input.Length,done=0; byte[] buffer=new byte[4194304]; int read;
   while((read=input.Read(buffer,0,buffer.Length))>0){sha.TransformBlock(buffer,0,read,null,0);done+=read;progress(done,total);}
   sha.TransformFinalBlock(new byte[0],0,0); return BitConverter.ToString(sha.Hash).Replace("-","").ToLowerInvariant();
  }
 }
}
'@
Add-Type -TypeDefinition $source -Language CSharp
$image=New-Object -ComObject IMAPI2FS.MsftFileSystemImage
$image.FileSystemsToCreate=4
$image.UDFRevision=0x102
$requiredMediaBlocks=[long][Math]::Ceiling($ExpectedMediaSize/2048.0)+65536
if($ExpectedMediaSize-lt 0-or $requiredMediaBlocks-gt [int]::MaxValue){throw 'Offline payload is too large for the IMAPI2 UDF block limit'}
$image.FreeMediaBlocks=[int]$requiredMediaBlocks
$image.VolumeName='KIEMTHE_SERVER'
$image.Root.AddTree($SourceDirectory,$false)
$result=$image.CreateResultImage()
$last=-1
$callback=[Action[long,long]]{param([long]$written,[long]$total) $pct=[int][Math]::Floor(($written/[double]$total)*90); if($pct-ne $script:last){$script:last=$pct; Write-Output "PERCENT=$pct"}}
$hash=[ForgeIsoWriter]::Write($result.ImageStream,$OutputIso,[int]$result.BlockSize,[long]$result.TotalBlocks,$callback)
Write-Output 'PERCENT=91'
$mounted=$null
$verificationError=$null
$dismountError=$null
try {
 $mounted=Mount-DiskImage -ImagePath $OutputIso -PassThru -ErrorAction Stop
 $deadline=[DateTime]::UtcNow.AddSeconds(20)
 do {
  $volume=$mounted|Get-Volume|Where-Object {$_.DriveLetter -and $_.FileSystem -eq 'UDF'}|Select-Object -First 1
  if($volume){break}
  Start-Sleep -Milliseconds 250
 } while([DateTime]::UtcNow -lt $deadline)
 if(-not $volume -or -not $volume.DriveLetter -or $volume.FileSystem-ne'UDF'){throw 'Mounted ISO is not UDF'}
 $root=$volume.DriveLetter+':\'
 $names=@(Get-ChildItem -LiteralPath $root -Force | ForEach-Object {$_.Name})
 $expected=@('Setup.exe','Payload.ktpkg','README.txt','manifests')
 if($names.Count-ne$expected.Count -or @(Compare-Object $names $expected).Count-ne 0){throw ('Unexpected ISO root entries: '+($names-join', '))}
 $setup=Join-Path $root 'Setup.exe'
 $payload=Join-Path $root 'Payload.ktpkg'
 $manifest=Join-Path $root 'manifests\release.json'
 if((Get-Item -LiteralPath $setup).Length-ne$ExpectedSetupSize){throw 'ISO Setup.exe size mismatch'}
 if((Get-Item -LiteralPath $payload).Length-ne$ExpectedPayloadSize){throw 'ISO payload size mismatch'}
 if((Get-Item -LiteralPath $manifest).Length-ne$ExpectedManifestSize){throw 'ISO manifest size mismatch'}
 if(-not(Test-Path -LiteralPath (Join-Path $root 'README.txt') -PathType Leaf)){throw 'ISO is missing README.txt'}
 if((Get-FileHash -LiteralPath $setup -Algorithm SHA256).Hash.ToLowerInvariant()-ne$ExpectedSetupHash.ToLowerInvariant()){throw 'ISO Setup.exe hash mismatch'}
 if((Get-FileHash -LiteralPath $manifest -Algorithm SHA256).Hash.ToLowerInvariant()-ne$ExpectedManifestHash.ToLowerInvariant()){throw 'ISO manifest hash mismatch'}
 Write-Output 'PERCENT=92'
 $verifyLast=-1
 $verifyCallback=[Action[long,long]]{param([long]$done,[long]$total) $pct=92+[int][Math]::Floor(($done/[double]$total)*8); if($pct-ne$script:verifyLast){$script:verifyLast=$pct; Write-Output "PERCENT=$pct"}}
 $payloadHash=[ForgeIsoWriter]::HashFile($payload,$verifyCallback)
 if($payloadHash-ne$ExpectedPayloadHash.ToLowerInvariant()){throw 'ISO payload hash mismatch'}
 Write-Output 'PERCENT=100'
} catch {
 $verificationError=$_
} finally {
 if($mounted){
  try {Dismount-DiskImage -ImagePath $OutputIso -ErrorAction Stop|Out-Null}
  catch {$dismountError=$_}
 }
}
if($verificationError){
 if($dismountError){throw ('ISO verification failed: '+$verificationError.Exception.Message+'; dismount failed: '+$dismountError.Exception.Message)}
 throw $verificationError
}
if($dismountError){throw ('ISO verification succeeded but dismount failed: '+$dismountError.Exception.Message)}
Write-Output "ISO_SHA256=$hash"
`
