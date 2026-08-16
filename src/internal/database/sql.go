package database

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

type SQLValidation struct {
	HasDropTable   bool
	ExpectedTables []string
}

var blockComments = regexp.MustCompile(`(?s)/\*.*?\*/`)
var createTable = regexp.MustCompile("(?i)\\bCREATE\\s+TABLE\\s+(?:IF\\s+NOT\\s+EXISTS\\s+)?(?:`([A-Za-z0-9_$]+)`|([A-Za-z0-9_$]+))\\s*\\(")
var mysqlClientCommand = regexp.MustCompile(`(?i)^(?:charset|clear|connect|delimiter|edit|exit|help|nopager|notee|nowarning|pager|print|prompt|quit|rehash|source|status|system|tee|warnings)(?:\s|;|$)`)

func ValidateSQL(path string, expectedSize int64, expectedSHA256 string) (SQLValidation, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return SQLValidation{}, err
	}
	if int64(len(raw)) != expectedSize {
		return SQLValidation{}, fmt.Errorf("SQL size mismatch")
	}
	sum := sha256.Sum256(raw)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), expectedSHA256) {
		return SQLValidation{}, fmt.Errorf("SQL SHA-256 mismatch")
	}
	rawText := string(raw)
	upperRaw := strings.ToUpper(rawText)
	if strings.Contains(upperRaw, "/*!") || strings.Contains(upperRaw, "/*M!") {
		return SQLValidation{}, fmt.Errorf("SQL contains an executable server comment")
	}
	text := blockComments.ReplaceAllString(rawText, " ")
	var cleaned []string
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimPrefix(strings.TrimSpace(line), "\ufeff")
		if strings.HasPrefix(trimmed, "--") || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, `\`) || mysqlClientCommand.MatchString(trimmed) {
			return SQLValidation{}, fmt.Errorf("SQL contains a mysql client command")
		}
		cleaned = append(cleaned, line)
	}
	text = strings.ToUpper(strings.Join(cleaned, "\n"))
	for _, required := range []string{"CREATE TABLE", "ACCOUNT", "LOGINNAME", "PASSWORD_HASH"} {
		if !strings.Contains(text, required) {
			return SQLValidation{}, fmt.Errorf("SQL is missing required token %s", required)
		}
	}
	for _, forbidden := range []string{
		"DROP DATABASE", "CREATE DATABASE", "CREATE USER", "ALTER USER", "DROP USER", "GRANT ",
		"REVOKE ", "SET PASSWORD", "INTO OUTFILE", "INTO DUMPFILE", "LOAD DATA",
		"USE ", "SHUTDOWN", "CREATE TRIGGER", "CREATE PROCEDURE", "CREATE FUNCTION",
		"CREATE VIEW", "CREATE EVENT", "SET GLOBAL", "SET @@GLOBAL", "INSTALL PLUGIN",
		"UNINSTALL PLUGIN", "DATA DIRECTORY", "INDEX DIRECTORY", "LOAD XML", "HANDLER ",
	} {
		if strings.Contains(text, forbidden) {
			return SQLValidation{}, fmt.Errorf("SQL contains forbidden statement %s", forbidden)
		}
	}
	matches := createTable.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return SQLValidation{}, fmt.Errorf("SQL does not contain a supported CREATE TABLE statement")
	}
	seen := make(map[string]struct{}, len(matches))
	tables := make([]string, 0, len(matches))
	for _, match := range matches {
		name := match[1]
		if name == "" {
			name = match[2]
		}
		name = strings.ToLower(name)
		if _, exists := seen[name]; exists {
			return SQLValidation{}, fmt.Errorf("SQL creates table %s more than once", name)
		}
		seen[name] = struct{}{}
		tables = append(tables, name)
	}
	sort.Strings(tables)
	if _, exists := seen["account"]; !exists {
		return SQLValidation{}, fmt.Errorf("SQL does not create the required account table")
	}
	return SQLValidation{HasDropTable: strings.Contains(text, "DROP TABLE"), ExpectedTables: tables}, nil
}
