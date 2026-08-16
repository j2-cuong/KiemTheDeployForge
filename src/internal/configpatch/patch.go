package configpatch

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"kiemthedeployforge/internal/winfile"
)

type Rule struct {
	RelativePath string
	Section      string
	Key          string
	Value        string
}

// GameServerCount is the number of GameServer instances whose [GameServer]
// InIp and OutIp keys are rewritten to the detected LAN IPv4.
const GameServerCount = 9

// ServerTargetRoot is the installed name of the server tree. The bot reads it
// through BOT_GAMESERVER_DIR, so both must stay in sync.
const ServerTargetRoot = "Server"

func LANRules(ip string) []Rule {
	rules := make([]Rule, 0, GameServerCount*2+3)
	for i := 1; i <= GameServerCount; i++ {
		path := fmt.Sprintf("%s/Gameserver/GS%dservercfg.ini", ServerTargetRoot, i)
		rules = append(rules,
			Rule{path, "GameServer", "InIp", ip},
			Rule{path, "GameServer", "OutIp", ip},
		)
	}
	rules = append(rules,
		Rule{"Client/user/uicommon.ini", "Region_0", "1_Address", ip},
		Rule{"Client/user/serverlistdebug.ini", "Region_1", "1_Address", ip},
		Rule{"Client/AutoPk/serverlist.ini", "Region_0", "0_Address", ip},
	)
	return rules
}

func Apply(root string, rules []Rule, backupRoot string) error {
	for _, rule := range rules {
		path := filepath.Join(root, filepath.FromSlash(rule.RelativePath))
		backup := filepath.Join(backupRoot, filepath.FromSlash(rule.RelativePath))
		if err := backupFile(path, backup); err != nil {
			return fmt.Errorf("backup %s: %w", rule.RelativePath, err)
		}
		if err := PatchINI(path, rule.Section, rule.Key, rule.Value); err != nil {
			return fmt.Errorf("patch %s [%s] %s: %w", rule.RelativePath, rule.Section, rule.Key, err)
		}
	}
	return nil
}

func Verify(root string, rules []Rule) error {
	for _, rule := range rules {
		path := filepath.Join(root, filepath.FromSlash(rule.RelativePath))
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", rule.RelativePath, err)
		}
		patched, matches, err := patchBytes(raw, rule.Section, rule.Key, rule.Value)
		if err != nil {
			return err
		}
		if matches != 1 || !bytes.Equal(raw, patched) {
			return fmt.Errorf("config drift in %s [%s] %s", rule.RelativePath, rule.Section, rule.Key)
		}
	}
	return nil
}

// ValidateTarget checks that a source INI has one unambiguous patch location
// without changing the file.
func ValidateTarget(path, section, key string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	_, matches, err := patchBytes(raw, section, key, "127.0.0.1")
	if err != nil {
		return err
	}
	if matches != 1 {
		return fmt.Errorf("expected exactly one match, got %d", matches)
	}
	return nil
}

func PatchINI(path, section, key, value string) error {
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("invalid multiline value")
	}
	original, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	patched, matches, err := patchBytes(original, section, key, value)
	if err != nil {
		return err
	}
	if matches != 1 {
		return fmt.Errorf("expected exactly one match, got %d", matches)
	}
	return replaceContentPreserving(path, info, patched)
}

// replaceContentPreserving rewrites path with patched bytes through a temporary
// file on the same volume, keeping the original timestamp and attributes.
func replaceContentPreserving(path string, info os.FileInfo, patched []byte) error {
	attrs, err := winfile.Attributes(path)
	if err != nil {
		return err
	}
	temp := path + fmt.Sprintf(".ktpatch-%d", os.Getpid())
	file, err := os.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	keep := true
	defer func() {
		file.Close()
		if keep {
			_ = os.Remove(temp)
		}
	}()
	if _, err := file.Write(patched); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Chtimes(temp, info.ModTime(), info.ModTime()); err != nil {
		return err
	}
	const readOnly = uint32(0x1)
	if err := winfile.SetAttributes(temp, attrs&^readOnly); err != nil {
		return err
	}
	if attrs&readOnly != 0 {
		if err := winfile.SetAttributes(path, attrs&^readOnly); err != nil {
			return err
		}
	}
	if err := winfile.Replace(temp, path); err != nil {
		_ = winfile.SetAttributes(path, attrs)
		return err
	}
	if err := winfile.SetAttributes(path, attrs); err != nil {
		return err
	}
	keep = false
	return nil
}

func patchBytes(input []byte, wantedSection, wantedKey, value string) ([]byte, int, error) {
	wantedSectionBytes := []byte(wantedSection)
	wantedKeyBytes := []byte(wantedKey)
	result := make([]byte, 0, len(input)+32)
	inSection := false
	matches := 0
	for offset := 0; offset < len(input); {
		end := bytes.IndexByte(input[offset:], '\n')
		if end < 0 {
			end = len(input)
		} else {
			end = offset + end + 1
		}
		line := input[offset:end]
		contentEnd := len(line)
		if contentEnd > 0 && line[contentEnd-1] == '\n' {
			contentEnd--
		}
		if contentEnd > 0 && line[contentEnd-1] == '\r' {
			contentEnd--
		}
		content := line[:contentEnd]
		trimmed := bytes.TrimSpace(bytes.TrimPrefix(content, []byte{0xef, 0xbb, 0xbf}))
		if len(trimmed) >= 2 && trimmed[0] == '[' && trimmed[len(trimmed)-1] == ']' {
			inSection = asciiEqualFold(bytes.TrimSpace(trimmed[1:len(trimmed)-1]), wantedSectionBytes)
			result = append(result, line...)
			offset = end
			continue
		}
		if inSection && len(trimmed) > 0 && trimmed[0] != ';' && trimmed[0] != '#' {
			equals := bytes.IndexByte(content, '=')
			if equals >= 0 && asciiEqualFold(bytes.TrimSpace(content[:equals]), wantedKeyBytes) {
				valueStart := equals + 1
				for valueStart < contentEnd && (line[valueStart] == ' ' || line[valueStart] == '\t') {
					valueStart++
				}
				valueEnd := contentEnd
				for valueEnd > valueStart && (line[valueEnd-1] == ' ' || line[valueEnd-1] == '\t') {
					valueEnd--
				}
				result = append(result, line[:valueStart]...)
				result = append(result, value...)
				result = append(result, line[valueEnd:]...)
				matches++
				offset = end
				continue
			}
		}
		result = append(result, line...)
		offset = end
	}
	return result, matches, nil
}

func asciiEqualFold(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		a, b := left[i], right[i]
		if a >= 'A' && a <= 'Z' {
			a += 'a' - 'A'
		}
		if b >= 'A' && b <= 'Z' {
			b += 'a' - 'A'
		}
		if a != b {
			return false
		}
	}
	return true
}

func backupFile(source, destination string) error {
	if _, err := os.Stat(destination); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	raw, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return os.WriteFile(destination, raw, 0o600)
}
