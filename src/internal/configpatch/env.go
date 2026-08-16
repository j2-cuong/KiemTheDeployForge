package configpatch

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// BotEnvRelativePath is the bot configuration file rewritten during install.
const BotEnvRelativePath = "Bot/loginprobe.env"

// EnvRule is a single KEY=VALUE assignment enforced in a dotenv style file.
type EnvRule struct {
	RelativePath string
	Key          string
	Value        string
}

// BotEnvRules binds the bot to the installed server tree and to the local
// MySQL account created by Setup. BOT_GAMESERVER_DIR must be the absolute path
// of the installed Gameserver directory or the bot cannot find the server.
func BotEnvRules(installRoot, botUser, botPassword string) []EnvRule {
	gameserver := filepath.Join(installRoot, ServerTargetRoot, "Gameserver")
	return []EnvRule{
		{BotEnvRelativePath, "BOT_GAMESERVER_DIR", gameserver},
		{BotEnvRelativePath, "BOT_DB_HOST", "127.0.0.1"},
		{BotEnvRelativePath, "BOT_DB_PORT", "3306"},
		{BotEnvRelativePath, "BOT_DB_USER", botUser},
		{BotEnvRelativePath, "BOT_DB_PASSWORD", botPassword},
		{BotEnvRelativePath, "BOT_DB_NAME", "jxaccount"},
	}
}

// ApplyEnv writes every rule into its file under root, backing up the original
// once per file before the first change.
func ApplyEnv(root string, rules []EnvRule, backupRoot string) error {
	for _, rule := range rules {
		path := filepath.Join(root, filepath.FromSlash(rule.RelativePath))
		backup := filepath.Join(backupRoot, filepath.FromSlash(rule.RelativePath))
		if err := backupFile(path, backup); err != nil {
			return fmt.Errorf("backup %s: %w", rule.RelativePath, err)
		}
		if err := PatchEnv(path, rule.Key, rule.Value); err != nil {
			return fmt.Errorf("patch %s %s: %w", rule.RelativePath, rule.Key, err)
		}
	}
	return nil
}

// VerifyEnv reports drift without touching any file.
func VerifyEnv(root string, rules []EnvRule) error {
	for _, rule := range rules {
		path := filepath.Join(root, filepath.FromSlash(rule.RelativePath))
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", rule.RelativePath, err)
		}
		actual, active, err := envValue(raw, rule.Key)
		if err != nil {
			return fmt.Errorf("read %s %s: %w", rule.RelativePath, rule.Key, err)
		}
		if active != 1 || actual != rule.Value {
			return fmt.Errorf("config drift in %s %s", rule.RelativePath, rule.Key)
		}
	}
	return nil
}

// ValidateEnvTarget checks that a source env file can be patched
// unambiguously, without changing it.
func ValidateEnvTarget(path, key string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if _, _, err := envValue(raw, key); err != nil {
		return err
	}
	return nil
}

// PatchEnv sets key to value. An existing active assignment is rewritten in
// place; otherwise the first commented-out assignment is activated; otherwise
// the assignment is appended. Values must stay on one line.
func PatchEnv(path, key, value string) error {
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("invalid multiline value")
	}
	if strings.TrimSpace(key) != key || key == "" || strings.ContainsAny(key, "=\r\n") {
		return fmt.Errorf("invalid env key %q", key)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	patched, err := patchEnvBytes(original, key, value)
	if err != nil {
		return err
	}
	if bytes.Equal(original, patched) {
		return nil
	}
	return replaceContentPreserving(path, info, patched)
}

type envMatch struct {
	start  int // offset of the whole physical line, including any leading BOM
	end    int // offset just past the line terminator
	active bool
}

// scanEnv returns every line that assigns key, whether it is commented out or
// not, in file order.
func scanEnv(input []byte, key string) []envMatch {
	keyBytes := []byte(key)
	var matches []envMatch
	for offset := 0; offset < len(input); {
		end := bytes.IndexByte(input[offset:], '\n')
		if end < 0 {
			end = len(input)
		} else {
			end = offset + end + 1
		}
		content := trimLineEnding(input[offset:end])
		trimmed := bytes.TrimSpace(bytes.TrimPrefix(content, []byte{0xef, 0xbb, 0xbf}))
		active := true
		for len(trimmed) > 0 && (trimmed[0] == '#' || trimmed[0] == ';') {
			active = false
			trimmed = bytes.TrimSpace(trimmed[1:])
		}
		if equals := bytes.IndexByte(trimmed, '='); equals >= 0 {
			name := bytes.TrimSpace(trimmed[:equals])
			if bytes.Equal(name, keyBytes) {
				matches = append(matches, envMatch{start: offset, end: end, active: active})
			}
		}
		offset = end
	}
	return matches
}

// envValue returns the current value of the single active assignment and how
// many active assignments exist.
func envValue(input []byte, key string) (string, int, error) {
	matches := scanEnv(input, key)
	active := 0
	value := ""
	for _, match := range matches {
		if !match.active {
			continue
		}
		active++
		content := trimLineEnding(input[match.start:match.end])
		content = bytes.TrimPrefix(content, []byte{0xef, 0xbb, 0xbf})
		equals := bytes.IndexByte(content, '=')
		value = string(bytes.TrimSpace(content[equals+1:]))
	}
	if active > 1 {
		return "", active, fmt.Errorf("env key %s is assigned %d times", key, active)
	}
	return value, active, nil
}

func patchEnvBytes(input []byte, key, value string) ([]byte, error) {
	matches := scanEnv(input, key)
	activeCount := 0
	for _, match := range matches {
		if match.active {
			activeCount++
		}
	}
	if activeCount > 1 {
		return nil, fmt.Errorf("env key %s is assigned %d times", key, activeCount)
	}
	target := -1
	for i, match := range matches {
		if match.active {
			target = i
			break
		}
	}
	if target < 0 && len(matches) > 0 {
		target = 0
	}
	assignment := key + "=" + value
	if target < 0 {
		return appendEnvAssignment(input, assignment), nil
	}
	match := matches[target]
	line := input[match.start:match.end]
	terminator := line[len(trimLineEnding(line)):]
	if len(terminator) == 0 {
		terminator = dominantLineEnding(input)
	}
	result := make([]byte, 0, len(input)+len(assignment))
	result = append(result, input[:match.start]...)
	if bytes.HasPrefix(line, []byte{0xef, 0xbb, 0xbf}) {
		result = append(result, 0xef, 0xbb, 0xbf)
	}
	result = append(result, assignment...)
	result = append(result, terminator...)
	result = append(result, input[match.end:]...)
	return result, nil
}

func appendEnvAssignment(input []byte, assignment string) []byte {
	ending := dominantLineEnding(input)
	result := make([]byte, 0, len(input)+len(assignment)+2*len(ending))
	result = append(result, input...)
	if len(input) > 0 && input[len(input)-1] != '\n' {
		result = append(result, ending...)
	}
	result = append(result, assignment...)
	result = append(result, ending...)
	return result
}

func trimLineEnding(line []byte) []byte {
	end := len(line)
	if end > 0 && line[end-1] == '\n' {
		end--
	}
	if end > 0 && line[end-1] == '\r' {
		end--
	}
	return line[:end]
}

func dominantLineEnding(input []byte) []byte {
	if bytes.Contains(input, []byte("\r\n")) {
		return []byte("\r\n")
	}
	if bytes.Contains(input, []byte("\n")) {
		return []byte("\n")
	}
	return []byte("\r\n")
}
